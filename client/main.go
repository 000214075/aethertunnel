// Command aethertunnel-client connects to an AetherTunnel server, publishes the
// tunnels listed in its configuration and forwards each visiting connection to the
// matching local service.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// Stamped at build time; see the server's main.go for the ldflags form.
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

type client struct {
	cfg       *config.Config
	cipher    *crypto.Cipher
	identity  *crypto.Identity
	tlsConfig *tls.Config
	logger    *log.Logger

	// resolver is the DHT node, nil unless [dht] is enabled with a discover name.
	// target is the server address to dial: the configured one, or the one the
	// resolver last reported.
	resolver *discovery.Node
	target   string

	// vpnTransport is the packet path of the current session, nil unless the server
	// gave this client a tunnel address.
	vpnMu        sync.Mutex
	vpnTransport *vpn.ChannelTransport

	mu              sync.Mutex
	session         string
	p2pPort         int
	sessionKey      []byte
	activeStreams   sync.WaitGroup
	registeredNames []string
}

// deliverVPN hands a packet received on the control connection to the tunnel.
func (c *client) deliverVPN(packet []byte) {
	c.vpnMu.Lock()
	transport := c.vpnTransport
	c.vpnMu.Unlock()
	if transport == nil {
		c.logger.Printf("the server sent a tunnel packet but this session holds no address")
		return
	}
	if !transport.Deliver(packet) {
		c.logger.Printf("dropped a tunnel packet because the tunnel's buffer is full")
	}
}

// serverAddr is the address the next connection attempt dials.
func (c *client) serverAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

