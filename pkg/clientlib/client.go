// Package clientlib implements the AetherTunnel client as a reusable library.
//
// The aethertunnel-client command is a thin wrapper around [Run]: it parses
// flags, loads the configuration and installs the signal handling, then hands
// the pieces to Run. Embedding applications — the mobile bindings in
// pkg/mobile, for example — drive the same client through Run with their own
// context, configuration and logger.
package clientlib

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"

	"github.com/aethertunnel/aethertunnel/pkg/vpn"
	"golang.org/x/time/rate"
)

// Stamped at build time; see scripts/build-release.sh for the ldflags form.
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

const (
	// A client that resolves its server by name may start before the server has announced
	// the record, or before its own DHT node has heard from a bootstrap peer. Both are
	// normal startup order rather than a wrong name, so the first resolution is retried
	// for this long before the client gives up.
	dhtStartupGrace = 30 * time.Second
	dhtStartupRetry = 3 * time.Second
)

type client struct {
	cfg *config.Config
	// cipher is the session's record layer. A reload that adopts a new auth
	// token rebuilds it when encryption keys are derived from the token, so
	// the pointer goes through an atomic: the dial paths read it while the
	// SIGHUP goroutine may be swapping it.
	cipher    atomic.Pointer[crypto.Cipher]
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
	// healthMu guards the per-proxy health states the probers write and the
	// data path reads; pluginsMu guards the in-process plugin servers.
	healthMu      sync.Mutex
	health        map[string]*healthState
	staticFilesMu sync.Mutex
	staticFiles   map[string]*staticFileServer
	// pluginsStopped records that stopPluginServers has run. A stream goroutine
	// that reaches dialForPlugin after that point would build an *http.Server the
	// snapshot no longer sees, and nothing would ever stop it. It is guarded by
	// staticFilesMu and checked by the two helpers that build a server.
	pluginsStopped bool

	// controlFramer is the live session's framer; withdrawals and
	// re-registrations write through it. specs mirrors what this client has
	// registered, so a recovered health check can register the proxy again.
	controlFramerMu sync.Mutex
	// reloadMu serializes configuration application. A reload arrives on a signal
	// goroutine and on the admin API's HTTP goroutine, and the two interleaving
	// their probe and visitor restarts lost state: a stopHealthCheck could delete
	// the state the concurrent startHealthCheck had just installed, leaving a
	// failing service reported healthy forever.
	reloadMu      sync.Mutex
	controlFramer *protocol.Framer
	specsMu       sync.Mutex
	specs         map[string]protocol.ProxySpec

	// withdrawMu serializes withdrawals; withdrawWait receives the server's
	// ack from the control reader. withdrawUnsupported is set when a server
	// stays silent — an older release that ignores the frame — and the health
	// check falls back to refusing dials instead.
	withdrawMu          sync.Mutex
	withdrawWait        chan protocol.ProxyWithdrawAck
	withdrawUnsupported bool

	// baseCtx is the client's lifetime; resources a reload creates (visitor
	// listeners, health probes) hang off children of it so a reload can stop
	// them one by one without touching the session.
	baseCtx context.Context
	// serviceCtx is cancelled by stopStartupServices. The admin server and the
	// reload watcher wait on it rather than on baseCtx, which belongs to the
	// embedder and stays alive after Run has returned an error: without it their
	// supervising goroutines outlived the Run that started them, one per attempt.
	serviceCtx    context.Context
	serviceCancel context.CancelFunc
	// proxies and visitors are the dispatch lists a reload swaps: findProxy,
	// the health checks and the visitor listeners read them instead of the
	// frozen session-level configuration.
	proxies   []config.ProxyConfig
	visitors  []config.VisitorConfig
	proxiesMu sync.RWMutex

	// store keeps the entries created at runtime through the admin API; nil
	// unless [store].path is set. fileProxies and fileVisitors are what the
	// configuration itself selects, which the store's entries are merged onto
	// whenever either side changes.
	store        *store
	fileMu       sync.Mutex
	fileProxies  []config.ProxyConfig
	fileVisitors []config.VisitorConfig

	// visitorsMu also guards the context the visitor listeners run under; a
	// reload that changes the set cancels it and starts the new listeners.
	visitorsMu     sync.Mutex
	visitorsCtx    context.Context
	visitorsCancel context.CancelFunc
	// visitorsStarted records that the listeners for visitorsCtx have been started,
	// so the session loop does not start the same set a second time after a reload
	// that landed before it.
	visitorsStarted bool
	// cfgMu guards the client-section token, which a reload swaps while a
	// reconnect reads it, and the credential the current session presented.
	cfgMu sync.Mutex
	// presentedToken is the credential the running session authenticated with:
	// the static token, or the access token for [oidc]. Per-proxy stream keys
	// are derived from it, because the server derives them from the credential
	// the session actually presented — deriving from cfg here would keep a
	// rotated or placeholder token pointing at a different key than the one
	// the server answered.
	presentedToken string

	healthStopsMu sync.Mutex
	// healthStops holds one entry per running probe. The stored context is
	// what identifies the probe: cancel functions are not comparable, so a
	// finishing goroutine proves it is still the current probe by matching
	// its context.
	healthStops map[string]healthProbe
	// adminServer is the running management API, kept so a failed Run can
	// stop it: an embedder's context stays alive after Run has returned.
	adminServer interface {
		Shutdown(context.Context) error
	}

	// pluginTLS caches the https2http certificate per proxy name.
	pluginTLSMu sync.Mutex
	pluginTLS   map[string]*tls.Config

	// sessionEstablished reports that a session has logged in at least once,
	// which is what client.login_fail_exit waits for before giving up.
	sessionEstablished atomic.Bool
	// stopped is set once stopStartupServices has torn the client's services
	// down. A SIGHUP that was already mid-flight when Run failed would
	// otherwise apply a reload for a client that has stopped: the reload
	// restarts health probes and binds visitor listeners under the caller's
	// context, which outlives Run, so the listeners and probes would leak
	// until the embedder cancels its context.
	stopped atomic.Bool
	// giveUp carries the error that made client.login_fail_exit stop the
	// client after a refused first login.
	giveUp error

	// oidcMu guards the cached access token of an [oidc] client.
	oidcMu     sync.Mutex
	oidcToken  string
	oidcExpiry time.Time

	// pluginPolicies caches the allow_targets matcher per proxy name.
	pluginPolicies map[string]*socks.TargetPolicy

	// vpnShellOpen and vpnShellProtect are set by RunWithShell for a platform that
	// supplies the layer-3 device itself. vpnShellOpen is called at the moment the
	// session is up and the server's address assignment is known; vpnProtect runs
	// at socket-creation time for the connections to the server.
	vpnShellOpen    func(mtu int, address string, prefix int, subnet string) (vpn.Device, error)
	vpnShellProtect func(fd int)

	mu              sync.Mutex
	session         string
	p2pPort         int
	sessionKey      []byte
	registeredNames []string

	// poolRefused records that the server refused a pooled connection, so the
	// reason is logged once per session rather than once per worker.
	poolRefused atomic.Bool

	// vpnNoAddrDrops counts tunnel packets that arrived while this session held
	// no tunnel device; it shares vpnDropLogAt's throttle with vpnDropCount.
	vpnNoAddrDrops atomic.Uint64
	// vpnDropCount counts tunnel packets dropped since the last warning, and
	// vpnDropLogAt is the Unix second that warning was written. Both are touched by
	// deliverVPN only.
	vpnDropCount atomic.Uint64
	vpnDropLogAt atomic.Int64
}

// vpnDropLogInterval is how many seconds apart a tunnel that keeps dropping
// packets repeats itself. A congested tunnel drops one packet per read, so a line
// each would fill the log faster than the packets arrive and bury the event it is
// reporting; one line per interval with the count keeps the fact visible.
const vpnDropLogInterval = 30

