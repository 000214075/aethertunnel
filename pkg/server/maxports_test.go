package server

import (
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestMaxPortsPerClientCapsARegistration walks the cap: a server that allows
// one public port per client accepts the first registration and refuses the
// second by name, while a proxy without a public port is unaffected.
func TestMaxPortsPerClientCapsARegistration(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.MaxPortsPerClient = 1
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	agent.register(protocol.ProxySpec{
		Name: "first", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: freePort(t),
	})

	if err := agent.client.register(protocol.ProxySpec{
		Name: "second", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:2", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register the second: %v", err)
	}
	refusal := agent.expectRefused("second")
	if !strings.Contains(refusal, "max_ports_per_client") {
		t.Fatalf("the refusal does not name the setting: %s", refusal)
	}

	// A proxy that opens no public port is not part of the count.
	if err := agent.client.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: "127.0.0.1:3", SecretKey: "s3cret",
	}); err != nil {
		t.Fatalf("register a private proxy: %v", err)
	}
	agent.expectAccepted("private")
}
