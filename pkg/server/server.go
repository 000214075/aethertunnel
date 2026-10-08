package server

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/oidc"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Options carries the build metadata the server reports to clients, to the
// dashboard and on its own startup line.
type Options struct {
	Version   string
	BuildTime string
	GitCommit string
	Logger    *log.Logger
	// VPNOpen opens the layer-3 interface. nil uses the operating system's own tun
	// support. It is here so a test can run the layer-3 path on a machine that
	// cannot provide an interface.
	VPNOpen DeviceOpener
}

// Server is the AetherTunnel server.
//
// One listening socket carries both control connections (a client authenticates
// and keeps the session alive) and data connections (a client dials back to carry
// one stream of a published tunnel). Keeping a single port means a firewall only
// needs one rule, and it removes the v1 bug where the same address was bound twice
// and the second bind aborted startup.
type Server struct {
	cfg    *config.Config
	cipher *crypto.Cipher
	logger *log.Logger

	// remotePorts is [server].allow_ports in parsed form; nil allows every port.
	remotePorts *config.PortSet

	version   string
	buildTime string
	gitCommit string

	startedAt time.Time

	sessions    *SessionManager
	tunnels     *TunnelManager
	metrics     *Metrics
	acl         *AccessControl
	bans        *banList
	auditor     *Auditor
	ledger      *ledgerStore
	vhost       *vhostSet
	tcpmux      *tcpmuxSet
	sni         *sniSet
	httpPlugins *httpPluginManager
	oidc        *oidc.Verifier
	p2p         *p2pRendezvous
	directory   *directory
	vpn         *vpnService
	sshGateway  *sshTunnelGateway

	tlsConfig  *tls.Config
	identities []ed25519.PublicKey
	nonces     *crypto.NonceCache

	// listener is assigned by Run when it binds, and read by the dashboard's
	// readiness probe and by Shutdown, so it is guarded rather than written once.
	listenerMu sync.Mutex
	listener   net.Listener

	totalConnections atomic.Int64

	closing atomic.Bool
	wg      sync.WaitGroup

	// shutdownDone is closed by Shutdown once it has run to the end. Run's accept
	// loop waits for it before closing the stores: the SSH gateway's handlers are
	// waited for by Shutdown, not by wg, so without this signal that loop could
	// close the ledger while a gateway handler was still writing its session's
	// entry to it.
	shutdownDone chan struct{}
}

// New builds a server from a validated configuration.
func New(cfg *config.Config, opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	cipher, err := cfg.Cipher(config.RoleServer)
	if err != nil {
		return nil, err
	}

	// [oidc] is wired at startup: discovery needs the issuer reachable, and a
	// verifier that cannot come up should stop the server now instead of
	// refusing every oidc login later.
	var oidcVerifier *oidc.Verifier
	if cfg.OIDC != nil {
		timeout := time.Duration(cfg.OIDC.TimeoutSecs) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		options := []oidc.Option{oidc.WithClient(&http.Client{Timeout: timeout})}
		if cfg.OIDC.JWKSURL != "" {
			options = append(options, oidc.WithJWKSURL(cfg.OIDC.JWKSURL))
		}
		if cfg.OIDC.SkipIssuerCheck {
			options = append(options, oidc.WithSkipIssuer())
		}
		if cfg.OIDC.SkipExpiryCheck {
			options = append(options, oidc.WithSkipExpiry())
		}
		verifier, err := oidc.NewVerifier(context.Background(), cfg.OIDC.Issuer, cfg.OIDC.Audience, options...)
		if err != nil {
			return nil, err
		}
		oidcVerifier = verifier
	}
	acl, err := NewAccessControl(cfg.Server.AllowCIDRs, cfg.Server.DenyCIDRs,
		cfg.Server.RateLimitPerSecond, cfg.Server.RateLimitBurst)
	if err != nil {
		return nil, err
	}
	bans, err := newBanList(cfg.Server.BanAfterFailures,
		time.Duration(cfg.Server.BanSeconds)*time.Second,
		time.Duration(cfg.Server.BanMaxSeconds)*time.Second,
		cfg.Server.BanIgnoreCIDRs)
	if err != nil {
		return nil, err
	}
	auditor, err := NewAuditorWithRetention(cfg.Audit.Enabled, cfg.Audit.Path, cfg.Audit.MaxBytes, cfg.Audit.Keep, logger)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	bandwidth, err := openLedger(cfg, logger)
	if err != nil {
		_ = auditor.Close()
		return nil, err
	}
	dir, err := openDirectory(cfg, logger)
	if err != nil {
		_ = bandwidth.Close()
		_ = auditor.Close()
		return nil, err
	}
	tunnels, err := openVPN(cfg, logger, opts.VPNOpen)
	if err != nil {
		_ = dir.Close()
		_ = bandwidth.Close()
		_ = auditor.Close()
		return nil, err
	}
	vpnService := tunnels
	// Every failure past openVPN has to undo the stores the lines above
	// opened, matching the cleanup chain they already implement.
	closeOpened := func(err error) (*Server, error) {
		_ = vpnService.Close()
		_ = dir.Close()
		_ = bandwidth.Close()
		_ = auditor.Close()
		return nil, err
	}
	tlsConfig, err := cfg.ServerTLSConfig()
	if err != nil {
		return closeOpened(err)
	}
	identities, err := cfg.AllowedIdentities()
	if err != nil {
		return closeOpened(err)
	}

	remotePorts, err := config.ParsePortRanges(cfg.Server.AllowPorts)
	if err != nil {
		return closeOpened(err)
	}

	s := &Server{
		cfg:         cfg,
		cipher:      cipher,
		logger:      logger,
		version:     opts.Version,
		buildTime:   opts.BuildTime,
		gitCommit:   opts.GitCommit,
		startedAt:   time.Now(),
		metrics:     newMetrics().withAudit(auditor),
		acl:         acl,
		bans:        bans,
		auditor:     auditor,
		ledger:      bandwidth,
		tlsConfig:   tlsConfig,
		identities:  identities,
		remotePorts: remotePorts,
		nonces:      crypto.NewNonceCache(0),
		directory:   dir,
		vpn:         vpnService,

		shutdownDone: make(chan struct{}),
	}
	sessions := newSessionManager(cfg.Server.MaxConnections)
	s.sessions = sessions
	s.tunnels = newTunnelManager(cfg, logger, cipher, sessions, s.metrics, auditor, remotePorts)
	s.tunnels.directory = dir

	s.vhost = newVhostSet(cfg, logger, s.metrics)
	s.tunnels.vhost = s.vhost
	s.tcpmux = newTCPMuxSet(cfg, logger)
	s.tunnels.tcpmux = s.tcpmux
	s.sni = newSNISet(cfg, logger)
	s.tunnels.sni = s.sni
	s.httpPlugins = newHTTPPluginManager(cfg, logger)
	s.tunnels.httpPlugins = s.httpPlugins
	s.oidc = oidcVerifier
	if cfg.Server.P2PPort > 0 {
		s.p2p = newP2PRendezvous(logger)
		s.tunnels.p2p = s.p2p
	}
	return s, nil
}

// Metrics exposes the counter set for the dashboard endpoint.
func (s *Server) Metrics() *Metrics { return s.metrics }

// Auditor exposes the audit log for call sites outside the accept path.
func (s *Server) Auditor() *Auditor { return s.auditor }

// framerOptions maps the [obfuscation] section onto frame-level padding and
// jitter. The receiver unpads from the frame flag alone, so the two ends do not
// have to agree on these values.
func (s *Server) framerOptions() protocol.FramerOptions {
	return protocol.FramerOptions{
		MaxPayload: protocol.DefaultMaxPayload,
		PadTo:      s.cfg.Obfuscation.PadToBytes(),
		Jitter:     s.cfg.Obfuscation.Jitter(),
	}
}

// Listener returns the bound listener, or nil before Run has bound one.
func (s *Server) Listener() net.Listener {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	return s.listener
}

// setListener records the bound listener.
func (s *Server) setListener(listener net.Listener) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	s.listener = listener
}

