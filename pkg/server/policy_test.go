package server

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- the server's own rule for a published name ---------------------------------

// TestTheServerRefusesARegistrationItsPolicyDoesNotDescribe covers the half of the
// policy that decides whether a name may be published at all, and as what.
func TestTheServerRefusesARegistrationItsPolicyDoesNotDescribe(t *testing.T) {
	echo := startEcho(t)
	pinnedPort := freePort(t)

	cfg := testConfig(t, false)
	cfg.Proxies = []config.ProxyConfig{{
		Name: "pinned", Type: config.ProxyTypeTCP, RemotePort: pinnedPort,
	}}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"pinned": streamHandler(echo)})

	// Another public port: the message names the port the server expects.
	if err := agent.client.register(protocol.ProxySpec{
		Name: "pinned", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	refusal := agent.expectRefused("pinned")
	if !strings.Contains(refusal, fmt.Sprintf("remote_port %d", pinnedPort)) {
		t.Errorf("the refusal does not name the pinned port: %s", refusal)
	}

	// Another type: refused as well, even on the port the policy names.
	if err := agent.client.register(protocol.ProxySpec{
		Name: "pinned", Type: protocol.ProxyTypeUDP, LocalAddr: echo, RemotePort: pinnedPort,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	refusal = agent.expectRefused("pinned")
	if !strings.Contains(refusal, protocol.ProxyTypeTCP) {
		t.Errorf("the refusal does not name the type the server publishes: %s", refusal)
	}

	// The registration the policy describes goes through, and the port it names is
	// the port that answers.
	agent.register(protocol.ProxySpec{
		Name: "pinned", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: pinnedPort,
	})
	waitForListener(t, rs.server, "pinned")
	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", pinnedPort), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := visitor.Write([]byte("through")); err != nil {
		t.Fatalf("visitor: write: %v", err)
	}
	got := make([]byte, len("through"))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor: read: %v", err)
	}
	if string(got) != "through" {
		t.Fatalf("the visitor received %q", got)
	}
}

// TestAPolicyWithoutATypePinsNothing keeps the policy from over-reaching: an entry
// that only names a proxy is a rule about that name, not about the type or the port
// the client picks.
func TestAPolicyWithoutATypePinsNothing(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Proxies = []config.ProxyConfig{{Name: "free"}}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"free": streamHandler(echo)})
	publicPort := freePort(t)
	agent.register(protocol.ProxySpec{
		Name: "free", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	})
	waitForListener(t, rs.server, "free")
}

// TestTheServerPolicyForVisitorsAppliesOnTopOfTheProxyList is the visitor half: the
// operator's list refuses a source the client's own list would have admitted, and
// the client cannot widen it.
func TestTheServerPolicyForVisitorsAppliesOnTopOfTheProxyList(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Proxies = []config.ProxyConfig{{
		Name: "narrow", AllowCIDRs: []string{"10.9.9.0/24"},
	}}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"narrow": streamHandler(echo)})
	// The client asks for no restriction of its own, so only the server's list can
	// be refusing the visitor below.
	agent.register(protocol.ProxySpec{
		Name: "narrow", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	})
	waitForListener(t, rs.server, "narrow")

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = visitor.Write([]byte("not welcome"))
	if n, err := visitor.Read(make([]byte, 64)); err == nil {
		t.Fatalf("a visitor the server's policy excludes received %d byte(s)", n)
	}
	if denied := rs.server.metrics.visitorDenied.Load(); denied != 1 {
		t.Errorf("the metric reports %d refused visitors, want 1", denied)
	}
}

// TestVisitorRulesDenyWinsOverAllow pins the rule both lists are applied with.
func TestVisitorRulesDenyWinsOverAllow(t *testing.T) {
	allow := compileCIDRs([]string{"127.0.0.0/8"})
	deny := compileCIDRs([]string{"127.0.0.1/32"})

	for _, tc := range []struct {
		name      string
		allow     []*net.IPNet
		deny      []*net.IPNet
		ip        string
		want      bool
		reasonHas string
	}{
		{"deny wins", allow, deny, "127.0.0.1", false, "deny list"},
		{"allowed by both", allow, deny, "127.0.0.2", true, ""},
		{"nothing configured", nil, nil, "10.0.0.1", true, ""},
		{"only a deny list", nil, deny, "10.0.0.1", true, ""},
		{"outside the allow list", allow, nil, "10.0.0.1", false, "allow list"},
	} {
		allowed, reason := visitorRulesAllow(tc.allow, tc.deny, net.ParseIP(tc.ip))
		if allowed != tc.want {
			t.Errorf("%s: %s allowed = %v, want %v", tc.name, tc.ip, allowed, tc.want)
		}
		if tc.reasonHas != "" && !strings.Contains(reason, tc.reasonHas) {
			t.Errorf("%s: the reason %q does not mention %q", tc.name, reason, tc.reasonHas)
		}
	}
}
