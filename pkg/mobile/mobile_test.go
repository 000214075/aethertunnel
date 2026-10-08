package mobile

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

const minimalConfig = `
[client]
server_addr = "127.0.0.1:1"
auth_token = "mobile-test-token-0123456789ab"
`

func TestRunRejectsAnInvalidConfiguration(t *testing.T) {
	err := Run("this is not toml ]")
	if err == nil {
		t.Fatal("Run accepted a configuration that does not parse")
	}
	if !strings.Contains(err.Error(), "parse config embedded") {
		t.Fatalf("error %q does not name the embedded configuration", err)
	}
}

func TestStopEndsARunningClient(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- Run(minimalConfig)
	}()

	// The client dials 127.0.0.1:1, which fails at once, and then waits out the
	// reconnect backoff. Stop has to cut the wait short and end Run with nil.
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before Stop was called", err)
	case <-time.After(300 * time.Millisecond):
	}

	Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

func TestStopWithoutARunningClientIsHarmless(t *testing.T) {
	Stop()
}

type stubShell struct {
	openTunCalls int
}

func (s *stubShell) OpenTun(mtu int32, address string, prefix int32, subnet string) (int32, error) {
	s.openTunCalls++
	return -1, errors.New("OpenTun should not run before a session is up")
}

func (s *stubShell) ProtectSocket(fd int32) {}

func TestRunVPNRejectsAConfigWithoutAVPNSection(t *testing.T) {
	err := RunVPN(minimalConfig, &stubShell{})
	if err == nil || !strings.Contains(err.Error(), "[vpn]") {
		t.Fatalf("error is %v, want a refusal naming [vpn]", err)
	}
}

const vpnConfig = `
[client]
server_addr = "127.0.0.1:1"
auth_token = "mobile-test-token-0123456789ab"

[vpn]
enabled = true
`

func TestRunVPNAsksTheShellForTheInterfaceOnlyWhenTheSessionIsUp(t *testing.T) {
	// The client is dialing 127.0.0.1:1, which fails at once, so no session ever
	// comes up and the shell must never be asked for an interface: the address it
	// needs is assigned by the server, and there is no server here.
	shell := &stubShell{}
	done := make(chan error, 1)
	go func() {
		done <- RunVPN(vpnConfig, shell)
	}()

	select {
	case err := <-done:
		t.Fatalf("RunVPN returned %v before Stop was called", err)
	case <-time.After(300 * time.Millisecond):
	}
	if shell.openTunCalls != 0 {
		t.Fatalf("OpenTun ran %d times before any session existed", shell.openTunCalls)
	}

	Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunVPN after Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunVPN did not return after Stop")
	}
}

// fdShell hands over a descriptor the test opened, which is what the Android
// VpnService.establish path does.
type fdShell struct {
	read *os.File
}

func (s *fdShell) OpenTun(mtu int32, address string, prefix int32, subnet string) (int32, error) {
	return int32(s.read.Fd()), nil
}

func (s *fdShell) ProtectSocket(fd int32) {}

// NewFromFD owns the descriptor only when it succeeds. On the failure path the
// shell's descriptor used to be dropped, so a pipe's write end kept succeeding
// for the life of the app. An MTU past vpn.MaxMTU is the cheapest way to fail
// with a live descriptor.
func TestAFailedTunnelSetupClosesTheShellDescriptor(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer write.Close()

	if _, err := openShellTun(&fdShell{read: read}, vpn.MaxMTU+1, "10.0.0.2", 24, "10.0.0.0/24"); err == nil {
		t.Fatal("openShellTun accepted an MTU outside the range")
	}

	// With the read end still open the pipe buffers the byte and Write returns
	// nil; once it is closed the write fails.
	drained := make(chan error, 1)
	go func() {
		write.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := write.Write([]byte("x"))
		drained <- err
	}()
	select {
	case err := <-drained:
		if err == nil {
			t.Fatal("the descriptor the shell opened was left open after a failed setup")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write to the pipe never returned")
	}
}
