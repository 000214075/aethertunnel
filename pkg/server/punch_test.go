package server

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A visitor that punched a direct path stops using its control connection without
// saying anything, so the server learns the outcome from the path report the
// visitor sends over the rendezvous socket. These tests drive a real visitor
// connection and a real rendezvous socket.

// visitorWithPunchOffer connects a visitor to an xtcp proxy and returns its control
// connection together with the punch token the server handed out.
func visitorWithPunchOffer(t *testing.T, rs *runningServer) (net.Conn, *protocol.Framer, protocol.P2PPeer) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "p2p", Secret: "s3cret", Type: protocol.ProxyTypeXTCP, AuthToken: testToken,
	}); err != nil {
		conn.Close()
		t.Fatalf("send the visitor connect: %v", err)
	}

	var offer protocol.P2PPeer
	if err := framer.ReadJSON(protocol.TypeP2PPeer, &offer); err != nil {
		conn.Close()
		t.Fatalf("read the punch offer: %v", err)
	}
	return conn, framer, offer
}

// reportPath sends the datagram a visitor uses to tell the server which path it
// took.
func reportPath(t *testing.T, cfg *config.Config, token string, path byte) {
	t.Helper()

	conn, err := net.Dial("udp", net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.P2PPort)))
	if err != nil {
		t.Fatalf("dial the rendezvous port: %v", err)
	}
	defer conn.Close()

	payload := protocol.EncodePunchResult(token, path)
	if payload == nil {
		t.Fatalf("cannot encode a path report for token %q", token)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("send the path report: %v", err)
	}
}

// waitForAuditEvent waits until the audit log holds an event with the given name
// and returns everything it holds by then. Waiting for the count of lines instead
// would race: the server writes the event after the counter it belongs to, and a
// visitor connection adds lines of its own.
func waitForAuditEvent(t *testing.T, path, name string) []AuditEvent {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var events []AuditEvent
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var event AuditEvent
				if err := json.Unmarshal([]byte(line), &event); err == nil {
					events = append(events, event)
				}
			}
			for _, event := range events {
				if event.Event == name {
					return events
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log at %s holds no %s event after 10s: %v", path, name, events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func eventNames(events []AuditEvent) map[string]bool {
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Event] = true
	}
	return seen
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// xtcpTestServer starts a server with a rendezvous port, an audit log and one
// client publishing an xtcp proxy.
func xtcpTestServer(t *testing.T) (*runningServer, *config.Config, string) {
	t.Helper()

	auditPath := t.TempDir() + "/audit.jsonl"
	cfg := testConfig(t, false)
	cfg.Server.P2PPort = freeUDPPort(t)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = auditPath
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	echo := startEcho(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"p2p": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "p2p", Type: protocol.ProxyTypeXTCP, LocalAddr: echo, SecretKey: "s3cret",
	})
	return rs, cfg, auditPath
}

func TestAVisitorThatReportsADirectPathIsCountedAndAudited(t *testing.T) {
	rs, cfg, auditPath := xtcpTestServer(t)

	conn, _, offer := visitorWithPunchOffer(t, rs)
	waitFor(t, "the punch attempt to be registered", func() bool {
		return rs.server.metrics.p2pPunches.Load() == 1
	})

	reportPath(t, cfg, offer.Token, protocol.PunchPathDirect)
	conn.Close()

	waitFor(t, "the direct path to be counted", func() bool {
		return rs.server.metrics.p2pDirect.Load() == 1
	})
	if relayed := rs.server.metrics.p2pRelayed.Load(); relayed != 0 {
		t.Errorf("p2p_relayed counted %d, want 0", relayed)
	}

	events := waitForAuditEvent(t, auditPath, EventP2PDirect)
	seen := eventNames(events)
	if seen[EventP2PAbandoned] {
		t.Errorf("a visitor that reported a direct path was also recorded as %s: %v", EventP2PAbandoned, events)
	}
}

func TestAVisitorThatLeavesWithoutAReportIsNotCountedAsDirect(t *testing.T) {
	rs, _, auditPath := xtcpTestServer(t)

	conn, _, _ := visitorWithPunchOffer(t, rs)
	waitFor(t, "the punch attempt to be registered", func() bool {
		return rs.server.metrics.p2pPunches.Load() == 1
	})
	conn.Close()

	// Closing the control connection is not evidence that the punch worked: the
	// visitor may equally have given up. It is recorded as unknown, and never as a
	// direct path. Waiting for the abandoned event also proves the server has
	// finished the report window before the counter is read.
	events := waitForAuditEvent(t, auditPath, EventP2PAbandoned)
	if eventNames(events)[EventP2PDirect] {
		t.Errorf("a visitor that reported nothing was recorded as %s: %v", EventP2PDirect, events)
	}
	if direct := rs.server.metrics.p2pDirect.Load(); direct != 0 {
		t.Errorf("p2p_direct counted %d streams, want 0", direct)
	}
}

func TestAVisitorThatAsksForTheRelayIsCountedAsRelayed(t *testing.T) {
	rs, _, auditPath := xtcpTestServer(t)

	conn, framer, offer := visitorWithPunchOffer(t, rs)
	if err := framer.WriteJSON(protocol.TypeP2PFallback,
		protocol.P2PFallback{Proxy: "p2p", Token: offer.Token}); err != nil {
		t.Fatalf("ask for the relay: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the relayed path was refused: %s", ack.Error)
	}
	conn.Close()

	if relayed := rs.server.metrics.p2pRelayed.Load(); relayed != 1 {
		t.Errorf("p2p_relayed counted %d, want 1", relayed)
	}
	waitForAuditEvent(t, auditPath, EventP2PRelayed)
}

func TestAPathReportForAnUnknownTokenChangesNothing(t *testing.T) {
	rs, cfg, _ := xtcpTestServer(t)

	before := rs.server.metrics.p2pDirect.Load()
	reportPath(t, cfg, "00000000000000000000000000000000", protocol.PunchPathDirect)
	// The rendezvous loop answers datagrams in order, so a second one that does
	// belong to an attempt is the barrier that proves the first was processed.
	conn, _, offer := visitorWithPunchOffer(t, rs)
	reportPath(t, cfg, offer.Token, protocol.PunchPathDirect)
	conn.Close()

	waitFor(t, "the real attempt to be counted", func() bool {
		return rs.server.metrics.p2pDirect.Load() == before+1
	})
}
