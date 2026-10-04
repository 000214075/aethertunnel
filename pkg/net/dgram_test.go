package net

import (
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// newEchoPump starts a pump whose every session is answered by an echo service,
// which is what a udp proxy or a sudp visitor ultimately reaches.
//
// Each session gets its own framed transport, exactly as the tunnel gives each
// data connection its own socket. The echo reader is started inside Open, before
// the pump writes its first datagram, because the pipe between them is
// unbuffered.
func newEchoPump(t *testing.T, idle time.Duration) (*DatagramPump, net.PacketConn) {
	t.Helper()
	return newEchoPumpWith(t, idle, nil)
}

// newEchoPumpWith starts an echo pump after configure has attached the callbacks
// it wants, because the pump reads those fields from its own goroutines and so
// they must be in place before the first datagram arrives.
func newEchoPumpWith(t *testing.T, idle time.Duration, configure func(*DatagramPump)) (*DatagramPump, net.PacketConn) {
	t.Helper()

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	pump := &DatagramPump{
		Socket:      socket,
		IdleTimeout: idle,
		Logger:      log.New(io.Discard, "", 0),
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			pumpSide, serviceSide := net.Pipe()
			service := protocol.NewFramer(serviceSide, nil, 0)

			go func() {
				for {
					msg, err := service.ReadFrame()
					if err != nil {
						return
					}
					if msg.Type != protocol.TypeUDPPacket {
						continue
					}
					if err := service.WriteFrame(&protocol.Message{
						Type:    protocol.TypeUDPPacket,
						Payload: msg.Payload,
					}); err != nil {
						return
					}
				}
			}()

			return protocol.NewFramer(pumpSide, nil, 0), func() {
				_ = pumpSide.Close()
				_ = serviceSide.Close()
			}, nil
		},
	}
	if configure != nil {
		configure(pump)
	}
	pump.Start()

	t.Cleanup(func() {
		pump.Shutdown()
		_ = socket.Close()
	})
	return pump, socket
}

// sendAndReceive sends a datagram from a fresh source address until it comes
// back, because the first datagram of a new address is dropped while the session
// is being established.
func sendAndReceive(t *testing.T, socket net.PacketConn, target net.Addr, payload string) string {
	t.Helper()

	_ = socket.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	for attempt := 0; attempt < 25; attempt++ {
		if _, err := socket.WriteTo([]byte(payload), target); err != nil {
			t.Fatalf("send: %v", err)
		}
		n, _, err := socket.ReadFrom(buf)
		if err == nil {
			return string(buf[:n])
		}
	}
	t.Fatalf("%q never came back", payload)
	return ""
}

func TestDatagramPumpRelaysPerSourceAddress(t *testing.T) {
	pump, _ := newEchoPump(t, 2*time.Second)

	const senders = 3
	var wg sync.WaitGroup
	results := make([]string, senders)

	wg.Add(senders)
	for i := 0; i < senders; i++ {
		go func(index int) {
			defer wg.Done()
			socket, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				return
			}
			defer socket.Close()
			results[index] = sendAndReceive(t, socket, pump.Socket.LocalAddr(), string(rune('a'+index))+"-sender")
		}(i)
	}
	wg.Wait()

	for i := 0; i < senders; i++ {
		if want := string(rune('a'+i)) + "-sender"; results[i] != want {
			t.Fatalf("sender %d received %q, want %q", i, results[i], want)
		}
	}
	if sessions := pump.Sessions(); sessions != senders {
		t.Fatalf("the pump tracks %d sessions, want %d", sessions, senders)
	}
}

func TestDatagramPumpKeepsOneSessionPerAddress(t *testing.T) {
	pump, _ := newEchoPump(t, 2*time.Second)
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	// Repeated datagrams from one address must reuse the session rather than
	// opening a stream each time.
	for i := 0; i < 5; i++ {
		got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), "payload")
		if got != "payload" {
			t.Fatalf("attempt %d received %q", i, got)
		}
	}
	if sessions := pump.Sessions(); sessions != 1 {
		t.Fatalf("one address produced %d sessions", sessions)
	}
}

