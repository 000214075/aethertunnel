package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// directory publishes the server's proxies into a DHT so a visitor can find the
// server that serves a proxy name without being configured with its address.
//
// Every published proxy gets one record naming the address a visitor should dial
// for it. That address is not always the control port: an http or https proxy is
// reached through one of the shared listeners, and a private proxy (stcp, sudp,
// xtcp) is reached by name through the control port. The record therefore carries
// the type as well, and a reader that needs to dial a port uses the record's
// Server field rather than assuming.
type directory struct {
	node      *discovery.Node
	advertise string // host part of the addresses this server publishes
	logger    *log.Logger

	// ports are the listener ports the record's Server field is built from.
	controlPort int
	httpPort    int
	httpsPort   int
}

// openDirectory starts the DHT node described by the [dht] section, or returns nil
// when the section is disabled.
func openDirectory(cfg *config.Config, logger *log.Logger) (*directory, error) {
	if !cfg.DHT.Enabled {
		return nil, nil
	}

	node, err := discovery.Start(cfg.DHTSettings(logger))
	if err != nil {
		return nil, err
	}

	host := cfg.DHT.AdvertiseHost
	if host == "" {
		host = cfg.Server.BindAddr
	}

	d := &directory{
		node:        node,
		advertise:   host,
		logger:      logger,
		controlPort: cfg.Server.BindPort,
		httpPort:    cfg.Server.HTTPPort,
		httpsPort:   cfg.Server.HTTPSPort,
	}
	logger.Printf("dht: node %s on %s, namespace %q, %d bootstrap peer(s)",
		node.Self(), node.Addr(), node.Namespace(), len(cfg.DHT.Bootstrap))
	return d, nil
}

// Close stops the DHT node.
func (d *directory) Close() error {
	if d == nil {
		return nil
	}
	return d.node.Close()
}

// Publishes reports how many names the node is announcing.
func (d *directory) Publishes() int {
	if d == nil {
		return 0
	}
	return len(d.node.Announced())
}

// serverAddrFor is the address a visitor should dial to reach the given proxy.
//
// A tcp or udp proxy has a port of its own; http and https proxies are reached
// through the shared listeners; a private proxy is reached by name through the
// control port, so that is what the record names.
func (d *directory) serverAddrFor(spec protocol.ProxySpec) string {
	port := d.controlPort
	switch spec.Type {
	case config.ProxyTypeHTTP:
		port = d.httpPort
	case config.ProxyTypeHTTPS:
		port = d.httpsPort
	case config.ProxyTypeTCP, config.ProxyTypeUDP:
		if spec.RemotePort > 0 {
			port = spec.RemotePort
		}
	}
	return net.JoinHostPort(d.advertise, strconv.Itoa(port))
}

// publish announces one proxy. A failure is logged and reported to the caller, but
// never stops the proxy from being published on the server itself: the DHT is a
// convenience, not a dependency of the data path.
func (d *directory) publish(spec protocol.ProxySpec) error {
	if d == nil {
		return nil
	}

	record := discovery.Record{
		Name:    spec.Name,
		Type:    spec.Type,
		Server:  d.serverAddrFor(spec),
		Domains: spec.Domains,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := d.node.Publish(ctx, record); err != nil {
		d.logger.Printf("dht: cannot publish %q: %v", spec.Name, err)
		return err
	}
	d.logger.Printf("dht: published %q as %s at %s", spec.Name, spec.Type, record.Server)
	return nil
}

// withdraw stops announcing a proxy.
func (d *directory) withdraw(name string) {
	if d == nil {
		return
	}
	if err := d.node.Withdraw(name); err != nil {
		d.logger.Printf("dht: cannot withdraw %q: %v", name, err)
		return
	}
	d.logger.Printf("dht: withdrew %q", name)
}

// Lookup resolves a proxy name, which is what the server's own -dht-lookup flag
// and the client's discovery use.
func (d *directory) Lookup(name string) (discovery.Record, error) {
	if d == nil {
		return discovery.Record{}, fmt.Errorf("the DHT is not enabled")
	}
	return d.node.Resolve(name)
}

// LookupProxy starts the DHT node described by cfg, resolves one proxy name and
// shuts the node down again. It is what the server binary's -dht-lookup flag calls,
// and it needs no running tunnel server: any node that can reach the DHT resolves
// the same record.
func LookupProxy(cfg *config.Config, logger *log.Logger, name string) (discovery.Record, error) {
	dir, err := openDirectory(cfg, logger)
	if err != nil {
		return discovery.Record{}, err
	}
	if dir == nil {
		return discovery.Record{}, fmt.Errorf("the DHT is not enabled: set [dht] enabled = true and a bootstrap address")
	}
	defer func() { _ = dir.Close() }()

	if len(cfg.DHT.Bootstrap) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, failure := range dir.node.Bootstrap(ctx) {
			logger.Printf("dht: bootstrap %v", failure)
		}
		cancel()
	}
	return dir.Lookup(name)
}

// summary is the payload of GET /api/dht.
func (d *directory) summary() map[string]any {
	if d == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":      true,
		"node_id":      d.node.Self(),
		"addr":         d.node.Addr(),
		"namespace":    d.node.Namespace(),
		"contacts":     d.node.Contacts(),
		"advertise_as": d.advertise,
		"announced":    d.node.Announced(),
	}
}
