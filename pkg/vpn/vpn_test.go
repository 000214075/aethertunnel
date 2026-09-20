package vpn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- test doubles -------------------------------------------------------------

// fakeDevice is an in-memory layer-3 interface. Close unblocks a pending Read, which
// is the contract a real tun device satisfies by being closed.
type fakeDevice struct {
	name     string
	mtu      int
	incoming chan []byte
	sent     chan []byte

	mu      sync.Mutex
	closed  bool
	address string

	// failRead and failWrite inject I/O errors.
	failRead  error
	failWrite error
}

func newFakeDevice(name string, mtu int) *fakeDevice {
	return &fakeDevice{
		name:     name,
		mtu:      mtu,
		incoming: make(chan []byte, 64),
		sent:     make(chan []byte, 64),
	}
}

func (d *fakeDevice) Name() string { return d.name }
func (d *fakeDevice) MTU() int     { return d.mtu }

func (d *fakeDevice) Read(packet []byte) (int, error) {
	d.mu.Lock()
	fail := d.failRead
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	if fail != nil {
		return 0, fail
	}

	next, ok := <-d.incoming
	if !ok {
		return 0, io.ErrClosedPipe
	}
	if len(next) > len(packet) {
		return 0, io.ErrShortBuffer
	}
	return copy(packet, next), nil
}

func (d *fakeDevice) Write(packet []byte) (int, error) {
	d.mu.Lock()
	fail := d.failWrite
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	if fail != nil {
		return 0, fail
	}
	copied := append([]byte(nil), packet...)
	select {
	case d.sent <- copied:
	default:
	}
	return len(packet), nil
}

func (d *fakeDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		close(d.incoming)
	}
	return nil
}

// SetAddress records the address, so a test can check what was assigned.
func (d *fakeDevice) SetAddress(address string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.address = address
	return nil
}

func (d *fakeDevice) assigned() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.address
}

// fakeTransport carries packets over two channels, which is what a connection would
// carry them over.
type fakeTransport struct {
	out      chan []byte
	in       chan []byte
	closed   chan struct{}
	closeOne sync.Once

	// failSend is returned by Send without touching out.
	failSend error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		out:    make(chan []byte, 64),
		in:     make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (t *fakeTransport) Send(packet []byte) error {
	if t.failSend != nil {
		return t.failSend
	}
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	case t.out <- append([]byte(nil), packet...):
		return nil
	}
}

func (t *fakeTransport) Receive() ([]byte, error) {
	select {
	case <-t.closed:
		return nil, io.ErrClosedPipe
	case packet := <-t.in:
		return packet, nil
	}
}

func (t *fakeTransport) Close() error {
	t.closeOne.Do(func() { close(t.closed) })
	return nil
}

// link joins two transports so a packet sent on one arrives on the other.
func link(a, b *fakeTransport) {
	go func() {
		for {
			select {
			case <-a.closed:
				return
			case <-b.closed:
				return
			case packet := <-a.out:
				select {
				case b.in <- packet:
				case <-b.closed:
					return
				}
			}
		}
	}()
	go func() {
		for {
			select {
			case <-a.closed:
				return
			case <-b.closed:
				return
			case packet := <-b.out:
				select {
				case a.in <- packet:
				case <-a.closed:
					return
				}
			}
		}
	}()
}

// ipv4Packet builds a well-formed IPv4 packet with a correct header checksum.
func ipv4Packet(source, destination string, payload []byte) []byte {
	total := 20 + len(payload)
	packet := make([]byte, total)
	packet[0] = 0x45 // version 4, 5 words of header
	packet[1] = 0    // DSCP and ECN
	packet[2] = byte(total >> 8)
	packet[3] = byte(total)
	packet[4], packet[5] = 0x12, 0x34 // identification
	packet[6], packet[7] = 0x40, 0x00 // do not fragment
	packet[8] = 64                    // TTL
	packet[9] = 17                    // UDP
	copy(packet[12:16], net.ParseIP(source).To4())
	copy(packet[16:20], net.ParseIP(destination).To4())
	copy(packet[20:], payload)
	sum := Checksum(packet[:20])
	packet[10] = byte(sum >> 8)
	packet[11] = byte(sum)
	return packet
}