// refreshTarget re-resolves [dht].discover, so a proxy that moved to another server
// is picked up on the next reconnect instead of requiring a restart.
//
// A resolution failure leaves the previous address in place: a DHT that is briefly
// unreachable should not take a working session's replacement away.
func (c *client) refreshTarget() {
	if c.resolver == nil {
		return
	}
	record, err := c.resolver.Resolve(c.cfg.DHT.Discover)
	if err != nil {
		c.logger.Printf("dht: cannot resolve %q: %v (still using %s)",
			c.cfg.DHT.Discover, err, c.serverAddr())
		return
	}
	if record.Server == c.serverAddr() {
		return
	}
	c.mu.Lock()
	c.target = record.Server
	c.mu.Unlock()
	if record.Verified {
		c.logger.Printf("dht: %q resolves to %s (type %s), signed by %s",
			record.Name, record.Server, record.Type, record.PublicKey)
		return
	}
	c.logger.Printf("dht: %q resolves to %s (type %s), unsigned", record.Name, record.Server, record.Type)
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		configPath  = flag.String("config", "", "path to the client configuration file (default client.toml)")
		checkConfig = flag.Bool("check", false, "validate the configuration and exit")
		showID      = flag.Bool("identity", false, "print this client's public identity key and exit")
		discover    = flag.String("discover", "", "resolve a proxy name through the [dht] network and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [config-file]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("aethertunnel-client %s (protocol %d, built %s, commit %s)\n",
			version, protocol.ProtocolVersion, buildTime, gitCommit)
		return
	}

	path := *configPath
	if path == "" {
		if flag.NArg() > 0 {
			path = flag.Arg(0)
		} else {
			path = "client.toml"
		}
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient})
	if err != nil {
		logger.Fatalf("%v", err)
	}

	for _, warning := range cfg.Warnings {
		logger.Printf("warning: %s", warning)
	}
	if *checkConfig {
		fmt.Printf("%s is valid\n", path)
		return
	}

	if *showID {
		identity, err := crypto.LoadIdentity(cfg.Identity.KeyFile)
		if err != nil {
			logger.Fatalf("identity: %v", err)
		}
		fmt.Printf("%s\n", identity.PublicKeyHex())
		return
	}

	if *discover != "" {
		if !cfg.DHT.Enabled {
			logger.Fatalf("dht: -discover needs a [dht] section with enabled = true")
		}
		resolver, err := discovery.Start(cfg.DHTSettings(logger))
		if err != nil {
			logger.Fatalf("dht: %v", err)
		}
		defer func() { _ = resolver.Close() }()

		for _, failure := range resolver.Bootstrap(resolver.Context()) {
			logger.Printf("dht: bootstrap %v", failure)
		}
		record, err := resolver.Resolve(*discover)
		if err != nil {
			logger.Fatalf("dht lookup of %q failed: %v", *discover, err)
		}
		if record.Verified {
			fmt.Printf("%s -> %s (type %s, announced %s, signed by %s)\n",
				record.Name, record.Server, record.Type,
				record.Updated.UTC().Format(time.RFC3339), record.PublicKey)
			return
		}
		fmt.Printf("%s -> %s (type %s, announced %s, unsigned)\n",
			record.Name, record.Server, record.Type, record.Updated.UTC().Format(time.RFC3339))
		return
	}

	cipher, err := cfg.Cipher(config.RoleClient)
	if err != nil {
		logger.Fatalf("encryption configuration: %v", err)
	}
	tlsConfig, err := cfg.ClientTLSConfig()
	if err != nil {
		logger.Fatalf("transport configuration: %v", err)
	}
	c := &client{cfg: cfg, cipher: cipher, tlsConfig: tlsConfig, logger: logger, target: cfg.Client.ServerAddr}

	if cfg.Identity.Enabled {
		identity, err := crypto.LoadIdentity(cfg.Identity.KeyFile)
		if err != nil {
			logger.Fatalf("identity configuration: %v", err)
		}
		c.identity = identity
		logger.Printf("client identity %s (from %s)", identity.PublicKeyHex(), cfg.Identity.KeyFile)
	}

	if cfg.DHT.Enabled && cfg.DHT.Discover != "" {
		resolver, err := discovery.Start(cfg.DHTSettings(logger))
		if err != nil {
			logger.Fatalf("dht: %v", err)
		}
		c.resolver = resolver
		defer func() {
			if err := resolver.Close(); err != nil {
				logger.Printf("closing the DHT node: %v", err)
			}
		}()
		logger.Printf("dht: node %s on %s, resolving %q", resolver.Self(), resolver.Addr(), cfg.DHT.Discover)
		c.refreshTarget()
		if c.target == "" {
			logger.Fatalf("dht: %q did not resolve and client.server_addr is empty, so there is nothing to connect to",
				cfg.DHT.Discover)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("AetherTunnel client %s (protocol %d) -> %s, encryption %s, %d tunnel(s), %d visitor(s) configured",
		version, protocol.ProtocolVersion, c.target, cipher.Algorithm(),
		len(cfg.Proxies), len(cfg.Visitors))

	c.run(ctx)
	c.logger.Printf("client stopped")
}

// run keeps a session alive, reconnecting with exponential backoff.
func (c *client) run(ctx context.Context) {
	backoff := time.Duration(c.cfg.Client.ReconnectSeconds) * time.Second
	maxBackoff := time.Duration(c.cfg.Client.MaxReconnectSeconds) * time.Second

	// Visitor listeners are independent of the control session: each visiting
	// connection opens its own connection to the server.
	c.runVisitors(ctx)

	for {
		if ctx.Err() != nil {
			return
		}

		startedAt := time.Now()
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}

		// A session that lasted a while is a success as far as backoff is
		// concerned: the next failure is a new incident, not a retry storm.
		if time.Since(startedAt) > 60*time.Second {
			backoff = time.Duration(c.cfg.Client.ReconnectSeconds) * time.Second
		}

		wait := jitter(backoff)
		if err != nil {
			c.logger.Printf("session ended: %v; reconnecting in %s", err, wait.Round(time.Second))
		} else {
			c.logger.Printf("session ended; reconnecting in %s", wait.Round(time.Second))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// jitter spreads reconnects out by 卤20% so a fleet of clients does not retry in
// lockstep after a server restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

// dialServer opens a TCP connection to the server, wrapped in TLS when
// [transport].enable_tls is set.
func (c *client) dialServer() (net.Conn, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	dialer := &net.Dialer{Timeout: dialTimeout}

	target := c.serverAddr()
	var conn net.Conn
	var err error
	if c.tlsConfig == nil {
		conn, err = dialer.Dial("tcp", target)
	} else {
		conn, err = tls.DialWithDialer(dialer, "tcp", target, c.tlsConfig)
	}
	if err != nil {
		return nil, err
	}

	// The disguise is the outermost layer, so an observer sees it rather than this
	// program's frame header. Wrapping inside TLS would hide them instead.
	if disguise := c.cfg.ObfuscationDisguise(); disguise != obfs.DisguiseNone {
		disguised, err := obfs.Wrap(conn, disguise)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return disguised, nil
	}
	return conn, nil
}

// attachIdentity adds the Ed25519 assertion the server checks when
// [identity].enabled is set there.
func (c *client) attachIdentity(request *protocol.AuthRequest) error {
	if c.identity == nil {
		return nil
	}
	nonce, err := crypto.Nonce()
	if err != nil {
		return err
	}
	now := time.Now().Unix()

	request.Identity = c.identity.PublicKey()
	request.IdentityNonce = nonce
	request.IdentityTime = now
	request.IdentitySignature = c.identity.SignChallenge(nonce, now)
	return nil
}

// sessionKeyCopy returns the current post-quantum session key, or nil.
func (c *client) sessionKeyCopy() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.sessionKey...)
}

// session runs one control connection until it fails or ctx is cancelled.
func (c *client) runSession(ctx context.Context) error {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	// Re-resolving here means a proxy that moved is followed on the next attempt,
	// and a session that is already up keeps running on its established connection.
	c.refreshTarget()
	conn, err := c.dialServer()
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.serverAddr(), err)
	}
	defer conn.Close()

	// A stop signal has to end the session at once rather than at the next frame:
	// the reader below blocks on the control connection, and the only frames that
	// arrive unprompted are heartbeat acknowledgements, which are one
	// heartbeat_seconds apart.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessionDone:
		}
	}()

	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	request := protocol.AuthRequest{
		Token:         c.cfg.Client.AuthToken,
		ClientVersion: version,
		Protocol:      protocol.ProtocolVersion,
		Encryption:    c.cipher.Algorithm(),
		VPN:           c.cfg.VPN.Enabled,
	}

	var kexState []byte
	if c.cfg.PostQuantum() {
		public, state, err := crypto.HybridClientInit()
		if err != nil {
			return fmt.Errorf("post-quantum key agreement: %w", err)
		}
		request.KEX, kexState = public, state
	}
	if err := c.attachIdentity(&request); err != nil {
		return fmt.Errorf("identity assertion: %w", err)
	}

	if err := framer.WriteJSON(protocol.TypeAuthRequest, request); err != nil {
		return fmt.Errorf("send auth request: %w", err)
	}

	var response protocol.AuthResponse
	if err := framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if !response.OK {
		return fmt.Errorf("authentication rejected: %s", response.Error)
	}
	if response.ProtocolMismatch {
		c.logger.Printf("warning: server speaks protocol %d, this client speaks %d", response.Protocol, protocol.ProtocolVersion)
	}
	if response.Encryption != c.cipher.Algorithm() {
		return fmt.Errorf("encryption mismatch: server uses %q, this client uses %q", response.Encryption, c.cipher.Algorithm())
	}

	// The server switched to the agreed key right after it wrote the response,
	// so this side does the same here.
	var sessionKey []byte
	if len(request.KEX) > 0 {
		if len(response.KEX) == 0 {
			return errors.New("the server answered without a post-quantum key, but this client requires one")
		}
		sessionKey, err = crypto.HybridClientFinish(kexState, response.KEX)
		if err != nil {
			return fmt.Errorf("post-quantum key agreement: %w", err)
		}
		controlCipher, err := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, sessionKey)
		if err != nil {
			return fmt.Errorf("post-quantum key agreement: %w", err)
		}
		framer = protocol.NewFramerWithOptions(conn, controlCipher, c.framerOptions())
		c.logger.Printf("post-quantum session key %s agreed with the server", crypto.SessionKeyID(sessionKey))
	}
	_ = conn.SetDeadline(time.Time{})

	c.mu.Lock()
	c.session = response.Session
	c.p2pPort = response.P2PPort
	c.sessionKey = sessionKey
	c.mu.Unlock()

	if response.P2PPort > 0 {
		c.logger.Printf("the server offers xtcp hole punching on udp port %d", response.P2PPort)
	}

	c.logger.Printf("connected to %s as session %s (server %s)", c.serverAddr(), response.Session, response.ServerVersion)

	if err := c.registerProxies(framer); err != nil {
		return err
	}

	// The server gave this session an address on its layer-3 subnet, so the tunnel
	// runs for as long as the session does and stops with it.
	if response.VPNAddress != "" {
		stopTunnel, err := c.startVPN(ctx, framer, response)
		if err != nil {
			return err
		}
		defer stopTunnel()
	}

	heartbeatDone := make(chan struct{})
	go c.heartbeatLoop(heartbeatDone, framer, c.heartbeatInterval(response.HeartbeatSecs))

	defer close(heartbeatDone)

	// One reader: this loop owns the control framer for the life of the session.
	for {
		if ctx.Err() != nil {
			return nil
		}
		msg, err := framer.ReadFrame()
		if err != nil {
			return err
		}

		switch msg.Type {
		case protocol.TypeHeartbeatAck:
			// nothing to do; the ack proves the link is alive
		case protocol.TypeDataRequest:
			var request protocol.DataRequest
			if err := json.Unmarshal(msg.Payload, &request); err != nil {
				c.logger.Printf("malformed data request: %v", err)
				continue
			}
			go c.serveStream(response.Session, request)
		case protocol.TypeVPNPacket:
			c.deliverVPN(msg.Payload)
		case protocol.TypeP2PPrepare:
			var prepare protocol.P2PPrepare
			if err := json.Unmarshal(msg.Payload, &prepare); err != nil {
				c.logger.Printf("malformed p2p-prepare: %v", err)
				continue
			}
			go c.servePunch(prepare)
		case protocol.TypeProxyList:
			c.logProxyList(msg.Payload)
		case protocol.TypeError:
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			c.logger.Printf("server reported: %s", payload.Error)
		default:
			c.logger.Printf("ignoring unexpected control frame %s", msg.Type)
		}
	}
}

