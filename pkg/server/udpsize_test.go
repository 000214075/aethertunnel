package server

import (
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// newProxyGroup copies the configured datagram cap into every group it builds,
// so a udp proxy's pump drops what the operator said it should.
func TestNewProxyGroupCarriesTheUDPPacketSize(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.UDPPacketSize = 1200
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}

	group := newProxyGroup(protocol.ProxySpec{Name: "dns", Type: protocol.ProxyTypeUDP}, manager)
	if group.udpPacketSize != 1200 {
		t.Errorf("the group's datagram cap is %d, want 1200", group.udpPacketSize)
	}
}
