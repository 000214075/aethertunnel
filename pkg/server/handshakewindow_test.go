package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// udpRoundTrip sends one datagram through the published port and reports whether
// the echo came back inside the wait.
func udpRoundTrip(t *testing.T, visitor net.Conn, payload string, wait time.Duration) bool {
	t.Helper()
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("send %q: %v", payload, err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		_ = visitor.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 2048)
		n, err := visitor.Read(buf)
		if err != nil {
			continue
		}
		if string(buf[:n]) == payload {
			return true
		}
	}
	return false
}

// The accepting side arms a read deadline of server.handshake_timeout_seconds on
// every connection, and the datagram relay sets none of its own. Leaving it armed
// on a data connection that was not parked made the first read of a udp session
// fail one handshake window after the connection was accepted, which tore the
// session down in the middle of a flow — with the default window, ten seconds into
// a transfer that was working.
func TestAUDPTunnelOutlivesTheHandshakeWindow(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.HandshakeTimeoutSecs = 1
	remotePort := freeUDPPort(t)

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo-udp": datagramHandler(echo)})
	agent.register(protocol.ProxySpec{Name: "echo-udp", Type: protocol.ProxyTypeUDP, RemotePort: remotePort})

	visitor, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err != nil {
		t.Fatalf("dial the tunnel: %v", err)
	}
	defer visitor.Close()

	if !udpRoundTrip(t, visitor, "before-the-window", 3*time.Second) {
		t.Fatal("the tunnel did not answer before the handshake window lapsed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for rs.server.metrics.udpSessions.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the datagram session was never tracked as open")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Quiet for longer than the window, with the session's data connection held
	// open the whole time. A relay that inherited the handshake deadline reads its
	// error here and releases the session, which is the churn this test detects:
	// the session is gone before the next datagram arrives.
	time.Sleep(1500 * time.Millisecond)
	if got := rs.server.metrics.udpSessions.Load(); got != 1 {
		t.Fatalf("the udp session was released during the handshake window (%d tracked)", got)
	}

	if !udpRoundTrip(t, visitor, "after-the-window", 3*time.Second) {
		t.Fatal("the udp session stopped answering once the handshake window lapsed")
	}
}
