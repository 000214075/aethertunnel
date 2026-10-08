package clientlib

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// fakePoolServer answers one client's connections the way a server does, with a
// switch for how it treats a connection the client offers for the pool. It counts
// the offers, so a test can see how many the client made.
type fakePoolServer struct {
	// acceptPool decides what happens to an offered connection: true parks it
	// (the client keeps it and waits), false refuses it the way a server that has
	// no pool feature does.
	acceptPool bool

	mu     sync.Mutex
	offers int
	conns  []net.Conn
}

func (s *fakePoolServer) start(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		for _, conn := range s.conns {
			_ = conn.Close()
		}
		s.conns = nil
		s.mu.Unlock()
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go s.serve(conn)
		}
	}()
	return listener.Addr().String()
}

func (s *fakePoolServer) serve(conn net.Conn) {
	framer := protocol.NewFramerWithOptions(conn, nil, protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload})
	first := true
	for {
		msg, err := framer.ReadFrame()
		if err != nil {
			_ = conn.Close()
			return
		}
		switch {
		case first && msg.Type == protocol.TypeAuthRequest:
			first = false
			if err := framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
				OK: true, Session: "pool-test-session", ServerVersion: "test",
				Protocol: protocol.ProtocolVersion, HeartbeatSecs: 30,
				// The client compares this with its own cipher, so it has to name
				// the algorithm a session without [encryption] runs.
				Encryption: "none",
			}); err != nil {
				_ = conn.Close()
				return
			}
		case msg.Type == protocol.TypeHeartbeat:
			if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeatAck}); err != nil {
				_ = conn.Close()
				return
			}
		case msg.Type == protocol.TypeDataOpen:
			s.mu.Lock()
			s.offers++
			s.mu.Unlock()
			if !s.acceptPool {
				_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{
					OK: false, Error: "no public connection is waiting for that stream",
				})
				_ = conn.Close()
				return
			}
			if err := framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
				_ = conn.Close()
				return
			}
			// A parked connection is idle until the server names a stream on it,
			// so this side waits without answering.
			first = false
		default:
			first = false
		}
	}
}

func (s *fakePoolServer) offerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offers
}

func poolConfig(addr string, count int) *config.Config {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = addr
	cfg.Client.AuthToken = "test-token"
	cfg.Client.PoolCount = count
	cfg.Client.DialTimeoutSecs = 5
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1
	return cfg
}

// waitForOffers gives the client the moment it needs to park its connections.
func waitForOffers(t *testing.T, s *fakePoolServer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.offerCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the client offered %d connections, want %d", s.offerCount(), want)
}

// The client keeps exactly client.pool_count connections parked, and no more.
func TestTheClientParksTheConfiguredNumberOfConnections(t *testing.T) {
	server := &fakePoolServer{acceptPool: true}
	addr := server.start(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, poolConfig(addr, 2), log.New(io.Discard, "", 0)) }()

	waitForOffers(t, server, 2)
	// A third connection would mean the pool is not bounded by the key.
	time.Sleep(300 * time.Millisecond)
	if offers := server.offerCount(); offers != 2 {
		t.Fatalf("the client offered %d connections, want exactly 2", offers)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// lockedBuffer is a bytes.Buffer that can be written from the client's goroutines
// while the test reads it. The pool workers log from their own goroutines, so a
// plain buffer read by the test is a data race — which is exactly what the race
// detector reported for this test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A server that does not park connections is told so once, and the client stops
// offering: the streams still work, they are simply dialled as they always were.
func TestTheClientStopsPoolingWhenTheServerRefuses(t *testing.T) {
	server := &fakePoolServer{acceptPool: false}
	addr := server.start(t)

	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, poolConfig(addr, 3), log.New(logs, "", 0)) }()

	waitForOffers(t, server, 1)
	// Every worker offers once before any of them has been refused, so the offers
	// stop after at most one per worker — and then stop for good.
	time.Sleep(500 * time.Millisecond)
	first := server.offerCount()
	time.Sleep(700 * time.Millisecond)
	if second := server.offerCount(); second != first {
		t.Fatalf("the client kept offering after the refusals: %d offers, then %d", first, second)
	}
	if first > 3 {
		t.Fatalf("the client offered %d connections for a pool of 3", first)
	}

	// Stop the client before reading its log: the pool workers write to the same
	// logger from their own goroutines, so reading the buffer while they run is a
	// data race (found by the race detector).
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// One line for the whole session, not one per worker.
	if count := strings.Count(logs.String(), "the workers it refuses stop offering on this session"); count != 1 {
		t.Fatalf("the refusal was explained %d times: %q", count, logs.String())
	}
}
