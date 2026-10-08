package mobile

import (
	"strings"
	"testing"
	"time"
)

// Stop used to return as soon as it had cancelled the client, so a Run that
// started right after it could be rejected with "a client is already running"
// while the old client was still unwinding — and the error told the caller to
// do the Stop it had just done. Stop now waits for the unwind, which shows up
// as a clean state the moment it returns.
func TestStopWaitsUntilTheClientHasFinishedUnwinding(t *testing.T) {
	returned := make(chan error, 1)
	go func() { returned <- Run(minimalConfig) }()

	// The client dials 127.0.0.1:1, which fails at once, and settles into its
	// reconnect backoff.
	time.Sleep(300 * time.Millisecond)

	Stop()

	mu.Lock()
	stillSet := cancel != nil
	mu.Unlock()
	if stillSet {
		t.Fatal("Stop returned while the client was still unwinding (cancel is still set)")
	}

	// And the user-visible half of the same promise: a fresh Run reaches the
	// configuration parser instead of being told a client is running.
	err := Run("this is not toml ]")
	if err == nil {
		t.Fatal("an invalid configuration was accepted")
	}
	if strings.Contains(err.Error(), "already running") {
		t.Fatalf("a Run right after Stop was rejected as a second client: %v", err)
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("the stopped Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first Run did not return")
	}
}
