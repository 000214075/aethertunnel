package server

import (
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The virtual-host timeout, when the operator set one, bounds how long the
// terminating path waits for a local service's response headers.
func TestResponseHeaderTimeoutPrefersTheVhostTimeout(t *testing.T) {
	got := responseHeaderTimeout(5*time.Second, 30*time.Second)
	if got != 30*time.Second {
		t.Errorf("responseHeaderTimeout(5s, 30s) = %s, want 30s", got)
	}
}

// Without a virtual-host timeout the dial timeout stays in charge, which is
// how the server behaved before server.vhost_http_timeout existed.
func TestResponseHeaderTimeoutFallsBackToTheDialTimeout(t *testing.T) {
	got := responseHeaderTimeout(5*time.Second, 0)
	if got != 5*time.Second {
		t.Errorf("responseHeaderTimeout(5s, 0) = %s, want 5s", got)
	}
}

// newProxyGroup copies the configured virtual-host timeout into every group it
// builds, so a later vhost_http_timeout change needs a reload to apply.
func TestNewProxyGroupCarriesTheVhostTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.VhostHTTPTimeout = 45
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}

	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)
	if group.vhostTimeout != 45*time.Second {
		t.Errorf("the group's vhost timeout is %s, want 45s", group.vhostTimeout)
	}
}

// lookup hands the caller the slice the router stores, and remove compacts that
// same array in place when a client disconnects, so an in-flight request iterating
// the result raced the removal. The slice a request gets has to be its own.
func TestVhostLookupHandsOutItsOwnSlice(t *testing.T) {
	cfg := &config.Config{}
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)

	router := newVhostRouter("", cfg, manager.logger, newMetrics())
	router.mu.Lock()
	router.exact["site.example"] = []*ProxyGroup{group}
	router.mu.Unlock()

	first := router.lookup("site.example")
	if len(first) != 1 || first[0] != group {
		t.Fatalf("lookup returned %v, want the published group", first)
	}

	// What a request writing into the slice it was handed would do.
	first[0] = nil

	second := router.lookup("site.example")
	if len(second) != 1 || second[0] != group {
		t.Fatal("a caller's write reached the router's own slice")
	}
}

// A pooled http proxy can add hostnames the first member did not use, and a
// failure partway through has to undo the ones already taken: the member is
// detached, so the group must not keep serving names only that failed registration
// asked for, and no other client could publish them either.
func TestVhostExtendRollsBackTheHostnamesItTookOnFailure(t *testing.T) {
	cfg := &config.Config{}
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)
	other := newProxyGroup(protocol.ProxySpec{Name: "other", Type: protocol.ProxyTypeHTTP}, manager)

	router := newVhostRouter("", cfg, manager.logger, newMetrics())
	router.mu.Lock()
	router.exact["first.example"] = []*ProxyGroup{group}
	// A hostname another proxy already owns, so extending onto it is refused.
	router.exact["taken.example"] = []*ProxyGroup{other}
	router.mu.Unlock()

	binding := &vhostBinding{router: router, group: group, domains: []string{"first.example"}}
	err := binding.extend([]string{"second.example", "taken.example"})
	if err == nil {
		t.Fatal("extending onto a hostname another proxy owns was accepted")
	}

	router.mu.RLock()
	taken := router.exact["second.example"]
	router.mu.RUnlock()
	if len(taken) != 0 {
		t.Fatalf("the refused hostname stayed registered to the group (%v)", taken)
	}
	if len(binding.domains) != 1 {
		t.Fatalf("the binding claims %v, want only the hostname it was built with", binding.domains)
	}
	if groups := router.lookup("second.example"); len(groups) != 0 {
		t.Fatalf("lookup resolved the refused hostname to %v", groups)
	}
}
