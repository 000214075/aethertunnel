package server

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// server.max_pool_count, frp's maxPoolCount, caps how many data connections one
// session may hold parked. An offer beyond the cap is refused by name — the client
// then dials per stream, so the cap costs a dial, never the visitor a stream: the
// connection that did fit still serves it.
func TestServerMaxPoolCountCapsThePool(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.MaxPoolCount = 1
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	publicPort := freePort(t)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read the proxy list: %v", err)
	}

	// offer opens one connection and offers it to the pool. It returns the
	// established connection when the server parked it, or the refusal's wording
	// when it did not.
	offer := func() (net.Conn, *protocol.Framer, string) {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("park dial: %v", err)
		}
		framer := protocol.NewFramer(conn, client.cipher, 0)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{Session: client.session, Pool: true}); err != nil {
			_ = conn.Close()
			t.Fatalf("send the pool offer: %v", err)
		}
		var ack protocol.DataOpenAck
		if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
			_ = conn.Close()
			t.Fatalf("read the pool ack: %v", err)
		}
		_ = conn.SetReadDeadline(time.Time{})
		if ack.OK {
			return conn, framer, ""
		}
		_ = conn.Close()
		return nil, nil, ack.Error
	}

	parkedConn, parkedFramer, reason := offer()
	if reason != "" {
		t.Fatalf("the first offer was refused: %s", reason)
	}
	defer parkedConn.Close()

	for i := 0; i < 2; i++ {
		conn, _, reason := offer()
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("offer %d beyond max_pool_count was parked", i+2)
		}
		if !strings.Contains(reason, "maximum number of parked connections") {
			t.Fatalf("refusal %d does not name the cap: %q", i+1, reason)
		}
	}

	if refused := rs.server.metrics.poolRefused.Load(); refused != 2 {
		t.Fatalf("pool_refused is %d, want 2", refused)
	}
	if parked := rs.server.metrics.poolParked.Load(); parked != 1 {
		t.Fatalf("pool_parked is %d, want 1", parked)
	}

	// The stream is still served on the one connection that fit, which is why the
	// cap is safe to lower.
	served := make(chan error, 1)
	go func() {
		served <- client.serveParkedStreamOn(t, parkedConn, parkedFramer, echoAddr, 10*time.Second)
	}()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}

	payload := []byte("a stream that fits under the cap")
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	_ = visitor.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
	// The relay runs until one side closes, so the visitor closes before the test
	// waits for it; closing in the defer would block here for its whole idle time.
	visitor.Close()
	if err := <-served; err != nil {
		t.Fatalf("serving the stream on the parked connection: %v", err)
	}
	if used := rs.server.metrics.poolUsed.Load(); used != 1 {
		t.Fatalf("pool_used is %d, want 1: the stream did not ride the parked connection", used)
	}
}