// ipv6Packet builds a minimal well-formed IPv6 packet.
func ipv6Packet(source, destination string, payload []byte) []byte {
	packet := make([]byte, IPv6HeaderLen+len(payload))
	packet[0] = 0x60
	length := len(payload)
	packet[4] = byte(length >> 8)
	packet[5] = byte(length)
	packet[6] = 17 // next header: UDP
	packet[7] = 64 // hop limit
	copy(packet[8:24], net.ParseIP(source).To16())
	copy(packet[24:40], net.ParseIP(destination).To16())
	copy(packet[40:], payload)
	return packet
}

// --- header parsing -----------------------------------------------------------

func TestVersion(t *testing.T) {
	cases := []struct {
		name   string
		packet []byte
		want   int
	}{
		{"empty", nil, 0},
		{"ipv4", []byte{0x45, 0, 0, 20}, 4},
		{"ipv6", []byte{0x60, 0, 0, 0}, 6},
		{"nonsense", []byte{0xf0, 0, 0, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Version(tc.packet); got != tc.want {
				t.Fatalf("Version = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseIPv4ReadsTheHeader(t *testing.T) {
	packet := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("hello"))
	header, err := ParseIPv4(packet)
	if err != nil {
		t.Fatalf("ParseIPv4: %v", err)
	}
	if header.HeaderLen != 20 {
		t.Errorf("HeaderLen = %d, want 20", header.HeaderLen)
	}
	if header.TotalLen != len(packet) {
		t.Errorf("TotalLen = %d, want %d", header.TotalLen, len(packet))
	}
	if header.Protocol != 17 {
		t.Errorf("Protocol = %d, want 17", header.Protocol)
	}
	if !header.Source.Equal(net.ParseIP("10.7.0.2")) {
		t.Errorf("Source = %s, want 10.7.0.2", header.Source)
	}
	if !header.Destination.Equal(net.ParseIP("10.7.0.1")) {
		t.Errorf("Destination = %s, want 10.7.0.1", header.Destination)
	}
	if header.TTL != 64 {
		t.Errorf("TTL = %d, want 64", header.TTL)
	}
}

func TestParseIPv4RejectsMalformedHeaders(t *testing.T) {
	good := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("hello"))

	cases := []struct {
		name   string
		packet []byte
	}{
		{"not ipv4", append([]byte{0x60}, good[1:]...)},
		{"shorter than a header", good[:12]},
		{"header length field below the minimum", func() []byte { p := append([]byte(nil), good...); p[0] = 0x44; return p }()},
		{"header length field above the maximum", func() []byte { p := append([]byte(nil), good...); p[0] = 0x4f; return p }()},
		{"header length beyond the packet", func() []byte {
			p := append([]byte(nil), good[:20]...)
			p[0] = 0x46 // six words of header in a packet that holds only five
			return p
		}()},
		{"total length softer than the header", func() []byte { p := append([]byte(nil), good...); p[2], p[3] = 0, 10; return p }()},
		{"total length beyond the packet", func() []byte { p := append([]byte(nil), good...); p[2], p[3] = 0xff, 0xff; return p }()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseIPv4(tc.packet); !errors.Is(err, ErrMalformedPacket) {
				t.Fatalf("ParseIPv4 returned %v, want ErrMalformedPacket", err)
			}
		})
	}
}

func TestValidateEnforcesTheMTU(t *testing.T) {
	packet := ipv4Packet("10.7.0.2", "10.7.0.1", make([]byte, 100))

	if err := Validate(packet, 1280); err != nil {
		t.Fatalf("a 120-byte packet was rejected against a 1280-byte MTU: %v", err)
	}
	if err := Validate(packet, 100); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("Validate against a 100-byte MTU returned %v, want ErrPacketTooLarge", err)
	}
	// Zero disables the check, which is what a caller that has already bounded the
	// packet itself wants.
	if err := Validate(packet, 0); err != nil {
		t.Fatalf("MTU 0 should not bound the packet: %v", err)
	}
}

func TestValidateAcceptsIPv6AndRejectsNonsense(t *testing.T) {
	packet := ipv6Packet("2001:db8::2", "2001:db8::1", []byte("payload"))
	if err := Validate(packet, 1500); err != nil {
		t.Fatalf("a well-formed IPv6 packet was rejected: %v", err)
	}
	if err := Validate(packet, 30); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("an oversized IPv6 packet returned %v, want ErrPacketTooLarge", err)
	}

	if err := Validate([]byte{0x70, 0x00, 0x00, 0x00}, 1500); !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("a packet that is neither version returned %v, want ErrMalformedPacket", err)
	}
	if err := Validate([]byte{0x60, 0, 0, 0, 0, 8}, 1500); !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("a short IPv6 packet returned %v, want ErrMalformedPacket", err)
	}
	// An IPv6 length field that exceeds the bytes present is malformed.
	truncated := ipv6Packet("2001:db8::2", "2001:db8::1", []byte("payload"))
	truncated[4], truncated[5] = 0xff, 0xff
	if err := Validate(truncated, 1500); !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("an IPv6 packet claiming more bytes than it holds returned %v, want ErrMalformedPacket", err)
	}
}