// deliverVPN hands a packet received on the control connection to the tunnel.
func (c *client) deliverVPN(packet []byte) {
	c.vpnMu.Lock()
	transport := c.vpnTransport
	c.vpnMu.Unlock()
	if transport == nil {
		// The device is opened after the session comes up and torn down while
		// the session reader can still be running, so the server's forwarded
		// datagrams — ARP, probes, in-flight traffic — arrive here at packet
		// rate. One line per packet would bury the log exactly as the
		// buffer-full case below would, so the same throttle covers it.
		now := time.Now().Unix()
		last := c.vpnDropLogAt.Load()
		if last != 0 && now-last < vpnDropLogInterval {
			c.vpnNoAddrDrops.Add(1)
			return
		}
		if !c.vpnDropLogAt.CompareAndSwap(last, now) {
			c.vpnNoAddrDrops.Add(1)
			return
		}
		c.logger.Printf("dropped %d tunnel packet(s): this session holds no tunnel address",
			c.vpnNoAddrDrops.Swap(0)+1)
		return
	}
	if transport.Deliver(packet) {
		return
	}
	now := time.Now().Unix()
	last := c.vpnDropLogAt.Load()
	if last != 0 && now-last < vpnDropLogInterval {
		c.vpnDropCount.Add(1)
		return
	}
	if !c.vpnDropLogAt.CompareAndSwap(last, now) {
		c.vpnDropCount.Add(1)
		return
	}
	logging.Warnf(c.logger, "dropped %d tunnel packet(s) because the tunnel's buffer is full",
		c.vpnDropCount.Swap(0)+1)
}

// serverAddr is the address the next connection attempt dials.
func (c *client) serverAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

// namesAControlPort reports whether a record of this type carries the address of the
// server's control port. A private proxy is reached by name through the control port, so
// its record does; a tcp or udp proxy's record names its own public port, and an http or
// https proxy's names the shared listener — those are where visitors go, not where a
// client connects.
func namesAControlPort(proxyType string) bool {
	switch proxyType {
	case config.ProxyTypeSTCP, config.ProxyTypeSUDP, config.ProxyTypeXTCP:
		return true
	default:
		return false
	}
}

// usesDiscoveredAddress reports whether this client resolves [dht].discover instead of
// dialling the configured address.
//
// It is one or the other, never both. A configured address is what the operator pinned,
// and a DHT record is not signed unless dht.trusted_keys says so: whoever can answer for
// the name could otherwise redirect a client that named its server to an address of their
// choosing, and the client would hand that address its auth_token in the first frame.
// A client that wants to follow the name leaves server_addr empty, which is what the
// documentation for dht.discover describes.
func (c *client) usesDiscoveredAddress() bool {
	return c.cfg.DHT.Enabled && c.cfg.DHT.Discover != "" && c.cfg.Client.ServerAddr == ""
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
		logging.Warnf(c.logger, "dht: cannot resolve %q: %v (still using %s)",
			c.cfg.DHT.Discover, err, c.serverAddr())
		return
	}
	if record.Server == c.serverAddr() {
		return
	}
	c.mu.Lock()
	c.target = record.Server
	c.mu.Unlock()
	if !namesAControlPort(record.Type) {
		// The record names where *visitors* reach that proxy: its public port for tcp and
		// udp, the shared listener for http and https. Only a private proxy's record names
		// the control port, so this address can only serve as one by coincidence.
		c.logger.Printf("warning: the record for %q is a %s proxy, so %s is where visitors reach it, "+
			"not a control port; to resolve a server address, name a private proxy (stcp, sudp or xtcp)",
			record.Name, record.Type, record.Server)
	}
	if record.Verified {
		c.logger.Printf("dht: %q resolves to %s (type %s), signed by %s",
			record.Name, record.Server, record.Type, record.PublicKey)
		return
	}
	c.logger.Printf("dht: %q resolves to %s (type %s), unsigned", record.Name, record.Server, record.Type)
}

// Run starts the tunnel client with the given configuration and logger and
// blocks until ctx is cancelled or a fatal configuration error shows up. A
// caller that wants the usual command-line behaviour — stop on SIGINT and
// SIGTERM — installs signal.NotifyContext on ctx before calling Run.
//
// Run returns the error behind a failed dial-back loop only when the client
// gives up on its own; otherwise it returns nil after a clean stop.
// RunWithShell is Run for an embedding platform that supplies the layer-3 device
// itself — Android's VpnService, iOS's packet flow. openDevice is called at the
// moment the session is up and the server's address assignment is known, which is
// when a platform interface can finally be configured; the device it returns must
// already carry that address and whatever routes the shell chose. protect keeps
// the tunnel's own sockets out of those routes; a shell that routes only the
// tunnel's own subnet has no loop to fear and can leave it nil.
func RunWithShell(ctx context.Context, cfg *config.Config, logger *log.Logger,
	openDevice func(mtu int, address string, prefix int, subnet string) (vpn.Device, error),
	protect func(fd int),
) error {
	return runWithShell(ctx, cfg, logger, openDevice, protect)
}

// serviceLifetime is the context the supervising services — the admin server and
// the reload watcher — wait on. A client that went through Run has serviceCtx,
// which the failed-Run path cancels; one built in code just to start a service
// has only baseCtx, and that is then the lifetime which ends it.
func (c *client) serviceLifetime() context.Context {
	if c.serviceCtx != nil {
		return c.serviceCtx
	}
	if c.baseCtx != nil {
		return c.baseCtx
	}
	return context.Background()
}

// stopStartupServices undoes the services Run starts before its fatal-error
// returns: the health probes, the reload watcher and the admin server all
// wait on the caller's context, which an embedding application keeps alive
// after Run has returned, so they have to be stopped explicitly.
func (c *client) stopStartupServices() {
	// The apply paths — refreshDispatch and reloadFromFile — hold reloadMu for
	// their whole run, and each of them starts long-lived services: probes
	// under the caller's context, and visitor listeners under a fresh child
	// context. Reading the visitor cancel or stopping the probes outside this
	// lock let an apply install a probe or a listener after this function had
	// gone past it, and that service then outlived Run. Lock order: reloadMu
	// first, then healthStopsMu or visitorsMu, never the reverse.
	c.reloadMu.Lock()
	// First, so a reload goroutine that has not started applying yet sees
	// the flag in reloadFromFile no matter how the schedules interleave.
	c.stopped.Store(true)
	c.stopAllHealthChecks()
	// The visitor listeners are children of the caller's context, which an
	// embedder keeps alive after Run has returned; without this a
	// login_fail_exit retry cannot rebind the same visitor ports.
	c.visitorsMu.Lock()
	cancel := c.visitorsCancel
	c.visitorsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.reloadMu.Unlock()

	// Not under reloadMu: stopAdminServer waits for the handlers to return,
	// and a handler can be waiting for reloadMu here.
	c.stopReloadWatcher()
	c.stopAdminServer()
	// The admin server and the reload watcher supervise themselves from this
	// context, which is a child of the caller's: cancelling it ends those
	// goroutines now instead of when the embedder cancels its own context.
	if c.serviceCancel != nil {
		c.serviceCancel()
	}
}

// Run starts the tunnel client with the given configuration and logger and
// blocks until ctx is cancelled or a fatal configuration error shows up.
func Run(ctx context.Context, cfg *config.Config, logger *log.Logger) error {
	return runWithShell(ctx, cfg, logger, nil, nil)
}