func TestDatagramPumpReportsTraffic(t *testing.T) {
	var upload, download atomic.Int64

	pump, _ := newEchoPumpWith(t, 2*time.Second, func(p *DatagramPump) {
		p.OnDatagram = func(toPeer bool, n int) {
			if toPeer {
				upload.Add(int64(n))
			} else {
				download.Add(int64(n))
			}
		}
	})

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	// The first datagram opens the session, and its own bytes are carried once the
	// session is up; the second datagram then travels on the established session.
	const first = "counted"
	if got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), first); got != first {
		t.Fatalf("the first datagram came back as %q", got)
	}

	const payload = "counted-again"
	if again := sendAndReceive(t, socket, pump.Socket.LocalAddr(), payload); again != payload {
		t.Fatalf("the second datagram came back as %q", again)
	}

	// A reply reaches the socket before its bytes are counted, so the totals are
	// awaited rather than read once: reading them immediately would see the count
	// from before the last reply was booked.
	want := int64(len(first) + len(payload))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (upload.Load() < want || download.Load() < want) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := upload.Load(); got != want {
		t.Fatalf("uploaded %d bytes, want %d", got, want)
	}
	if got := download.Load(); got != want {
		t.Fatalf("downloaded %d bytes, want %d", got, want)
	}
}

func TestDatagramPumpAccountsForEverySession(t *testing.T) {
	var (
		mu       sync.Mutex
		opened   int
		closed   int
		reported int64
		seenUp   int64
		seenDown int64
	)

	pump, _ := newEchoPumpWith(t, 2*time.Second, func(p *DatagramPump) {
		p.OnSession = func(delta int) {
			mu.Lock()
			defer mu.Unlock()
			if delta > 0 {
				opened++
			} else {
				closed++
			}
		}
		// OnDatagram fires right after the byte counters are updated, so waiting on it
		// is what makes the totals below independent of how the pump is scheduled.
		p.OnDatagram = func(toPeer bool, n int) {
			mu.Lock()
			defer mu.Unlock()
			if toPeer {
				seenUp += int64(n)
			} else {
				seenDown += int64(n)
			}
		}
		p.Close = func(addr net.Addr, toPeer, fromPeer int64) {
			mu.Lock()
			reported += toPeer + fromPeer
			mu.Unlock()
		}
	})

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	const payload = "accounted"
	if got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), payload); got != payload {
		t.Fatalf("received %q", got)
	}

	// A client can see the echoed datagram before the pump has counted it: the reply is
	// written to the socket first and the counter is updated immediately after. Wait
	// for both directions to be accounted for instead of racing the pump, so the totals
	// the shutdown reports are complete. Without this the test fails when the machine is
	// loaded, which makes the whole suite unreliable.
	want := int64(len(payload))
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		settled := seenUp >= want && seenDown >= want
		mu.Unlock()
		if settled || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	pump.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	if opened != 1 || closed != 1 {
		t.Fatalf("%d sessions opened and %d closed, want one of each", opened, closed)
	}
	if reported < int64(2*len(payload)) {
		t.Fatalf("only %d bytes were accounted for", reported)
	}
	if pump.Sessions() != 0 {
		t.Fatalf("%d sessions survived the shutdown", pump.Sessions())
	}
}

func TestDatagramPumpReleasesIdleSessions(t *testing.T) {
	pump, _ := newEchoPump(t, 600*time.Millisecond)

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	if got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), "hello"); got != "hello" {
		t.Fatalf("received %q", got)
	}
	if pump.Sessions() == 0 {
		t.Fatal("no session was created")
	}

	// The reaper runs at a quarter of the idle timeout, so this needs a few
	// intervals to fire.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && pump.Sessions() > 0 {
		time.Sleep(200 * time.Millisecond)
	}
	if sessions := pump.Sessions(); sessions != 0 {
		t.Fatalf("%d sessions survived the idle timeout", sessions)
	}
}

