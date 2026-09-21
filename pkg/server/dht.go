package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
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

	// signingKey is the hex public key announcements are signed with, empty when
	// records are published unsigned. It is what an operator copies into a
	// client's [dht] trusted_keys.
	signingKey string

	// ports are the listener ports the record's Server field is built from.
	controlPort int
	httpPort    int
	httpsPort   int
}

// openDirectory starts the DHT node described by the [dht] section, or returns nil
// when the section is disabled.
//
// When the section names a signing key file the node signs every record it
// publishes with that key, which is what lets a reader tell this server's
// announcements from records any other node could write to the same keys.
func openDirectory(cfg *config.Config, logger *log.Logger) (*directory, error) {
	if !cfg.DHT.Enabled {
		return nil, nil
	}

	settings := cfg.DHTSettings(logger)
	signingKey := ""
	if cfg.DHT.SigningKeyFile != "" {
		identity, err := crypto.LoadIdentity(cfg.DHT.SigningKeyFile)
		if err != nil {
			return nil, fmt.Errorf("dht.signing_key_file: %w", err)
		}
		settings.Signer = identity
		signingKey = identity.PublicKeyHex()
		logger.Printf("dht: signing announcements with %s (key file %s)",
			signingKey, cfg.DHT.SigningKeyFile)
	}

	node, err := discovery.Start(settings)
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
		signingKey:  signingKey,
		controlPort: cfg.Server.BindPort,
		httpPort:    cfg.Server.HTTPPort,
		httpsPort:   cfg.Server.HTTPSPort,
	}
	logger.Printf("dht: node %s on %s, namespace %q, %d bootstrap peer(s)",
		node.Self(), node.Addr(), node.Namespace(), len(cfg.DHT.Bootstrap))
	return d, nil
}

// AnnouncementKey returns the hex public key announcements are signed with, which
// is what an operator copies into a client's [dht] trusted_keys.
//
// The key file is created when it does not exist yet, so the key can be read before
// the first announcement is ever published.
func AnnouncementKey(cfg *config.Config) (string, error) {
	if cfg.DHT.SigningKeyFile == "" {
		return "", fmt.Errorf("dht.signing_key_file is empty, so announcements are published unsigned and there is no key to read")
	}
	identity, err := crypto.LoadIdentity(cfg.DHT.SigningKeyFile)
	if err != nil {
		return "", fmt.Errorf("dht.signing_key_file: %w", err)
	}
	return identity.PublicKeyHex(), nil
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
	if d.signingKey != "" {
		d.logger.Printf("dht: published %q as %s at %s, signed with %s",
			spec.Name, spec.Type, record.Server, d.signingKey)
		return nil
	}
	d.logger.Printf("dht: published %q as %s at %s, unsigned", spec.Name, spec.Type, record.Server)
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

// LookupProxy resolves one proxy name through the DHT described by cfg and shuts the
// node down again. It is what the -dht-lookup flag calls, and it needs no running
// tunnel server: any node that can reach the DHT resolves the same record.
//
// The query node binds an ephemeral port rather than the configured one, so this can
// be run on the host that is already running the server described by the same file.
func LookupProxy(cfg *config.Config, logger *log.Logger, name string) (discovery.Record, error) {
	if !cfg.DHT.Enabled {
		return discovery.Record{}, fmt.Errorf("the DHT is not enabled: set [dht] enabled = true and a bootstrap address")
	}

	settings := cfg.DHTSettings(logger)
	settings.ListenAddr = "127.0.0.1:0"

	// With no bootstrap list configured, the node at the configured listen address is
	// asked instead. That is the server a deployment starts from this same file, so a
	// lookup works on the host that runs the server without a second configuration.
	if len(settings.Bootstrap) == 0 {
		if peer := localDHTAddr(cfg.DHT.ListenAddr); peer != "" {
			settings.Bootstrap = []string{peer}
			logger.Printf("dht: no bootstrap peer is configured; asking %s", peer)
		}
	}

	node, err := discovery.Start(settings)
	if err != nil {
		return discovery.Record{}, err
	}
	defer func() { _ = node.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, failure := range node.Bootstrap(ctx) {
		logger.Printf("dht: bootstrap %v", failure)
	}
	return node.Lookup(ctx, name)
}

// localDHTAddr turns a configured listen address into something a query node can
// send a datagram to: a wildcard host becomes the loopback address, because a node
// listening on every interface is reachable there.
func localDHTAddr(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
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
		// Empty when announcements are published unsigned.
		"signing_key": d.signingKey,
	}
}
