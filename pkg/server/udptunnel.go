package server

import (
	"net"

	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// startUDP publishes a udp tunnel.
//
// One datagram session per visitor address is carried by one data connection, so
// the client sees a normal stream of frames and the server only has to keep the
// address book. The pump owns the address book, the idle expiry and the frame
// relay; this function only wires the tunnel's counters into it.
func (t *Tunnel) startUDP() {
	t.pump = &flynet.DatagramPump{
		Socket: t.packet,
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			dc, release, err := t.openStream(false)
			if err != nil {
				return nil, nil, err
			}
			return dc.framer, func() {
				_ = dc.Close()
				release()
			}, nil
		},
		Close: func(addr net.Addr, toPeer, fromPeer int64) {
			t.BytesOut.Add(toPeer)
			t.BytesIn.Add(fromPeer)
			t.Total.Add(1)
			t.Session.RecordTraffic(toPeer, fromPeer)
			t.metrics.recordStream(t.Name, toPeer, fromPeer)
		},
		OnDatagram:  func(toPeer bool, n int) { t.metrics.udpDatagrams.Add(1) },
		OnSession:   func(delta int) { t.metrics.udpSessions.Add(int64(delta)) },
		IdleTimeout: t.idleTimeout,
		Logger:      t.logger,
	}
	t.pump.Start()
}

// UDP is the tunnel's datagram pump, or nil for a non-datagram tunnel.
func (t *Tunnel) UDP() *flynet.DatagramPump { return t.pump }