// startVPN opens the client's layer-3 interface and runs the tunnel for one session.
//
// It returns a function that stops the tunnel and closes the interface. The tunnel
// is started after the session exists, because the address it configures comes from
// the server's answer, and it stops when the session does: the packets it carries
// have nowhere to go without the session.
func (c *client) startVPN(ctx context.Context, framer *protocol.Framer, response protocol.AuthResponse) (func(), error) {
	if !c.cfg.VPN.Enabled {
		// The server offered an address but this client has no [vpn] section, so
		// there is nowhere to put the packets.
		c.logger.Printf("the server offered tunnel address %s but [vpn] enabled is false; ignoring it",
			response.VPNAddress)
		return func() {}, nil
	}

	mtu := response.VPNMTU
	if c.cfg.VPN.MTU != 0 {
		mtu = c.cfg.VPN.MTU
	}

	device, err := vpn.Open(c.cfg.VPN.Device, mtu)
	if err != nil {
		return nil, fmt.Errorf("open the tunnel interface: %w", err)
	}
	if err := vpn.AssignAddress(device, response.VPNAddress); err != nil {
		_ = device.Close()
		return nil, fmt.Errorf("configure %s with %s: %w", device.Name(), response.VPNAddress, err)
	}

	transport := vpn.NewChannelTransport(func(packet []byte) error {
		return framer.WriteFrame(&protocol.Message{Type: protocol.TypeVPNPacket, Payload: packet})
	})
	tunnel, err := vpn.New(device, transport, vpn.Options{MTU: mtu, Logger: c.logger})
	if err != nil {
		_ = device.Close()
		_ = transport.Close()
		return nil, err
	}

	c.vpnMu.Lock()
	c.vpnTransport = transport
	c.vpnMu.Unlock()

	tunnelCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := tunnel.Run(tunnelCtx); err != nil {
			c.logger.Printf("tunnel %s stopped: %v", device.Name(), err)
		}
	}()

	mask := response.VPNMask
	if mask == "" {
		mask = "255.255.255.0"
	}
	c.logger.Printf("tunnel interface %s is %s/%s, MTU %d", device.Name(), response.VPNAddress, mask, tunnel.MTU())

	return func() {
		cancel()
		<-done
		c.vpnMu.Lock()
		c.vpnTransport = nil
		c.vpnMu.Unlock()

		stats := tunnel.Stats().Snapshot()
		c.logger.Printf("tunnel stopped: %d packets out (%d bytes), %d in (%d bytes), %d dropped",
			stats.FromDevice, stats.BytesFromDevice, stats.ToDevice, stats.BytesToDevice, stats.Dropped)
	}, nil
}