// Cipher reports the encryption algorithm in use ("none" when disabled).
func (s *Server) Cipher() string { return s.cipher.Algorithm() }

// disguisedListener wraps what it accepts: TLS first, then the obfuscation wrapper.
//
// The order matters. The disguise has to be the outermost layer, because that is
// what an observer sees; wrapping inside TLS would put record headers inside a TLS
// session, where they are not visible.
type disguisedListener struct {
	net.Listener
	tlsConfig *tls.Config
	disguise  string
}

func (l *disguisedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	wrapped := conn
	if l.disguise != "" && l.disguise != obfs.DisguiseNone {
		// The disguise goes on first so it is the outermost layer, which is what an
		// observer sees; the TLS session then rides inside it. Wrapping the disguise
		// around a *tls.Conn instead put its record headers inside the TLS session,
		// where nothing on the wire showed them.
		disguised, err := obfs.Wrap(wrapped, l.disguise, obfs.Listener)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		wrapped = disguised
	}
	if l.tlsConfig != nil {
		wrapped = tls.Server(wrapped, l.tlsConfig)
	}
	return wrapped, nil
}

// Run listens and serves until the listener is closed or ctx is cancelled.
//
// A Run that returns an error releases what New opened — the ledger, the DHT node,
// the vpn device and the audit log — because Shutdown never runs on that path. The
// Server is therefore not reusable after a failed Run.
func (s *Server) Run(ctx context.Context) error {
	// Every early return below unwinds only the listeners it started; this closes
	// the stores as well, so a failed start does not leave the ledger, the DHT node,
	// the vpn device and the audit log open with nobody to close them. The flag is
	// set once the accept loop owns the shutdown: that loop waits for the handlers
	// and calls closeStores itself, after the last ledger entry and DHT withdrawal.
	serving := false
	defer func() {
		if !serving {
			s.closeStores()
		}
	}()
	listen := net.ListenConfig{}
	if seconds := s.cfg.Server.TCPKeepAliveSeconds; seconds > 0 {
		// Zero keeps the Go default (15 seconds); anything else is the
		// operator's probe interval for every client connection, which is
		// what frp's transport.tcpKeepalive sets.
		listen.KeepAlive = time.Duration(seconds) * time.Second
	}
	listener, err := listen.Listen(ctx, "tcp", s.cfg.ListenAddr())
	if err != nil {
		return flynet.ListenError(s.cfg.ListenAddr(), err)
	}
	if s.tlsConfig != nil || s.cfg.ObfuscationDisguise() != obfs.DisguiseNone {
		listener = &disguisedListener{
			Listener:  listener,
			tlsConfig: s.tlsConfig,
			disguise:  s.cfg.ObfuscationDisguise(),
		}
	}
	s.setListener(listener)

	s.logger.Printf("AetherTunnel server %s (protocol %d) listening on %s", s.version, protocol.ProtocolVersion, listener.Addr())
	s.logger.Printf("encryption: %s", s.Cipher())
	if s.cfg.PostQuantum() {
		s.logger.Printf("post-quantum key agreement: X25519 with ML-KEM-768, one key per data connection")
	}
	if s.tlsConfig != nil {
		s.logger.Printf("the control port is wrapped in TLS")
	}
	if s.cfg.ObfuscationDisguise() != obfs.DisguiseNone {
		if s.cfg.ObfuscationDisguise() == obfs.DisguiseTLSSession {
			s.logger.Printf("connection disguise: %s (a real TLS handshake with a fresh self-signed certificate per connection: real encryption, an anonymous server; identity is the identity layer's job)",
				s.cfg.ObfuscationDisguise())
		} else {
			s.logger.Printf("connection disguise: %s (nothing is encrypted by it; see [encryption] and [transport])",
				s.cfg.ObfuscationDisguise())
		}
	}
	if s.cfg.Identity.Enabled {
		s.logger.Printf("client identities: %d allowed key(s), require_identity=%v",
			len(s.identities), s.cfg.Identity.RequireIdentity)
	}
	s.logger.Printf("max connections: %d, heartbeat: %ds, idle timeout: %ds",
		s.cfg.Server.MaxConnections, s.cfg.Server.HeartbeatSeconds, s.cfg.Server.ReadTimeoutSecs)

	if s.tcpmux != nil {
		if err := s.tcpmux.start(); err != nil {
			_ = listener.Close()
			return err
		}
	}
	if s.sni != nil {
		if err := s.sni.start(); err != nil {
			if s.tcpmux != nil {
				s.tcpmux.stop()
			}
			_ = listener.Close()
			return err
		}
	}
	if err := s.vhost.start(); err != nil {
		if s.sni != nil {
			s.sni.stop()
		}
		if s.tcpmux != nil {
			s.tcpmux.stop()
		}
		_ = listener.Close()
		return err
	}
	if s.p2p != nil {
		if err := s.p2p.Start(s.cfg.P2PAddr()); err != nil {
			s.vhost.stop()
			if s.sni != nil {
				s.sni.stop()
			}
			if s.tcpmux != nil {
				s.tcpmux.stop()
			}
			_ = listener.Close()
			return err
		}
	}

	if err := s.startSSHGateway(); err != nil {
		s.vhost.stop()
		if s.sni != nil {
			s.sni.stop()
		}
		if s.tcpmux != nil {
			s.tcpmux.stop()
		}
		if s.p2p != nil {
			_ = s.p2p.Close()
		}
		_ = listener.Close()
		return err
	}

	// Both goroutines below take the derived context, not the caller's: Run
	// also ends when the listener is closed or Shutdown was called, and a
	// caller that stops the server that way never cancels its own context, so
	// goroutines parked on it would outlive Run for good. Every return path
	// cancels the derived one; the watcher still answers the caller's context
	// while Run is serving.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go s.watchCallerContext(ctx, runCtx)

	// The ban collector starts here rather than before p2p and the SSH
	// gateway: those two can fail and return, and a collector launched before
	// them would keep ticking for the life of the context after a failed Run
	// — a goroutine the failed Run promised not to leave behind. Nothing
	// consults the ban list before the server is serving.
	if s.bans.enabled() {
		go s.collectBans(runCtx)
	}

	serving = true
	// acceptErrors counts the accept failures of the current burst, and
	// lastAcceptSummary is when the burst last earned its one-line summary.
	var acceptErrors int
	var lastAcceptSummary time.Time
	for {
		conn, err := listener.Accept()
		if err != nil {
			if s.closing.Load() {
				// Every connection handler runs its teardown here, which is where
				// the last ledger entries and DHT withdrawals happen, so the
				// stores are only closed once they have all finished.
				s.wg.Wait()
				// The handlers waited for above are the control connections. The
				// SSH gateway's handlers are in the gateway's own WaitGroup, and
				// Shutdown waits for those as its last step but one; waiting for
				// Shutdown itself here is what keeps closeStores behind every
				// teardown, the gateway's ledger entries and audit records
				// included. There is no cycle: Shutdown never waits for this
				// loop, this loop waits for Shutdown, and a gateway handler waits
				// for neither. closing is set only by Shutdown, so this branch
				// cannot be reached with nothing left to close the channel.
				<-s.shutdownDone
				s.closeStores()
				return nil
			}
			// A listener that keeps failing — fd exhaustion above all — would
			// otherwise spin here ten times a second and bury the log. The
			// first failures are logged one by one, then a once-a-minute
			// summary carries the total; a successful accept starts a fresh
			// burst.
			acceptErrors++
			if acceptErrors <= acceptErrorLogLimit {
				s.logger.Printf("accept error: %v", err)
			} else if lastAcceptSummary.IsZero() || time.Since(lastAcceptSummary) >= time.Minute {
				lastAcceptSummary = time.Now()
				s.logger.Printf("accept error: %d failure(s) so far: %v", acceptErrors, err)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		acceptErrors = 0
		lastAcceptSummary = time.Time{}
		s.totalConnections.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// handshakeFailureHint names the settings that produce a given first-frame failure, for
// the log line that is the only record of it: the client is closed out without an answer,
// so the server's log is where the reason lives.
func handshakeFailureHint(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if strings.Contains(text, "message authentication failed") ||
		strings.Contains(text, "encryption mismatch") {
		return " (check that both ends agree on [encryption] algorithm, passphrase, salt and post_quantum)"
	}
	return ""
}

// admit applies the access-control rules to a freshly accepted connection. It
// returns false when the connection has already been closed.
func (s *Server) admit(conn net.Conn) bool {
	if banned, remaining := s.bans.blocked(remoteIP(conn)); banned {
		s.metrics.banRefused.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventBanRefused, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: fmt.Sprintf("the source is banned for another %s", remaining.Round(time.Second)),
		})
		logging.Warnf(s.logger, "connection from %s refused: the source is banned for another %s",
			conn.RemoteAddr(), remaining.Round(time.Second))
		_ = conn.Close()
		return false
	}

	switch s.acl.Check(conn) {
	case DenyCIDR:
		s.metrics.aclDenied.Add(1)
		// The refusal counters overlap on purpose: one answer is "how many were
		// turned away before the handshake at all", the other is "why".
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventACLDenied, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "source address rejected by allow/deny lists",
		})
		logging.Warnf(s.logger, "connection from %s denied by access control", conn.RemoteAddr())
		_ = conn.Close()
		return false
	case DenyRate:
		s.metrics.rateLimited.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventRateLimited, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "source address exceeded its connection rate",
		})
		logging.Warnf(s.logger, "connection from %s denied by rate limit", conn.RemoteAddr())
		_ = conn.Close()
		return false
	default:
		return true
	}
}

