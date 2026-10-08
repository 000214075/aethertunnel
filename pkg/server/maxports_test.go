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

// A group owns one endpoint, taken from its first member, so a later pool member's
// remote_port is not honoured. Counting it against the cap charged that session
// for a port it never held: with max_ports_per_client = 1 the joining client could
// not publish a proxy of its own.
func TestAMemberJoiningAPoolIsNotChargedForItsRequestedPort(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.MaxPortsPerClient = 1
	rs := startServer(t, cfg)
	first := startAgent(t, rs.addr, false, map[string]dataHandler{})
	second := startAgent(t, rs.addr, false, map[string]dataHandler{})

	first.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: freePort(t), Group: "pool",
	})

	// The second client joins the pool, asking for a port of its own that the pool
	// does not publish on.
	if err := second.client.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:2",
		RemotePort: freePort(t), Group: "pool",
	}); err != nil {
		t.Fatalf("register the pool member: %v", err)
	}
	second.expectAccepted("pooled")

	// It still holds no public port of its own, so the cap leaves it one.
	if err := second.client.register(protocol.ProxySpec{
		Name: "own", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:3", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register the second client's own proxy: %v", err)
	}
	second.expectAccepted("own")
}