func TestSourceAndDestination(t *testing.T) {
	v4 := ipv4Packet("10.7.0.2", "10.7.0.1", nil)
	if got := Source(v4); !got.Equal(net.ParseIP("10.7.0.2")) {
		t.Errorf("Source(ipv4) = %s, want 10.7.0.2", got)
	}
	if got := Destination(v4); !got.Equal(net.ParseIP("10.7.0.1")) {
		t.Errorf("Destination(ipv4) = %s, want 10.7.0.1", got)
	}

	v6 := ipv6Packet("2001:db8::2", "2001:db8::1", nil)
	if got := Source(v6); !got.Equal(net.ParseIP("2001:db8::2")) {
		t.Errorf("Source(ipv6) = %s, want 2001:db8::2", got)
	}
	if got := Destination(v6); !got.Equal(net.ParseIP("2001:db8::1")) {
		t.Errorf("Destination(ipv6) = %s, want 2001:db8::1", got)
	}

	// An unparseable packet yields no address rather than a wrong one.
	if got := Destination([]byte{0x70}); got != nil {
		t.Errorf("Destination of a nonsense packet = %s, want nil", got)
	}
	if got := Source(append([]byte{0x45}, make([]byte, 20)...)); got != nil {
		t.Errorf("Source of a truncated IPv4 packet = %s, want nil", got)
	}
}

func TestChecksum(t *testing.T) {
	// RFC 1071's worked example, with the checksum field zeroed.
	data := []byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}
	if got := Checksum(data); got != 0x220d {
		t.Errorf("Checksum = %#04x, want 0x220d", got)
	}
	if got := Checksum(nil); got != 0xffff {
		t.Errorf("Checksum of an empty slice = %#04x, want 0xffff", got)
	}
	// An odd-length slice is zero-padded on the right.
	if got, want := Checksum([]byte{0x01}), Checksum([]byte{0x01, 0x00}); got != want {
		t.Errorf("Checksum of an odd slice = %#04x, want %#04x", got, want)
	}
}