func runWithShell(ctx context.Context, cfg *config.Config, logger *log.Logger,
	openDevice func(mtu int, address string, prefix int, subnet string) (vpn.Device, error),
	protect func(fd int),
) error {
	// Run and RunWithShell accept a configuration built in code, which has not been
	// through Load's defaulting, and these client settings read a zero as an
	// immediate or zero duration at some use sites rather than as "unset".
	cfg.ApplyClientDefaults()
	cipher, err := cfg.Cipher(config.RoleClient)
	if err != nil {
		return fmt.Errorf("encryption configuration: %w", err)
	}
	tlsConfig, err := cfg.ClientTLSConfig()
	if err != nil {
		return fmt.Errorf("transport configuration: %w", err)
	}
	c := &client{cfg: cfg, tlsConfig: tlsConfig, logger: logger, target: cfg.Client.ServerAddr,
		vpnShellOpen: openDevice, vpnShellProtect: protect}
	c.cipher.Store(cipher)
	c.baseCtx = ctx
	c.serviceCtx, c.serviceCancel = context.WithCancel(ctx)
	c.proxies, c.visitors = selectByStart(cfg, logger)
	c.fileProxies, c.fileVisitors = c.proxies, c.visitors
	if cfg.Store.Path != "" {
		runtimeStore, err := newStore(cfg.Store.Path, logger)
		if err != nil {
			return fmt.Errorf("store configuration: %w", err)
		}
		// The snapshot is what validation uses, here and for every later write, so
		// a runtime entry is judged against the running configuration.
		storeCfg := c.validationConfig()
		runtimeStore.own(storeCfg)
		if err := runtimeStore.validateAgainst(storeCfg); err != nil {
			// One conflicting runtime entry used to fail every start — and the
			// admin API that could delete it only comes up after a start, so it
			// would fail forever. Drop the entries that cannot coexist instead:
			// the client starts, the rest still applies, and the log names what
			// was left out.
			dropped := runtimeStore.dropConflicts(storeCfg)
			for _, d := range dropped {
				logger.Printf("store %s: dropped a conflicting runtime entry: %s", cfg.Store.Path, d)
			}
			if len(dropped) == 0 {
				return fmt.Errorf("store %s: %w", cfg.Store.Path, err)
			}
		}
		c.store = runtimeStore
		c.proxies, c.visitors = c.effectiveLists()
		storedProxies, storedVisitors := runtimeStore.list()
		logger.Printf("store %s: %d runtime proxy entry(ies) and %d visitor entry(ies) restored, "+
			"taking precedence over entries of the same name in the configuration",
			cfg.Store.Path, len(storedProxies), len(storedVisitors))
	}
	c.startReloadWatcher()
	c.startHealthChecks(ctx)
	if err := c.startAdminServer(); err != nil {
		c.stopStartupServices()
		return err
	}

	if cfg.Identity.Enabled {
		identity, err := crypto.LoadIdentity(cfg.Identity.KeyFile)
		if err != nil {
			c.stopStartupServices()
			return fmt.Errorf("identity configuration: %w", err)
		}
		c.identity = identity
		logger.Printf("client identity %s (from %s)", identity.PublicKeyHex(), cfg.Identity.KeyFile)
	}

	if c.usesDiscoveredAddress() {
		resolver, err := discovery.Start(cfg.DHTSettings(logger))
		if err != nil {
			c.stopStartupServices()
			return fmt.Errorf("dht: %w", err)
		}
		c.resolver = resolver
		defer func() {
			if err := resolver.Close(); err != nil {
				logger.Printf("closing the DHT node: %v", err)
			}
		}()
		logger.Printf("dht: node %s on %s, resolving %q", resolver.Self(), resolver.Addr(), cfg.DHT.Discover)

		// Seed the routing table before the first lookup. A node that has not spoken to a
		// bootstrap peer has nobody to ask, so the lookup fails with "key not found" even
		// when the record is published — which is what a client used to do here, and then
		// exit as if the name did not exist.
		for _, failure := range resolver.Bootstrap(resolver.Context()) {
			logging.Warnf(logger, "dht: bootstrap %v", failure)
		}
		// A server that has not announced yet is also normal startup order, so keep asking
		// for a while before deciding the name really is unknown.
		deadline := time.Now().Add(dhtStartupGrace)
		for {
			c.refreshTarget()
			if c.target != "" {
				break
			}
			// A caller that cancels here is shutting down: waiting out the
			// grace would hold the process for up to dhtStartupGrace longer
			// than the stop it already asked for. The services started above
			// watch serviceLifetime, a child of this context, so they unwind
			// on their own, exactly as they do on every other cancelled run.
			if ctx.Err() != nil {
				return nil
			}
			if time.Now().After(deadline) {
				c.stopStartupServices()
				return fmt.Errorf("dht: %q did not resolve within %s and client.server_addr is empty, so there is nothing to connect to",
					cfg.DHT.Discover, dhtStartupGrace)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(dhtStartupRetry):
			}
		}
	}

	// Every other layer here announces itself — the post-quantum key agreement,
	// TLS, the identity, the disguise — because a layer that quietly stopped
	// being applied is the failure that costs an afternoon. The transport shape
	// is the one that changes what a middlebox sees, so it says so too, and a
	// report that quotes this line says which shape was on the wire.
	if cfg.Transport.Protocol == "websocket" {
		logger.Printf("every connection to the server is carried inside RFC 6455 websocket frames ([transport].protocol = \"websocket\")")
	}

	logger.Printf("AetherTunnel client %s (protocol %d) -> %s, encryption %s, %d tunnel(s), %d visitor(s) configured",
		Version, protocol.ProtocolVersion, c.target, cipher.Algorithm(),
		len(c.proxyList()), len(c.visitorList()))

	c.run(ctx)
	if c.giveUp != nil {
		c.logger.Printf("client stopped: %v", c.giveUp)
		// The embedder's context stays alive after Run returns, so the
		// services started before the first session — the admin server, the
		// health probes, the reload watcher — have to be stopped here too,
		// or a login_fail_exit retry would collide with them.
		c.stopStartupServices()
		return c.giveUp
	}
	c.logger.Printf("client stopped")
	return nil
}