func TestDatagramPumpKeepsWorkingAfterAnOpenFailure(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	fail := true
	var mu sync.Mutex

	pump := &DatagramPump{
		Socket:      socket,
		IdleTimeout: 2 * time.Second,
		Logger:      log.New(io.Discard, "", 0),
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return nil, nil, io.ErrClosedPipe
			}
			pumpSide, serviceSide := net.Pipe()
			service := protocol.NewFramer(serviceSide, nil, 0)
			go func() {
				for {
					msg, err := service.ReadFrame()
					if err != nil {
						return
					}
					if err := service.WriteFrame(&protocol.Message{
						Type:    protocol.TypeUDPPacket,
						Payload: msg.Payload,
					}); err != nil {
						return
					}
				}
			}()
			return protocol.NewFramer(pumpSide, nil, 0), func() {
				_ = pumpSide.Close()
				_ = serviceSide.Close()
			}, nil
		},
	}
	pump.Start()
	defer pump.Shutdown()

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client socket: %v", err)
	}
	defer client.Close()

	// A session whose stream cannot be opened is given up rather than kept.
	_ = client.SetDeadline(time.Now().Add(time.Second))
	_, _ = client.WriteTo([]byte("doomed"), socket.LocalAddr())
	time.Sleep(300 * time.Millisecond)
	if sessions := pump.Sessions(); sessions != 0 {
		t.Fatalf("%d sessions survived a failed open", sessions)
	}

	mu.Lock()
	fail = false
	mu.Unlock()

	if got := sendAndReceive(t, client, socket.LocalAddr(), "recovered"); got != "recovered" {
		t.Fatalf("received %q after the failure", got)
	}
}

// TestDatagramPumpIgnoresDatagramsAfterShutdown covers the delivery side of the
// shutdown race: a datagram that reaches deliver once the pump is closed must not
// open a session, because the shutdown that is already running has listed the
// sessions it will release and would never close this one. A session opened here
// keeps a data connection and leaves the session gauge one too high for good.
func TestDatagramPumpIgnoresDatagramsAfterShutdown(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	var opened, sessions atomic.Int64
	pump := &DatagramPump{
		Socket:      socket,
		IdleTimeout: time.Minute,
		Logger:      log.New(io.Discard, "", 0),
		OnSession:   func(delta int) { sessions.Add(int64(delta)) },
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			opened.Add(1)
			pumpSide, peerSide := net.Pipe()
			return protocol.NewFramer(pumpSide, nil, 0), func() {
				_ = pumpSide.Close()
				_ = peerSide.Close()
			}, nil
		},
	}
	pump.Start()
	pump.Shutdown()

	pump.deliver(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5555}, []byte("late"))
	time.Sleep(50 * time.Millisecond)

	if got := pump.Sessions(); got != 0 {
		t.Errorf("%d sessions were kept for a datagram that arrived after the shutdown", got)
	}
	if got := sessions.Load(); got != 0 {
		t.Errorf("the session gauge is %d after a datagram arrived post-shutdown, want 0", got)
	}
	if got := opened.Load(); got != 0 {
		t.Errorf("%d data connections were opened after the shutdown, want 0", got)
	}
}

