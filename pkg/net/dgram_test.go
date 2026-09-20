package net

import (
	"io"
	"log"
	"net"
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
