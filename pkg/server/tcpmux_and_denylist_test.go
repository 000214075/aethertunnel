package server

import (
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// bind gives a tcpmux group a binding and no listener, and an https proxy with
// tls_passthrough only an SNI binding, so published() has to count the bindings:
// the helper the tests wait on would otherwise time out on a group that is up.
func TestATCPMuxGroupWithItsBindingIsPublished(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.TCPMuxPort = freePort(t)
	rs := startServer(t, cfg)

	session := &Session{ID: "mux", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	if _, err := rs.server.tunnels.Register(session, protocol.ProxySpec{
		Name: "mux", Type: protocol.ProxyTypeTCPMux, Multiplexer: config.MultiplexerHTTPConnect,
		Domains: []string{"first.example"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	group, err := rs.server.tunnels.Get("mux")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !group.published() {
		t.Fatal("a tcpmux group holding its binding is not reported as published")
	}
}

// The gateway is an entry point like the control port, so the same admission
// refuses a denied source before the SSH handshake and books the refusal in the
// shared series. Without that call the gateway answers with its banner instead:
// the source reaches SSH, and the counters never hear about it.
func TestADenyListedSourceIsTurnedAwayAtTheGateway(t *testing.T) {
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  filepath.Join(t.TempDir(), "gateway_host_key"),
	}
	cfg.Server.DenyCIDRs = []string{"127.0.0.1/32"}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	waitForTCPPort(t, port)

	// The readiness probe dials the gateway too, and that attempt is refused and
	// counted, so the baseline waits for the counter to stop moving.
	before := settledCounter(&rs.server.metrics.controlRejected)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the gateway: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("a source the deny list refuses was answered by the gateway: %q", buf[:n])
	}
	if got := waitForCounter(&rs.server.metrics.controlRejected, before); got <= before {
		t.Fatalf("the refused gateway connection left control_rejected at %d", got)
	}
}

// settledCounter waits for an atomic counter to stop moving: a refusal is booked
// on the server's side of the connection, so a reading taken right after a dial
// can predate it.
func settledCounter(v *atomic.Int64) int64 {
	last := v.Load()
	for i := 0; i < 40; i++ {
		time.Sleep(25 * time.Millisecond)
		now := v.Load()
		if now == last {
			return now
		}
		last = now
	}
	return last
}

// waitForCounter waits for a counter to pass want, the way an operator watches a
// series: the increment lands asynchronously, on the server's side.
func waitForCounter(v *atomic.Int64, want int64) int64 {
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := v.Load()
		if got > want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}
