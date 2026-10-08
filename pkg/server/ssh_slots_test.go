package server

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A registration that fails gives its max_proxies slot back at once. The refusal
// tells the operator to fix the command and try again, and the slot used to be
// released by a deferred call that only ran when the whole SSH connection ended —
// waitDone holds runCommand open for exactly that long. With max_proxies = 1,
// every later attempt on that connection was therefore refused as over the cap
// while nothing at all was published.
func TestAFailedSSHGatewayRegistrationFreesItsMaxProxiesSlot(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr:   "127.0.0.1",
		BindPort:   port,
		KeyFile:    keyPath,
		MaxProxies: 1,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	startServer(t, cfg)

	// The port the first command asks for is held here, so the server's own bind
	// for it fails: a registration failure, which is what has to free the slot.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	first, err := client.NewSession()
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	firstOut, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("first stdout: %v", err)
	}
	if err := first.Start(fmt.Sprintf("tcp --proxy_name one --remote_port %d --token %s", busyPort, testToken)); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	readSSHUntil(t, firstOut, "already in use", 5*time.Second)

	// The retry the refusal invites publishes a different name on a port that is
	// free. It was refused with "maximum of 1 proxies" while the failed attempt's
	// slot was still held.
	second, err := client.NewSession()
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	secondOut, err := second.StdoutPipe()
	if err != nil {
		t.Fatalf("second stdout: %v", err)
	}
	if err := second.Start(fmt.Sprintf("tcp --proxy_name two --remote_port %d --token %s", freePort(t), testToken)); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	readSSHUntil(t, secondOut, "ProxyName: two", 5*time.Second)
}

// The newProxy plugin runs inside Register, after the caller checked
// server.allow_ports, so the rewritten remote_port has to meet the same list: a
// plugin that names a port the operator excluded must not be able to publish it.
func TestANewProxyRewriteCannotPublishAPortOutsideAllowPorts(t *testing.T) {
	// Inside the allowlist for the registration, outside it for the rewrite.
	const askedPort, rewrittenPort = 50005, 50020
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		return http.StatusOK, fmt.Sprintf(
			`{"reject":false,"unchange":false,"content":{"name":"echo","type":"tcp","remote_port":%d}}`, rewrittenPort)
	})

	cfg := testConfig(t, false)
	cfg.Server.AllowPorts = []string{fmt.Sprintf("%d-%d", askedPort, askedPort+10)}
	cfg.HTTPPlugins = []config.HTTPPluginConfig{
		{Name: "rewrite", Addr: hook.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	allowPorts, err := config.ParsePortRanges(cfg.Server.AllowPorts)
	if err != nil {
		t.Fatalf("allow_ports: %v", err)
	}

	manager := newTunnelManager(cfg, discardLogger(), nil, newSessionManager(0), newMetrics(), nil, allowPorts)
	manager.httpPlugins = newHTTPPluginManager(cfg, discardLogger())

	_, err = manager.Register(&Session{}, protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, RemotePort: askedPort,
	})
	if err == nil {
		t.Fatal("a rewrite to a port outside allow_ports was published")
	}
	if !strings.Contains(err.Error(), "outside allow_ports") {
		t.Fatalf("the refusal does not name allow_ports: %v", err)
	}
}

// The check above is the allowlist, not a blanket refusal of rewritten ports: a
// rewrite that stays inside it proceeds to the rest of Register, which here
// refuses the registration for its own reason.
func TestANewProxyRewriteInsideAllowPortsPassesTheAllowlist(t *testing.T) {
	const port = 50005
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		return http.StatusOK, fmt.Sprintf(
			`{"reject":false,"unchange":false,"content":{"name":"mux","type":"tcpmux","remote_port":%d}}`, port)
	})

	cfg := testConfig(t, false)
	cfg.Server.AllowPorts = []string{fmt.Sprintf("%d-%d", port, port+10)}
	cfg.HTTPPlugins = []config.HTTPPluginConfig{
		{Name: "rewrite", Addr: hook.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	allowPorts, err := config.ParsePortRanges(cfg.Server.AllowPorts)
	if err != nil {
		t.Fatalf("allow_ports: %v", err)
	}

	manager := newTunnelManager(cfg, discardLogger(), nil, newSessionManager(0), newMetrics(), nil, allowPorts)
	manager.httpPlugins = newHTTPPluginManager(cfg, discardLogger())

	_, err = manager.Register(&Session{}, protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, RemotePort: port,
		Multiplexer: config.MultiplexerHTTPConnect, Domains: []string{"mux.example"},
	})
	if err == nil {
		t.Fatal("a tcpmux registration that opens a public port was published")
	}
	if strings.Contains(err.Error(), "allow_ports") {
		t.Fatalf("a rewritten port inside allow_ports was refused by the allowlist: %v", err)
	}
	if !strings.Contains(err.Error(), "tcpmux") {
		t.Fatalf("the registration failed for an unexpected reason: %v", err)
	}
}