// watchCallerContext shuts the server down when the caller's context ends. It
// also exits when the derived run context is cancelled, which is what every
// return path of Run does: a server stopped through Shutdown or a closed
// listener must not leave this watcher hanging on a context nobody cancels.
func (s *Server) watchCallerContext(callerCtx, runCtx context.Context) {
	select {
	case <-callerCtx.Done():
		s.Shutdown("server shutting down")
	case <-runCtx.Done():
	}
}

// collectBans drops ban entries that have expired and carry no failures, so a
// long-running server does not keep one entry per source that ever failed.
func (s *Server) collectBans(ctx context.Context) {
	interval := time.Duration(s.cfg.Server.BanSeconds) * time.Second
	if interval < time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.bans.collect()
		}
	}
}

// recordAuthFailure books one failed authentication and bans the source when it
// has failed often enough. It is the only place a ban is imposed, so every way of
// failing to authenticate counts.
func (s *Server) recordAuthFailure(conn net.Conn) {
	banned, count := s.bans.fail(remoteIP(conn))
	if !banned {
		return
	}

	// The entry was reset, so the count is the number of bans the source has had.
	s.metrics.bans.Add(1)
	s.auditor.Record(AuditEvent{
		Event: EventSourceBanned, Remote: conn.RemoteAddr().String(),
		Outcome: "denied",
		Detail:  fmt.Sprintf("banned after %d failed attempt(s), ban number %d", s.cfg.Server.BanAfterFailures, count),
	})
	logging.Warnf(s.logger, "source %s banned after %d failed attempt(s)", conn.RemoteAddr(), s.cfg.Server.BanAfterFailures)
}

// recordAuthSuccess clears a source's failure count, so a client that eventually
// authenticates does not carry its earlier typos towards a ban.
func (s *Server) recordAuthSuccess(conn net.Conn) {
	s.bans.succeed(remoteIP(conn))
}

// Shutdown stops the listener, lets the streams that are already running finish,
// and then disconnects every client.
//
// The wait is bounded by [server] graceful_shutdown_seconds. Without it a restart
// or a deployment cut every transfer in flight the moment the signal arrived, which
// for a long upload means starting over.
//
// Shutdown returns as soon as the clients have been disconnected; the handlers
// that tear those sessions down are still writing their audit records and ledger
// entries at that point, so the stores are closed with them, not here.
func (s *Server) Shutdown(reason string) {
	if s.closing.Swap(true) {
		return
	}
	s.logger.Printf("shutting down: %s", reason)

	// Stop accepting: no new control connection and no new visitor reaches a proxy
	// from here on, so the streams that are counted below are all that is left.
	if listener := s.Listener(); listener != nil {
		_ = listener.Close()
	}
	s.vhost.stop()
	s.stopAcceptingSSHGateway()
	if s.sni != nil {
		s.sni.stop()
	}
	if s.tcpmux != nil {
		s.tcpmux.stop()
	}
	if s.p2p != nil {
		_ = s.p2p.Close()
	}

	grace := time.Duration(s.cfg.Server.GracefulShutdownSecs) * time.Second
	if grace > 0 {
		if remaining := s.sessions.Drain(grace); remaining > 0 {
			s.logger.Printf("graceful shutdown: %d stream(s) were still running after %s; disconnecting",
				remaining, grace)
		} else {
			s.logger.Printf("graceful shutdown: every stream finished within %s", grace)
		}
	}

	// CloseAll takes the snapshot itself, under the lock that also closes the
	// registry: a session registered between a separate List and this call used
	// to be closed without ever being disconnected here. The snapshot has to
	// come from before the control connections close — a handler removes its
	// session from the registry the moment its control connection drops —
	// which is why CloseAll returns what it closed instead of Shutdown
	// listing first. The streams the grace period did not wait for are
	// disconnected here: their data connections are sockets of their own, so
	// closing the sessions above — which closes the control connections —
	// would leave the visitor connected to a tunnel the server has stopped
	// serving.
	for _, session := range s.sessions.CloseAll(reason) {
		session.disconnectStreams()
	}
	// The SSH gateway's connections outlive the drain above on purpose: closing
	// them ends the sessions they own, so ending them earlier would have taken
	// those sessions out of the registry before Drain could wait for the streams
	// they carry. The wait below is what keeps Run's closeStores behind these
	// handlers: they withdraw their proxies and write the last ledger entries on
	// the way out, and they are not in the WaitGroup that loop waits on.
	s.disconnectSSHGateway()

	// Nothing after this may write to the stores. Run's accept loop waits for
	// this signal before it closes them, so everything above is finished by the
	// time they go.
	close(s.shutdownDone)
}

// closeStores releases everything that connection teardown writes to: the
// bandwidth ledger, the DHT directory, the vpn router and the audit log. It is
// called once the last handler has returned, because a handler that is still
// tearing down a session appends a ledger entry and withdraws the session's
// proxies from the DHT.
//
// The audit log belongs here rather than in Shutdown for the same reason: a
// session's teardown is what records client_disconnected and proxy_removed, and a
// record that arrives after the file is closed has nowhere to go. It is safe to
// call more than once.
func (s *Server) closeStores() {
	if err := s.ledger.Close(); err != nil {
		s.logger.Printf("closing the bandwidth ledger: %v", err)
	}
	if err := s.directory.Close(); err != nil {
		s.logger.Printf("closing the DHT node: %v", err)
	}
	if err := s.vpn.Close(); err != nil {
		s.logger.Printf("closing the vpn interface: %v", err)
	}
	if err := s.auditor.Close(); err != nil {
		s.logger.Printf("closing the audit log: %v", err)
	}
}

// identityAssertion is one client identity assertion in the wire form both the
// control connection and a visitor connection carry.
type identityAssertion struct {
	PublicKey []byte
	Nonce     []byte
	Timestamp int64
	Signature []byte
}

// checkIdentity verifies an Ed25519 assertion. It returns nil when no assertion
// was offered and none is required.
func (s *Server) checkIdentity(in identityAssertion) error {
	if !s.cfg.Identity.Enabled {
		return nil
	}
	if len(in.PublicKey) == 0 {
		if s.cfg.Identity.RequireIdentity {
			return errors.New("this server requires a client identity; set [identity].enabled and [identity].key_file")
		}
		return nil
	}

	publicKey, err := crypto.ParseIdentityKey(hex.EncodeToString(in.PublicKey))
	if err != nil {
		return err
	}
	return crypto.VerifyIdentity(crypto.IdentityCheckInput{
		PublicKey: publicKey,
		Nonce:     in.Nonce,
		Timestamp: in.Timestamp,
		Signature: in.Signature,
		Allowed:   s.identities,
		Seen:      s.nonces,
	})
}