// run keeps a session alive, reconnecting with exponential backoff.
func (c *client) run(ctx context.Context) {
	// The in-process plugin servers are not children of this context, so they are
	// stopped here on every return, not only on the fatal one: an http.Server keeps
	// serving its in-memory listener whatever happens to the client's context, and
	// an embedder's process outlives Run.
	defer c.stopPluginServers()
	backoff := time.Duration(c.cfg.Client.ReconnectSeconds) * time.Second
	maxBackoff := time.Duration(c.cfg.Client.MaxReconnectSeconds) * time.Second

	// Visitor listeners are independent of the control session: each visiting
	// connection opens its own connection to the server. They run on a child of
	// the client's context so a reload can restart them as a set.
	c.startVisitors()

	for {
		if ctx.Err() != nil {
			return
		}

		startedAt := time.Now()
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		// client.login_fail_exit mirrors frp's loginFailExit: a client whose
		// very first login is refused gives up instead of retrying forever. A
		// session that logged in once still reconnects when it drops.
		if err != nil && c.cfg.Client.LoginFailExit && !c.sessionEstablished.Load() {
			logging.Warnf(c.logger, "login failed and client.login_fail_exit is set: %v", err)
			c.giveUp = err
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

// jitter spreads reconnects out by ±20% so a fleet of clients does not retry in
// lockstep after a server restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

// dialServer opens a connection to the server and adds the configured layers,
// outermost first: the disguise, then TLS, then the websocket.
//
// The order is the point. The disguise is the outermost layer, so an observer sees
// it rather than this program's frame header; wrapping a *tls.Conn instead writes
// the disguise's records inside the TLS session, where nothing on the wire shows
// them, and a tls-session disguise then also loses the anonymous certificate that
// is the reason to use it. The dial_via path is composed the same way, so both
// shapes put the same bytes on the wire.
func (c *client) dialServer(ctx context.Context) (net.Conn, error) {
	dialer := c.dialer()

	target := c.serverAddr()
	conn, err := c.dialRaw(ctx, dialer, target)
	if err != nil {
		return nil, err
	}

	if disguise := c.cfg.ObfuscationDisguise(); disguise != obfs.DisguiseNone {
		disguised, err := obfs.Wrap(conn, disguise, obfs.Dialer)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = disguised
	}

	if c.tlsConfig != nil {
		secured, err := c.handshakeTLS(ctx, conn, target)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = secured
	}

	// The websocket is the innermost layer, so the tunnel's own framing rides
	// inside binary frames while the disguise and TLS keep their places — the
	// wss shape when TLS is on. The server recognises the upgrade on the
	// shared port and needs no setting of its own.
	if c.cfg.Transport.Protocol == "websocket" {
		host := target
		if h, _, err := net.SplitHostPort(target); err == nil {
			host = h
		}
		upgraded, err := flynet.WebsocketDial(conn, host, time.Duration(c.cfg.Client.DialTimeoutSecs)*time.Second)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = upgraded
	}
	return conn, nil
}

// dialRaw opens the TCP connection to the server, directly or through the
// dial_via intermediary, and adds nothing on top: the caller composes the
// disguise, TLS and the websocket so both paths put the same layers in the same
// order on the wire.
//
// ctx bounds the dial as well as the caller's own deadline: a visitor whose
// fallback_timeout_ms expires has to be sent to the fallback, and a shutdown has
// to stop waiting for a server that is not answering, neither of which happened
// while this dial was built from context.Background().
func (c *client) dialRaw(ctx context.Context, dialer *net.Dialer, target string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cfg.Client.DialVia == "" {
		return dialer.DialContext(ctx, "tcp", target)
	}
	timeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return dialVia(dialCtx, c.cfg.Client.DialVia, dialer, target)
}

// handshakeTLS runs the transport handshake on an already-dialled connection. The
// server name is filled in from the address that was dialled when neither
// transport.server_name nor client.server_addr supplied one, which is what
// tls.DialWithDialer used to do before the disguise had to be put underneath it.
//
// The caller's ctx bounds the handshake as well as the dial: a visitor whose
// fallback_timeout_ms expires has to be sent to the fallback, and a shutdown has
// to stop waiting, neither of which happened while this step was built from
// context.Background().
func (c *client) handshakeTLS(ctx context.Context, conn net.Conn, target string) (net.Conn, error) {
	config := c.tlsConfig
	if config.ServerName == "" && !config.InsecureSkipVerify {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, fmt.Errorf("transport.server_name is empty and %q is not host:port: %w", target, err)
		}
		config = config.Clone()
		config.ServerName = host
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return tlsConn, nil
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

// dialer builds the dialer for the connections to the server. When a platform
// shell protects sockets from the routes the VPN carries, the hook runs at
// socket-creation time: a tunnel socket captured by the very interface the
// tunnel feeds would be a routing loop.
// validationConfig is the configuration a runtime store judges a write against. It
// is a copy, because the store validates on the admin API's goroutine while a
// reload may be writing the original, and because a runtime entry has to be judged
// against the proxies that are actually running: a reload's selection, not what
// the file held at startup.
func (c *client) validationConfig() *config.Config {
	c.cfgMu.Lock()
	snapshot := *c.cfg
	c.cfgMu.Unlock()
	c.fileMu.Lock()
	snapshot.Proxies = append([]config.ProxyConfig(nil), c.fileProxies...)
	snapshot.Visitors = append([]config.VisitorConfig(nil), c.fileVisitors...)
	c.fileMu.Unlock()
	snapshot.Warnings = nil
	return &snapshot
}

func (c *client) dialer() *net.Dialer {
	d := &net.Dialer{Timeout: time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second}
	if seconds := c.cfg.Client.TCPKeepAliveSeconds; seconds > 0 {
		// Zero keeps the Go default (15 seconds); anything else is how often
		// the kernel probes the server connection for a dead peer, which is
		// what frp's transport.dialServerKeepalive sets.
		d.KeepAlive = time.Duration(seconds) * time.Second
	}
	if address := c.cfg.Client.ConnectServerLocalIP; address != "" {
		// client.connect_server_local_ip picks the source address the server
		// sees, which is frp's connectServerLocalIP. An address that is not on
		// this machine fails the dial with the kernel's own message.
		if ip := net.ParseIP(address); ip != nil {
			d.LocalAddr = &net.TCPAddr{IP: ip}
		}
	}
	if address := c.cfg.Client.DNSServer; address != "" {
		// client.dns_server names the resolver for the server address, which is
		// frp's dnsServer. PreferGo makes the dialer use this resolver instead
		// of the platform's; the lookup itself goes over UDP, which is what a
		// plain resolver answers on.
		d.Resolver = &net.Resolver{
			PreferGo: true,
			// The dial goes through this dialer, so the resolver's own socket gets
			// the same Control hook as the connection it is resolving for. A fresh
			// dialer here left that socket unprotected, and on a platform that
			// protects the tunnel's sockets (mobile RunWithShell) a full-device
			// route table then captured the lookup of the server's own name into
			// the interface that was trying to come up.
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// A copy of this dialer without this resolver: DialContext looks a
				// non-literal address up through d.Resolver, so dialing the configured
				// dns_server by name here - which is the case when it names a host -
				// would re-enter this function without end. The Control hook and the
				// timeouts are carried over, so the lookup's socket is protected like
				// every other one. The clone is taken at dial time, after the hook is
				// installed below.
				inner := *d
				inner.Resolver = nil
				// A lookup goes over UDP. Carrying the local address over would try to
				// bind connect_server_local_ip's *net.TCPAddr to a UDP socket, which the
				// dialer refuses outright: every lookup would fail with "mismatched local
				// address type" and the server could never be resolved.
				inner.LocalAddr = nil
				// The network the exchange asked for, not always udp: a truncated answer
				// makes the resolver retry the query over TCP, and dialing udp again sent
				// the length-prefixed message to a socket that cannot read it.
				if network == "" {
					network = "udp"
				}
				return inner.DialContext(ctx, network, address)
			},
		}
	}
	if c.vpnShellProtect != nil {
		d.Control = func(network, address string, rawConn syscall.RawConn) error {
			_ = rawConn.Control(func(fd uintptr) {
				c.vpnShellProtect(int(fd))
			})
			return nil
		}
	}
	return d
}

// session runs one control connection until it fails or ctx is cancelled.
func (c *client) runSession(ctx context.Context) error {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	// Re-resolving here means a proxy that moved is followed on the next attempt,
	// and a session that is already up keeps running on its established connection.
	c.refreshTarget()
	conn, err := c.dialServer(ctx)
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

	// Captured once: this session authenticates with the cipher it dialed
	// under, whatever a concurrent reload swaps in behind the pointer.
	cipher := c.cipher.Load()
	framer := protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())

	// [oidc] replaces the static token with an access token fetched from the
	// identity provider; a server that verifies [oidc] accepts either.
	c.cfgMu.Lock()
	token := c.cfg.Client.AuthToken
	c.cfgMu.Unlock()
	if c.cfg.OIDC != nil && c.cfg.OIDC.TokenEndpointURL != "" {
		access, err := c.oidcAccessToken(ctx)
		if err != nil {
			return err
		}
		token = access
	}
	// Recorded before the request goes out, so no stream can derive its key
	// from anything but what this session is authenticating with.
	c.cfgMu.Lock()
	c.presentedToken = token
	c.cfgMu.Unlock()

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	request := protocol.AuthRequest{
		Token:         token,
		ClientVersion: Version,
		Protocol:      protocol.ProtocolVersion,
		Encryption:    cipher.Algorithm(),
		ClientID:      c.cfg.Client.ClientID,
		VPN:           c.cfg.VPN.Enabled,
		Metas:         c.cfg.Client.Metas,
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
		// The deadline set above covers the write and the read, so a timeout
		// is this client giving up on a server that said nothing: it never
		// closed anything, and claiming otherwise would send the operator
		// looking for a refusal the server's log does not hold.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("%w (the server did not answer the authentication request within dial_timeout: "+
				"nothing was closed, so the server's log has no refusal to look for)", err)
		}
		// The server closes a connection it will not talk to instead of answering, so the
		// client only sees a reset. It cannot know which rule refused it — a mismatch in
		// [encryption], [transport] or [obfuscation], an access list, a ban — so name the
		// candidates and point at the log that does know, which is the server's.
		return fmt.Errorf("%w (the server accepted the connection and closed it without answering an "+
			"authentication request: the server's log names the reason; usual causes are a mismatch in "+
			"[encryption], [transport] or [obfuscation], or an access rule that refuses this source)", err)
	}
	if !response.OK {
		// IdentityRequired is the server saying that the refusal is about the
		// identity assertion, which a client that never configured one cannot
		// guess from the error text alone — and a server with
		// detailed_errors_to_client = false sends a text that says nothing
		// more. The flag travels in both cases, so it is what this hint is
		// built on.
		if response.IdentityRequired {
			return fmt.Errorf("authentication rejected: %s (the server requires an identity assertion: "+
				"set [identity] enabled = true with a key_file on this client, and ask the server's operator "+
				"to list the client's public key in [identity].allowed_keys)", response.Error)
		}
		return fmt.Errorf("authentication rejected: %s", response.Error)
	}
	c.sessionEstablished.Store(true)
	if response.ProtocolMismatch {
		c.logger.Printf("warning: server speaks protocol %d, this client speaks %d", response.Protocol, protocol.ProtocolVersion)
	}
	if response.Encryption != cipher.Algorithm() {
		return fmt.Errorf("encryption mismatch: server uses %q, this client uses %q", response.Encryption, cipher.Algorithm())
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
	} else if len(response.SessionSalt) > 0 {
		// The server drew a fresh salt for this session and answered inside the
		// channel the static cipher still protects; from here both ends seal
		// under a key derived from it, so the long-lived static key stops
		// sealing every session's records itself. The secret is the token this
		// client presented, which the server just verified.
		c.cfgMu.Lock()
		token := c.presentedToken
		c.cfgMu.Unlock()
		sessionKey, err = crypto.SessionKey(token, response.SessionSalt)
		if err != nil {
			return fmt.Errorf("session key derivation: %w", err)
		}
		controlCipher, err := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, sessionKey)
		if err != nil {
			return fmt.Errorf("session key derivation: %w", err)
		}
		framer = protocol.NewFramerWithOptions(conn, controlCipher, c.framerOptions())
		c.logger.Printf("per-session key %s agreed with the server", crypto.SessionKeyID(sessionKey))
	}
	_ = conn.SetDeadline(time.Time{})

	c.mu.Lock()
	c.session = response.Session
	c.p2pPort = response.P2PPort
	c.sessionKey = sessionKey
	c.mu.Unlock()
	// The session ends when this function returns, whatever the reason, and the
	// admin API reads these two: without clearing them, /api/status kept reporting
	// connected and confirmed through an outage, and forever if the server is gone.
	defer func() {
		c.mu.Lock()
		c.session = ""
		c.registeredNames = nil
		// The post-quantum key and the rendezvous port belong to the session that
		// agreed them: a punch or a late stream goroutine that outlives it must not
		// use the dead session's key or offer its port.
		c.sessionKey = nil
		c.p2pPort = 0
		c.mu.Unlock()
		// The framer goes with the session: setControlFramer's contract is that
		// a nil framer means no session is up, and leaving the dead one in place
		// made every later reload or health check write into a socket that is
		// gone instead of taking the no-session branch.
		c.setControlFramer(nil)
	}()

	if response.P2PPort > 0 {
		c.logger.Printf("the server offers xtcp hole punching on udp port %d", response.P2PPort)
	}

	c.logger.Printf("connected to %s as session %s (server %s)", c.serverAddr(), response.Session, response.ServerVersion)

	c.setControlFramer(framer)
	c.resetHealthWithdrawals()
	// A new session is a new chance to learn what the server supports: the
	// flag records what one server ignored, and this reconnect may be to a
	// different or an upgraded server.
	c.withdrawMu.Lock()
	c.withdrawUnsupported = false
	c.withdrawMu.Unlock()
	// A new session gets a fresh answer about pooling: the server it is talking to
	// may not be the one that refused the last time.
	c.poolRefused.Store(false)
	if err := c.registerProxies(framer); err != nil {
		return err
	}
	// The re-withdrawal waits for the server's ack, which only the control
	// reader below can deliver — this goroutine is about to become that
	// reader, so the withdrawal runs on its own.
	go c.reWithdrawUnhealthy()

	// client.pool_count: keep that many data connections parked at the server, so
	// a visitor's first byte does not wait for a dial. Nothing happens without the
	// key, and a stream still works when a parked connection turns out to be gone.
	if c.cfg.Client.PoolCount > 0 {
		// runPool's own contract is that it returns when the session ends, and its
		// readers have no deadline: with the client-lifetime context a reconnect
		// left the old workers running against the dead session's id, each holding
		// a parked socket until keepalive gave up.
		poolCtx, stopPool := context.WithCancel(ctx)
		defer stopPool()
		go c.runPool(poolCtx, response.Session, c.cfg.Client.PoolCount, cipher)
	}

	// The server gave this session an address on its layer-3 subnet, so the tunnel
	// runs for as long as the session does and stops with it.
	if response.VPNAddress != "" {
		stopTunnel, err := c.startVPN(ctx, framer, response)
		if err != nil {
			return err
		}
		// The tunnel's device-to-server goroutine can be parked inside a
		// control-conn write that the server stopped reading; closing the
		// connection is what unblocks it. Closing before waiting keeps a
		// wedged server from stalling the whole teardown — the deferred
		// conn.Close registered earlier would otherwise only run after
		// stopTunnel returns.
		defer func() {
			_ = conn.Close()
			stopTunnel()
		}()
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
				logging.Warnf(c.logger, "malformed data request: %v", err)
				continue
			}
			go c.serveStream(response.Session, request, cipher)
		case protocol.TypeVPNPacket:
			c.deliverVPN(msg.Payload)
		case protocol.TypeP2PPrepare:
			var prepare protocol.P2PPrepare
			if err := json.Unmarshal(msg.Payload, &prepare); err != nil {
				logging.Warnf(c.logger, "malformed p2p-prepare: %v", err)
				continue
			}
			go c.servePunch(prepare)
		case protocol.TypeProxyList:
			c.deliverProxyList(msg.Payload)
		case protocol.TypeProxyWithdrawAck:
			var withdrawAck protocol.ProxyWithdrawAck
			_ = json.Unmarshal(msg.Payload, &withdrawAck)
			c.deliverWithdrawAck(withdrawAck)
		case protocol.TypeError:
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			c.logger.Printf("server reported: %s", payload.Error)
		default:
			logging.Warnf(c.logger, "ignoring unexpected control frame %s", msg.Type)
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
		logging.Warnf(c.logger, "the server offered tunnel address %s but [vpn] enabled is false; ignoring it",
			response.VPNAddress)
		return func() {}, nil
	}

	mtu := response.VPNMTU
	if c.cfg.VPN.MTU != 0 {
		mtu = c.cfg.VPN.MTU
	}

	mask := response.VPNMask
	if mask == "" {
		mask = "255.255.255.0"
	}
	ip := net.ParseIP(response.VPNAddress).To4()
	m := net.IPMask(net.ParseIP(mask).To4())
	prefix, ones := m.Size()
	if ip == nil || m == nil || ones == 0 {
		return nil, fmt.Errorf("the server's tunnel address %s/%s is not an IPv4 pair", response.VPNAddress, mask)
	}
	subnet := ip.Mask(m).String()

	var device vpn.Device
	var err error
	if c.vpnShellOpen != nil {
		device, err = c.vpnShellOpen(mtu, response.VPNAddress, prefix, subnet)
		if err != nil {
			return nil, fmt.Errorf("open the tunnel interface: %w", err)
		}
	} else {
		device, err = vpn.Open(c.cfg.VPN.Device, mtu)
		if err != nil {
			return nil, fmt.Errorf("open the tunnel interface: %w", err)
		}
		if err := vpn.AssignAddress(device, response.VPNAddress, mask); err != nil {
			_ = device.Close()
			return nil, fmt.Errorf("configure %s with %s: %w", device.Name(), response.VPNAddress, err)
		}
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

// proxySpec builds the registration payload for one configured proxy. The
// visitor filters and, for a socks5 tunnel, the ranges it may dial travel
// with the registration, because both are properties of this client rather
// than of the server's configuration.
// proxySpec is the registration frame for one proxy. The identity the client
// gave in [client].user travels with it: the server hands it to the group, so a
// private proxy without allow_users is reachable by that identity alone.
func proxySpec(proxy config.ProxyConfig, user string) protocol.ProxySpec {
	return protocol.ProxySpec{
		Name:              proxy.Name,
		Type:              proxy.Type,
		LocalAddr:         proxy.LocalAddr(),
		RemotePort:        proxy.RemotePort,
		Domains:           proxy.Domains,
		Locations:         proxy.Locations,
		HostHeaderRewrite: proxy.HostHeaderRewrite,
		ProxyProtocol:     proxy.ProxyProtocol,
		Subdomain:         proxy.Subdomain,
		HTTPUser:          proxy.HTTPUser,
		RouteByHTTPUser:   proxy.RouteByHTTPUser,
		HTTPPassword:      proxy.HTTPPassword,
		Multiplexer:       proxy.Multiplexer,
		UseCompression:    proxy.UseCompression,
		UseEncryption:     proxy.UseEncryption,
		RequestHeaders:    proxy.RequestHeaders,
		ResponseHeaders:   proxy.ResponseHeaders,
		TLSPassthrough:    proxy.TLSPassthrough,
		SecretKey:         proxy.SecretKey,
		AuthMethod:        proxy.AuthMethod,
		Group:             proxy.Group,
		Multipath:         proxy.Multipath,
		AllowCIDRs:        proxy.AllowCIDRs,
		DenyCIDRs:         proxy.DenyCIDRs,
		AllowTargets:      proxy.AllowTargets,
		User:              user,
		AllowUsers:        proxy.AllowUsers,
		Annotations:       proxy.Annotations,
	}
}

// registerProxies publishes every configured tunnel on the current session.
// The list is read once: an empty check and a loop over separate snapshots
// would let a reload that lands in between publish nothing on this session
// while the configuration is not empty at all.
func (c *client) registerProxies(framer *protocol.Framer) error {
	proxies := c.proxyList()
	if len(proxies) == 0 {
		c.logger.Printf("no [[proxies]] configured: the connection is up but publishes nothing")
		return nil
	}
	for _, proxy := range proxies {
		spec := proxySpec(proxy, c.cfg.Client.User)
		c.rememberSpec(spec)
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

// deliverProxyList folds the server's confirmed tunnel list into the client: the
// names become the confirmed set, and a proxy that is back in the list has really
// been published again, so its withdrawal mark goes. The server sends the list
// only after a registration succeeded and never for one it refused, so the list
// is what separates "the registration frame went out" from "the tunnel is up" —
// the distinction a republish after a health recovery depends on.
func (c *client) deliverProxyList(payload []byte) {
	names := c.logProxyList(payload)
	for _, name := range names {
		state := c.healthStateFor(name)
		if state == nil || !state.isWithdrawn() {
			continue
		}
		state.clearWithdrawn()
		c.logger.Printf("proxy %q: the server confirms the tunnel is published again", name)
	}
}

func (c *client) logProxyList(payload []byte) []string {
	var statuses []protocol.ProxyStatus
	if err := json.Unmarshal(payload, &statuses); err != nil {
		logging.Warnf(c.logger, "malformed proxy list: %v", err)
		return nil
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
		parts = append(parts, describePublishedProxy(status))
	}
	c.logger.Printf("server confirms %d tunnel(s): %s", len(parts), strings.Join(parts, ", "))
	return names
}

// describePublishedProxy writes one confirmed tunnel the way the operator can act on
// it: what type it is, the port the server listens on, and how many clients share the
// name. The share count is what tells the operator of a second client that it joined
// the pool that was already published instead of publishing a second endpoint.
func describePublishedProxy(status protocol.ProxyStatus) string {
	parts := make([]string, 0, 4)
	if status.Type != "" {
		parts = append(parts, fmt.Sprintf("%s (%s)", status.Name, status.Type))
	} else {
		parts = append(parts, status.Name)
	}
	if status.RemotePort != 0 {
		parts = append(parts, fmt.Sprintf("on port %d", status.RemotePort))
	}
	if status.GroupMembers > 1 {
		parts = append(parts, fmt.Sprintf("shared by %d clients", status.GroupMembers))
	}
	return strings.Join(parts, " ")
}

// streamOrigin names how a stream reached this client, which is what an operator
// needs when one of the two paths misbehaves: a connection to the published port,
// or a visitor asking for this proxy by name.
func streamOrigin(request protocol.DataRequest) string {
	if request.Visitor {
		return "from a visitor"
	}
	return "from the public port"
}

// framerOptions maps the client's [obfuscation] section onto frame padding and
// jitter. The server unpads from the frame flag alone, so the two ends do not have
// to agree on these values.
func (c *client) framerOptions() protocol.FramerOptions {
	return protocol.FramerOptions{
		MaxPayload: protocol.DefaultMaxPayload,
		PadTo:      c.cfg.Obfuscation.PadToBytes(),
		Jitter:     c.cfg.Obfuscation.Jitter(),
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

// preparedStream is the local half of a stream: the proxy it belongs to and the
// connection to the local service. It is dialled before the server is told
// anything about the stream, so a failure reaches the visitor as a reason instead
// of a timeout.
type preparedStream struct {
	proxy config.ProxyConfig
	// local is nil for a datagram tunnel, which dials its own UDP socket later,
	// and for a socks5 UDP relay, whose datagrams name their own targets.
	local net.Conn
	// bandwidth is the proxy's parsed client-side rate for a datagram tunnel:
	// its UDP socket is dialled after the data-open, outside dialLocalService,
	// and would otherwise bypass the limit the byte-stream paths apply.
	bandwidth int64
}

// prepareStream finds the proxy a request names and dials its local service. It
// does not report anything itself: the caller knows which connection the server is
// waiting on — a fresh one, or the connection the client had parked — and that is
// where the reason belongs.
func (c *client) prepareStream(request protocol.DataRequest) (preparedStream, error) {
	proxy, err := c.findProxy(request.Proxy)
	if err != nil {
		return preparedStream{}, err
	}
	prepared := preparedStream{proxy: proxy}

	if config.IsDatagramProxyType(proxy.Type) || request.SocksUDP {
		// The refusal lives in dialLocalService, but a datagram tunnel is
		// exactly what a health check cannot withdraw for the visitor on the
		// wire, so it is checked here too.
		if !c.proxyHealthy(proxy) {
			return preparedStream{}, errLocalUnhealthy
		}
		// The bandwidth the byte-stream paths wrap into dialLocalService is
		// parsed here for a datagram tunnel, whose UDP socket does not exist
		// yet. A rate the parser refuses was already reported by validation;
		// here it only has to not turn into a limiter nobody can grant.
		if proxy.Bandwidth != "" {
			if bw, bwErr := config.ParseBandwidth(proxy.Bandwidth); bwErr == nil {
				prepared.bandwidth = bw
			}
		}
		return prepared, nil
	}
	local, err := c.dialLocalService(proxy, request.Target)
	if err != nil {
		return preparedStream{}, err
	}
	prepared.local = local
	return prepared, nil
}

// dialLocalService opens the proxy's local service with the wraps every path
// owes it: the health check, and the bandwidth limit. The relayed path and the
// direct path carry the same visitor traffic, so both go through here.
func (c *client) dialLocalService(proxy config.ProxyConfig, target string) (net.Conn, error) {
	if !c.proxyHealthy(proxy) {
		return nil, errLocalUnhealthy
	}
	local, err := c.dialForProxy(proxy, target)
	if err != nil {
		return nil, err
	}
	if proxy.Bandwidth != "" {
		if bw, bwErr := config.ParseBandwidth(proxy.Bandwidth); bwErr == nil {
			local = newLimitedConn(bw, local)
		}
	}
	return local, nil
}

// closePrepared releases the local connection of a stream that did not reach the
// point where it is served. The path that serves it owns the connection instead.
func (c *client) closePrepared(p preparedStream) {
	if p.local != nil {
		_ = p.local.Close()
	}
}

// serveStream opens a second connection to the server for one visiting connection
// and forwards it to the local service.
//
// cipher is the static cipher this session authenticated under, captured by
// runSession: the server seals a data connection's first frame with the cipher it
// was built with, so a reload that rebuilt the client's key must not reach a
// session that is already up.
func (c *client) serveStream(session string, request protocol.DataRequest, cipher *crypto.Cipher) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second

	prepared, err := c.prepareStream(request)
	if err != nil {
		// The visitor is waiting for an answer, and this is the only side that
		// knows the local service could not be reached, so the reason is sent.
		c.reportStreamFailure(session, request.Proxy, request.StreamID, err, cipher)
		return
	}

	conn, err := c.dialServer(c.baseCtx)
	if err != nil {
		c.closePrepared(prepared)
		logging.Warnf(c.logger, "stream for %q: cannot reach the server: %v", request.Proxy, err)
		return
	}

	fail := func(reason string) {
		c.closePrepared(prepared)
		c.logger.Printf("stream for %q (%s): %s", request.Proxy, streamOrigin(request), reason)
		_ = conn.Close()
	}

	framer := protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())
	if err := c.openStreamOn(conn, framer, session, request, dialTimeout); err != nil {
		fail(err.Error())
		return
	}

	c.afterDataOpen(prepared, request, conn, framer, false, cipher)
}

// serveParkedStream serves a stream on a connection the client had already parked
// at the server: the dial and the acknowledgement happened when it was parked, so
// only the local service is still opened here. The connection carries this one
// stream and is closed with it.
func (c *client) serveParkedStream(session string, request protocol.DataRequest, conn net.Conn, framer *protocol.Framer, cipher *crypto.Cipher) {
	prepared, err := c.prepareStream(request)
	if err != nil {
		// The server is waiting on this connection for the stream, so the reason is
		// written here rather than on a fresh connection: the parked connection
		// carries the answer, and no second dial is needed.
		c.reportStreamFailureOn(conn, framer, session, request.Proxy, request.StreamID, err)
		_ = conn.Close()
		return
	}
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	if err := c.openStreamOn(conn, framer, session, request, dialTimeout); err != nil {
		// The acknowledgement is the server's, and it did not arrive or did not
		// accept: the connection has nothing left to carry.
		c.logger.Printf("stream for %q (from a connection parked in advance): %v", request.Proxy, err)
		c.closePrepared(prepared)
		_ = conn.Close()
		return
	}
	c.afterDataOpen(prepared, request, conn, framer, true, cipher)
}

// openStreamOn asks the server to pair this stream over a connection that is
// already open — a fresh one, or one the client parked in advance — and waits for
// the acknowledgement. The deadline is the dial timeout, because a server that
// does not answer at all has to be given up on.
func (c *client) openStreamOn(conn net.Conn, framer *protocol.Framer, session string, request protocol.DataRequest, timeout time.Duration) error {
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    request.Proxy,
		StreamID: request.StreamID,
	}); err != nil {
		return fmt.Errorf("cannot send data-open: %w", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		return fmt.Errorf("cannot read data-open ack: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("server refused the stream: %s", ack.Error)
	}
	return nil
}

// afterDataOpen carries one stream once both ends have agreed to it: it derives
// the stream's own key when the session has one, and hands the bytes to the local
// service, the datagram pump or the socks5 UDP relay.
//
// cipher is the session's static cipher, which is what the server's own data
// connection falls back to when the session agreed no key.
func (c *client) afterDataOpen(prepared preparedStream, request protocol.DataRequest, conn net.Conn, framer *protocol.Framer, parked bool, cipher *crypto.Cipher) {
	defer conn.Close()
	if prepared.local != nil {
		defer prepared.local.Close()
	}
	proxy := prepared.proxy
	local := prepared.local

	origin := streamOrigin(request)
	if parked {
		origin += ", from a connection parked in advance"
	}

	// With a post-quantum session both ends switch to a key derived from the
	// session key and this stream's identifier, so no two streams share a
	// keystream. Without one, the session's static cipher applies — not the
	// client's current one, which a reload may have rebuilt.
	streamCipher := cipher
	if key := c.sessionKeyCopy(); len(key) > 0 {
		streamKey, keyErr := crypto.StreamKey(key, request.StreamID)
		if cipher, cipherErr := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, streamKey); keyErr == nil && cipherErr == nil {
			streamCipher = cipher
			framer = protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())
		} else {
			logging.Warnf(c.logger, "stream for %q: cannot derive a stream key: %v%v", request.Proxy, keyErr, cipherErr)
		}
	}

	if config.IsDatagramProxyType(proxy.Type) {
		c.serveDatagrams(request.Proxy, conn, framer, proxy.LocalAddr(), prepared.bandwidth)
		return
	}

	if request.SocksUDP {
		c.serveSocksUDP(proxy, framer)
		return
	}

	// The server echoed the proxy's use_encryption, and the session has no cipher
	// of its own: this proxy's byte stream gets its own layer, whose key both ends
	// derive from the shared secret and the proxy's name. A session that already
	// agreed a cipher keeps it, so the flag changes nothing there. Datagram and
	// socks5-UDP streams carry frames rather than a byte stream and are left
	// alone; validation says so.
	if request.Encrypted && streamCipher == nil {
		c.cfgMu.Lock()
		token := c.presentedToken
		c.cfgMu.Unlock()
		proxyCipher, cipherErr := crypto.ProxyStreamCipher(token, request.Proxy)
		if cipherErr != nil {
			logging.Warnf(c.logger, "stream for %q: cannot derive the per-proxy key: %v", request.Proxy, cipherErr)
		} else {
			streamCipher = proxyCipher
		}
	}

	var serverSide net.Conn = &cryptoStreamConn{Stream: crypto.NewStream(conn, streamCipher), conn: conn}
	if request.Compressed {
		// The server echoed the proxy's use_compression; the wrap goes outside
		// the encryption layer, where the payload is still plaintext.
		serverSide = flynet.CompressConn(serverSide)
	}
	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toServer, fromServer := flynet.Pipe(local, serverSide, idle)
	c.logger.Printf("stream for %q (%s) finished (sent %d bytes to the server, received %d)",
		request.Proxy, origin, toServer, fromServer)
}

// dialForProxy opens the connection a stream should carry. A socks5 tunnel is
// dialled at the address the visitor asked for, checked against the ranges its
// configuration allows; every other type is dialled at its configured local
// service.
func (c *client) dialForProxy(proxy config.ProxyConfig, target string) (net.Conn, error) {
	if proxy.Plugin != "" {
		return c.dialForPlugin(proxy)
	}
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
func (c *client) reportStreamFailure(session, proxy, streamID string, cause error, cipher *crypto.Cipher) {
	c.logger.Printf("stream for %q: %v", proxy, cause)

	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	conn, err := c.dialServer(c.baseCtx)
	if err != nil {
		logging.Warnf(c.logger, "stream for %q: cannot report the failure to the server: %v", proxy, err)
		return
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	framer := protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    proxy,
		StreamID: streamID,
		Error:    cause.Error(),
	}); err != nil {
		logging.Warnf(c.logger, "stream for %q: cannot report the failure: %v", proxy, err)
		return
	}

	var ack protocol.DataOpenAck
	_ = framer.ReadJSON(protocol.TypeDataOpenAck, &ack)
}

// reportStreamFailureOn reports a stream the client cannot serve on the
// connection the server is waiting on — a connection the client had parked — so
// the visitor learns the reason without another dial.
func (c *client) reportStreamFailureOn(conn net.Conn, framer *protocol.Framer, session, proxy, streamID string, cause error) {
	c.logger.Printf("stream for %q (from a connection parked in advance): %v", proxy, cause)
	// The session is what the server looks the pending stream up by. Without it
	// the report is refused as coming from an unknown session and the visitor
	// waits out the server's whole stream window for an answer this connection
	// was already carrying.
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    proxy,
		StreamID: streamID,
		Error:    cause.Error(),
	}); err != nil {
		logging.Warnf(c.logger, "stream for %q: cannot report the failure: %v", proxy, err)
	}
}

