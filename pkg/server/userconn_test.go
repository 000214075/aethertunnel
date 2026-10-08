package server

import (
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// server.user_conn_timeout_seconds bounds the wait for a client to hand back a
// data connection, on its own when the operator set it.
func TestUserConnTimeoutReachesTheStreamWait(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DialTimeoutSecs = 5
	cfg.Server.UserConnTimeoutSeconds = 20
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}

	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeTCP}, manager)
	if group.userConnTimeout != 20*time.Second {
		t.Fatalf("the group's stream wait is %s, want 20s", group.userConnTimeout)
	}
	tunnel := &Tunnel{dialTimeout: group.dialTimeout, userConnTimeout: group.userConnTimeout}
	if got := tunnel.streamWait(); got != 20*time.Second {
		t.Errorf("streamWait() = %s, want the configured 20s", got)
	}
}

// Without the key the dial timeout stays in charge, which is the wait the
// server had before user_conn_timeout_seconds existed.
func TestUserConnTimeoutFallsBackToTheDialTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DialTimeoutSecs = 5
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}

	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeTCP}, manager)
	if group.userConnTimeout != 0 {
		t.Fatalf("the group's stream wait is %s, want 0 (unset)", group.userConnTimeout)
	}
	tunnel := &Tunnel{dialTimeout: group.dialTimeout, userConnTimeout: group.userConnTimeout}
	if got := tunnel.streamWait(); got != 5*time.Second {
		t.Errorf("streamWait() = %s, want the dial timeout 5s", got)
	}
}
