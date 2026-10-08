package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// feature_gates are the server's own policy: a client whose configuration has no
// gates still meets them here. A visitor whose path needs a turned-off capability
// is refused with the gate's name, on both gated paths — the WebRTC transport and
// the Groth16 visitor proof.
func TestTheServerRefusesVisitorsAGateTurnsOff(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.FeatureGates = map[string]bool{config.FeatureWebRTC: false, config.FeatureSnark: false}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodSNARK,
	})

	refuse := func(transport string) string {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		framer := protocol.NewFramer(conn, nil, 0)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
			Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
			Secret: "s3cret", Transport: transport,
		}); err != nil {
			t.Fatalf("send the visitor connect: %v", err)
		}
		var ack protocol.DataOpenAck
		if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
			t.Fatalf("read the ack: %v", err)
		}
		if ack.OK {
			t.Fatalf("a visitor needing a turned-off gate (transport %q) was accepted", transport)
		}
		return ack.Error
	}

	if reason := refuse(config.TransportWebRTC); !strings.Contains(reason, "feature_gates.WebRTC") {
		t.Fatalf("the webrtc refusal does not name the gate: %q", reason)
	}
	if reason := refuse(config.TransportDefault); !strings.Contains(reason, "feature_gates.Snark") {
		t.Fatalf("the snark refusal does not name the gate: %q", reason)
	}
}

// With no gate set, the same visitor reaches the proof: the refusal, if any, is
// about the secret and not about a gate.
func TestVisitorsReachTheProofWhenNoGateTurnsThemOff(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodSNARK,
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send the visitor connect: %v", err)
	}

	// The snark path answers a visitor connect with a challenge, not an ack: that
	// alone proves the gate did not refuse this connection.
	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		t.Fatalf("read the challenge: %v", err)
	}
	if len(challenge.Nonce) == 0 {
		t.Fatal("the server sent an empty challenge nonce")
	}
}