// serveDatagrams relays a datagram stream: each TypeUDPPacket frame carries one
// datagram for the local UDP service, and each reply becomes one frame.
//
// The service is dialled as a connected UDP socket, so a reply is only accepted
// from the address the requests were sent to — which is what a local service
// does. The frames are already sealed individually by the framer, so no record
// layer is layered on top. The bandwidth, when the proxy configures one, wraps
// the local socket like dialLocalService wraps a byte-stream's connection, so
// the relayed datagrams pay the same client-side rate every other path pays.
func (c *client) serveDatagrams(proxy string, conn net.Conn, framer *protocol.Framer, localAddr string, bandwidth int64) {
	defer conn.Close()

	local, err := net.Dial("udp", localAddr)
	if err != nil {
		logging.Warnf(c.logger, "stream for %q: cannot reach the local service %s: %v", proxy, localAddr, err)
		return
	}
	if bandwidth > 0 {
		local = newLimitedConn(bandwidth, local)
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
	for _, proxy := range c.proxyList() {
		if proxy.Name == name {
			return proxy, nil
		}
	}
	return config.ProxyConfig{}, errors.New("not in this client's configuration")
}

// serveSocksUDP relays one socks5 UDP ASSOCIATE stream. Each TypeUDPPacket frame
// carries a whole SOCKS5 UDP datagram, whose header names the target; this client
// dials the target, sends the data and wraps every reply back in the same header.
// It runs until the server closes the data connection, which happens when the
// visitor's association ends.
func (c *client) serveSocksUDP(proxy config.ProxyConfig, framer *protocol.Framer) {
	policy, err := socks.NewTargetPolicy(proxy.AllowTargets)
	if err != nil {
		c.logger.Printf("stream for %q: socks5 udp: %v", proxy.Name, err)
		return
	}

	var bandwidth int64
	if proxy.Bandwidth != "" {
		// A rate the parser refuses was already reported by validation; here it
		// only has to not turn into a limiter nobody can grant.
		if bw, err := config.ParseBandwidth(proxy.Bandwidth); err == nil {
			bandwidth = bw
		}
	}
	relay := &socksUDPRelay{
		policy:      policy,
		framer:      framer,
		logger:      c.logger,
		name:        proxy.Name,
		idleTimeout: time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second,
		dialTimeout: time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second,
		limiter:     newBandwidthLimiter(bandwidth),
		maxSockets:  maxSocksUDPTargets,
		sockets:     make(map[string]*socksUDPTarget),
	}
	defer relay.close()
	relay.run()
}

// run reads wrapped datagrams from the server until the connection ends, dialling
// each target and forwarding the data.
func (r *socksUDPRelay) run() {
	for {
		msg, err := r.framer.ReadFrame()
		if err != nil {
			return
		}
		if msg.Type != protocol.TypeUDPPacket {
			continue
		}
		target, data, err := socks.ParseUDPDatagram(msg.Payload)
		if err != nil {
			logging.Warnf(r.logger, "stream for %q: dropping a malformed socks5 udp datagram: %v", r.name, err)
			continue
		}
		socket, err := r.socketFor(target)
		if err != nil {
			logging.Warnf(r.logger, "stream for %q: cannot reach %s: %v", r.name, target, err)
			continue
		}
		if _, err := socket.conn.Write(data); err != nil {
			r.logger.Printf("stream for %q: writing to %s: %v", r.name, target, err)
			r.drop(socket)
		}
	}
}

// socksUDPTarget is one connected UDP socket the relay keeps for a target.
type socksUDPTarget struct {
	conn     net.Conn
	target   string
	lastUsed uint64 // monotonic use counter of the most recent use; guarded by r.mu
}

// maxSocksUDPTargets bounds how many distinct targets one socks5 UDP association
// may hold a socket for. A visitor that probes many addresses otherwise gets one
// file descriptor per probe, and can exhaust the client's whole descriptor table.
const maxSocksUDPTargets = 256

// socksUDPRelay keeps the per-target UDP sockets of one socks5 UDP association.
// Each socket has its own reply reader, so replies from several targets can be
// interleaved without waiting on each other.
type socksUDPRelay struct {
	policy      *socks.TargetPolicy
	framer      *protocol.Framer
	logger      *log.Logger
	name        string
	idleTimeout time.Duration
	dialTimeout time.Duration
	// limiter is the one token bucket every target socket of this association
	// draws from, built from the proxy's bandwidth setting; nil leaves it uncapped.
	// A socks5 UDP association used to be the one path around the setting, because
	// it never went through dialLocalService, which is the only place the stream
	// paths apply the cap. One bucket per target socket would still have let a
	// visitor reach that rate once per address it probed, so the whole association
	// shares a single bucket.
	limiter *rate.Limiter
	// maxSockets bounds the per-target socket cache; zero selects
	// maxSocksUDPTargets. It is a field so a test can drive a small cap.
	maxSockets int

	// useSeq numbers every use of a target socket. lastUsed compares these, so
	// eviction is a total order even where the platform's clock would tie.
	useSeq uint64

	mu      sync.Mutex
	sockets map[string]*socksUDPTarget
}

// socketFor returns the UDP socket for target, dialling and caching it the first
// time. The target is the header's address exactly as the visitor named it, so
// two datagrams to the same target share a socket and its reply reader.
func (r *socksUDPRelay) socketFor(target string) (*socksUDPTarget, error) {
	r.mu.Lock()
	r.useSeq++
	if entry, ok := r.sockets[target]; ok {
		entry.lastUsed = r.useSeq
		r.mu.Unlock()
		return entry, nil
	}
	r.mu.Unlock()

	// The dial resolves the target and can take the whole dial timeout; holding
	// the cache lock across it stalls every other datagram of this association,
	// including the ones whose targets are already cached.
	conn, err := r.policy.DialUDP(target, r.dialTimeout)
	if err != nil {
		return nil, err
	}
	// Both directions of this target's traffic go through the wrapper below: the
	// relay writes with entry.conn and its reply reader reads with it, so the
	// association's bucket is charged once for the pair. It is shared with every
	// other target socket of this association.
	conn = newSharedLimitedConn(r.limiter, conn)

	r.mu.Lock()
	r.useSeq++
	if entry, ok := r.sockets[target]; ok {
		// Another datagram for this target won the race while the socket was being
		// dialled; keep the one already serving replies.
		entry.lastUsed = r.useSeq
		r.mu.Unlock()
		_ = conn.Close()
		return entry, nil
	}
	entry := &socksUDPTarget{conn: conn, target: target, lastUsed: r.useSeq}
	r.sockets[target] = entry
	r.evictLocked()
	r.mu.Unlock()

	go r.readReplies(entry)
	return entry, nil
}

// evictLocked closes the least-recently-used socket once the cache outgrows its
// cap. It must be called with r.mu held; closing a socket unblocks its reader,
// which then removes itself on the next mutex turn.
func (r *socksUDPRelay) evictLocked() {
	cap := r.maxSockets
	if cap == 0 {
		cap = maxSocksUDPTargets
	}
	if len(r.sockets) <= cap {
		return
	}
	var oldest *socksUDPTarget
	for _, entry := range r.sockets {
		if oldest == nil || entry.lastUsed < oldest.lastUsed {
			oldest = entry
		}
	}
	if oldest != nil {
		delete(r.sockets, oldest.target)
		_ = oldest.conn.Close()
	}
}

// readReplies forwards everything the target sends back, wrapped in the SOCKS5
// UDP header that names it as the source.
func (r *socksUDPRelay) readReplies(entry *socksUDPTarget) {
	buf := make([]byte, 65535)
	for {
		if r.idleTimeout > 0 {
			_ = entry.conn.SetReadDeadline(time.Now().Add(r.idleTimeout))
		}
		n, err := entry.conn.Read(buf)
		if err != nil {
			r.drop(entry)
			return
		}
		wrapped, err := socks.WrapUDPDatagram(entry.target, buf[:n])
		if err != nil {
			r.logger.Printf("stream for %q: wrapping a reply from %s: %v", r.name, entry.target, err)
			continue
		}
		if err := r.framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: wrapped}); err != nil {
			r.drop(entry)
			return
		}
	}
}

