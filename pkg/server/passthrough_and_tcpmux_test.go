package server

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// tls_passthrough relays a TLS session chosen by its ClientHello, and the
// passthrough listener serves that shape only. The configuration validator
// refuses the flag on any other type, and a registration written by hand used to
// be accepted: the proxy was then published on a listener the flag cannot act on,
// and the address announced for it named the wrong port.
func TestTLSPassthroughIsRefusedOnANonHTTPSProxy(t *testing.T) {
	cfg := testConfig(t, false)
	// A configured passthrough port, so the SNI listener exists: without it every
	// passthrough registration is refused for the missing port instead, which hid
	// the flag on a type the listener cannot route.
	cfg.Server.HTTPSPassthroughPort = freePort(t)
	rs := startServer(t, cfg)

	session := &Session{ID: "plain", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	_, err := rs.server.tunnels.Register(session, protocol.ProxySpec{
		Name: "plain", Type: protocol.ProxyTypeHTTP, TLSPassthrough: true,
		Domains: []string{"plain.example"},
	})
	if err == nil {
		t.Fatal("an http proxy with tls_passthrough was registered")
	}
	if !strings.Contains(err.Error(), "tls_passthrough") {
		t.Fatalf("error %q does not name the flag it refused", err)
	}
	if groups := rs.server.tunnels.Groups(); len(groups) != 0 {
		t.Fatalf("the refused registration left %d proxies published", len(groups))
	}
}

// A tcp or udp tunnel registered without a remote_port binds nothing, so a record
// for it would name the control port — an address that cannot serve it. Nothing
// reaches the DHT node for such a proxy: the nil node here makes that literal,
// since any announcement would panic on it.
func TestAProxyWithNoEndpointIsNotAnnounced(t *testing.T) {
	for _, spec := range []protocol.ProxySpec{
		{Name: "tcp", Type: protocol.ProxyTypeTCP},
		{Name: "udp", Type: protocol.ProxyTypeUDP},
	} {
		if hasPublicEndpoint(spec) {
			t.Fatalf("%s with no remote_port reports a public endpoint", spec.Type)
		}
		dir := &directory{advertise: "203.0.113.7", logger: log.New(io.Discard, "", 0), controlPort: 7000}
		if err := dir.publish(spec); err != nil {
			t.Fatalf("publish(%s with no remote_port) = %v, want no error", spec.Type, err)
		}
	}
	for _, spec := range []protocol.ProxySpec{
		{Name: "tcp", Type: protocol.ProxyTypeTCP, RemotePort: 7001},
		{Name: "http", Type: protocol.ProxyTypeHTTP},
		{Name: "tcpmux", Type: protocol.ProxyTypeTCPMux},
	} {
		if !hasPublicEndpoint(spec) {
			t.Fatalf("%s reports no public endpoint", spec.Type)
		}
	}
}

// A pooled member's hostnames have to reach the binding that routes by name.
// tcpmux chooses a member by the name the visitor sent, and a name that was never
// registered answers as if no proxy owned it.
func TestAPooledTCPMuxMemberPublishesItsOwnDomains(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.TCPMuxPort = freePort(t)
	rs := startServer(t, cfg)

	first := &Session{ID: "first", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	second := &Session{ID: "second", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	spec := protocol.ProxySpec{
		Name: "mux", Type: protocol.ProxyTypeTCPMux, Multiplexer: config.MultiplexerHTTPConnect,
		Group: "pool", Domains: []string{"first.example"},
	}
	if _, err := rs.server.tunnels.Register(first, spec); err != nil {
		t.Fatalf("the first member: %v", err)
	}
	if got := rs.server.tunnels.tcpmux.lookup("first.example"); got == nil {
		t.Fatal("the first member's hostname is not published")
	}

	joined := spec
	joined.Domains = []string{"second.example"}
	joined.LocalAddr = "127.0.0.1:8000"
	if _, err := rs.server.tunnels.Register(second, joined); err != nil {
		t.Fatalf("the joining member: %v", err)
	}
	if got := rs.server.tunnels.tcpmux.lookup("second.example"); got == nil {
		t.Fatal("the joining member's hostname was not added to the tcpmux binding")
	}

	// A hostname another proxy already publishes is refused, and the member that
	// tried to take it is not left in the pool.
	other := &Session{ID: "other", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	owner := protocol.ProxySpec{
		Name: "owner", Type: protocol.ProxyTypeTCPMux, Multiplexer: config.MultiplexerHTTPConnect,
		Domains: []string{"taken.example"},
	}
	if _, err := rs.server.tunnels.Register(other, owner); err != nil {
		t.Fatalf("the owner of taken.example: %v", err)
	}
	// A third session, because a session registering its own name replaces its own
	// member rather than joining the pool.
	third := &Session{ID: "third", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	conflicting := spec
	conflicting.Domains = []string{"taken.example"}
	conflicting.LocalAddr = "127.0.0.1:8001"
	if _, err := rs.server.tunnels.Register(third, conflicting); err == nil {
		t.Fatal("a joining member took a hostname another proxy already publishes")
	}
	if got := rs.server.tunnels.tcpmux.lookup("taken.example"); got == nil || got.Name != "owner" {
		t.Fatalf("taken.example is now owned by %v, want the proxy that published it first", got)
	}
}

// A refused join drops only the hostnames that join added. The rollback used to
// take every hostname the call had looked at, including one the pool already
// published, so a member that repeated the pool's own hostname and also named a
// hostname another proxy owns left the pool unreachable for the life of the
// group — and nothing put the name back.
func TestARefusedJoinLeavesThePoolsOwnHostnamePublished(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.TCPMuxPort = freePort(t)
	rs := startServer(t, cfg)

	first := &Session{ID: "first", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	other := &Session{ID: "other", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	joiner := &Session{ID: "joiner", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}

	pool := protocol.ProxySpec{
		Name: "pool", Type: protocol.ProxyTypeTCPMux, Multiplexer: config.MultiplexerHTTPConnect,
		Group: "pool-group", Domains: []string{"pool.example"},
	}
	if _, err := rs.server.tunnels.Register(first, pool); err != nil {
		t.Fatalf("the pool's first member: %v", err)
	}
	owner := protocol.ProxySpec{
		Name: "owner", Type: protocol.ProxyTypeTCPMux, Multiplexer: config.MultiplexerHTTPConnect,
		Domains: []string{"taken.example"},
	}
	if _, err := rs.server.tunnels.Register(other, owner); err != nil {
		t.Fatalf("the owner of taken.example: %v", err)
	}

	joining := pool
	joining.Domains = []string{"pool.example", "taken.example"}
	joining.LocalAddr = "127.0.0.1:8100"
	if _, err := rs.server.tunnels.Register(joiner, joining); err == nil {
		t.Fatal("the joining member took a hostname another proxy owns")
	}
	if got := rs.server.tunnels.tcpmux.lookup("pool.example"); got == nil || got.Name != "pool" {
		t.Fatalf("the failed join unpublished the pool's own hostname: lookup returned %v", got)
	}
	if got := rs.server.tunnels.tcpmux.lookup("taken.example"); got == nil || got.Name != "owner" {
		t.Fatalf("taken.example is owned by %v, want the proxy that published it first", got)
	}
}