// registerProxies publishes every configured tunnel on the current session.
func (c *client) registerProxies(framer *protocol.Framer) error {
	if len(c.cfg.Proxies) == 0 {
		c.logger.Printf("no [[proxies]] configured: the connection is up but publishes nothing")
		return nil
	}
	for _, proxy := range c.cfg.Proxies {
		spec := protocol.ProxySpec{
			Name:       proxy.Name,
			Type:       proxy.Type,
			LocalAddr:  proxy.LocalAddr(),
			RemotePort: proxy.RemotePort,
			Domains:    proxy.Domains,
			SecretKey:  proxy.SecretKey,
			AuthMethod: proxy.AuthMethod,
			Group:      proxy.Group,
			Multipath:  proxy.Multipath,
			// The visitor filters and, for a socks5 tunnel, the ranges it may
			// dial travel with the registration, because both are properties of
			// this client rather than of the server's configuration.
			AllowCIDRs:   proxy.AllowCIDRs,
			DenyCIDRs:    proxy.DenyCIDRs,
			AllowTargets: proxy.AllowTargets,
		}
		if err := framer.WriteJSON(protocol.TypeRegisterProxy, spec); err != nil {
			return fmt.Errorf("register proxy %q: %w", proxy.Name, err)
		}
		if proxy.Type == config.ProxyTypeSOCKS {
			c.logger.Printf("requested tunnel %q (%s) -> the address the visitor asks for (public port %d)",
				proxy.Name, proxy.Type, proxy.RemotePort)
			continue
		}
		c.logger.Printf("requested tunnel %q (%s) -> %s (public port %d)",
			proxy.Name, proxy.Type, spec.LocalAddr, proxy.RemotePort)
	}
	return nil
}