// handleConn dispatches the first frame: an auth request opens a control session,
// a data-open claims a stream that a public connection is waiting for.
func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			// A panic in one connection must not take the server down.
			logging.Errorf(s.logger, "panic while handling %s: %v", conn.RemoteAddr(), r)
			_ = conn.Close()
		}
	}()

	if !s.admit(conn) {
		return
	}

	handshakeTimeout := time.Duration(s.cfg.Server.HandshakeTimeoutSecs) * time.Second
	// The same port carries every connection shape: a peer that opens with the
	// websocket upgrade for [transport].protocol = "websocket" is answered and
	// continues as binary frames, every other peer gets its peeked bytes
	// replayed in front of it and proceeds unchanged. A failed upgrade has
	// already been answered and closed.
	carried, err := flynet.AcceptOrPass(conn, handshakeTimeout)
	if err != nil {
		logging.Warnf(s.logger, "websocket upgrade from %s failed: %v", conn.RemoteAddr(), err)
		return
	}
	conn = carried

	if handshakeTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	}

	framer := protocol.NewFramerWithOptions(conn, s.cipher, s.framerOptions())
	msg, err := framer.ReadFrame()
	if err != nil {
		// The client is closed out without an answer, so this line is the only place the
		// reason is written down. Say which settings could produce it: an authentication
		// failure on the first frame is what a passphrase, salt or algorithm that differs
		// between the two ends looks like from here, and an operator reading it should not
		// have to know that.
		s.refuseHandshake(conn, err)
		_ = conn.Close()
		return
	}

	// The first frame has arrived, so the handshake window is over. Every path
	// that reads afterwards sets the deadline it needs (the control session its
	// heartbeat window, the visitor its own handshake window), and the datagram
	// relay sets none at all: leaving this one armed made the first read of a
	// udp or sudp session fail server.handshake_timeout_seconds after the data
	// connection was accepted, which tore the session down mid-flow.
	_ = conn.SetReadDeadline(time.Time{})

	switch msg.Type {
	case protocol.TypeAuthRequest:
		s.handleControl(conn, framer, msg)
	case protocol.TypeDataOpen:
		s.handleData(conn, framer, msg)
	case protocol.TypeVisitorConnect:
		s.handleVisitor(conn, framer, msg)
	default:
		// An answered refusal, not a dead connection: the peer is told what a first
		// frame has to be, so this is recorded like every other refusal. It used to
		// be a log line only, which left a scan of the control port with nothing
		// (malformed) or a line nobody watches (this one) in the counters and the
		// audit log.
		s.refuseFirstFrame(conn, fmt.Sprintf("first frame is %s, which cannot start a connection", msg.Type))
		_ = framer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{
			// The detailed form names the three frames the server accepts,
			// which is a description of this server's protocol rather than
			// something a peer needs: it is only sent while detailed errors
			// are on.
			Error: s.clientError("the first frame is not a valid request",
				fmt.Sprintf("first frame must be %s, %s or %s, got %s",
					protocol.TypeAuthRequest, protocol.TypeDataOpen, protocol.TypeVisitorConnect, msg.Type)),
		})
		_ = conn.Close()
	}
}

// refuseFirstFrame books a connection whose first frame was not a usable request:
// the aggregate rejection counter, the series that names this reason, and the audit
// log. The connection is closed by the caller, after it has been answered.
func (s *Server) refuseFirstFrame(conn net.Conn, detail string) {
	s.metrics.unusableFrames.Add(1)
	s.refuseControl(conn, EventControlRejected, detail)
}

// refuseHandshake books a connection whose first frame could not be read at all,
// which is what a disguise, encryption or TLS mismatch looks like, and also what a
// peer that connected and went away without sending one looks like.
//
// This is the counterpart of refuseFirstFrame for bytes that never became a frame,
// and it is booked the same way for the same reason: the connection is closed
// without an answer, so the counters and the audit log are the only record an
// operator has. It used to be a log line only, which left a fleet whose passphrase
// had been changed on one side — or a scan of the control port — invisible in
// GET /metrics and in the audit trail.
func (s *Server) refuseHandshake(conn net.Conn, err error) {
	detail := fmt.Sprintf("%v%s", err, handshakeFailureHint(err))
	s.metrics.handshakeFailures.Add(1)
	// The aggregate the alerts watch, so a refusal here is not the one answer
	// missing from it.
	s.metrics.controlRejected.Add(1)
	s.auditor.Record(AuditEvent{
		Event: EventHandshakeFailed, Remote: conn.RemoteAddr().String(),
		Outcome: "denied", Detail: detail,
	})
	logging.Warnf(s.logger, "handshake from %s failed: %s", conn.RemoteAddr(), detail)
}

// refuseControl books a control connection the server answered with a refusal
// rather than dropping. The aggregate rejection counter is the one an alert watches
// when the reason does not matter, so every answered refusal has to move it, and
// the audit record is what makes the reason attributable afterwards; the connection
// is closed by the caller after it has been answered.
func (s *Server) refuseControl(conn net.Conn, event, detail string) {
	s.metrics.controlRejected.Add(1)
	s.auditor.Record(AuditEvent{
		Event: event, Remote: conn.RemoteAddr().String(),
		Outcome: "denied", Detail: detail,
	})
	logging.Warnf(s.logger, "refusing %s: %s", conn.RemoteAddr(), detail)
}

// refuseVPN tears down a session whose tunnel request could not be met. The session
// was already registered, so it is removed from both registries and its address, if
// one was assigned, is released.
func (s *Server) refuseVPN(session *Session, conn net.Conn, reason string) {
	// controlRejected stays out: the session passed the handshake and was
	// counted as accepted, and docs/SECURITY.md puts every post-acceptance
	// failure on the accepted side — counting it here too would make
	// accepted plus rejected exceed the connections the server answered.
	s.auditor.Record(AuditEvent{
		Event: EventVPNRejected, ClientID: session.ClientName(), Remote: session.RemoteAddr,
		Outcome: "denied", Detail: reason,
	})
	logging.Warnf(s.logger, "refusing %s: %s", conn.RemoteAddr(), reason)

	_ = session.Framer().WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
		OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
		Encryption: s.Cipher(),
		// The reason names what the server has or lacks, so it is shortened
		// when server.detailed_errors_to_client is off; the log line above and
		// the audit record keep it whole.
		Error: s.clientError("the tunnelled connection was refused", reason),
	})
	s.tunnels.RemoveSessionTunnels(session, "tunnel request refused")
	s.sessions.Remove(session.ID)
	session.Close("tunnel request refused")
	s.vpn.release(session.ID)
}

// acceptErrorLogLimit is how many accept failures Run logs one by one before
// the once-a-minute summary takes over.
const acceptErrorLogLimit = 64

// maxSanitizedLogText caps how much of a client-supplied string reaches the log.
const maxSanitizedLogText = 200

