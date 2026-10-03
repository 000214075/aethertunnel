package server

import (
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestWebsocketTransportSharesThePort covers the whole websocket arrangement:
// an agent that upgrades every connection to the server — control and data —
// works end to end, and a plain agent on the same port is untouched, which is
// what the server's per-connection sniffing promises.
func TestWebsocketTransportSharesThePort(t *testing.T) {
	echo := echoService(t)
	cfg := testConfig(t, false)
	port := freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	// The websocket agent: the same dial address, an upgrade in front of the
	// framer, exactly what [transport].protocol = "websocket" builds.
	agent := startAgentWebsocket(t, rs.addr, false, map[string]dataHandler{"echo": framedStreamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: port,
	})

	roundtrip := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoaOf(port)), 5*time.Second)
		if err != nil {
			t.Fatalf("dial the public port: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		payload := "ping-over-websocket"
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
		reply := make([]byte, len(payload))
		if _, err := readFullWithDeadline(conn, reply); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(reply) != payload {
			t.Fatalf("the tunnel carried %q, want %q", reply, payload)
		}
	}
	roundtrip()
	roundtrip()

	// A plain agent on the same server still authenticates and serves, so the
	// sniffing really is per connection and the raw path lost nothing.
	plain := startAgent(t, rs.addr, false, map[string]dataHandler{"plain": framedStreamHandler(echo)})
	plainPort := freePort(t)
	plain.register(protocol.ProxySpec{
		Name: "plain", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: plainPort,
	})
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoaOf(plainPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the plain proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("plain-ping")); err != nil {
		t.Fatalf("write to the plain proxy: %v", err)
	}
	reply := make([]byte, len("plain-ping"))
	if _, err := readFullWithDeadline(conn, reply); err != nil {
		t.Fatalf("read from the plain proxy: %v", err)
	}
	if string(reply) != "plain-ping" {
		t.Fatalf("the plain proxy carried %q", reply)
	}
}

// startAgentWebsocket is startAgent with every connection to the server
// wrapped in the tunnel's websocket upgrade first.
func startAgentWebsocket(t *testing.T, serverAddr string, encryption bool, handlers map[string]dataHandler) *testAgent {
	t.Helper()
	client, err := newTestClientWebsocket(t, serverAddr, encryption)
	if err != nil {
		t.Fatalf("connect over websocket: %v", err)
	}
	if _, err := client.authenticate("test-agent", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	agent := startAgentWithClient(t, client, serverAddr, handlers)
	agent.websocket = true
	return agent
}

func newTestClientWebsocket(t *testing.T, serverAddr string, encryption bool) (*testClient, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	host := serverAddr
	if h, _, splitErr := net.SplitHostPort(serverAddr); splitErr == nil {
		host = h
	}
	upgraded, err := flynet.WebsocketDial(conn, host, 5*time.Second)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var cipher *crypto.Cipher
	if encryption {
		cipher, err = crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, testToken, "test-salt")
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return &testClient{conn: upgraded, framer: protocol.NewFramer(upgraded, cipher, 0), cipher: cipher}, nil
}

func readFullWithDeadline(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
