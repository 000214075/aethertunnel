package net

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

// TestListenCauseNamesTheConditionsOnBothPlatforms covers the two conditions that make
// up almost every refusal to bind. The message under test is the one an operator sees
// when the port will not open, and the two platforms render the same failure very
// differently: Unix says "bind: address already in use", Windows says
// "bind: winapi error #10048" — a number to look up. ListenCause exists so that the
// phrase does not depend on the platform's wording, which is why the tests below feed
// the syscall errors directly instead of going through the network stack.
func TestListenCauseNamesTheConditionsOnBothPlatforms(t *testing.T) {
	taken := &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
	if got := ListenCause(taken); !strings.Contains(got, "already in use") {
		t.Errorf("an address in use is not named: %q", got)
	}

	// A port below 1024 without the privilege, and on Windows a port that is reserved
	// or excluded from the dynamic range, reads as EACCES.
	privileged := &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EACCES}
	if got := ListenCause(privileged); !strings.Contains(got, "permission denied") {
		t.Errorf("a permission failure is not named: %q", got)
	}

	// An address this host does not have: the third of the recognisable cases.
	noaddr := &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRNOTAVAIL}
	if got := ListenCause(noaddr); !strings.Contains(got, "no such address") {
		t.Errorf("an unavailable address is not named: %q", got)
	}

	// Anything else keeps net's own text: the helper must not invent a cause.
	other := errors.New("something else entirely")
	if got := ListenCause(other); got != "" {
		t.Errorf("an unrecognised error was given a cause: %q", got)
	}
	if got := ListenError("127.0.0.1:1", other); got.Error() !=
		"listen on 127.0.0.1:1: something else entirely" {
		t.Errorf("an unrecognised error lost net's own text: %q", got.Error())
	}
}

// TestListenErrorNamesTheAddress checks the wrapping: the message has to carry the
// address that was asked for, because that is what the operator looks up in the
// configuration, and the cause phrase has to be there in place of the platform's
// number.
func TestListenErrorNamesTheAddress(t *testing.T) {
	taken := &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
	want := "listen on 127.0.0.1:34807: the address is already in use (another process is listening on it)"
	if got := ListenError("127.0.0.1:34807", taken).Error(); got != want {
		t.Errorf("ListenError = %q, want %q", got, want)
	}
}