// sanitizeLogText makes a client-supplied string safe for the log: control
// characters — a newline above all, which would forge the next log line — are
// dropped, and anything past the cap with it.
func sanitizeLogText(s string) string {
	if len(s) > maxSanitizedLogText {
		s = s[:maxSanitizedLogText]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// maxClientIDLen bounds the client_id an auth request may carry. It names the
// client in the log, the audit record, the dashboard and the bandwidth ledger,
// whose totals map is keyed by it for the life of the file: an unbounded value
// let one authenticated connection write itself into permanent storage once
// per closed stream. Anything past a display name's length is not a name.
const maxClientIDLen = 128

// handleControl authenticates a client and then serves its session loop.
func (s *Server) handleControl(conn net.Conn, framer *protocol.Framer, msg *protocol.Message) {
	var req protocol.AuthRequest
	if len(msg.Payload) == 0 || json.Unmarshal(msg.Payload, &req) != nil {
		// The peer is answered rather than dropped, so this counts as a refusal: the
		// only trace of someone speaking the wrong protocol to the control port used
		// to be the answer they got, which the server itself never wrote down.
		s.refuseFirstFrame(conn, "malformed auth request")
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "malformed auth request",
		})
		_ = conn.Close()
		return
	}
	if len(req.ClientID) > maxClientIDLen {
		s.refuseFirstFrame(conn, fmt.Sprintf("auth request: client_id is longer than %d bytes", maxClientIDLen))
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "auth request: client_id is too long",
		})
		_ = conn.Close()
		return
	}

	// Constant-time comparison so the token cannot be recovered byte by byte from
	// response timing, and never log the offered token. An [oidc] server also
	// accepts a verified access token in the token's place.
	tokenAccepted := crypto.EqualTokens(req.Token, s.cfg.Server.AuthToken)
	if !tokenAccepted && s.oidc != nil {
		if err := s.oidc.Verify(req.Token); err == nil {
			tokenAccepted = true
			s.logger.Printf("session from %s accepted with an oidc token", conn.RemoteAddr())
		}
	}
	if !tokenAccepted {
		s.metrics.authFailures.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "invalid auth token",
		})
		logging.Warnf(s.logger, "authentication failed for %s (client %s)", conn.RemoteAddr(), req.ClientVersion)
		s.recordAuthFailure(conn)
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "invalid auth token",
		})
		_ = conn.Close()
		return
	}

	reject := func(reason string, identityRequired bool) {
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: reason, IdentityRequired: identityRequired,
		})
		_ = conn.Close()
	}

	if err := s.checkIdentity(identityAssertion{
		PublicKey: req.Identity,
		Nonce:     req.IdentityNonce,
		Timestamp: req.IdentityTime,
		Signature: req.IdentitySignature,
	}); err != nil {
		s.metrics.authFailures.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: err.Error(),
		})
		logging.Warnf(s.logger, "identity check failed for %s: %v", conn.RemoteAddr(), err)
		s.recordAuthFailure(conn)
		// The actionable half of this refusal is the IdentityRequired flag,
		// which travels whatever the wording is; the text names the rule that
		// failed, so it is only sent in full when detailed errors are on.
		reject(s.clientError("identity check failed", err.Error()), s.cfg.Identity.RequireIdentity)
		return
	}

	// The post-quantum handshake runs before the session exists, because the
	// session's key is what protects every stream it later opens.
	sessionCipher := s.cipher
	var sessionKey, kexResponse, sessionSalt []byte
	if s.cfg.PostQuantum() {
		if len(req.KEX) == 0 {
			s.refuseControl(conn, EventControlRejected,
				"this server requires a post-quantum key exchange (encryption.post_quantum)")
			reject("this server requires a post-quantum key exchange (encryption.post_quantum)", false)
			return
		}
		response, key, err := crypto.HybridServerFinish(req.KEX)
		if err != nil {
			s.metrics.authFailures.Add(1)
			s.refuseControl(conn, EventAuthFailed, fmt.Sprintf("post-quantum key agreement failed: %v", err))
			s.recordAuthFailure(conn)
			reject("post-quantum key agreement failed", false)
			return
		}
		sessionCipher, err = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if err != nil {
			s.refuseControl(conn, EventControlRejected, fmt.Sprintf("post-quantum session key is unusable: %v", err))
			reject("post-quantum key agreement failed", false)
			return
		}
		sessionKey, kexResponse = key, response
	} else if s.cipher.Enabled() {
		// Without a key agreement, the static cipher would seal every record of
		// every session with random nonces under one long-lived key, which
		// AES-GCM's random-IV bound (NIST SP 800-38D: 2^32 invocations per key)
		// puts at risk on a server that relays bulk traffic for months. A fresh
		// salt per session — delivered inside the response this static cipher
		// still protects — gives every session its own key, anchored to the
		// token the client just proved it holds.
		salt, saltErr := crypto.SessionSalt()
		if saltErr != nil {
			s.refuseControl(conn, EventControlRejected, fmt.Sprintf("cannot draw a session salt: %v", saltErr))
			reject("the server cannot draw a session salt", false)
			return
		}
		key, keyErr := crypto.SessionKey(req.Token, salt)
		if keyErr != nil {
			s.refuseControl(conn, EventControlRejected, fmt.Sprintf("the session key is unusable: %v", keyErr))
			reject("the session key is unusable", false)
			return
		}
		sessionCipher, keyErr = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if keyErr != nil {
			s.refuseControl(conn, EventControlRejected, fmt.Sprintf("the session key is unusable: %v", keyErr))
			reject("the session key is unusable", false)
			return
		}
		sessionKey, sessionSalt = key, salt
	}

	if req.Protocol != protocol.ProtocolVersion {
		s.logger.Printf("client %s speaks protocol %d, this server speaks %d: continuing, but upgrade the client if traffic misbehaves",
			conn.RemoteAddr(), req.Protocol, protocol.ProtocolVersion)
	}

	session := newSession(s, conn, framer, &req, sessionCipher.Enabled(), s.cfg.Server.HeartbeatSeconds)
	session.streamKey = sessionKey

	// The [[http_plugins]] webhooks see the login before the session is
	// registered: a reject answers the auth response with the plugin's reason,
	// the same treatment a built-in check gets.
	if reason := s.httpPlugins.runLogin(session, &req); reason != "" {
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventControlRejected, ClientID: session.ClientName(), Remote: session.RemoteAddr,
			Outcome: "denied", Detail: "rejected by http plugin: " + reason,
		})
		// The refusal answer goes out before the session's teardown closes
		// the connection.
		reject(reason, false)
		session.Close("rejected by http plugin")
		return
	}

	if err := s.sessions.Add(session); err != nil {
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventControlRejected, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: err.Error(),
		})
		logging.Warnf(s.logger, "rejecting %s: %v", conn.RemoteAddr(), err)
		reject(s.clientError("the server cannot accept this session", err.Error()), false)
		return
	}
	s.metrics.controlAccepted.Add(1)
	s.recordAuthSuccess(conn)
	s.auditor.Record(AuditEvent{
		Event: EventControlAccepted, ClientID: session.ClientName(), Remote: session.RemoteAddr,
		Outcome: "ok",
		Detail: fmt.Sprintf("client %s, protocol %d, encryption %s, post_quantum %v, identity %v",
			req.ClientVersion, req.Protocol, s.Cipher(), len(sessionKey) > 0, len(req.Identity) > 0),
	})

	// A tunnel address is assigned before the response is written, because the
	// response is what carries it.
	var vpnAddress, vpnMask string
	var vpnMTU int
	switch {
	case s.cfg.VPN.Require && !req.VPN:
		// The operator wants every session on the tunnel, and this client did not
		// ask for one.
		s.refuseVPN(session, conn, "this server requires a layer-3 tunnel (vpn.require); set [vpn] enabled = true")
		return
	case req.VPN && s.vpn == nil:
		s.refuseVPN(session, conn, "this server has no layer-3 tunnel configured")
		return
	case req.VPN:
		address, mtu, err := s.attachVPN(session)
		if err != nil {
			logging.Warnf(s.logger, "client %s: cannot provide a tunnel address: %v", session.ID, err)
			s.refuseVPN(session, conn, "no tunnel address is available")
			return
		}
		vpnAddress, vpnMask, vpnMTU = address, s.vpn.Mask(), mtu
		s.auditor.Record(AuditEvent{
			Event: EventVPNAssigned, ClientID: session.ClientName(), Remote: session.RemoteAddr,
			Outcome: "ok", Detail: fmt.Sprintf("tunnel address %s, mtu %d", vpnAddress, vpnMTU),
		})
	}

	response := protocol.AuthResponse{
		OK:               true,
		Session:          session.ID,
		ServerVersion:    s.version,
		Protocol:         protocol.ProtocolVersion,
		Encryption:       s.Cipher(),
		HeartbeatSecs:    s.cfg.Server.HeartbeatSeconds,
		ProtocolMismatch: req.Protocol != protocol.ProtocolVersion,
		P2PPort:          s.cfg.Server.P2PPort,
		KEX:              kexResponse,
		SessionSalt:      sessionSalt,
		VPNAddress:       vpnAddress,
		VPNMask:          vpnMask,
		VPNMTU:           vpnMTU,
	}
	// The response itself is still protected by the configured cipher; both
	// sides switch to the agreed key immediately afterwards.
	if err := framer.WriteJSON(protocol.TypeAuthResponse, response); err != nil {
		s.sessions.Remove(session.ID)
		session.Close("failed to send auth response")
		// attachVPN leased an address before this write; the teardown defer that
		// releases it is registered below, so this path has to release it here or
		// a client that drops the socket during auth leaks the address for good.
		s.vpn.release(session.ID)
		return
	}
	if len(sessionKey) > 0 {
		session.SetFramer(protocol.NewFramerWithOptions(conn, sessionCipher, s.framerOptions()))
		if len(kexResponse) > 0 {
			s.logger.Printf("post-quantum session key %s agreed with %s", crypto.SessionKeyID(sessionKey), conn.RemoteAddr())
		} else {
			s.logger.Printf("per-session key %s derived with %s", crypto.SessionKeyID(sessionKey), conn.RemoteAddr())
		}
	}

	if vpnAddress != "" {
		// The auth response is on the wire and the framer has switched, so the
		// tunnel transport attached above may write now without preceding the
		// response or framing under the superseded cipher.
		session.markVPNReady()
	}

	s.logger.Printf("client %s connected as %s (client_id %q, protocol %d, encryption %s, version %s)",
		conn.RemoteAddr(), session.ID, session.ClientID, req.Protocol, s.Cipher(), req.ClientVersion)

	defer func() {
		s.tunnels.RemoveSessionTunnels(session, "client disconnected")
		s.sessions.Remove(session.ID)
		session.Close("control connection ended")
		// The address is released after the session is closed, so the peer stops
		// routing packets here before the lease disappears.
		s.vpn.release(session.ID)
		// A stream records its bytes on the tunnel when its pipe returns, and the ledger
		// entry is written from those counters: a client that leaves while a stream is
		// still finishing would otherwise have that stream's bytes left out of the entry
		// entirely. The wait is bounded, because a stream that outlives its control
		// connection is closed with it.
		if s.ledger != nil {
			s.waitForStreamsToFinish(session, ledgerSettle)
		}
		// The ledger entry is written after the member endpoints are gone, so it
		// covers the whole life of the proxy and is written once.
		s.recordSessionUsage(session)
		s.auditor.Record(AuditEvent{
			Event: EventClientGone, ClientID: session.ClientName(), Remote: session.RemoteAddr,
			Outcome: "closed",
			Detail:  fmt.Sprintf("connected for %s", session.Uptime().Round(time.Second)),
		})
		s.logger.Printf("client %s (%s) disconnected after %s",
			session.ID, session.RemoteAddr, session.Uptime().Round(time.Second))
	}()

	s.sessionLoop(session)
}

