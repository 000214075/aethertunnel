package server

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- graceful shutdown ---------------------------------------------------------
//
// [server] graceful_shutdown_seconds used to be read by nothing: every signal cut
// the transfers in flight on the spot. These tests hold a stream open across a
// shutdown and check what the grace period guarantees: the stream finishes, an idle
// server does not wait for it, the wait is bounded, and a visitor that arrives while
// the server is winding down is refused instead of being left to time out.

// holdStream opens one stream through the published port and keeps it open. The
// returned connection carries the visitor side.
func holdStream(t *testing.T, port int, echoAddr string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("held")); err != nil {
		conn.Close()
		t.Fatalf("write: %v", err)
	}
	answer := make([]byte, len("held"))
	if _, err := io.ReadFull(conn, answer); err != nil {
		conn.Close()
		t.Fatalf("read: %v", err)
	}
	if string(answer) != "held" {
		conn.Close()
		t.Fatalf("the stream answered %q, want held", answer)
	}
	return conn
}

// startGracefulServer publishes one tcp proxy with the given grace period.
func startGracefulServer(t *testing.T, graceSeconds int) (*runningServer, int, net.Conn) {
	t.Helper()

	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.GracefulShutdownSecs = graceSeconds
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	})
	waitForListener(t, rs.server, "echo")

	return rs, publicPort, nil
}

func TestShutdownLetsARunningStreamFinish(t *testing.T) {
	rs, port, _ := startGracefulServer(t, 3)

	visitor := holdStream(t, port, "")
	defer visitor.Close()

	done := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		rs.server.Shutdown("graceful test")
		done <- time.Since(started)
	}()

	// While the server winds down, the stream that is already running keeps working.
	time.Sleep(300 * time.Millisecond)
	if _, err := visitor.Write([]byte("still-open")); err != nil {
		t.Fatalf("the visitor could not write during the grace period: %v", err)
	}
	answer := make([]byte, len("still-open"))
	if _, err := io.ReadFull(visitor, answer); err != nil {
		t.Fatalf("the stream stopped working during the grace period: %v", err)
	}
	if string(answer) != "still-open" {
		t.Fatalf("the stream answered %q during the grace period", answer)
	}

	// Ending the stream releases the drain, so the shutdown does not sit out the
	// rest of the grace period.
	visitor.Close()
	select {
	case took := <-done:
		if took > 2*time.Second {
			t.Errorf("the shutdown took %s after the last stream ended, want it to stop waiting", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown did not return after the last stream ended")
	}
}

func TestShutdownDoesNotWaitWithoutStreams(t *testing.T) {
	rs, _, _ := startGracefulServer(t, 5)
	_ = agentForIdleSession(t, rs)

	started := time.Now()
	rs.server.Shutdown("nothing in flight")
	if took := time.Since(started); took > 1500*time.Millisecond {
		t.Errorf("an idle server waited %s of its 5s grace period, want an immediate stop", took)
	}
}

// agentForIdleSession registers a session with no traffic, which is what a fleet of
// connected but idle clients looks like.
func agentForIdleSession(t *testing.T, rs *runningServer) *testAgent {
	t.Helper()

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})
	agent.register(protocol.ProxySpec{
		Name: "idle", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:9", RemotePort: freePort(t),
	})
	waitForListener(t, rs.server, "idle")
	return agent
}

func TestShutdownGivesUpAfterTheGracePeriod(t *testing.T) {
	rs, port, _ := startGracefulServer(t, 1)

	visitor := holdStream(t, port, "")
	defer visitor.Close()

	started := time.Now()
	rs.server.Shutdown("a stream that never ends")
	took := time.Since(started)

	if took < 900*time.Millisecond {
		t.Errorf("the shutdown returned after %s, before the 1s grace period was over", took)
	}
	if took > 4*time.Second {
		t.Errorf("the shutdown waited %s for a 1s grace period", took)
	}

	// Giving up means the stream is disconnected, so the visitor sees it end.
	_ = visitor.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := visitor.Read(make([]byte, 32)); err == nil {
		t.Error("the stream was still open after the grace period ran out")
	}
}

func TestANewStreamIsRefusedWhileDraining(t *testing.T) {
	rs, port, _ := startGracefulServer(t, 3)

	visitor := holdStream(t, port, "")
	defer visitor.Close()

	done := make(chan struct{})
	go func() {
		rs.server.Shutdown("refuse new streams")
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)

	// A visitor that arrives during the grace period is refused: its connection is
	// closed instead of waiting for a data connection that would only be cut off.
	late, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial during the drain: %v", err)
	}
	defer late.Close()
	_ = late.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := late.Write([]byte("too late")); err != nil {
		t.Fatalf("write during the drain: %v", err)
	}
	if n, err := late.Read(make([]byte, 32)); err == nil {
		t.Fatalf("a stream opened during the drain answered %d byte(s)", n)
	}

	if refused := rs.server.metrics.drainRefused.Load(); refused != 1 {
		t.Errorf("the metric reports %d refused streams, want 1", refused)
	}

	visitor.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown did not finish after the last stream ended")
	}
}