// TestDatagramPumpClosesThePathsOfASessionThatEndedWhileTheyOpened covers the other
// side of that race: opening a path runs on its own goroutine, so a session can end
// — a shutdown, the reaper, a failed write — before its first path comes up. By then
// closeFramers has run and seen no paths, so the paths that arrive afterwards have to
// be closed where they are opened. Leaving them to closeFramers leaks the data
// connection and the goroutine that would read it.
func TestDatagramPumpClosesThePathsOfASessionThatEndedWhileTheyOpened(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	var sessions atomic.Int64
	started := make(chan struct{})
	proceed := make(chan struct{})
	released := make(chan struct{}, 1)

	pump := &DatagramPump{
		Socket:      socket,
		IdleTimeout: time.Minute,
		Logger:      log.New(io.Discard, "", 0),
		OnSession:   func(delta int) { sessions.Add(int64(delta)) },
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			close(started)
			<-proceed
			pumpSide, peerSide := net.Pipe()
			return protocol.NewFramer(pumpSide, nil, 0), func() {
				_ = pumpSide.Close()
				_ = peerSide.Close()
				released <- struct{}{}
			}, nil
		},
	}
	pump.Start()
	t.Cleanup(pump.Shutdown)

	// The first datagram opens the session; its single path is held inside Open.
	pump.deliver(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5556}, []byte("first"))
	<-started

	if got := sessions.Load(); got != 1 {
		t.Fatalf("the session gauge is %d while the path is being opened, want 1", got)
	}

	// The pump shuts down before the path comes up, so the session ends first.
	pump.Shutdown()
	if got := sessions.Load(); got != 0 {
		t.Fatalf("the session gauge is %d after the shutdown, want 0", got)
	}

	// Let Open return now that the session it belongs to is already gone.
	close(proceed)

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the path opened for a session that had already ended was never released")
	}
	if got := sessions.Load(); got != 0 {
		t.Errorf("the session gauge is %d after the late path came up, want 0", got)
	}
}

// A datagram over the cap is dropped and reported instead of being relayed:
// the read buffer stops one byte past the cap, so the pump can tell an
// oversized datagram from one that exactly fills it rather than truncating a
// datagram the local service would then see shortened.
func TestDatagramPumpDropsDatagramsOverTheCap(t *testing.T) {
	var dropped atomic.Int64
	var droppedSize atomic.Int64
	pump, socket := newEchoPumpWith(t, 2*time.Second, func(p *DatagramPump) {
		p.MaxDatagram = 64
		p.OnOversize = func(n int) {
			dropped.Add(1)
			droppedSize.Store(int64(n))
		}
	})

	fitting := strings.Repeat("a", 64)
	if got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), fitting); got != fitting {
		t.Fatalf("a datagram that exactly fills the cap came back as %d bytes, want 64", len(got))
	}

	if _, err := socket.WriteTo([]byte(strings.Repeat("b", 65)), pump.Socket.LocalAddr()); err != nil {
		t.Fatalf("send the oversized datagram: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := dropped.Load(); got != 1 {
		t.Fatalf("the pump dropped %d datagrams for size, want 1", got)
	}
	if got := droppedSize.Load(); got != 65 {
		t.Fatalf("the drop reported %d bytes, want 65", got)
	}
	if sessions := pump.Sessions(); sessions != 1 {
		t.Fatalf("the pump tracks %d sessions, want 1: the oversized datagram opened none", sessions)
	}
}

// Without a cap the pump relays a datagram larger than frp's 1500-byte default,
// which is what a configuration that does not set udp_packet_size keeps.
func TestDatagramPumpWithoutACapRelaysLargeDatagrams(t *testing.T) {
	pump, socket := newEchoPump(t, 2*time.Second)

	payload := strings.Repeat("x", 4000)
	_ = socket.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8192)
	var got []byte
	for attempt := 0; attempt < 25 && got == nil; attempt++ {
		if _, err := socket.WriteTo([]byte(payload), pump.Socket.LocalAddr()); err != nil {
			t.Fatalf("send: %v", err)
		}
		n, _, err := socket.ReadFrom(buf)
		if err == nil {
			got = append([]byte(nil), buf[:n]...)
		}
	}
	if len(got) != len(payload) {
		t.Fatalf("a %d-byte datagram came back as %d bytes, want all of it", len(payload), len(got))
	}
}