// sessionLoop serves control frames until the client goes away or stops sending
// heartbeats.
func (s *Server) sessionLoop(session *Session) {
	heartbeat := time.Duration(session.HeartbeatSeconds) * time.Second
	deadline := 3 * heartbeat

	for {
		if deadline > 0 {
			_ = session.conn.SetReadDeadline(time.Now().Add(deadline))
		}

		msg, err := session.Framer().ReadFrame()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				s.logger.Printf("client %s missed its heartbeat window (%s), closing", session.ID, deadline)
			} else {
				s.logger.Printf("client %s control read ended: %v", session.ID, err)
			}
			return
		}

		switch msg.Type {
		case protocol.TypeHeartbeat:
			session.touchHeartbeat()
			// A client that sends heartbeats but never reads fills the kernel's
			// send buffer, and the ack write then parks here forever: the read
			// loop stops running, so the heartbeat window that would have ended
			// the session can never fire. The window bounds the write too, and
			// its failure tears the session down like any other.
			//
			// The control connection also carries the VPN transport's writes
			// (pkg/vpn's peer sendLoop), and the armed window applies to a
			// write of theirs caught mid-park as well as to this one. That is
			// convergent, not harmful: on one ordered stream a write only
			// parks when the client has stopped reading entirely, and the
			// ack's own failure tears the same session down moments later.
			if err := controlWrite(session, deadline, func() error {
				return session.Framer().WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeatAck})
			}); err != nil {
				return
			}

		case protocol.TypeRegisterProxy:
			var spec protocol.ProxySpec
			if err := json.Unmarshal(msg.Payload, &spec); err != nil {
				// A parse error says nothing about this server, so it is not
				// shortened by server.detailed_errors_to_client.
				s.replyError(session, fmt.Sprintf("malformed register-proxy: %v", err), deadline)
				continue
			}
			// Every refusal below can describe the server's own setup — a port
			// that is taken, an address the server lacks, a name already in
			// use — so it goes through clientError; the log line and the audit
			// record written next to each one keep the full reason.
			refuse := func(detail string) {
				s.replyError(session, s.clientError(
					fmt.Sprintf("cannot register the proxy %q", spec.Name), detail), deadline)
			}
			if spec.Subdomain != "" {
				if s.cfg.Server.SubdomainHost == "" {
					refuse(fmt.Sprintf("proxy %q: a subdomain needs [server].subdomain_host on the server", spec.Name))
					continue
				}
				label := strings.ToLower(spec.Subdomain)
				if err := config.ValidateDNSLabel(label); err != nil {
					refuse(fmt.Sprintf("proxy %q: subdomain %q: %v", spec.Name, spec.Subdomain, err))
					continue
				}
				spec.Domains = append(spec.Domains, label+"."+s.cfg.Server.SubdomainHost)
			}
			if bad := firstBadDomain(spec); bad != "" {
				refuse(fmt.Sprintf("proxy %q: domain %q is not a usable hostname or \"*.suffix\" wildcard", spec.Name, bad))
				continue
			}
			if spec.RemotePort != 0 && !s.remotePorts.Contains(spec.RemotePort) {
				logging.Warnf(s.logger, "client %s: proxy %q asks for remote port %d, which is outside allow_ports", session.ID, spec.Name, spec.RemotePort)
				s.auditor.Record(AuditEvent{
					Event: EventProxyRejected, ClientID: session.ClientName(), Proxy: spec.Name,
					Outcome: "denied", Detail: fmt.Sprintf("remote port %d is outside allow_ports", spec.RemotePort),
				})
				refuse(fmt.Sprintf("remote port %d is outside allow_ports", spec.RemotePort))
				continue
			}
			tunnel, err := s.tunnels.Register(session, spec)
			if err != nil {
				logging.Warnf(s.logger, "client %s: cannot register proxy %q: %v", session.ID, spec.Name, err)
				s.auditor.Record(AuditEvent{
					Event: EventProxyRejected, ClientID: session.ClientName(), Proxy: spec.Name,
					Outcome: "denied", Detail: err.Error(),
				})
				refuse(err.Error())
				continue
			}
			if err := session.RegisterProxy(tunnel); err != nil {
				s.tunnels.Unregister(spec.Name, session, "session closed during registration")
				if tunnel.createdUsage && !tunnel.reachable.Load() {
					// The registration created the session's counter for this name
					// and its endpoint never came up, so the ledger must not bill an
					// empty entry for it at teardown. A name that was reachable
					// keeps its counter: a stream may still be holding it.
					session.forgetUsage(spec.Name)
				}
				refuse(err.Error())
				continue
			}
			s.auditor.Record(AuditEvent{
				Event: EventProxyRegistered, ClientID: session.ClientName(), Proxy: spec.Name,
				Outcome: "ok",
				Detail:  fmt.Sprintf("type=%s local=%s remote_port=%d", spec.Type, spec.LocalAddr, spec.RemotePort),
			})
			s.sendProxyList(session, deadline)

		case protocol.TypeProxyWithdraw:
			var withdraw protocol.ProxyWithdraw
			if err := json.Unmarshal(msg.Payload, &withdraw); err != nil {
				s.replyError(session, fmt.Sprintf("malformed withdraw: %v", err), deadline)
				continue
			}
			// The tunnel manager is keyed by name alone, so a lookup only says the
			// name exists somewhere: it may be another session's registration.
			// Unregister would then remove nothing while the ack said OK, and the
			// client would stop with the endpoint still published. The session's
			// own list is what decides, and the message is the same either way so a
			// client cannot probe for other sessions' names.
			if !sessionHoldsName(session, withdraw.Name) {
				ack := protocol.ProxyWithdrawAck{Name: withdraw.Name, OK: false, Error: "no such proxy"}
				_ = controlWrite(session, deadline, func() error {
					return session.Framer().WriteJSON(protocol.TypeProxyWithdrawAck, ack)
				})
				continue
			}
			s.tunnels.Unregister(withdraw.Name, session, "withdrawn by the client")
			// The session's own list drives the dashboard view, the teardown that
			// follows, and the max_ports_per_client count, so the name has to leave it
			// with the registration.
			session.UnregisterProxy(withdraw.Name)
			ack := protocol.ProxyWithdrawAck{Name: withdraw.Name, OK: true}
			if err := controlWrite(session, deadline, func() error {
				return session.Framer().WriteJSON(protocol.TypeProxyWithdrawAck, ack)
			}); err != nil {
				logging.Warnf(s.logger, "client %s: could not deliver the withdraw ack: %v", session.ID, err)
			}

		case protocol.TypeVPNPacket:
			// A packet from the client goes to the transport the session's peer
			// reads from. A session without a tunnel must not be able to flood the
			// router, so the frame is dropped and counted. The lines are
			// rate-limited: the packets themselves arrive at line rate, so one
			// line per drop would let an authenticated client flood the log.
			if transport := session.vpnTransportFor(); transport != nil {
				if !transport.Deliver(msg.Payload) {
					session.vpnPacketDrops.add(1, func(total uint64) {
						logging.Warnf(s.logger, "client %s: dropped %d tunnel packet(s) because the tunnel's buffer is full", session.ID, total)
					})
				}
			} else {
				session.vpnPacketsWithoutAddr.add(1, func(total uint64) {
					s.logger.Printf("client %s sent %d tunnel packet(s) but holds no tunnel address", session.ID, total)
				})
			}

		case protocol.TypeError:
			// The payload is the client's own words about its error, and an
			// authenticated client can send this frame at line rate with any
			// bytes it likes: the text is truncated and stripped of control
			// characters so it cannot forge log lines, and the line is
			// throttled so it cannot flood the log.
			session.errorReports.add(1, func(total uint64) {
				s.logger.Printf("client %s reported %d error(s), last: %s", session.ID, total, sanitizeLogText(string(msg.Payload)))
			})

		default:
			// An authenticated client can send an unhandled type at line rate,
			// so the line is throttled the way the TypeError one above is; the
			// last type is kept so the line still names what arrived.
			lastType := msg.Type
			session.unknownFrames.add(1, func(total uint64) {
				logging.Warnf(s.logger, "client %s sent %d unhandled control frame(s), last %s; ignoring", session.ID, total, lastType)
			})
		}
	}
}