// drop closes and forgets one target's socket. It takes the entry the caller
// was using, not the target name: a reader that was evicted while blocked in
// Read must not close the socket that replaced it for the same target.
func (r *socksUDPRelay) drop(entry *socksUDPTarget) {
	r.mu.Lock()
	cur, ok := r.sockets[entry.target]
	if ok && cur != entry {
		// The entry this caller held is already gone; whatever sits in the
		// map now belongs to a newer reader.
		r.mu.Unlock()
		return
	}
	if ok {
		delete(r.sockets, entry.target)
	}
	r.mu.Unlock()
	if ok {
		_ = entry.conn.Close()
	}
}

// close closes every socket the relay still holds, which unblocks their readers.
func (r *socksUDPRelay) close() {
	r.mu.Lock()
	entries := make([]*socksUDPTarget, 0, len(r.sockets))
	for _, entry := range r.sockets {
		entries = append(entries, entry)
	}
	r.sockets = make(map[string]*socksUDPTarget)
	r.mu.Unlock()
	for _, entry := range entries {
		_ = entry.conn.Close()
	}
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

// CloseWrite half-closes the underlying connection and leaves the read side open, which is
// what a half-closed stream needs: flynet.Pipe finishes each direction with CloseWrite when
// the type has one and closes the whole connection otherwise, so a wrapper without this
// method turns every half-close into a full close and cuts off the reply that a service
// only sends once it has seen the end of the request.
func (c *cryptoStreamConn) CloseWrite() error {
	if hc, ok := c.conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.conn.Close()
}
