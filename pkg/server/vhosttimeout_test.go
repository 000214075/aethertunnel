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