func TestSetIPv4ChecksumRepairsAHeader(t *testing.T) {
	packet := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("payload"))
	copy(packet[16:20], net.ParseIP("10.7.0.9").To4())

	if Checksum(packet[:20]) == 0 {
		t.Fatal("the header still sums to zero after the address changed, so the test proves nothing")
	}
	if err := SetIPv4Checksum(packet); err != nil {
		t.Fatalf("SetIPv4Checksum: %v", err)
	}
	if sum := Checksum(packet[:20]); sum != 0 {
		t.Fatalf("the repaired header sums to %#04x, want 0", sum)
	}
	if err := SetIPv4Checksum([]byte{0x70, 0, 0, 0}); !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("SetIPv4Checksum on a nonsense packet returned %v, want ErrMalformedPacket", err)
	}
}

// --- address pool -------------------------------------------------------------

func TestPoolReservesTheServerAndBroadcastAddresses(t *testing.T) {
	pool, err := NewPool("10.7.0.0/29")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if got := pool.Server().String(); got != "10.7.0.1" {
		t.Errorf("Server() = %s, want 10.7.0.1", got)
	}
	// A /29 holds 8 addresses: the network, the server, the broadcast, and five
	// clients.
	if pool.Size() != 5 {
		t.Errorf("Size() = %d, want 5", pool.Size())
	}
	if pool.Contains(pool.Server()) {
		t.Error("the pool hands out the address reserved for the server")
	}
	if pool.Contains(net.ParseIP("10.7.0.0")) {
		t.Error("the pool hands out the network address")
	}
	if pool.Contains(net.ParseIP("10.7.0.7")) {
		t.Error("the pool hands out the broadcast address")
	}
	if pool.Contains(net.ParseIP("10.7.1.1")) {
		t.Error("the pool hands out an address outside its subnet")
	}
	if !pool.Contains(net.ParseIP("10.7.0.3")) {
		t.Error("the pool rejects one of its own addresses")
	}
	if pool.Contains(net.ParseIP("2001:db8::1")) {
		t.Error("the pool claims an IPv6 address")
	}
}

func TestPoolHandsOutEveryAddressThenReportsExhaustion(t *testing.T) {
	pool, err := NewPool("10.7.0.0/29")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < pool.Size(); i++ {
		ip, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if seen[ip.String()] {
			t.Fatalf("Acquire returned %s twice", ip)
		}
		seen[ip.String()] = true
		if !pool.Contains(ip) {
			t.Fatalf("Acquire returned %s, which is not a client address in the pool", ip)
		}
	}
	if pool.InUse() != pool.Size() {
		t.Errorf("InUse() = %d, want %d", pool.InUse(), pool.Size())
	}

	if _, err := pool.Acquire(); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Acquire on an exhausted pool returned %v, want ErrPoolExhausted", err)
	}

	// Releasing one makes it available again, and the released address is reused.
	var released net.IP
	for ip := range seen {
		released = net.ParseIP(ip)
		break
	}
	pool.Release(released)
	if pool.InUse() != pool.Size()-1 {
		t.Errorf("InUse() = %d after one release, want %d", pool.InUse(), pool.Size()-1)
	}
	next, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire after a release: %v", err)
	}
	if !next.Equal(released) {
		t.Errorf("Acquire returned %s after %s was released, want the released address", next, released)
	}
}

func TestPoolReleaseIgnoresAddressesItNeverHandedOut(t *testing.T) {
	pool, err := NewPool("10.7.0.0/29")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	held, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Releasing an address outside the pool, the server's address, and the network
	// address must change nothing.
	pool.Release(net.ParseIP("8.8.8.8"))
	pool.Release(net.ParseIP("10.7.0.1"))
	pool.Release(net.ParseIP("10.7.0.0"))
	pool.Release(nil)
	if pool.InUse() != 1 {
		t.Fatalf("InUse() = %d after releasing addresses the pool does not own, want 1", pool.InUse())
	}
	if !pool.Contains(held) {
		t.Fatal("the held address was released by a call for a different address")
	}
}