// Two exec channels on one SSH connection that name the same proxy hold one
// max_proxies slot between them. The second registration replaces the first
// member of the same session rather than adding a proxy, so charging a slot for
// each let one published proxy consume the whole cap until the connection ended.
// The check and the count share one critical section, and the slot is remembered
// by name: apart, the second command read the session's tunnels while the first
// registration was still in flight, saw nothing published, and took a second slot.
func TestTwoCommandsNamingOneProxyHoldOneMaxProxiesSlot(t *testing.T) {
	conn := &sshGatewayConn{session: &Session{}}

	if over, taken := conn.reserveProxySlot("web", 2); over || !taken {
		t.Fatalf("the first command reserved no slot (over=%v taken=%v)", over, taken)
	}
	// Nothing is published yet: the first registration is still in flight, and
	// the second command naming the same proxy pins the slot the first one took
	// instead of taking a second. The pin is what lets its failure release only
	// its own share and leave the slot with the command that goes on publishing.
	if over, taken := conn.reserveProxySlot("web", 2); over || !taken {
		t.Fatalf("the second command naming the same proxy did not pin the shared slot (over=%v taken=%v)", over, taken)
	}
	if over, taken := conn.reserveProxySlot("ssh", 2); over || !taken {
		t.Fatalf("a second, different proxy did not get the second slot (over=%v taken=%v)", over, taken)
	}
	if over, taken := conn.reserveProxySlot("smtp", 2); !over || taken {
		t.Fatalf("a third name was not refused by max_proxies = 2 (over=%v taken=%v)", over, taken)
	}
	conn.releaseProxySlot("ssh")
	if over, taken := conn.reserveProxySlot("smtp", 2); over || !taken {
		t.Fatalf("the slot a failed registration released was not free again (over=%v taken=%v)", over, taken)
	}
}

// A name's metric series goes when the name leaves the manager's map, not when
// its group closes. close runs while the manager still holds the entry, and a
// same-name registration can already be building the group that takes the name
// over — its first stream creates the series lazily, so forgetting inside close
// deleted the series of the group that had just taken the name and left it
// missing until that group's next stream.
func TestTheSeriesOfAProxyOutlivesItsGroupUntilItsNameLeavesTheManager(t *testing.T) {
	cfg := testConfig(t, false)
	allowPorts, err := config.ParsePortRanges(cfg.Server.AllowPorts)
	if err != nil {
		t.Fatalf("allow_ports: %v", err)
	}
	metrics := newMetrics()
	manager := newTunnelManager(cfg, discardLogger(), nil, newSessionManager(0), metrics, nil, allowPorts)
	manager.httpPlugins = newHTTPPluginManager(cfg, discardLogger())

	session := &Session{}
	if _, err := manager.Register(session, protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeTCP}); err != nil {
		t.Fatalf("register: %v", err)
	}
	group, err := manager.Get("web")
	if err != nil {
		t.Fatalf("the name is not published: %v", err)
	}
	// A stream for the name, or the same-name rebuild's first stream.
	metrics.tunnel("web")
	if _, ok := metrics.lookupTunnel("web"); !ok {
		t.Fatal("the series was not created for the test")
	}

	group.close("test")
	if _, ok := metrics.lookupTunnel("web"); !ok {
		t.Fatal("closing a group dropped the series of a name that is still in the manager's map")
	}

	manager.Unregister("web", session, "test")
	if _, ok := metrics.lookupTunnel("web"); ok {
		t.Fatal("the series outlived the name it belonged to")
	}
}

// Stop closes a metrics listener that Serve never picked up, which is the window
// between Start's two lock sections and the reason m.server can be nil. The early
// return on a nil server skipped the listener close and left the port bound for
// the life of the process, on the one path where nothing else could close it.
func TestStoppingAMetricsListenerBeforeServeClosesItsListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()

	m := &MetricsListener{}
	m.ln = listener
	m.Stop()

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the metrics listener's port is still bound after Stop: %v", err)
	}
	_ = again.Close()
}

// A session whose own end of the pipe is closed has to take its SSH connection
// with it. The dashboard's disconnect closes the internal session, and the
// session's side of the pipe then reads EOF — with only that, the SSH connection
// stayed up with its proxies still published, and the teardown that withdraws
// them (closeSession, in handleConn's deferred path) never ran. The connection is
// what made the ssh client's remote forwarding live, so leaving it open meant the
// disconnect did nothing at all.
func TestClosingAnSSHSessionClosesItsConnectionAndWithdrawsItsTunnels(t *testing.T) {
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  filepath.Join(t.TempDir(), "gateway_host_key"),
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}
	exec, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	out, err := exec.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := exec.Start(fmt.Sprintf("tcp --proxy_name one --remote_port %d --token %s", freePort(t), testToken)); err != nil {
		t.Fatalf("exec: %v", err)
	}
	readSSHUntil(t, out, "ProxyName: one", 5*time.Second)

	group, err := rs.server.tunnels.Get("one")
	if err != nil {
		t.Fatalf("the proxy is not published: %v", err)
	}
	members := group.Members()
	if len(members) != 1 {
		t.Fatalf("the group has %d member(s), want 1", len(members))
	}
	// What the dashboard's DELETE /api/clients/{id} does.
	go members[0].Session.Close("disconnected from the dashboard")

	// The SSH connection has to end: the exec channel's waitDone returns only
	// when the connection's done channel closes.
	gone := make(chan struct{})
	go func() { _ = client.Wait(); close(gone) }()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh connection stayed up after its session was closed")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := rs.server.tunnels.Get("one"); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the published proxy outlived the session that was closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
