package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// TestADataConnectionTheServerCannotPairIsCounted covers the data path's refusals.
//
// A data-open is dispatched before anything is authenticated, so a well-formed frame
// naming a session or a stream the server is not waiting for is answered with a
// refusal. Every other refusal the server writes moves a counter; this one moved none,
// which left the only trace of something walking the data path a log line.
func TestADataConnectionTheServerCannotPairIsCounted(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	metrics := rs.server.metrics

	rejectedBefore := metrics.controlRejected.Load()
	openedBefore := metrics.dataConnections.Load()

	// A session the server has never heard of. Sent by hand, because no client of this
	// server would produce one.
	bogus := sendDataOpen(t, rs.addr, protocol.DataOpen{
		Session: "0123456789abcdef0123456789abcdef", Proxy: "nothing",
		StreamID: "00000001",
	})
	if bogus.OK {
		t.Error("a data-open naming an unknown session was accepted")
	}

	if got := metrics.dataUnmatched.Load(); got != 1 {
		t.Errorf("the unmatched counter is %d after a data-open with an unknown session, want 1", got)
	}

	// A real session, but nothing is waiting for the stream it names.
	echo := startEcho(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	sessions := rs.server.sessions.List()
	if len(sessions) != 1 {
		t.Fatalf("the server holds %d sessions, want the one the agent opened", len(sessions))
	}
	unpaired := sendDataOpen(t, rs.addr, protocol.DataOpen{
		Session: sessions[0].ID, Proxy: "web", StreamID: "0000ffff",
	})
	if unpaired.OK {
		t.Error("a data-open naming a stream nobody waits for was accepted")
	}

	if got := metrics.dataUnmatched.Load(); got != 2 {
		t.Errorf("the unmatched counter is %d after a data-open with no waiting stream, want 2", got)
	}

	// A client that cannot reach its own local service says so in the frame, and the
	// visitor waiting for that stream is told. That is a normal outcome rather than a
	// connection the server could not pair, so it must not be counted here.
	visitorAgent := startAgent(t, rs.addr, false, nil)
	publicPort := freePort(t)
	visitorAgent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the socks5 endpoint: %v", err)
	}
	defer conn.Close()
	if code := socksRequest(t, conn, "127.0.0.1:9"); code == socks.ReplySucceeded {
		t.Fatalf("the socks5 request succeeded although the client serves no target")
	}

	if got := metrics.dataUnmatched.Load(); got != 2 {
		t.Errorf("the unmatched counter is %d after a client reported it could not serve the stream, want it to stay at 2", got)
	}
	// The refused connection is still a data connection the client opened, and the
	// server still answered it, so neither of those counters may move.
	if got := metrics.controlRejected.Load(); got != rejectedBefore {
		t.Errorf("the rejection counter moved to %d for a data connection, want it to stay at %d", got, rejectedBefore)
	}
	if got := metrics.dataConnections.Load(); got != openedBefore+3 {
		t.Errorf("the data-connection counter is %d, want %d: every parseable data-open is one", got, openedBefore+3)
	}
}

// sendDataOpen dials the control port, sends one data-open as the first frame, and
// returns the answer the server wrote before closing the connection.
func sendDataOpen(t *testing.T, serverAddr string, open protocol.DataOpen) protocol.DataOpenAck {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	if err := framer.WriteJSON(protocol.TypeDataOpen, open); err != nil {
		t.Fatalf("write the data-open: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return ack
}
