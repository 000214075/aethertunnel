package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// countingEcho starts a TCP service that answers every read with a marker, so a
// test can tell which member of a pool served a connection.
func countingEcho(t *testing.T, marker string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 256)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						if _, err := conn.Write([]byte(marker)); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func loadBalancedConfig(t *testing.T, strategy string) *config.Config {
	t.Helper()
	cfg := testConfig(t, false)
	cfg.Server.LoadBalance = strategy
	cfg.Server.DialTimeoutSecs = 2
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// joinPool registers one more member of the named pool and returns the agent.
func joinPool(t *testing.T, rs *runningServer, name, local, group, secret string, port int) *testAgent {
	t.Helper()

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{name: streamHandler(local)})
	spec := protocol.ProxySpec{
		Name: name, Type: protocol.ProxyTypeTCP, LocalAddr: local,
		RemotePort: port, Group: group,
	}
	if secret != "" {
		spec.Type = protocol.ProxyTypeSTCP
		spec.RemotePort = 0
		spec.SecretKey = secret
	}
	agent.register(spec)
	return agent
}

func echoOnce(target, payload string) string {
	conn, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		return ""
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

func TestPooledProxiesRoundRobinAcrossMembers(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)
	joinPool(t, rs, "pooled", countingEcho(t, "B"), "pool", "", port)

	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if members := group.memberCount(); members != 2 {
		t.Fatalf("the pool has %d members, want 2", members)
	}

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	seen := map[string]int{}
	for attempt := 0; attempt < 6; attempt++ {
		seen[echoOnce(target, "hello")]++
	}
	if seen["A"] == 0 || seen["B"] == 0 {
		t.Fatalf("round-robin did not reach both members: %v", seen)
	}
}

func TestPooledProxiesShareOnePublishedPort(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceFailover)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "primary"), "pool", "", port)

	// The second member asks for a different public port. The pool already owns
	// its endpoint, so the request must not take a second port.
	other := freePort(t)
	joinPool(t, rs, "pooled", countingEcho(t, "secondary"), "pool", "", other)

	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got := group.RemotePort; got != port {
		t.Fatalf("the pool moved to port %d, want %d", got, port)
	}

	// Failover keeps using the first member while it is healthy.
	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	if reply := echoOnce(target, "hello"); reply != "primary" {
		t.Fatalf("failover served %q, want the first member", reply)
	}

	// The port the second member asked for must not have been bound.
	if _, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(other)), 300*time.Millisecond); err == nil {
		t.Fatal("the pool opened the second port as well")
	}
}

// A pool owns one endpoint, so a member that asks for a different public port is
// pooled anyway and that request is not honoured. The server has to tell the member
// the port its name is actually reachable on: reporting the requested one would send
// an operator to a port nothing is listening on.
func TestAPooledMemberIsToldThePoolsPort(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)

	poolPort := freePort(t)
	joinPool(t, rs, "pooled", countingEcho(t, "primary"), "pool", "", poolPort)

	requested := freePort(t)
	second, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(second.close)
	if _, err := second.authenticate("pool-second", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := second.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: countingEcho(t, "secondary"),
		RemotePort: requested, Group: "pool",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	msg, err := second.framer.ReadFrame()
	if err != nil {
		t.Fatalf("proxy list: %v", err)
	}
	if msg.Type != protocol.TypeProxyList {
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(msg.Payload, &payload)
		t.Fatalf("the pooled member was refused: %s", payload.Error)
	}
	var statuses []protocol.ProxyStatus
	if err := json.Unmarshal(msg.Payload, &statuses); err != nil {
		t.Fatalf("decode proxy list: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("the member was told about %d proxies, want its own", len(statuses))
	}
	if got := statuses[0].RemotePort; got != poolPort {
		t.Fatalf("the member was told port %d, want the pool's %d (it asked for %d)",
			got, poolPort, requested)
	}

	// A proxy outside a pool is still reported on the port it asked for.
	soloPort := freePort(t)
	solo, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(solo.close)
	if _, err := solo.authenticate("solo", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := solo.register(protocol.ProxySpec{
		Name: "solo", Type: protocol.ProxyTypeTCP, LocalAddr: countingEcho(t, "solo"),
		RemotePort: soloPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	soloMsg, err := solo.framer.ReadFrame()
	if err != nil {
		t.Fatalf("proxy list: %v", err)
	}
	var soloStatuses []protocol.ProxyStatus
	if err := json.Unmarshal(soloMsg.Payload, &soloStatuses); err != nil {
		t.Fatalf("decode proxy list: %v", err)
	}
	if len(soloStatuses) != 1 || soloStatuses[0].RemotePort != soloPort {
		t.Fatalf("an unpooled proxy reported %+v, want port %d", soloStatuses, soloPort)
	}
}

func TestPoolRefusesAMemberWithoutTheGroup(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)

	otherLocal := countingEcho(t, "B")
	other := startAgent(t, rs.addr, false, map[string]dataHandler{"pooled": streamHandler(otherLocal)})
	if err := other.client.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: otherLocal, RemotePort: port,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if refusal := other.expectRefused("pooled"); !strings.Contains(refusal, "group") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestPoolRefusesAMemberWithADifferentType(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)

	otherLocal := countingEcho(t, "B")
	other := startAgent(t, rs.addr, false, map[string]dataHandler{"pooled": streamHandler(otherLocal)})
	if err := other.client.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeUDP, LocalAddr: otherLocal,
		RemotePort: port, Group: "pool",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if refusal := other.expectRefused("pooled"); !strings.Contains(refusal, "already published as type") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestPoolRequiresTheSameSecretFromEveryMember(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	local := countingEcho(t, "A")

	joinPool(t, rs, "private", local, "private-pool", "shared-secret", 0)

	other := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(local)})
	if err := other.client.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: local,
		SecretKey: "a-different-secret", Group: "private-pool",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if refusal := other.expectRefused("private"); !strings.Contains(refusal, "secret_key") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestPoolKeepsServingAfterOneMemberLeaves(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	port := freePort(t)

	first := joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)
	second := joinPool(t, rs, "pooled", countingEcho(t, "B"), "pool", "", port)
	_ = first

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	if reply := echoOnce(target, "hello"); reply == "" {
		t.Fatal("the pool did not answer before the member left")
	}

	second.client.close()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if reply := echoOnce(target, "hello"); reply == "A" {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("the pool stopped answering from the remaining member")
}

func TestAMemberThatCannotAnswerIsSkipped(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceFailover)
	rs := startServer(t, cfg)
	port := freePort(t)
	local := countingEcho(t, "good")

	// The first member publishes the port but never answers a data request, so
	// the group has to move on to the second one.
	silent := startAgent(t, rs.addr, false, map[string]dataHandler{})
	silent.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: local,
		RemotePort: port, Group: "pool",
	})

	joinPool(t, rs, "pooled", local, "pool", "", port)

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if reply := echoOnce(target, "hello"); reply == "good" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the healthy member never served the visitor")
}