func TestPoolIsSafeForConcurrentUse(t *testing.T) {
	pool, err := NewPool("10.7.0.0/24")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	// The invariant is that no two holders ever hold the same address at once. The
	// same address being handed out again after a release is expected, so the
	// release and the bookkeeping that follows it happen under one lock: otherwise
	// a second holder can observe the address between the two.
	var mu sync.Mutex
	held := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				ip, err := pool.Acquire()
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}

				mu.Lock()
				if held[ip.String()] {
					t.Errorf("%s was handed to two holders at the same time", ip)
				}
				held[ip.String()] = true
				mu.Unlock()

				mu.Lock()
				pool.Release(ip)
				delete(held, ip.String())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if pool.InUse() != 0 {
		t.Errorf("InUse() = %d after every address was released, want 0", pool.InUse())
	}
}

func TestNewPoolRejectsImpossibleSubnets(t *testing.T) {
	cases := []struct {
		name string
		cidr string
	}{
		{"not a CIDR", "10.7.0.0"},
		{"garbage", "not-an-address/24"},
		{"IPv6", "2001:db8::/64"},
		{"network with only one address", "10.7.0.1/32"},
		{"point to point link with no client address", "10.7.0.0/31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPool(tc.cidr); err == nil {
				t.Fatalf("%q was accepted", tc.cidr)
			}
		})
	}
}

func TestPoolAcceptsASubnetWithOneClientAddress(t *testing.T) {
	// A /30 has four addresses: the network, the server, one client and broadcast.
	pool, err := NewPool("10.7.0.0/30")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if pool.Size() != 1 {
		t.Errorf("Size() = %d, want 1", pool.Size())
	}
	ip, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := ip.String(); got != "10.7.0.2" {
		t.Errorf("the only client address is %s, want 10.7.0.2", got)
	}
}

func TestPoolMaskAndNetwork(t *testing.T) {
	pool, err := NewPool("10.7.0.0/24")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if got := pool.Network().String(); got != "10.7.0.0/24" {
		t.Errorf("Network() = %s, want 10.7.0.0/24", got)
	}
	if ones, bits := pool.Mask().Size(); ones != 24 || bits != 32 {
		t.Errorf("Mask() is a /%d of %d bits, want a /24 of 32", ones, bits)
	}
}

// --- the tunnel ---------------------------------------------------------------

func TestTunnelForwardsPacketsInBothDirections(t *testing.T) {
	left := newFakeDevice("tun-a", 1500)
	right := newFakeDevice("tun-b", 1500)
	leftTransport := newFakeTransport()
	rightTransport := newFakeTransport()
	link(leftTransport, rightTransport)

	leftTunnel, err := New(left, leftTransport, Options{})
	if err != nil {
		t.Fatalf("New(left): %v", err)
	}
	rightTunnel, err := New(right, rightTransport, Options{})
	if err != nil {
		t.Fatalf("New(right): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- leftTunnel.Run(ctx) }()
	go func() { done <- rightTunnel.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Run: %v", err)
			}
		}
	})

	request := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("ping"))
	left.incoming <- request

	got := receivePacket(t, right.sent)
	if !Source(got).Equal(net.ParseIP("10.7.0.2")) || !Destination(got).Equal(net.ParseIP("10.7.0.1")) {
		t.Fatalf("the packet arrived as %s -> %s, want 10.7.0.2 -> 10.7.0.1", Source(got), Destination(got))
	}

	// The reverse direction uses the same path.
	reply := ipv6Packet("2001:db8::1", "2001:db8::2", []byte("pong"))
	right.incoming <- reply
	got = receivePacket(t, left.sent)
	if !Destination(got).Equal(net.ParseIP("2001:db8::2")) {
		t.Fatalf("the reply arrived for %s, want 2001:db8::2", Destination(got))
	}

	leftStats := leftTunnel.Stats().Snapshot()
	if leftStats.FromDevice != 1 {
		t.Errorf("the left tunnel counted %d packets from its device, want 1", leftStats.FromDevice)
	}
	waitFor(t, func() bool { return leftTunnel.Stats().Snapshot().ToDevice == 1 },
		"the left tunnel never counted the reply it wrote to its device")
	if got := leftTunnel.Stats().Snapshot().BytesToDevice; got != int64(len(reply)) {
		t.Errorf("BytesToDevice = %d, want %d", got, len(reply))
	}
	waitFor(t, func() bool {
		stats := rightTunnel.Stats().Snapshot()
		return stats.ToDevice == 1 && stats.FromDevice == 1
	}, "the right tunnel never counted one packet each way")
}

