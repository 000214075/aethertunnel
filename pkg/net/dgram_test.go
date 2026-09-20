package net

import (
	"io"
	"log"
	"net"
	"sync"
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
	pump, _ := newEchoPump(t, 2*time.Second)

	var (
		mu       sync.Mutex
		upload   int64
		download int64
	)

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	got := sendAndReceive(t, socket, pump.Socket.LocalAddr(), "counted")

	// The counters are only read once the session has been observed, so attach
	// them after the fact and send one more datagram.
	mu.Lock()
	pump.OnDatagram = func(toPeer bool, n int) {
		mu.Lock()
		defer mu.Unlock()
		if toPeer {
			upload += int64(n)
		} else {
			download += int64(n)
		}
	}
	mu.Unlock()

	const payload = "counted-again"
	if got != "counted" {
		t.Fatalf("the first datagram came back as %q", got)
	}
	if again := sendAndReceive(t, socket, pump.Socket.LocalAddr(), payload); again != payload {
		t.Fatalf("the second datagram came back as %q", again)
	}

	mu.Lock()
	defer mu.Unlock()
	if upload != int64(len(payload)) {
		t.Fatalf("uploaded %d bytes, want %d", upload, len(payload))
	}
	if download != int64(len(payload)) {
		t.Fatalf("downloaded %d bytes, want %d", download, len(payload))
	}
}

func TestDatagramPumpAccountsForEverySession(t *testing.T) {
	pump, _ := newEchoPump(t, 2*time.Second)

	var (
		mu       sync.Mutex
		opened   int
		closed   int
		reported int64
	)
	pump.OnSession = func(delta int) {
		mu.Lock()
		defer mu.Unlock()
		if delta > 0 {
			opened++
		} else {
			closed++
		}
	}
	pump.Close = func(addr net.Addr, toPeer, fromPeer int64) {
		mu.Lock()
		reported += toPeer + fromPeer
		mu.Unlock()
	}

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
