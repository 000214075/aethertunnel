package server

import (
	"fmt"
	"net"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// proxyPolicies is the server operator's rule for each published name, taken from
// the [[proxies]] list of the server configuration.
//
// A client registers a proxy and sends the terms it wants; this is the other side of
// the bargain. An entry decides which type and which public port a name may be
// published as, and which visitor sources may use it, whatever the registering client
// asks for. A name with no entry is not governed by anything here and is admitted as
// before.
type proxyPolicies struct {
	entries map[string]config.ProxyConfig
}

func newProxyPolicies(configured []config.ProxyConfig) *proxyPolicies {
	entries := make(map[string]config.ProxyConfig, len(configured))
	for _, entry := range configured {
		if entry.Name != "" {
			entries[entry.Name] = entry
		}
	}
	return &proxyPolicies{entries: entries}
}

// configured reports how many names the server holds a policy for.
func (p *proxyPolicies) configured() int { return len(p.entries) }

// admits reports whether a registration may be published. The returned error is
// sent to the client and written to the audit log by the caller.
func (p *proxyPolicies) admits(name, proxyType string, remotePort int) error {
	entry, ok := p.entries[name]
	if !ok {
		return nil
	}
	if entry.Type != "" && entry.Type != proxyType {
		return fmt.Errorf("the server publishes %q as type %q, and this registration is %q",
			name, entry.Type, proxyType)
	}
	if entry.RemotePort != 0 && entry.RemotePort != remotePort {
		return fmt.Errorf("the server publishes %q on remote_port %d, and this registration asks for %d",
			name, entry.RemotePort, remotePort)
	}
	return nil
}

// visitorRules returns the visitor lists the server itself enforces for a name.
func (p *proxyPolicies) visitorRules(name string) (allow, deny []*net.IPNet) {
	entry, ok := p.entries[name]
	if !ok {
		return nil, nil
	}
	return compileCIDRs(entry.AllowCIDRs), compileCIDRs(entry.DenyCIDRs)
}