func TestPooledProxiesReportThemselvesToTheDashboard(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRoundRobin)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)
	joinPool(t, rs, "pooled", countingEcho(t, "B"), "pool", "", port)

	dashboard, err := NewDashboard(rs.server, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if err := dashboard.Start(); err != nil {
		t.Fatalf("start the dashboard: %v", err)
	}
	defer dashboard.Stop()

	response, err := http.Get(fmt.Sprintf("http://%s/api/proxies", dashboard.Listener().Addr()))
	if err != nil {
		t.Fatalf("fetch the proxy list: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	var payload struct {
		Proxies []struct {
			Name        string `json:"name"`
			Group       string `json:"group"`
			MemberCount int    `json:"member_count"`
			Members     []struct {
				ClientID string  `json:"client_id"`
				Healthy  bool    `json:"healthy"`
				Latency  float64 `json:"latency_ms"`
			} `json:"members"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode the dashboard response: %v (body %s)", err, body)
	}
	if len(payload.Proxies) != 1 {
		t.Fatalf("the dashboard lists %d proxies, want 1: %s", len(payload.Proxies), body)
	}

	proxy := payload.Proxies[0]
	if proxy.MemberCount != 2 || len(proxy.Members) != 2 {
		t.Fatalf("the dashboard reports %d members: %s", proxy.MemberCount, body)
	}
	if proxy.Group != "pool" {
		t.Fatalf("the dashboard reports group %q", proxy.Group)
	}
	for _, member := range proxy.Members {
		if member.ClientID == "" {
			t.Fatalf("a member has no client id: %s", body)
		}
		if !member.Healthy {
			t.Fatalf("a live member is reported as unhealthy: %s", body)
		}
	}
}

func TestMultipathSpreadsDatagramsOverSeveralConnections(t *testing.T) {
	udpEcho := startUDPEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.DialTimeoutSecs = 3

	// A udp proxy binds UDP, so the port comes from a UDP probe.
	port := freeUDPPort(t)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"multi": datagramHandler(udpEcho),
	})
	agent.register(protocol.ProxySpec{
		Name: "multi", Type: protocol.ProxyTypeUDP, LocalAddr: udpEcho,
		RemotePort: port, Multipath: 3,
	})

	group, err := rs.server.tunnels.Get("multi")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if group.Multipath != 3 {
		t.Fatalf("the group reports multipath %d", group.Multipath)
	}
	if pump := group.datagramPump(); pump == nil || pump.Paths != 3 {
		t.Fatal("the pump was not given three paths")
	}

	visitor, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer visitor.Close()

	answered := false
	for attempt := 0; attempt < 15 && !answered; attempt++ {
		if _, err := visitor.Write([]byte("spread")); err != nil {
			t.Fatalf("send: %v", err)
		}
		_ = visitor.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		buf := make([]byte, 256)
		if n, err := visitor.Read(buf); err == nil && string(buf[:n]) == "spread" {
			answered = true
		}
	}
	if !answered {
		t.Fatal("the multipath session never answered")
	}
}

func TestMultipathWarnsOnAByteStreamProxy(t *testing.T) {
	// A client configuration: multipath is a property of the service a client
	// publishes, and a server-side [[proxies]] entry is reported for having no
	// effect rather than being judged on what spread it would use.
	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7001"
	cfg.Client.AuthToken = testToken
	cfg.Proxies = []config.ProxyConfig{{
		Name: "stream", Type: config.ProxyTypeTCP, LocalPort: 1, RemotePort: 2, Multipath: 3,
	}}
	if err := cfg.Validate(config.RoleClient); err != nil {
		t.Fatalf("config: %v", err)
	}

	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "multipath only spreads datagrams") {
			return
		}
	}
	t.Fatalf("no warning was issued for a multipath byte stream: %v", cfg.Warnings)
}