// clientError picks the wording of a refusal that is sent to a client: the
// detail while server.detailed_errors_to_client is on, which is the default,
// and a short summary when it is off.
//
// It is applied where the text depends on what the server has or lacks — a port
// that is taken, a subdomain_host that is missing, a session limit that is
// reached, an identity rule that failed — because that is the text a client
// could read the server's setup from. The server's log and the audit trail
// carry the detail in every case, so turning the key off costs the operator
// nothing.
func (s *Server) clientError(summary, detail string) string {
	if s.cfg.DetailedErrors() {
		return detail
	}
	return summary
}

// controlWrite arms the session's write deadline around a control write from the
// session loop. The read deadline is armed by the loop itself, but a write has to
// bound itself too: a client that registers enough proxies to fill the kernel's
// send buffer and then stops reading parks the loop inside the write, and the
// armed read deadline can never fire because the loop never returns to ReadFrame.
// The heartbeat ack already carried this window; the proxy list, the withdraw ack
// and the error replies went out unbounded.
//
// The window also covers a VPN transport write caught mid-park, because the control
// connection carries those as well. That is convergent, not harmful: on one ordered
// stream a write only parks when the client has stopped reading entirely, and the
// session is torn down moments later either way.
func controlWrite(session *Session, deadline time.Duration, write func() error) error {
	if deadline > 0 {
		_ = session.conn.SetWriteDeadline(time.Now().Add(deadline))
	}
	err := write()
	if deadline > 0 {
		_ = session.conn.SetWriteDeadline(time.Time{})
	}
	return err
}

func (s *Server) replyError(session *Session, message string, deadline time.Duration) {
	if err := controlWrite(session, deadline, func() error {
		return session.Framer().WriteJSON(protocol.TypeError, protocol.ErrorPayload{Error: message})
	}); err != nil {
		logging.Warnf(s.logger, "client %s: could not deliver error %q: %v", session.ID, message, err)
	}
}

func (s *Server) sendProxyList(session *Session, deadline time.Duration) {
	statuses := make([]protocol.ProxyStatus, 0)

	for _, tunnel := range s.tunnels.List() {
		if tunnel.Session != session {
			continue
		}
		status := protocol.ProxyStatus{
			Name:       tunnel.Name,
			Type:       tunnel.Spec.Type,
			RemotePort: tunnel.PublicPort(),
		}
		if tunnel.group != nil {
			status.GroupMembers = tunnel.group.memberCount()
		}
		statuses = append(statuses, status)
	}

	if err := controlWrite(session, deadline, func() error {
		return session.Framer().WriteJSON(protocol.TypeProxyList, statuses)
	}); err != nil {
		logging.Warnf(s.logger, "client %s: could not send proxy list: %v", session.ID, err)
	}
}

// handleData accepts a data connection from a client and hands it to the public
// connection that is waiting for that stream id.
func (s *Server) handleData(conn net.Conn, framer *protocol.Framer, msg *protocol.Message) {
	var open protocol.DataOpen
	if len(msg.Payload) == 0 || json.Unmarshal(msg.Payload, &open) != nil {
		// A data-open is dispatched before anything is authenticated, so a payload that
		// does not parse is a garbage frame aimed at this port; it is counted on the
		// series that names that reason. Not a control refusal, though: this connection
		// never became a session, and the aggregate rejection counter is about the ones
		// that tried to.
		s.metrics.unusableFrames.Add(1)
		logging.Warnf(s.logger, "data connection from %s rejected: malformed data-open", conn.RemoteAddr())
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: "malformed data-open"})
		_ = conn.Close()
		return
	}
	if open.Pool {
		s.parkDataConn(conn, framer, open)
		return
	}
	// Counted where the connection enters, so every parseable data-open counts
	// exactly once: parkDataConn counts the pooled ones it accepts, and this
	// line the direct ones — serveData no longer counts, or a pooled
	// connection put to use would land here a second time.
	s.metrics.dataConnections.Add(1)
	s.serveData(conn, framer, open)
}

