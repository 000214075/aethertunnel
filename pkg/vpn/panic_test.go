package vpn

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// panicDevice is the in-memory device with a Read that trips over the packet it was
// handed, which is how these tests stand in for a handler that panics on one input.
type panicDevice struct {
	*fakeDevice
}

func (d *panicDevice) Read([]byte) (int, error) { panic("device read") }

// panicTransport is a peer whose receive direction panics, which is what a packet a
// handler cannot cope with looks like from the router's side.
type panicTransport struct{}

func (t *panicTransport) Send([]byte) error        { return nil }
func (t *panicTransport) Receive() ([]byte, error) { panic("peer receive") }
func (t *panicTransport) Close() error             { return nil }

// TestRouterRunSurvivesADevicePanic covers the guard on the router's own read loop.
// The loop runs on a goroutine the router owns, not on one belonging to a connection,
// so the guard a control connection has (Server.handleConn) cannot reach it: without
// one, a panic here would end the process, and the caller waiting on Run's result
// would wait forever.
func TestRouterRunSurvivesADevicePanic(t *testing.T) {
	device := &panicDevice{fakeDevice: newFakeDevice("tun-panic", 1500)}
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close() })

	done := make(chan error, 1)
	go func() { done <- router.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after the device panicked")
		}
		// The error names what happened, so an operator reading the log is not left
		// wondering why the tunnel stopped.
		if !strings.Contains(err.Error(), "panicked") {
			t.Errorf("Run returned %v, want an error naming the panic", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run neither returned nor panicked, so the caller would wait forever")
	}

	if got := router.Stats().Errors; got == 0 {
		t.Error("the panic was not counted as an error")
	}
}

// TestPeerLoopPanicDetachesOnlyThatPeer covers the guard on a peer's two directions.
// They run on goroutines of their own, so a packet one client sent that trips the
// receive path must cost that client its peer and nothing else: the router keeps its
// other peers, and the address becomes free for a later session.
func TestPeerLoopPanicDetachesOnlyThatPeer(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close() })

	// A healthy peer on another address, which must survive the other one's panic.
	healthy := newFakeTransport()
	if _, err := router.Add(healthy, net.ParseIP("10.7.0.2")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	address := net.ParseIP("10.7.0.3")
	if _, err := router.Add(&panicTransport{}, address); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := router.PeerCount(); got != 2 {
		t.Fatalf("PeerCount() = %d after two adds, want 2", got)
	}

	waitFor(t, func() bool { return router.PeerCount() == 1 }, "the panicking peer was not detached")
	if _, ok := router.PeerFor(address); ok {
		t.Error("the address of the detached peer is still taken")
	}
	if _, ok := router.PeerFor(net.ParseIP("10.7.0.2")); !ok {
		t.Error("the healthy peer was detached along with the panicking one")
	}
	if got := router.Stats().Errors; got == 0 {
		t.Error("the panic was not counted as an error")
	}

	// The address is free again, so the next session that is given it can attach.
	if _, err := router.Add(newFakeTransport(), address); err != nil {
		t.Fatalf("the freed address could not be taken again: %v", err)
	}
}
