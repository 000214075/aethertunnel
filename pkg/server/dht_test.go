package server

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// dhtConfig is a server configuration with the DHT enabled on a free loopback port.
func dhtConfig(t *testing.T, encryption bool) *config.Config {
	t.Helper()
	cfg := testConfig(t, encryption)
	cfg.DHT.Enabled = true
	cfg.DHT.ListenAddr = "127.0.0.1:" + itoa(freeUDPPort(t))
	cfg.DHT.AdvertiseHost = "127.0.0.1"
	cfg.DHT.AnnounceTTLSeconds = 60
	cfg.DHT.RepublishSeconds = 20
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("dht config invalid: %v", err)
	}
	return cfg
}

func itoa(v int) string { return strconv.Itoa(v) }

// --- tests --------------------------------------------------------------------

func TestDirectoryPublishesARegisteredProxy(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := dhtConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	if _, err := client.authenticate("test-client", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{
		Name: "ssh", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "ssh")

	// A separate node, bootstrapped on the server's DHT address, resolves the name.
	visitor, err := discovery.Start(discovery.Config{
		ListenAddr: "127.0.0.1:" + itoa(freeUDPPort(t)),
		Bootstrap:  []string{rs.server.directory.node.Addr()},
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("start the resolving node: %v", err)
	}
	defer visitor.Close()

	record, err := resolveEventually(t, visitor, "ssh")
	if err != nil {
		t.Fatalf("resolve ssh: %v", err)
	}

	want := net.JoinHostPort("127.0.0.1", itoa(publicPort))
	if record.Server != want {
		t.Errorf("the announcement names %q, want %q", record.Server, want)
	}
	if record.Type != "tcp" {
		t.Errorf("the announcement reports type %q, want tcp", record.Type)
	}
}

func TestDirectoryWithdrawsWhenTheLastMemberLeaves(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := dhtConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := client.authenticate("test-client", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{
		Name: "ssh", Type: "tcp", LocalAddr: echoAddr, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "ssh")

	if got := rs.server.directory.node.Announced(); len(got) != 1 || got[0] != "ssh" {
		t.Fatalf("the directory announces %v, want [ssh]", got)
	}

	client.close()

	// The withdrawal happens on the connection handler's teardown path.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(rs.server.directory.node.Announced()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy was still announced 5s after the client left")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := rs.server.directory.Lookup("ssh"); err == nil {
		t.Error("the withdrawn proxy still resolves")
	}
}

func TestDirectoryReportsItsState(t *testing.T) {
	cfg := dhtConfig(t, false)
	rs := startServer(t, cfg)

	summary := rs.server.directory.summary()
	if enabled, _ := summary["enabled"].(bool); !enabled {
		t.Fatalf("the summary reports the DHT as disabled: %v", summary)
	}
	if summary["advertise_as"] != "127.0.0.1" {
		t.Errorf("advertise_as is %v, want 127.0.0.1", summary["advertise_as"])
	}
	nodeID, _ := summary["node_id"].(string)
	if len(nodeID) != 40 {
		t.Errorf("node_id is %q, want 40 hex characters", nodeID)
	}
	if namespace, _ := summary["namespace"].(string); namespace != config.DefaultDHTNamespace {
		t.Errorf("namespace is %q, want %q", namespace, config.DefaultDHTNamespace)
	}
}

func TestDirectoryDisabledIsInert(t *testing.T) {
	cfg := testConfig(t, false)
	dir, err := openDirectory(cfg, discardLogger())
	if err != nil {
		t.Fatalf("openDirectory with the DHT disabled: %v", err)
	}
	if dir != nil {
		t.Fatal("a DHT node was started even though dht.enabled is false")
	}
	if err := dir.Close(); err != nil {
		t.Errorf("closing a disabled directory: %v", err)
	}
	if err := dir.publish(protocol.ProxySpec{Name: "ssh", Type: "tcp"}); err != nil {
		t.Errorf("publishing through a disabled directory: %v", err)
	}
	dir.withdraw("ssh")
	if dir.Publishes() != 0 {
		t.Error("a disabled directory reports publications")
	}
	if _, err := dir.Lookup("ssh"); err == nil {
		t.Error("a disabled directory resolved a name")
	}
	summary := dir.summary()
	if enabled, _ := summary["enabled"].(bool); enabled {
		t.Errorf("a disabled directory reports itself as enabled: %v", summary)
	}
}

func TestServerAddrForPicksThePortAVisitorDials(t *testing.T) {
	dir := &directory{advertise: "203.0.113.7", controlPort: 7000, httpPort: 7080, httpsPort: 7443}

	cases := []struct {
		name string
		spec protocol.ProxySpec
		want string
	}{
		{"tcp uses its own port", protocol.ProxySpec{Type: config.ProxyTypeTCP, RemotePort: 22}, "203.0.113.7:22"},
		{"udp uses its own port", protocol.ProxySpec{Type: config.ProxyTypeUDP, RemotePort: 51820}, "203.0.113.7:51820"},
		{"http uses the shared listener", protocol.ProxySpec{Type: config.ProxyTypeHTTP}, "203.0.113.7:7080"},
		{"https uses the shared TLS listener", protocol.ProxySpec{Type: config.ProxyTypeHTTPS}, "203.0.113.7:7443"},
		{"stcp is reached through the control port", protocol.ProxySpec{Type: config.ProxyTypeSTCP}, "203.0.113.7:7000"},
		{"sudp is reached through the control port", protocol.ProxySpec{Type: config.ProxyTypeSUDP}, "203.0.113.7:7000"},
		{"xtcp is reached through the control port", protocol.ProxySpec{Type: config.ProxyTypeXTCP}, "203.0.113.7:7000"},
		{"tcp without a remote port falls back to the control port", protocol.ProxySpec{Type: config.ProxyTypeTCP}, "203.0.113.7:7000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dir.serverAddrFor(tc.spec); got != tc.want {
				t.Fatalf("serverAddrFor(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

func TestServerAddrForBracketsAnIPv6AdvertiseAddress(t *testing.T) {
	dir := &directory{advertise: "2001:db8::1", controlPort: 7000}
	if got := dir.serverAddrFor(protocol.ProxySpec{Type: config.ProxyTypeSTCP}); got != "[2001:db8::1]:7000" {
		t.Fatalf("serverAddrFor over IPv6 = %q, want [2001:db8::1]:7000", got)
	}
}

func TestLookupProxyWithoutTheDHTEnabled(t *testing.T) {
	cfg := testConfig(t, false)
	if _, err := LookupProxy(cfg, discardLogger(), "ssh"); err == nil {
		t.Fatal("LookupProxy succeeded with the DHT disabled")
	}
}

// resolveEventually waits for a record to become resolvable: the joining node
// learns the record from the DHT, which takes a round trip.
func resolveEventually(t *testing.T, node *discovery.Node, name string) (discovery.Record, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		record, err := node.Resolve(name)
		if err == nil {
			return record, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return discovery.Record{}, lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLookupProxyResolvesThroughASecondNode is the operator path: a process that is
// not the server resolves a published name with -dht-lookup.
func TestLookupProxyResolvesThroughASecondNode(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := dhtConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("test-client", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{
		Name: "ssh", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "ssh")

	serverDHTAddr := rs.server.directory.node.Addr()
	// A second configuration resolves through the server's DHT node, and its
	// listen_addr deliberately names the port the running server already holds: a
	// lookup node binds its own port, so that collision must not matter.
	lookupCfg := &config.Config{}
	lookupCfg.DHT.Enabled = true
	lookupCfg.DHT.ListenAddr = serverDHTAddr
	lookupCfg.DHT.Bootstrap = []string{serverDHTAddr}
	if err := lookupCfg.Validate(""); err != nil {
		t.Fatalf("lookup config invalid: %v", err)
	}

	record, err := LookupProxy(lookupCfg, discardLogger(), "ssh")
	if err != nil {
		t.Fatalf("LookupProxy: %v", err)
	}
	if record.Server != net.JoinHostPort("127.0.0.1", itoa(publicPort)) {
		t.Errorf("resolved %q, want the published port %d", record.Server, publicPort)
	}
}

// TestLookupProxyFallsBackToTheConfiguredListenAddress covers the operator who runs
// the lookup with the server's own configuration, which names no bootstrap peer
// because the file describes the node that starts the DHT.
func TestLookupProxyFallsBackToTheConfiguredListenAddress(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := dhtConfig(t, false)
	cfg.DHT.ListenAddr = "0.0.0.0:" + itoa(freeUDPPort(t))
	cfg.DHT.AdvertiseHost = "127.0.0.1"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("test-client", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{
		Name: "ssh", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "ssh")

	// The server's own configuration, with no bootstrap peer of its own.
	record, err := LookupProxy(cfg, discardLogger(), "ssh")
	if err != nil {
		t.Fatalf("LookupProxy with no bootstrap peer: %v", err)
	}
	if record.Server != net.JoinHostPort("127.0.0.1", itoa(publicPort)) {
		t.Errorf("resolved %q, want the published port %d", record.Server, publicPort)
	}
}

func TestLocalDHTAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:7001", "127.0.0.1:7001"},
		{"0.0.0.0:7001", "127.0.0.1:7001"},
		{":7001", "127.0.0.1:7001"},
		{"[::]:7001", "127.0.0.1:7001"},
		{"2001:db8::1:7001", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := localDHTAddr(tc.in); got != tc.want {
			t.Errorf("localDHTAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLookupProxyReportsAnUnknownName checks the error an operator sees when a name
// was never published.
func TestLookupProxyReportsAnUnknownName(t *testing.T) {
	cfg := dhtConfig(t, false)
	rs := startServer(t, cfg)

	lookupCfg := &config.Config{}
	lookupCfg.DHT.Enabled = true
	lookupCfg.DHT.ListenAddr = "127.0.0.1:" + itoa(freeUDPPort(t))
	lookupCfg.DHT.Bootstrap = []string{rs.server.directory.node.Addr()}
	lookupCfg.DHT.LookupTimeoutSeconds = 1
	if err := lookupCfg.Validate(""); err != nil {
		t.Fatalf("lookup config invalid: %v", err)
	}

	if _, err := LookupProxy(lookupCfg, discardLogger(), "nothing-here"); err == nil {
		t.Fatal("resolving a name that was never published succeeded")
	}
}
