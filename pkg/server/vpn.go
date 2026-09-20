package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// DeviceOpener opens a layer-3 interface. It is a parameter of openVPN rather than
// a direct call so that a test can exercise the whole layer-3 path without a real
// interface, which a machine without the driver cannot provide.
type DeviceOpener func(name string, mtu int) (vpn.Device, error)

// vpnService is the server's layer-3 side: one interface, one address pool, and a
// router that sends each packet to the session that owns its destination address.
//
// The interface is opened once at startup. A client's packets travel as
// TypeVPNPacket frames on its control connection, which is what keeps the layer-3
// path on the port the client already reached the server on.
type vpnService struct {
	router *vpn.Router
	pool   *vpn.Pool
	logger *log.Logger

	mu     sync.Mutex
	leases map[string]net.IP // session id -> address

	cancel  context.CancelFunc
	done    chan error
	stopped sync.Once
}

// openVPN builds the service described by [vpn], or returns nil when the section is
// disabled.
//
// Opening the interface happens here rather than on the first client, so a machine
// that cannot provide one reports that at startup instead of after a client has
// already connected. An error is returned, not logged: a server told to provide a
// tunnel and unable to do so must not start silently without one.
func openVPN(cfg *config.Config, logger *log.Logger, open DeviceOpener) (*vpnService, error) {
	if !cfg.VPN.Enabled {
		return nil, nil
	}
	if open == nil {
		open = vpn.Open
	}

	pool, err := vpn.NewPool(cfg.VPN.Address)
	if err != nil {
		return nil, err
	}

	device, err := open(cfg.VPN.Device, cfg.VPN.MTU)
	if err != nil {
		return nil, err
	}

	router, err := vpn.NewRouter(device, vpn.Options{MTU: cfg.VPN.MTU, Logger: logger})
	if err != nil {
		_ = device.Close()
		return nil, err
	}

	// The server's own address on the subnet is the first usable one. Assigning it
	// here is what makes the pool's promise true; adding a route to the subnet is
	// left to the operator, because that changes the host's routing table.
	if addressable, ok := device.(vpn.Addressable); ok {
		if err := addressable.SetAddress(pool.Server().String()); err != nil {
			_ = device.Close()
			return nil, fmt.Errorf("vpn: assigning %s to %s: %w", pool.Server(), device.Name(), err)
		}
	} else {
		logger.Printf("vpn: %s cannot be given an address by this program; configure %s on it yourself",
			device.Name(), pool.Server())
	}

	ctx, cancel := context.WithCancel(context.Background())
	service := &vpnService{
		router: router,
		pool:   pool,
		logger: logger,
		leases: make(map[string]net.IP),
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { service.done <- router.Run(ctx) }()

	logger.Printf("vpn: interface %s, MTU %d, subnet %s, server address %s",
		device.Name(), router.MTU(), pool.Network(), pool.Server())
	return service, nil
}

// Address is the server's own address on the tunnel subnet.
func (v *vpnService) Address() net.IP {
	if v == nil {
		return nil
	}
	return v.pool.Server()
}

// MTU is the packet size limit.
func (v *vpnService) MTU() int {
	if v == nil {
		return 0
	}
	return v.router.MTU()
}

// Mask is the subnet mask, as a dotted quad.
func (v *vpnService) Mask() string {
	if v == nil {
		return ""
	}
	return net.IP(v.pool.Mask()).String()
}

// acquire reserves an address for a session and attaches the transport it should be
// routed to.
//
// A session that already holds an address keeps it: a client that reconnects within
// one session's lifetime does not lose its lease.
func (v *vpnService) acquire(sessionID string, transport vpn.Transport) (net.IP, *vpn.Peer, error) {
	if v == nil {
		return nil, nil, fmt.Errorf("vpn: the server has no tunnel configured")
	}

	v.mu.Lock()
	if address, held := v.leases[sessionID]; held {
		v.mu.Unlock()
		return address, nil, fmt.Errorf("vpn: session %s already holds %s", sessionID, address)
	}
	v.mu.Unlock()

	address, err := v.pool.Acquire()
	if err != nil {
		return nil, nil, err
	}

	peer, err := v.router.Add(transport, address)
	if err != nil {
		v.pool.Release(address)
		return nil, nil, err
	}

	v.mu.Lock()
	v.leases[sessionID] = address
	v.mu.Unlock()

	v.logger.Printf("vpn: session %s holds %s", sessionID, address)
	return address, peer, nil
}

// release returns a session's address to the pool and detaches its peer.
func (v *vpnService) release(sessionID string) {
	if v == nil {
		return
	}

	v.mu.Lock()
	address, held := v.leases[sessionID]
	if held {
		delete(v.leases, sessionID)
	}
	v.mu.Unlock()
	if !held {
		return
	}

	v.router.Remove(address)
	v.pool.Release(address)
	v.logger.Printf("vpn: session %s released %s", sessionID, address)
}

// Close stops the router and closes the interface.
func (v *vpnService) Close() error {
	if v == nil {
		return nil
	}

	var err error
	v.stopped.Do(func() {
		v.cancel()
		err = v.router.Close()
		select {
		case runErr := <-v.done:
			if err == nil && runErr != nil {
				err = runErr
			}
		case <-time.After(5 * time.Second):
			err = fmt.Errorf("vpn: the router did not stop within 5s")
		}
	})
	return err
}

// summary is the payload of the vpn part of GET /api/config and GET /api/vpn.
func (v *vpnService) summary() map[string]any {
	if v == nil {
		return map[string]any{"enabled": false, "reason": "vpn.enabled is false"}
	}
	stats := v.router.Stats()
	return map[string]any{
		"enabled":        true,
		"device":         v.router.Device().Name(),
		"server_address": v.Address().String(),
		"subnet":         v.pool.Network().String(),
		"mask":           v.Mask(),
		"mtu":            v.router.MTU(),
		"pool_size":      v.pool.Size(),
		"addresses_used": v.pool.InUse(),
		"peers":          stats.Peers,
		"packets": map[string]any{
			"from_device": stats.FromDevice,
			"to_device":   stats.ToDevice,
			"delivered":   stats.Delivered,
			"unroutable":  stats.Unroutable,
			"dropped":     stats.Dropped,
			"errors":      stats.Errors,
		},
	}
}

// attachVPN gives a session a tunnel address and wires its packet path.
//
// It is called right after the auth response is written, because that response is
// what tells the client which address it has.
func (s *Server) attachVPN(session *Session) (string, int, error) {
	if s.vpn == nil {
		return "", 0, nil
	}

	transport := vpn.NewChannelTransport(func(packet []byte) error {
		return session.framer.WriteFrame(&protocol.Message{Type: protocol.TypeVPNPacket, Payload: packet})
	})

	address, peer, err := s.vpn.acquire(session.ID, transport)
	if err != nil {
		_ = transport.Close()
		return "", 0, err
	}
	session.setVPN(peer, transport)
	return address.String(), s.vpn.MTU(), nil
}
