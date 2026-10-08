package net

import (
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// flakyReplyConn fails the first N reply writes and then passes everything
// through, the way a public interface's kernel send buffer fails a burst of
// sends while the socket itself is fine.
type flakyReplyConn struct {
	net.PacketConn
	failures atomic.Int64
}

func (c *flakyReplyConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.failures.Add(-1) >= 0 {
		return 0, errors.New("transient: send buffer is full")
	}
	return c.PacketConn.WriteTo(p, addr)
}

// One failed reply write used to end the whole datagram session: a full kernel
// send buffer on a busy public interface — exactly the moment datagrams peak —
// tore down an established tunnel instead of dropping one datagram.
func TestATransientReplyWriteFailureDoesNotEndTheSession(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	flaky := &flakyReplyConn{PacketConn: socket}
	flaky.failures.Store(2)

	var closeCalls, starts atomic.Int64
	pump := &DatagramPump{
		Socket:      flaky,
		IdleTimeout: 10 * time.Second,
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
					_ = service.WriteFrame(&protocol.Message{
						Type:    protocol.TypeUDPPacket,
						Payload: msg.Payload,
					})
				}
			}()
			return protocol.NewFramer(pumpSide, nil, 0), func() {
				_ = pumpSide.Close()
				_ = serviceSide.Close()
			}, nil
		},
		Close: func(addr net.Addr, toPeer, fromPeer int64) {
			closeCalls.Add(1)
		},
		OnSession: func(delta int) {
			if delta > 0 {
				starts.Add(1)
			}
		},
	}
	pump.Start()
	t.Cleanup(func() {
		pump.Shutdown()
		_ = socket.Close()
	})

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()
	target := pump.Socket.LocalAddr()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	// The first two replies are eaten by the flaky socket; the third gets
	// through. The session itself must survive the whole burst.
	for _, msg := range []string{"one", "two"} {
		if _, err := client.WriteTo([]byte(msg), target); err != nil {
			t.Fatalf("send %q: %v", msg, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if _, err := client.WriteTo([]byte("three"), target); err != nil {
		t.Fatalf("send %q: %v", "three", err)
	}
	buf := make([]byte, 64)
	for {
		n, _, err := client.ReadFrom(buf)
		if err != nil {
			t.Fatalf("the echo of the third datagram never came back: %v", err)
		}
		if string(buf[:n]) == "three" {
			break
		}
	}

	time.Sleep(200 * time.Millisecond)
	if got := closeCalls.Load(); got != 0 {
		t.Fatalf("reply-write failures ended the session %d time(s)", got)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("%d sessions were started for one source address, want 1", got)
	}
}

// Close belongs to every session's ending, including the one whose paths never
// opened: callers that pair Open with Close hear about a failed establish too.
func TestAFailedEstablishReportsTheSessionThroughClose(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	var closes, ends atomic.Int64
	var closeTo, closeFrom atomic.Int64
	pump := &DatagramPump{
		Socket: socket,
		Logger: log.New(io.Discard, "", 0),
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			return nil, nil, errors.New("no path is available")
		},
		Close: func(addr net.Addr, toPeer, fromPeer int64) {
			closes.Add(1)
			closeTo.Store(toPeer)
			closeFrom.Store(fromPeer)
		},
		OnSession: func(delta int) {
			if delta < 0 {
				ends.Add(1)
			}
		},
	}
	pump.Start()
	t.Cleanup(pump.Shutdown)

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()
	if _, err := client.WriteTo([]byte("hello"), pump.Socket.LocalAddr()); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && closes.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("Close was called %d time(s) after a failed establish, want 1", got)
	}
	if closeTo.Load() != 0 || closeFrom.Load() != 0 {
		t.Fatalf("Close reported %d/%d bytes for a session that never opened, want 0/0", closeTo.Load(), closeFrom.Load())
	}
	if got := ends.Load(); got != 1 {
		t.Fatalf("OnSession saw %d endings, want 1", got)
	}
}

// An empty websocket message reached the reader as (0, nil) — a zero-byte read
// with no error, which io.Reader implementations are told not to do and which a
// cooperative reader answers by calling Read again at line rate. No tunnel
// frame is empty on purpose, so an empty message is dropped, whether it arrives
// whole or as fragments that are all empty.
func TestAnEmptyWebsocketMessageIsNeverDeliveredAsAReadOfNothing(t *testing.T) {
	connSide, peerSide := net.Pipe()
	// A client-side connection, so the peer's frames are unmasked and can be
	// written raw: writeFrame always sets FIN, which cannot express a first
	// fragment.
	conn := &wsConn{Conn: connSide, reader: connSide, client: true}
	t.Cleanup(func() {
		_ = connSide.Close()
		_ = peerSide.Close()
	})

	// An empty message, the two empty fragments of a fragmented one, and then a
	// message that has to be what the reader sees.
	frames := []byte{
		0x82, 0x00, // binary, FIN, no payload
		0x02, 0x00, // binary, no FIN, no payload
		0x80, 0x00, // continuation, FIN, no payload
		0x82, 0x05, 'h', 'e', 'l', 'l', 'o',
	}
	go func() { _, _ = peerSide.Write(frames) }()

	got := make([]byte, 32)
	n, err := conn.Read(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 5 || string(got[:n]) != "hello" {
		t.Fatalf("the first read returned %d byte(s) %q, want the message that followed the empty ones", n, got[:n])
	}
}
