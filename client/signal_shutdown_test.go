package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signalHelperEnv marks the re-executed test binary as the child process
// TestASecondInterruptEndsAStuckShutdown starts. Without it the helper test below
// does nothing in an ordinary run.
const signalHelperEnv = "AETHERTUNNEL_SIGNAL_HELPER"

// TestSignalHelperProcess is the body of that child process, not a test of its
// own: it arms the shutdown signals the way run does, reports itself ready, and
// then behaves like a shutdown stuck in the drain.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv(signalHelperEnv) != "1" {
		t.Skip("this is the child of TestASecondInterruptEndsAStuckShutdown")
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
// shutdown was stuck had nothing left but SIGKILL. The client needs this as much
// as the server does: the second signal is sent to a real process here, because
// what the fix restores is that process's default action.
func TestASecondInterruptEndsAStuckShutdown(t *testing.T) {
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
