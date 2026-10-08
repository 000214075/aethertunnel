package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// freeTCPPort reserves a loopback TCP port and releases it, the pattern the
// package tests hand to a configuration that names its own listeners.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a TCP port: %v", err)
	}
	port := portOfAddress(t, listener.Addr())
	_ = listener.Close()
	return port
}

func portOfAddress(t *testing.T, addr net.Addr) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port of %q: %v", addr, err)
	}
	return number
}

// serverConfigWithListeners is a valid server configuration whose control port is
// the one given and whose panel and metrics endpoint each have an address of
// their own.
func serverConfigWithListeners(controlPort, dashboardPort, metricsPort int) string {
	return fmt.Sprintf(`
[server]
bind_addr = "127.0.0.1"
bind_port = %d
auth_token = "0123456789abcdef0123456789abcdef"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = %d

[metrics]
enabled = true
bind_addr = "127.0.0.1"
port = %d
`, controlPort, dashboardPort, metricsPort)
}

// A Run that fails to start used to return from run() before the dashboard and the
// metrics listener were stopped: only the path where Run returned nil reached the
// two Stop calls. The published binary exits on the next line, so the window there
// is small, but run() is called in-process by this package's tests, and there the
// two listeners kept their ports and kept answering on the stores Run had already
// closed.
func TestAFailedRunStopsTheDashboardAndMetricsListeners(t *testing.T) {
	// An occupied control port is the cheapest Run failure: New succeeds, the
	// panel and the metrics endpoint start, and the listen itself is refused.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a control port: %v", err)
	}
	defer blocker.Close()
	controlPort := portOfAddress(t, blocker.Addr())

	dashboardPort := freeTCPPort(t)
	metricsPort := freeTCPPort(t)
	path := writeServerConfig(t, serverConfigWithListeners(controlPort, dashboardPort, metricsPort))

	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-server", []string{"-config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run with an occupied control port returned %d, want 1 (stderr %q)", code, stderr.String())
	}

	// A port that can be bound again is the observable form of "the listener was
	// stopped"; on the old code both are still held by the failed run.
	for _, listener := range []struct {
		name string
		port int
	}{
		{"dashboard", dashboardPort},
		{"metrics", metricsPort},
	} {
		conn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listener.port)))
		if err != nil {
			t.Errorf("the %s listener still holds port %d after a failed Run: %v", listener.name, listener.port, err)
			continue
		}
		_ = conn.Close()
	}
}

// signalHelperEnv marks the re-executed test binary as the child process
// TestASecondInterruptEndsAShutdownThatIsStuck starts. Without it the helper test
// below does nothing in an ordinary run.
const signalHelperEnv = "AETHERTUNNEL_SIGNAL_HELPER"

// TestSignalHelperProcess is the body of that child process, not a test of its
// own: it arms the shutdown signals the way run does, reports itself ready, and
// then behaves like a shutdown stuck in the drain.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv(signalHelperEnv) != "1" {
		t.Skip("this is the child of TestASecondInterruptEndsAShutdownThatIsStuck")
	}
	ctx, stop := notifyShutdown()
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
	// Well past the parent's deadline: a swallowed second interrupt has to leave
	// this process alive long enough for the parent to see it.
	time.Sleep(30 * time.Second)
	os.Exit(3)
}

// signal.NotifyContext stops relaying after the first signal but leaves the
// registration installed, so a second Ctrl-C went to a channel nobody reads and
// the default action — terminating the process — never happened. An operator whose
// shutdown was stuck in the drain had nothing left but SIGKILL. The second signal
// is sent to a real process here, because what the fix restores is that process's
// default action, which cannot be observed in the test's own.
func TestASecondInterruptEndsAShutdownThatIsStuck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the default action of a second interrupt is a POSIX behaviour")
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSignalHelperProcess")
	cmd.Env = append(os.Environ(), signalHelperEnv+"=1")
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// The first interrupt must not arrive before the registration exists.
	ready := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if strings.Contains(line, "ready") {
				ready <- nil
				return
			}
			if err != nil {
				ready <- err
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("the helper never reported itself ready: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper never reported itself ready")
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send the first interrupt: %v", err)
	}
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()

	// The helper's shutdown never finishes on its own, so the interrupt is repeated
	// until the process ends: with the registration still installed every one of
	// them is swallowed, and once it is dropped the next one kills the process.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-exit:
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("the helper ended with %v, want it killed by an interrupt", err)
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
				t.Fatalf("the helper ended with %v, want the second interrupt's default action", err)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the shutdown stayed stuck through repeated interrupts: the second signal was swallowed")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("send a repeated interrupt: %v", err)
		}
	}
}
