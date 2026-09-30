package mobile

import (
	"strings"
	"testing"
	"time"
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
