package server

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A same-session re-registration replaces its own member, and the group keeps the
// endpoint it already bound: nothing rebinds it, while Register announces the new
// spec to the DHT. A re-registration that moves remote_port would therefore be
// announced at a port nothing listens on, with the old port still serving and
// nobody pointing at it. The refusal names the field; the identical spec stays
// acceptable, which is what a health check's republish and the gateway's second
// channel send.
func TestAReRegistrationMayNotMoveThePublishedEndpoint(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	port := freePort(t)
	spec := protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: port,
	}
	agent.register(spec)

	if err := agent.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("send the re-registration: %v", err)
	}
	refusal := agent.expectRefused("web")
	if !strings.Contains(refusal, "remote_port") {
		t.Fatalf("the refusal does not name the field that moved: %s", refusal)
	}

	group, err := rs.server.tunnels.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if group.RemotePort != port {
		t.Fatalf("the published endpoint is on port %d, want %d", group.RemotePort, port)
	}
	if members := group.Members(); len(members) != 1 || members[0].RemotePort != port {
		t.Fatalf("the member list is %+v, want the original registration", members)
	}

	// The endpoint did not move, so the same spec is still a plain replacement.
	agent.register(spec)
}

// A registration whose endpoint came up and that is then rolled back — here the
// session closes between the bind and the session's own bookkeeping — must keep
// the name's counter. The endpoint was live, so a visitor stream may already hold
// the counter and be writing into it; dropping it lost those bytes and left the
// ledger without the name it carried.
func TestARegistrationRolledBackAfterItsEndpointCameUpKeepsItsCounter(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, Options{Version: "test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.auditor.Close() })

	session := newPipeSession(t, srv, "ssh-client", 0)
	// The session is gone before the registration: Register binds the endpoint,
	// then RegisterProxy refuses to add it to a closed session.
	session.Close("shutdown: the session ends during the registration")
	if tunnel, err := srv.registerSSHProxy(session, protocol.ProxySpec{
		Name: "ssh-echo", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: freePort(t),
	}); err == nil {
		t.Fatal("a closed session published a proxy")
	} else if tunnel != nil {
		t.Fatal("the failed registration returned a tunnel")
	}

	if usage := session.Usage(); len(usage) != 1 {
		t.Fatalf("the session dropped the counter of a name whose endpoint was live: %v", usage)
	}
}

// A registration that never published anything still leaves no counter behind, and
// the SSH gateway path is checked for it next to the client path: here the bind
// fails, so no visitor can have been served.
func TestASSHGatewayRegistrationThatNeverPublishedLeavesNoCounter(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, Options{Version: "test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.auditor.Close() })

	// A port that is already taken, so the endpoint cannot come up.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	srvSession := newPipeSession(t, srv, "ssh-client", 0)
	if _, err := srv.registerSSHProxy(srvSession, protocol.ProxySpec{
		Name: "ssh-clash", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: taken.Addr().(*net.TCPAddr).Port,
	}); err == nil {
		t.Fatal("a registration whose endpoint was already taken was published")
	}
	if usage := srvSession.Usage(); len(usage) != 0 {
		t.Fatalf("the session kept counters for names it never published: %v", usage)
	}
}

// A stream asked of a member that has already left its group must be refused, and
// it must not count against the name: counting recreated the metrics series
// forgetTunnel had just dropped, so a withdrawn name kept a row in every scrape.
func TestAStreamAskedOfARemovedMemberIsRefused(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: freePort(t),
	})

	group, err := rs.server.tunnels.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	members := group.Members()
	if len(members) != 1 {
		t.Fatalf("%d members are registered, want one", len(members))
	}
	member := members[0]

	// What the withdrawal does: the member is closed and its series is dropped
	// while a dispatch that already picked it is still on its way here.
	rs.server.metrics.forgetTunnel("web")
	member.closed.Store(true)

	if _, _, err := member.openStreamWith(protocol.DataRequest{Proxy: "web"}); !errors.Is(err, errProxyUnpublished) {
		t.Fatalf("openStreamWith = %v, want errProxyUnpublished", err)
	}
	if rendered := rs.server.metrics.Render(); strings.Contains(rendered, `tunnel="web"`) {
		t.Fatalf("the refused stream recreated the series of an unpublished proxy:\n%s", rendered)
	}
}

// A proxy name is not validated where it is registered, and an SSH gateway exec
// command can carry any byte, so the exposition has to survive one: the Prometheus
// text format wants UTF-8, and a byte that does not decode as a rune is replaced
// rather than copied through, where one bad name would make the whole scrape
// unreadable.
func TestPromLabelsReplaceBytesThatAreNotUTF8(t *testing.T) {
	// The replacement is percent-encoded rather than a literal '?': '?' mapped
	// every invalid byte to the same character, so two names — one carrying an
	// invalid byte, one carrying a literal '?' — rendered as one label value
	// and a single scrape carried two identical series lines.
	if got := promLabel("a\xffb"); got != `"a%FFb"` {
		t.Fatalf("promLabel of a name with an invalid byte = %q, want %q", got, `"a%FFb"`)
	}
	// A truncated multi-byte sequence: both bytes are invalid on their own.
	if got := promLabel("bad\xe4\xb8end"); got != `"bad%E4%B8end"` {
		t.Fatalf("promLabel of a truncated rune = %q, want %q", got, `"bad%E4%B8end"`)
	}
	// A valid multi-byte name is not touched: the replacement is for bytes that
	// are not part of a rune, not for anything non-ASCII.
	if got := promLabel("隧道"); got != `"隧道"` {
		t.Fatalf("promLabel of a valid name = %q, want it unchanged", got)
	}
}

// An http proxy is routed by hostname: bind installs a virtual host and opens no
// public port, so the remote_port beside it buys nothing. Counting it against
// max_ports_per_client spent a client's allowance on a port that was never opened
// and refused it the port it did ask for.
func TestAnHTTPProxyDoesNotSpendThePublicPortBudget(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.MaxPortsPerClient = 1
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	agent.register(protocol.ProxySpec{
		Name: "site", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"site.example"}, RemotePort: freePort(t),
	})

	if err := agent.client.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:2", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register the tcp proxy: %v", err)
	}
	agent.expectAccepted("echo")
}
