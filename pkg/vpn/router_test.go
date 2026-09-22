package vpn

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// --- channel transport --------------------------------------------------------

func TestChannelTransportSendsThroughItsFunction(t *testing.T) {
	var sent [][]byte
	transport := NewChannelTransport(func(packet []byte) error {
		sent = append(sent, append([]byte(nil), packet...))
		return nil
	})

	if err := transport.Send([]byte("one")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sent) != 1 || string(sent[0]) != "one" {
		t.Fatalf("the send function saw %q, want one packet holding one", sent)
	}

	// Deliver hands a packet to Receive.
	if !transport.Deliver([]byte("two")) {
		t.Fatal("Deliver refused a packet while the buffer was empty")
	}
	got, err := transport.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(got) != "two" {
		t.Fatalf("Receive returned %q, want two", got)
	}
	if transport.Buffered() != 0 {
		t.Errorf("Buffered() = %d after the packet was taken, want 0", transport.Buffered())
	}
}

func TestChannelTransportDropsWhenTheBufferIsFull(t *testing.T) {
	transport := NewChannelTransport(nil)
	for i := 0; i < DefaultReceiveBuffer; i++ {
		if !transport.Deliver([]byte{byte(i)}) {
			t.Fatalf("Deliver refused packet %d with the buffer not yet full", i)
		}
	}
	if transport.Deliver([]byte{0xff}) {
		t.Fatal("Deliver accepted a packet with the buffer full")
	}
	if got := transport.Buffered(); got != DefaultReceiveBuffer {
		t.Errorf("Buffered() = %d, want %d", got, DefaultReceiveBuffer)
	}
}