func (c *client) logProxyList(payload []byte) {
	var statuses []protocol.ProxyStatus
	if err := json.Unmarshal(payload, &statuses); err != nil {
		c.logger.Printf("malformed proxy list: %v", err)
		return
	}
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, status.Name)
	}
	c.mu.Lock()
	c.registeredNames = names
	c.mu.Unlock()

	// Name the port the server says the proxy is reachable on, not the one this
	// client asked for. The two differ when the proxy joined a pool that was already
	// published on another port: a pool owns one endpoint, so a later member's own
	// request is not honoured, and repeating it here would send an operator to a
	// port nothing is listening on.
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.RemotePort != 0 {
			parts = append(parts, fmt.Sprintf("%s on port %d", status.Name, status.RemotePort))
			continue
		}
		parts = append(parts, status.Name)
	}
	c.logger.Printf("server confirms %d tunnel(s): %s", len(parts), strings.Join(parts, ", "))
}

// framerOptions maps the client's [obfuscation] section onto frame padding and
// jitter. The server unpads from the frame flag alone, so the two ends do not have
// to agree on these values.
func (c *client) framerOptions() protocol.FramerOptions {
	return protocol.FramerOptions{
		MaxPayload: protocol.DefaultMaxPayload,
		PadTo:      c.cfg.Obfuscation.PadTo,
		Jitter:     time.Duration(c.cfg.Obfuscation.JitterMillis) * time.Millisecond,
	}
}