func TestTunnelDropsPacketsThatAreNotValidIP(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	transport := newFakeTransport()
	tunnel, err := New(device, transport, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tunnel.Run(ctx) }()

	// A packet from the device that is not IP at all, and one that is above the MTU.
	device.incoming <- []byte{0x70, 0x00, 0x00, 0x00}
	device.incoming <- ipv4Packet("10.7.0.2", "10.7.0.1", make([]byte, 1500))
	// A packet from the peer that is truncated.
	transport.in <- []byte{0x45, 0x00}

	// One good packet, so the test can tell the drops apart from a stall.
	good := ipv4Packet("10.7.0.2", "10.7.0.1", []byte("ok"))
	device.incoming <- good

	if got := receivePacket(t, transport.out); len(got) != len(good) {
		t.Fatalf("the forwarded packet is %d bytes, want %d", len(got), len(good))
	}
	waitFor(t, func() bool { return tunnel.Stats().Snapshot().Dropped == 3 },
		"the three invalid packets were not all counted as dropped")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := tunnel.Stats().Snapshot()
	if stats.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", stats.Dropped)
	}
	if stats.FromDevice != 1 {
		t.Errorf("FromDevice = %d, want 1", stats.FromDevice)
	}
	if stats.ToDevice != 0 {
		t.Errorf("ToDevice = %d, want 0: every packet from the peer was rejected", stats.ToDevice)
	}
}

func TestTunnelReportsADeviceReadFailure(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	device.failRead = errors.New("the interface went away")
	transport := newFakeTransport()

	tunnel, err := New(device, transport, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = tunnel.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil after a device read failure")
	}
	if !strings.Contains(err.Error(), "the interface went away") {
		t.Errorf("the error is %v, which does not mention the device failure", err)
	}
	if tunnel.Stats().Snapshot().Errors == 0 {
		t.Error("the failure was not counted")
	}
}

func TestTunnelReportsATransportSendFailure(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	transport := newFakeTransport()
	transport.failSend = errors.New("the session dropped")

	tunnel, err := New(device, transport, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	device.incoming <- ipv4Packet("10.7.0.2", "10.7.0.1", []byte("lost"))

	err = tunnel.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil after a transport send failure")
	}
	if !strings.Contains(err.Error(), "sending a packet") {
		t.Errorf("the error is %v, which does not name the send path", err)
	}
	if !strings.Contains(err.Error(), "the session dropped") {
		t.Errorf("the error is %v, which does not carry the underlying failure", err)
	}
}

func TestTunnelReturnsCleanlyWhenCancelled(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	transport := newFakeTransport()
	tunnel, err := New(device, transport, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tunnel.Run(ctx) }()

	// Cancel while both directions are blocked in a read.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation: a read was not unblocked")
	}
}