func TestChannelTransportCloseUnblocksAndRefuses(t *testing.T) {
	transport := NewChannelTransport(func([]byte) error { return nil })
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !transport.Closed() {
		t.Error("Closed() is false after Close")
	}
	if transport.Deliver([]byte("late")) {
		t.Error("Deliver accepted a packet after Close")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := transport.Receive(); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("Receive after Close returned %v, want io.ErrClosedPipe", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Receive did not return after Close")
	}
}

func TestChannelTransportWithoutASendFunction(t *testing.T) {
	transport := NewChannelTransport(nil)
	if err := transport.Send([]byte("x")); err == nil {
		t.Fatal("Send succeeded on a receive-only transport")
	}
}

func TestChannelTransportSendAfterCloseFails(t *testing.T) {
	transport := NewChannelTransport(func([]byte) error { return nil })
	_ = transport.Close()
	if err := transport.Send([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Send after Close returned %v, want io.ErrClosedPipe", err)
	}
}

// --- router -------------------------------------------------------------------

// captureTransport is a ChannelTransport whose Send records the packets, so a test
// can read what the router queued for a peer.
type captureTransport struct {
	*ChannelTransport
	sent chan []byte
}

func newCaptureTransport() *captureTransport {
	sent := make(chan []byte, 64)
	return &captureTransport{
		ChannelTransport: NewChannelTransport(func(packet []byte) error {
			sent <- append([]byte(nil), packet...)
			return nil
		}),
		sent: sent,
	}
}

// routerFixture is a router over one in-memory device with two peers attached.
type routerFixture struct {
	router    *Router
	device    *fakeDevice
	left      *captureTransport
	right     *captureTransport
	leftPeer  *Peer
	rightPeer *Peer
	cancel    context.CancelFunc
	done      chan error
}

func newRouterFixture(t *testing.T, mtu int) *routerFixture {
	t.Helper()

	device := newFakeDevice("tun-server", mtu)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	left := newCaptureTransport()
	right := newCaptureTransport()

	leftPeer, err := router.Add(left, net.ParseIP("10.7.0.2"))
	if err != nil {
		t.Fatalf("Add(left): %v", err)
	}
	rightPeer, err := router.Add(right, net.ParseIP("10.7.0.3"))
	if err != nil {
		t.Fatalf("Add(right): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Run(ctx) }()

	fixture := &routerFixture{
		router: router, device: device,
		left: left, right: right,
		leftPeer: leftPeer, rightPeer: rightPeer,
		cancel: cancel, done: done,
	}
	t.Cleanup(func() {
		cancel()
		if err := router.Close(); err != nil {
			t.Errorf("router.Close: %v", err)
		}
		select {
		case err := <-done:
			// Run reports ErrClosed when the router was closed before it started.
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("router.Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("router.Run did not return after Close")
		}
	})
	return fixture
}

// outbound reads what the router pushed through a peer's transport.
func outbound(t *testing.T, transport *captureTransport) []byte {
	t.Helper()
	return receivePacket(t, transport.sent)
}

func TestRouterSendsEachPacketToThePeerThatOwnsTheDestination(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	toLeft := ipv4Packet("10.7.0.1", "10.7.0.2", []byte("for left"))
	toRight := ipv4Packet("10.7.0.1", "10.7.0.3", []byte("for right"))
	fixture.device.incoming <- toLeft
	fixture.device.incoming <- toRight

	t.Logf("DEBUG left packet len=%d bytes=%x", len(func() []byte { return nil }()), 0)
	got := outbound(t, fixture.left)
	if got == nil {
		t.Fatal("the left peer received nothing")
	}
	if !Destination(got).Equal(net.ParseIP("10.7.0.2")) {
		t.Fatalf("the left peer received a packet for %s, want 10.7.0.2", Destination(got))
	}
	if !Source(got).Equal(net.ParseIP("10.7.0.1")) {
		t.Fatalf("the left peer received a packet from %s, want 10.7.0.1", Source(got))
	}
	if got := outbound(t, fixture.right); !Destination(got).Equal(net.ParseIP("10.7.0.3")) {
		t.Fatalf("the right peer received a packet for %s", Destination(got))
	}

	stats := fixture.router.Stats()
	if stats.Delivered != 2 {
		t.Errorf("Delivered = %d, want 2", stats.Delivered)
	}
	if stats.Peers != 2 {
		t.Errorf("Peers = %d, want 2", stats.Peers)
	}
	if stats.Unroutable != 0 {
		t.Errorf("Unroutable = %d, want 0", stats.Unroutable)
	}
}

func TestRouterDropsPacketsForAnUnknownDestination(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	fixture.device.incoming <- ipv4Packet("10.7.0.1", "10.7.0.9", []byte("nobody"))
	fixture.device.incoming <- ipv4Packet("10.7.0.1", "10.7.0.2", []byte("somebody"))
	// The second packet proves the first was handled rather than stuck.
	got := outbound(t, fixture.left)
	t.Logf("DEBUG left len=%d bytes=%x", len(got), got)
	if !Destination(got).Equal(net.ParseIP("10.7.0.2")) {
		t.Fatalf("the delivered packet is for %s, want 10.7.0.2", Destination(got))
	}

	waitFor(t, func() bool { return fixture.router.Stats().Unroutable == 1 },
		"the packet for an unattached address was not counted as unroutable")
}

func TestRouterWritesPeerTrafficToTheDevice(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	packet := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("hello from the left peer"))
	if !fixture.left.Deliver(packet) {
		t.Fatal("the left peer's transport refused the packet")
	}

	got := receivePacket(t, fixture.device.sent)
	if !Source(got).Equal(net.ParseIP("10.7.0.2")) {
		t.Fatalf("the device saw a packet from %s, want 10.7.0.2", Source(got))
	}
	// Two counters, two goroutines: the router counts the write and the peer counts the
	// packet it delivered, and one can lag the other. Wait for both before asserting, or
	// the check depends on which goroutine the scheduler ran first — which is how this
	// test failed under -race on a busy CI runner while passing here.
	waitFor(t, func() bool {
		return fixture.router.Stats().ToDevice == 1 && fixture.leftPeer.Stats().ToDevice == 1
	}, "the packet was not written to the device and counted by the peer")
	if got := fixture.leftPeer.Stats(); got.ToDevice != 1 {
		t.Errorf("the peer counted %d packets to the device, want 1", got.ToDevice)
	}
}

func TestRouterRejectsAPeerSendingAsAnotherAddress(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	// The left peer owns 10.7.0.2 but claims to be 10.7.0.3.
	forged := ipv4Packet("10.7.0.3", "10.7.0.1", []byte("spoofed"))
	honest := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("honest"))
	fixture.left.Deliver(forged)
	fixture.left.Deliver(honest)

	got := receivePacket(t, fixture.device.sent)
	if !Source(got).Equal(net.ParseIP("10.7.0.2")) {
		t.Fatalf("the device saw a forged packet from %s", Source(got))
	}
	waitFor(t, func() bool { return fixture.leftPeer.Stats().Dropped == 1 },
		"the forged packet was not counted as dropped")
}

func TestRouterRejectsMalformedAndOversizedPacketsFromAPeer(t *testing.T) {
	fixture := newRouterFixture(t, 600)

	fixture.left.Deliver([]byte{0x70, 0x00, 0x00, 0x00})
	fixture.left.Deliver(ipv4Packet("10.7.0.2", "10.7.0.1", make([]byte, 900)))
	good := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("ok"))
	fixture.left.Deliver(good)

	if got := receivePacket(t, fixture.device.sent); len(got) != len(good) {
		t.Fatalf("the device received %d bytes, want the %d-byte good packet", len(got), len(good))
	}
	waitFor(t, func() bool { return fixture.leftPeer.Stats().Dropped == 2 },
		"the malformed and oversized packets were not both dropped")
}

func TestRouterRefusesASecondPeerOnTheSameAddress(t *testing.T) {
	device := newFakeDevice("tun-server", 1500)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()

	if _, err := router.Add(NewChannelTransport(nil), net.ParseIP("10.7.0.2")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := router.Add(NewChannelTransport(nil), net.ParseIP("10.7.0.2")); err == nil {
		t.Fatal("two peers were attached to the same address")
	}
	if _, err := router.Add(nil, net.ParseIP("10.7.0.4")); err == nil {
		t.Fatal("a peer without a transport was attached")
	}
	if _, err := router.Add(NewChannelTransport(nil), nil); err == nil {
		t.Fatal("a peer without an address was attached")
	}
}

func TestRouterDetachesAPeerWhoseTransportFails(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	// Closing the peer's transport is what a dropped session looks like.
	_ = fixture.left.Close()

	waitFor(t, func() bool { return fixture.router.PeerCount() == 1 },
		"the failed peer was not detached")
	if _, ok := fixture.router.PeerFor(net.ParseIP("10.7.0.2")); ok {
		t.Error("the detached peer is still reachable by address")
	}
	if _, ok := fixture.router.PeerFor(net.ParseIP("10.7.0.3")); !ok {
		t.Error("the surviving peer was detached too")
	}
}

func TestRouterRemoveReleasesTheAddress(t *testing.T) {
	fixture := newRouterFixture(t, 1500)

	fixture.router.Remove(net.ParseIP("10.7.0.2"))
	waitFor(t, func() bool { return fixture.router.PeerCount() == 1 },
		"Remove did not detach the peer")

	// The address can be attached again once it is free.
	replacement := NewChannelTransport(nil)
	if _, err := fixture.router.Add(replacement, net.ParseIP("10.7.0.2")); err != nil {
		t.Fatalf("Add after Remove: %v", err)
	}
	// Removing an address that is not attached is a no-op.
	fixture.router.Remove(net.ParseIP("10.7.0.99"))
}

func TestRouterDropsForAPeerThatIsNotDraining(t *testing.T) {
	device := newFakeDevice("tun-server", 1500)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()

	// A transport whose Send never returns leaves the peer's send loop stuck, so the
	// outbound queue fills and the router starts dropping.
	blocking := newBlockingTransport()
	peer, err := router.Add(blocking, net.ParseIP("10.7.0.2"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = router.Run(ctx) }()
	waitFor(t, func() bool { return router.PeerCount() == 1 }, "the peer was not attached")

	// Far more packets than the peer can hold: one is in flight through Send, the
	// rest fill its queue, and everything beyond that must be dropped.
	for i := 0; i < DefaultReceiveBuffer*3; i++ {
		device.incoming <- ipv4Packet("10.7.0.1", "10.7.0.2", []byte("flood"))
	}

	waitFor(t, func() bool { return peer.Stats().Dropped >= DefaultReceiveBuffer },
		"the router kept queuing to a peer that was not draining")
}

// blockingTransport never completes a Send and never delivers a packet, so it models
// a peer that has stopped reading.
type blockingTransport struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{release: make(chan struct{})}
}

func (t *blockingTransport) Send([]byte) error {
	<-t.release
	return io.ErrClosedPipe
}

func (t *blockingTransport) Receive() ([]byte, error) {
	<-t.release
	return nil, io.ErrClosedPipe
}

func (t *blockingTransport) Close() error {
	t.once.Do(func() { close(t.release) })
	return nil
}

func TestRouterRunStopsOnCancellation(t *testing.T) {
	device := newFakeDevice("tun-server", 1500)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Run(ctx) }()

	// The read is blocked in the device, so cancellation is only noticed once the
	// device is closed; that is why Run is stopped by closing rather than by
	// cancelling alone.
	cancel()
	_ = device.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
}

func TestRouterCloseIsIdempotentAndStopsPeers(t *testing.T) {
	device := newFakeDevice("tun-server", 1500)
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	transport := NewChannelTransport(nil)
	if _, err := router.Add(transport, net.ParseIP("10.7.0.2")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := router.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if router.PeerCount() != 0 {
		t.Errorf("PeerCount() = %d after Close, want 0", router.PeerCount())
	}
	if !transport.Closed() {
		t.Error("the peer's transport was not closed")
	}
	if _, err := router.Add(NewChannelTransport(nil), net.ParseIP("10.7.0.5")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Add after Close returned %v, want ErrClosed", err)
	}
	if err := router.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close returned %v, want ErrClosed", err)
	}
}

func TestNewRouterValidatesItsArguments(t *testing.T) {
	if _, err := NewRouter(nil, Options{}); err == nil {
		t.Error("a router without a device was accepted")
	}
	device := newFakeDevice("tun-server", 1500)
	if _, err := NewRouter(device, Options{MTU: 100}); err == nil {
		t.Error("a router with an MTU below the minimum was accepted")
	}
	if _, err := NewRouter(device, Options{MTU: 1600}); err == nil {
		t.Error("a router with an MTU above the device's was accepted")
	}
	router, err := NewRouter(device, Options{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer router.Close()
	if router.MTU() != 1500 {
		t.Errorf("MTU() = %d, want the device's 1500", router.MTU())
	}
	if router.Device() != device {
		t.Error("Device() did not return the device the router was built with")
	}
}

// TestRouterWithoutADeviceOnThisPlatform checks that building a router for a real
// interface reports the platform limitation instead of pretending to work.
func TestRouterWithoutADeviceOnThisPlatform(t *testing.T) {
	device, err := Open("", 0)
	if err == nil {
		_ = device.Close()
		return
	}
	if runtime.GOOS == "linux" {
		t.Skipf("cannot open a tun device here: %v", err)
	}
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("Open failed with %v, want ErrNoDevice on %s", err, runtime.GOOS)
	}
}