// parkDataConn keeps a connection the client opened in advance, so the next
// stream of that session does not wait for a dial. Nothing is paired here: the
// server names the proxy and the stream when it puts the connection to use, and
// reads the client's answer then.
func (s *Server) parkDataConn(conn net.Conn, framer *protocol.Framer, open protocol.DataOpen) {
	// A parked connection is idle by design, so no deadline may be left on it:
	// the dispatch cleared the handshake one, and this clears anything the
	// caller's own paths set, because a connection older than
	// server.handshake_timeout_seconds is exactly what the pool is for.
	_ = conn.SetDeadline(time.Time{})

	session, ok := s.sessions.Get(open.Session)
	if !ok {
		s.metrics.dataUnmatched.Add(1)
		logging.Warnf(s.logger, "pooled connection from %s rejected: unknown or expired session", conn.RemoteAddr())
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: "unknown or expired session"})
		_ = conn.Close()
		return
	}
	if err := session.Park(conn, framer); err != nil {
		// A refusal, not a silent close: a client that parks too many connections
		// has to learn that the pool it asked for is not the pool it got.
		//
		// Only a full pool is pool pressure. Park also refuses when the session
		// closed between reading this frame and taking the lock, and counting
		// that on poolRefused made an ordinary disconnect read as a client that
		// parks more than it may.
		if errors.Is(err, errPoolFull) {
			s.metrics.poolRefused.Add(1)
		}
		logging.Warnf(s.logger, "pooled connection from %s refused: %v", conn.RemoteAddr(), err)
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: err.Error()})
		_ = conn.Close()
		return
	}
	s.metrics.poolParked.Add(1)
	s.metrics.dataConnections.Add(1)
	// The ack has to cross before the connection becomes visible to TakeParked:
	// the client's first read on a pooled connection is this ack, and a
	// DataRequest that arrived there first would be refused as a type mismatch,
	// burning the pooled connection and a round trip.
	if err := framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
		// The client will not know it is parked, so the connection is dropped
		// rather than left holding a slot it does not believe it has. The pool
		// slot goes with it: a closed connection handed out by the next
		// TakeParked would burn a round trip and leave the pool one short.
		logging.Warnf(s.logger, "pooled connection from %s: cannot confirm it: %v", conn.RemoteAddr(), err)
		session.DropParked(conn)
		_ = conn.Close()
		return
	}
	session.ConfirmParked(conn)
}

// awaitPooledData reads the client's answer on a connection the server took from
// the pool and pairs the stream exactly as a fresh data connection is paired. A
// parked connection the client has since dropped shows up here as a read error,
// and the stream is then asked for over the control connection instead, so a stale
// parked connection costs one round trip rather than the stream.
//
// wait is what the caller is waiting for the stream, and the read is given half of
// it: a connection that has been blackholed — no FIN, no RST, which is what a NAT
// that forgot the mapping looks like — is then given up on early enough for the
// control-connection request to be answered inside the same window, instead of
// holding the visitor until its own timeout. A local service that is slow to dial
// answers late and is refused like any other unpaired data connection, which is
// where the visitor already got its stream from the fallback.
func (s *Server) awaitPooledData(session *Session, conn net.Conn, framer *protocol.Framer, request protocol.DataRequest, wait time.Duration) {
	if half := wait / 2; half > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(half))
	}
	msg, err := framer.ReadFrame()
	if err != nil {
		s.metrics.poolMissed.Add(1)
		_ = conn.Close()
		// The pending entry is still registered under this stream's identifier, so
		// the client's answer to the control-channel request pairs with it.
		if writeErr := session.Framer().WriteJSON(protocol.TypeDataRequest, request); writeErr != nil {
			logging.Warnf(s.logger, "a parked connection for %s could not be used (%v), and the client cannot be asked for another: %v",
				request.Proxy, err, writeErr)
		}
		return
	}
	if msg.Type != protocol.TypeDataOpen {
		s.metrics.poolMissed.Add(1)
		s.logger.Printf("a parked connection from %s was used for something else (%s); asking over the control connection",
			conn.RemoteAddr(), msg.Type)
		_ = conn.Close()
		if writeErr := session.Framer().WriteJSON(protocol.TypeDataRequest, request); writeErr != nil {
			logging.Warnf(s.logger, "the client cannot be asked for %s over the control connection either: %v", request.Proxy, writeErr)
		}
		return
	}
	var open protocol.DataOpen
	if err := json.Unmarshal(msg.Payload, &open); err != nil {
		s.metrics.poolMissed.Add(1)
		logging.Warnf(s.logger, "a parked connection from %s answered with an unusable data-open: %v", conn.RemoteAddr(), err)
		_ = conn.Close()
		if writeErr := session.Framer().WriteJSON(protocol.TypeDataRequest, request); writeErr != nil {
			logging.Warnf(s.logger, "the client cannot be asked for %s over the control connection either: %v", request.Proxy, writeErr)
		}
		return
	}
	s.metrics.poolUsed.Add(1)
	// The read deadline above was for the answer; the stream manages its own.
	_ = conn.SetDeadline(time.Time{})
	s.serveData(conn, framer, open)
}

// serveData pairs a data connection with the public connection that is waiting
// for the stream it names. The connection was counted where it entered —
// handleData for a direct one, parkDataConn for a pooled one.
func (s *Server) serveData(conn net.Conn, framer *protocol.Framer, open protocol.DataOpen) {

	fail := func(reason string) {
		logging.Warnf(s.logger, "data connection from %s rejected: %s", conn.RemoteAddr(), reason)
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: reason})
		_ = conn.Close()
	}

	session, ok := s.sessions.Get(open.Session)
	if !ok {
		// A well-formed data-open naming a session this server is not holding. The answer
		// is a refusal like any other the server writes, so it is counted: this frame
		// type is dispatched before anything is authenticated, and the log line below was
		// the only trace it used to leave.
		s.metrics.dataUnmatched.Add(1)
		fail("unknown or expired session")
		return
	}

	pending, ok := session.TakePending(open.StreamID)
	if !ok {
		// Counted with the same reason class: the connection could not be paired. A
		// client that cannot reach its own local service is not counted here — it reports
		// that in the frame itself, and the visitor waiting for the stream is told.
		s.metrics.dataUnmatched.Add(1)
		fail("no public connection is waiting for that stream")
		return
	}

	// A client that cannot serve the stream says so before the handshake, so the
	// waiting visitor fails now instead of after its dial timeout.
	if open.Error != "" {
		session.resolveStream(pending, streamResult{err: errors.New(open.Error)})
		fail("the client could not serve the stream: " + open.Error)
		return
	}

	if err := framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
		session.resolveStream(pending, streamResult{err: errors.New("the stream was withdrawn before it was acknowledged")})
		_ = conn.Close()
		return
	}

	// The ack has been written, and both ends now switch to the key derived for
	// this stream, so the bytes that follow never share a keystream with another
	// stream of the same session.
	dc := &dataConn{conn: conn, framer: framer, cipher: s.cipher}
	if len(session.streamKey) > 0 {
		streamKey, keyErr := crypto.StreamKey(session.streamKey, open.StreamID)
		streamCipher, cipherErr := crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, streamKey)
		switch {
		case keyErr != nil || cipherErr != nil:
			logging.Warnf(s.logger, "data connection from %s: cannot derive a stream key: %v%v",
				conn.RemoteAddr(), keyErr, cipherErr)
		default:
			dc.cipher = streamCipher
			dc.framer = protocol.NewFramerWithOptions(conn, streamCipher, s.framerOptions())
		}
	}

	// From here the connection carries the stream — raw bytes for a stream
	// proxy, TypeUDPPacket frames for a datagram proxy. The visitor may have
	// given up while this was being prepared: the session marks that under its
	// lock, so either the connection is queued for the waiter or it is refused
	// here, and it is never left in the buffer with nobody to read it.
	if !session.resolveStream(pending, streamResult{conn: dc}) {
		logging.Warnf(s.logger, "data connection from %s arrived after its stream was given up", conn.RemoteAddr())
		_ = conn.Close()
	}
}

// totalBytes reports the traffic the server has relayed since it started.
//
// It is deliberately not the sum of the registered tunnels: those counters belong to
// a registration and drop to nothing when the last client publishing a proxy
// disconnects, while the number the status panel shows is a running total. The
// per-proxy figures, which do reset with the registration, are on GET /api/proxies.
func (s *Server) totalBytes() (in, out int64) {
	return s.metrics.bytesFromClients.Load(), s.metrics.bytesToClients.Load()
}

// firstBadDomain names the first entry of spec.Domains that is not a usable
// hostname or a "*.suffix" wildcard. The name-routing tables key on these
// strings verbatim, and a registration can arrive without the client's own
// validation in front of it — a hand-written protocol message, an SSH command,
// a plugin's rewritten spec — so the server refuses what it cannot route: a
// domain with stray whitespace or an empty label would publish successfully
// and then be unreachable, or hold a key no Host header can ever match.
func firstBadDomain(spec protocol.ProxySpec) string {
	for _, domain := range spec.Domains {
		if !config.ValidDomain(domain) {
			return domain
		}
	}
	return ""
}