func TestNewValidatesItsArguments(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	transport := newFakeTransport()

	if _, err := New(nil, transport, Options{}); err == nil {
		t.Error("a tunnel without a device was accepted")
	}
	if _, err := New(device, nil, Options{}); err == nil {
		t.Error("a tunnel without a transport was accepted")
	}
	if _, err := New(device, transport, Options{MTU: 100}); err == nil {
		t.Error("an MTU below the supported minimum was accepted")
	}
	if _, err := New(device, transport, Options{MTU: 20000}); err == nil {
		t.Error("an MTU above the supported maximum was accepted")
	}
	// The device's own MTU is the ceiling.
	if _, err := New(device, transport, Options{MTU: 1600}); err == nil {
		t.Error("an MTU larger than the device reports was accepted")
	}
	// The device's MTU is used when the option is zero.
	tunnel, err := New(device, transport, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tunnel.MTU() != 1500 {
		t.Errorf("MTU() = %d, want the device's 1500", tunnel.MTU())
	}
	if tunnel.Device() != device {
		t.Error("Device() did not return the device the tunnel was built with")
	}
}

func TestDefaultMTUIsUsedWhenTheDeviceDoesNotReportOne(t *testing.T) {
	device := newFakeDevice("tun-a", 0)
	tunnel, err := New(device, newFakeTransport(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tunnel.MTU() != DefaultMTU {
		t.Errorf("MTU() = %d, want the %d default", tunnel.MTU(), DefaultMTU)
	}
}

// --- device helpers -----------------------------------------------------------

func TestOpenRejectsUnusableNamesAndMTUs(t *testing.T) {
	if _, err := Open("tun/0", 0); err == nil {
		t.Error("a device name containing a slash was accepted")
	}
	if _, err := Open("a-name-that-is-way-too-long", 0); err == nil {
		t.Error("a device name longer than the kernel allows was accepted")
	}
	if _, err := Open("tun0", 100); err == nil {
		t.Error("an MTU below the supported minimum was accepted")
	}
}

func TestAssignAddressRequiresAnAddressableDevice(t *testing.T) {
	device := newFakeDevice("tun-a", 1500)
	if err := AssignAddress(device, "10.7.0.2"); err != nil {
		t.Fatalf("AssignAddress: %v", err)
	}
	if got := device.assigned(); got != "10.7.0.2" {
		t.Errorf("the device holds address %q, want 10.7.0.2", got)
	}

	if err := AssignAddress(&plainDevice{}, "10.7.0.2"); err == nil {
		t.Error("a device that cannot take an address was accepted")
	}
}

// plainDevice implements Device without Addressable.
type plainDevice struct{}

func (plainDevice) Read([]byte) (int, error)  { return 0, io.EOF }
func (plainDevice) Write([]byte) (int, error) { return 0, io.EOF }
func (plainDevice) Close() error              { return nil }
func (plainDevice) Name() string              { return "plain" }
func (plainDevice) MTU() int                  { return 1500 }

// TestOpenReportsWhatThePlatformCanDo checks the platform boundary. On a platform
// with no tun implementation in this build the call must fail with ErrNoDevice; on
// Linux it either succeeds, or fails because opening the device needs CAP_NET_ADMIN.
func TestOpenReportsWhatThePlatformCanDo(t *testing.T) {
	device, err := Open("", 0)

	if runtime.GOOS == "linux" {
		if err != nil {
			if !errors.Is(err, os.ErrPermission) && !strings.Contains(err.Error(), "operation not permitted") {
				t.Fatalf("Open on Linux failed with %v, want success or a permission error", err)
			}
			t.Skipf("this process cannot open a tun device: %v", err)
		}
		defer device.Close()
		if device.Name() == "" {
			t.Error("the opened device has no name")
		}
		if device.MTU() < MinMTU || device.MTU() > MaxMTU {
			t.Errorf("the opened device reports MTU %d, outside %d-%d", device.MTU(), MinMTU, MaxMTU)
		}
		return
	}

	if err == nil {
		_ = device.Close()
		t.Fatalf("Open succeeded on %s, where this build has no tun implementation", runtime.GOOS)
	}
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("Open on %s failed with %v, want ErrNoDevice", runtime.GOOS, err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("the error %v does not name the platform", err)
	}
}

// --- helpers ------------------------------------------------------------------

func receivePacket(t *testing.T, from chan []byte) []byte {
	t.Helper()
	select {
	case packet := <-from:
		return packet
	case <-time.After(5 * time.Second):
		t.Fatal("no packet arrived within 5s")
		return nil
	}
}

// waitFor blocks until condition holds, which keeps a counter assertion from racing
// the goroutine that updates it.
func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