// heartbeatInterval decides how often to send a heartbeat.
//
// The server dictates the interval and the client follows it, because the server is
// what drops a connection that has been quiet for three intervals. The configured
// [client].heartbeat_seconds is the fallback for a server that does not state one, so
// it is a real setting rather than a value that silently does nothing.
func (c *client) heartbeatInterval(serverSeconds int) time.Duration {
	if serverSeconds > 0 {
		fromServer := time.Duration(serverSeconds) * time.Second
		if configured := c.cfg.ClientHeartbeatInterval(); configured != fromServer {
			c.logger.Printf("the server asks for a heartbeat every %ds, so client.heartbeat_seconds (%s) has no effect on this session",
				serverSeconds, configured)
		}
		return fromServer
	}
	if configured := c.cfg.ClientHeartbeatInterval(); configured > 0 {
		return configured
	}
	return 30 * time.Second
}

// heartbeatLoop sends heartbeats until done is closed. It only writes, so it does
// not race with the session's reader.
func (c *client) heartbeatLoop(done <-chan struct{}, framer *protocol.Framer, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeat}); err != nil {
				return
			}
		}
	}
}

// serveStream opens a second connection to the server for one visiting connection
// and forwards it to the local service.
func (c *client) serveStream(session string, request protocol.DataRequest) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second

	proxy, err := c.findProxy(request.Proxy)
	if err != nil {
		c.logger.Printf("cannot serve stream for %q: %v", request.Proxy, err)
		return
	}

	// The service is dialled before the data connection is opened, so a failure is
	// reported to the server — and from there to the visitor — instead of leaving
	// the visitor to wait for the server's dial timeout. A socks5 tunnel dials the
	// address the visitor asked for, checked against the ranges its configuration
	// allows; a datagram tunnel dials its own UDP socket later.
	var local net.Conn
	if !config.IsDatagramProxyType(proxy.Type) {
		local, err = c.dialForProxy(proxy, request.Target)
		if err != nil {
			c.reportStreamFailure(session, request.Proxy, request.StreamID, err)
			return
		}
		defer local.Close()
	}

	conn, err := c.dialServer()
	if err != nil {
		c.logger.Printf("stream for %q: cannot reach the server: %v", request.Proxy, err)
		return
	}
	defer conn.Close()

	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())
	fail := func(reason string) {
		c.logger.Printf("stream for %q: %s", request.Proxy, reason)
		_ = conn.Close()
	}

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    request.Proxy,
		StreamID: request.StreamID,
	}); err != nil {
		fail(fmt.Sprintf("cannot send data-open: %v", err))
		return
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		fail(fmt.Sprintf("cannot read data-open ack: %v", err))
		return
	}
	if !ack.OK {
		fail(fmt.Sprintf("server refused the stream: %s", ack.Error))
		return
	}
	_ = conn.SetDeadline(time.Time{})

	// With a post-quantum session both ends switch to a key derived from the
	// session key and this stream's identifier, so no two streams share a
	// keystream. Without one, the configured cipher applies.
	streamCipher := c.cipher
	if key := c.sessionKeyCopy(); len(key) > 0 {
		streamKey, keyErr := crypto.StreamKey(key, request.StreamID)
		if cipher, cipherErr := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, streamKey); keyErr == nil && cipherErr == nil {
			streamCipher = cipher
			framer = protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())
		} else {
			c.logger.Printf("stream for %q: cannot derive a stream key: %v%v", request.Proxy, keyErr, cipherErr)
		}
	}

	if config.IsDatagramProxyType(proxy.Type) {
		c.serveDatagrams(request.Proxy, conn, framer, proxy.LocalAddr())
		return
	}

	serverSide := &cryptoStreamConn{Stream: crypto.NewStream(conn, streamCipher), conn: conn}
	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toServer, fromServer := flynet.Pipe(local, serverSide, idle)
	c.logger.Printf("stream for %q finished (sent %d bytes to the server, received %d)",
		request.Proxy, toServer, fromServer)
}

