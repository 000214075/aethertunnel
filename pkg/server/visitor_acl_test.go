package server

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The visitor rules bound pairing with a private proxy exactly as they bound a
// public endpoint: the server's [[proxies]] lists and the proxy's own
// allow_cidrs/deny_cidrs. The private path used to run neither, so a visitor the
// operator's policy excluded was served, and the denial never reached the metrics
// or the audit log.
func TestAVisitorIsRefusedWhenTheProxysListsDenyItsAddress(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"private": streamHandler(echo),
		"open":    streamHandler(echo),
	})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
		User: "ada", DenyCIDRs: []string{"127.0.0.1/32"},
	})
	agent.register(protocol.ProxySpec{
		Name: "open", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
		User: "ada",
	})

	// The same client, the same secret, the same source address: only the deny
	// list differs, so a refusal can only come from the list.
	if ack := visitorAckAs(t, rs.addr, "open", "s3cret", protocol.ProxyTypeSTCP, "ada"); !ack.OK {
		t.Fatalf("a private proxy with no lists refused its visitor: %s", ack.Error)
	}
	ack := visitorAckAs(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP, "ada")
	if ack.OK {
		t.Fatal("a visitor in the proxy's deny list was served")
	}
	if !strings.Contains(ack.Error, "deny") {
		t.Fatalf("the refusal does not name the list that refused it: %s", ack.Error)
	}
}

// An xtcp visitor that asks for the WebRTC transport used to take the punch path:
// the server sent a punch offer, the client answered it by punching, and the
// WebRTC offer it sends after a failed punch was relayed to the local service as
// data — the visitor then waited out an answer that never came. The transport
// decides the data path before the type does.
func TestAnXTCPVisitorThatAsksForWebRTCGetsTheWebRTCPath(t *testing.T) {
	rs, _, _ := xtcpTestServer(t)

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "p2p", Type: protocol.ProxyTypeXTCP, AuthToken: testToken,
		Secret: "s3cret", Transport: config.TransportWebRTC,
	}); err != nil {
		t.Fatalf("send the visitor connect: %v", err)
	}

	msg, err := framer.ReadFrame()
	if err != nil {
		t.Fatalf("read the server's answer: %v", err)
	}
	if msg.Type != protocol.TypeDataOpenAck {
		t.Fatalf("the first answer is a %s frame, want the WebRTC acceptance: the visitor was handed the punch path", msg.Type)
	}
	var ack protocol.DataOpenAck
	if err := json.Unmarshal(msg.Payload, &ack); err != nil {
		t.Fatalf("decode the acceptance: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the WebRTC path was refused: %s", ack.Error)
	}
}
