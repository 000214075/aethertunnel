package dht

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A read loop that gives up has to take the node down with it. The node was kept
// open before — closed still false, the socket still bound, cancel never called —
// so the refresh loop and bootstrap kept running and sendTo kept writing into a
// socket nobody read, while every holder still saw a live node. retire closes it,
// and the socket being closed is also what unblocked this loop.
func TestAGivingUpReaderRetiresTheNode(t *testing.T) {
	tab := newTable(t)

	// Close the socket underneath the node, which is what it cannot come back
	// from: every read fails at once and the give-up threshold is reached in
	// microseconds, well inside the wait below.
	tab.mu.Lock()
	conn := tab.conn
	tab.mu.Unlock()
	if conn == nil {
		t.Fatal("the node has no socket to close")
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for !tab.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("the node is still open after its reader gave up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The node reports itself gone instead of timing out on every request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tab.Ping(ctx, "127.0.0.1:1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping on a retired node = %v, want ErrClosed", err)
	}
	if _, err := tab.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start on a retired node = %v, want ErrClosed", err)
	}
	// Close stays idempotent: it is the normal path after a give-up.
	if err := tab.Close(); err != nil {
		t.Fatalf("Close after a give-up: %v", err)
	}
}