// dialForProxy opens the connection a stream should carry. A socks5 tunnel is
// dialled at the address the visitor asked for, checked against the ranges its
// configuration allows; every other type is dialled at its configured local
// service.
func (c *client) dialForProxy(proxy config.ProxyConfig, target string) (net.Conn, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second

	if proxy.Type != config.ProxyTypeSOCKS {
		conn, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
		if err != nil {
			return nil, fmt.Errorf("cannot reach the local service %s: %w", proxy.LocalAddr(), err)
		}
		return conn, nil
	}

	if target == "" {
		return nil, errors.New("a socks5 stream arrived without a target")
	}
	policy, err := socks.NewTargetPolicy(proxy.AllowTargets)
	if err != nil {
		return nil, err
	}
	conn, err := policy.Dial(target, dialTimeout)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// reportStreamFailure tells the server that no data connection is coming.
//
// It opens a data connection only to carry the refusal: the server's stream is
// waiting on that answer, so the visitor learns the reason now instead of waiting
// for the dial timeout. Nothing is sent on the connection afterwards.
func (c *client) reportStreamFailure(session, proxy, streamID string, cause error) {
	c.logger.Printf("stream for %q: %v", proxy, cause)

	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	conn, err := c.dialServer()
	if err != nil {
		c.logger.Printf("stream for %q: cannot report the failure to the server: %v", proxy, err)
		return
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    proxy,
		StreamID: streamID,
		Error:    cause.Error(),
	}); err != nil {
		c.logger.Printf("stream for %q: cannot report the failure: %v", proxy, err)
		return
	}

	var ack protocol.DataOpenAck
	_ = framer.ReadJSON(protocol.TypeDataOpenAck, &ack)
}

// serveDatagrams relays a datagram stream: each TypeUDPPacket frame carries one
// datagram for the local UDP service, and each reply becomes one frame.
//
// The service is dialled as a connected UDP socket, so a reply is only accepted
// from the address the requests were sent to 鈥?which is what a local service
// does. The frames are already sealed individually by the framer, so no record
// layer is layered on top.
func (c *client) serveDatagrams(proxy string, conn net.Conn, framer *protocol.Framer, localAddr string) {
	defer conn.Close()

	local, err := net.Dial("udp", localAddr)
	if err != nil {
		c.logger.Printf("stream for %q: cannot reach the local service %s: %v", proxy, localAddr, err)
		return
	}
	defer local.Close()

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if msg.Type != protocol.TypeUDPPacket {
				continue
			}
			if _, err := local.Write(msg.Payload); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
		for {
			if idle > 0 {
				_ = local.SetReadDeadline(time.Now().Add(idle))
			}
			n, err := local.Read(buf)
			if err != nil {
				return
			}
			if err := framer.WriteFrame(&protocol.Message{
				Type:    protocol.TypeUDPPacket,
				Payload: buf[:n],
			}); err != nil {
				return
			}
		}
	}()

	<-done
	_ = conn.Close()
	_ = local.Close()
	<-done
}

func (c *client) findProxy(name string) (config.ProxyConfig, error) {
	for _, proxy := range c.cfg.Proxies {
		if proxy.Name == name {
			return proxy, nil
		}
	}
	return config.ProxyConfig{}, errors.New("not in this client's configuration")
}

// cryptoStreamConn presents a crypto.Stream as a net.Conn for the pipe helper.
type cryptoStreamConn struct {
	*crypto.Stream
	conn net.Conn
}

func (c *cryptoStreamConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *cryptoStreamConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *cryptoStreamConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *cryptoStreamConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *cryptoStreamConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
