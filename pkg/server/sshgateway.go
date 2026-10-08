package server

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// sshTunnelGateway is frp's sshTunnelGateway: an SSH server of its own whose
// remote forwardings become tunnels, so a machine that will not run a client
// binary can still publish a service with nothing but ssh:
//
//	ssh -R :80:127.0.0.1:8080 v0@server -p 2200 tcp --proxy_name web --remote_port 9090 --token <server auth_token>
//
// The port in the SSH client's tcpip-forward request is not the published port:
// the gateway answers that request so the client runs its command, and the
// command line it sends names the proxy the server actually publishes. The
// registration goes through Server.registerSSHProxy and therefore through
// TunnelManager.Register, so allow_ports, the [[proxies]] policies,
// max_ports_per_client, the dashboard, the audit log and the metrics all see
// this exactly as they see a client's [[proxies]] entry.
type sshTunnelGateway struct {
	srv    *Server
	cfg    *config.SSHTunnelGatewayConfig
	logger *log.Logger

	signer ssh.Signer
	// authorized maps the wire form of every public key in
	// authorized_keys_file. Empty means no key is listed, so the callback
	// admits any key only when no shared password is configured either.
	authorized map[string]bool

	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closing  atomic.Bool
}

// sshGatewayVisitorAddress is the originator address reported on the
// forwarded-tcpip channels the gateway opens. The visitor's address is not on
// the DataRequest the server sends over the control pipe, so the channel carries
// a placeholder; the ssh client only uses the connected address and port to pick
// which -R rule the channel belongs to.
const sshGatewayVisitorAddress = "127.0.0.1"

// startSSHGateway brings up [server.ssh_tunnel_gateway] when it is enabled, and
// is a no-op otherwise.
func (s *Server) startSSHGateway() error {
	cfg := s.cfg.Server.SSHTunnelGateway
	if cfg == nil || cfg.BindPort <= 0 {
		return nil
	}
	gateway, err := newSSHTunnelGateway(s)
	if err != nil {
		return err
	}
	if err := gateway.start(); err != nil {
		return err
	}
	s.sshGateway = gateway
	return nil
}

// stopAcceptingSSHGateway closes the gateway's listener, so no new SSH
// connection builds a session while the server is winding down. The connections
// it is already carrying stay open: the sessions they own are drained with the
// rest of the server's, and disconnectSSHGateway ends them afterwards.
func (s *Server) stopAcceptingSSHGateway() {
	if s.sshGateway != nil {
		s.sshGateway.stopAccepting()
	}
}

// disconnectSSHGateway closes the SSH connections the gateway is still carrying
// and waits for their handlers to return. Their teardown withdraws the proxies
// they published and writes the last ledger entries, which is why this runs
// inside Shutdown, after the drain, and before Run closes the stores.
func (s *Server) disconnectSSHGateway() {
	if s.sshGateway != nil {
		s.sshGateway.disconnect()
	}
}

func newSSHTunnelGateway(s *Server) (*sshTunnelGateway, error) {
	cfg := s.cfg.Server.SSHTunnelGateway
	signer, err := loadOrCreateHostKey(cfg.KeyPath())
	if err != nil {
		return nil, err
	}
	authorized, err := loadAuthorizedKeys(cfg.AuthorizedKeysFile)
	if err != nil {
		return nil, err
	}
	return &sshTunnelGateway{
		srv:        s,
		cfg:        cfg,
		logger:     s.logger,
		signer:     signer,
		authorized: authorized,
		conns:      make(map[net.Conn]struct{}),
	}, nil
}

func (g *sshTunnelGateway) start() error {
	addr := net.JoinHostPort(g.cfg.BindAddr, strconv.Itoa(g.cfg.BindPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot listen for the ssh tunnel gateway on %s: %w", addr, err)
	}
	g.listener = listener
	g.logger.Printf("ssh tunnel gateway listening on %s (authentication: %s)", listener.Addr(), g.authDescription())
	// The accept loop is counted before it starts, so the count never drops to
	// zero while the loop can still register a connection: a WaitGroup Add that
	// races a Wait on a zero counter is a documented misuse, and the loop below
	// does exactly that Add for every connection it accepts.
	g.wg.Add(1)
	go g.acceptLoop()
	return nil
}

// authDescription names, for the startup line, how a client may authenticate.
// The key half and the password half are exclusive when no key is listed: a
// gateway with a password and no authorized_keys_file admits the password only.
func (g *sshTunnelGateway) authDescription() string {
	password := g.cfg.User != "" && g.cfg.Password != ""
	switch {
	case len(g.authorized) > 0 && password:
		return fmt.Sprintf("%d authorized key(s), shared password", len(g.authorized))
	case len(g.authorized) > 0:
		return fmt.Sprintf("%d authorized key(s)", len(g.authorized))
	case password:
		return "shared password"
	case g.cfg.User == "" && g.cfg.Password == "":
		return "any ssh key"
	default:
		// A user with no password registers neither a key rule nor a password
		// rule, so the gateway admits nobody. Validate refuses such a file; this
		// keeps the startup line honest for one built in memory.
		return "none"
	}
}

// stopAccepting closes the gateway's listener and nothing else. The connections
// already established stay open so the sessions they own take part in the
// server's graceful drain; closing them here would tear those sessions out of the
// registry before Shutdown could wait for their streams, and an SSH tunnel would
// never get the grace period a client's tunnel gets.
func (g *sshTunnelGateway) stopAccepting() {
	if g.closing.Swap(true) {
		return
	}
	if g.listener != nil {
		_ = g.listener.Close()
	}
}

// disconnect closes every SSH connection the gateway is carrying and waits for
// their handlers to return. It is what finally ends the sessions stopAccepting
// left draining, and its wait is the only guarantee that those handlers have
// written their last ledger entries before the stores are closed.
func (g *sshTunnelGateway) disconnect() {
	g.mu.Lock()
	conns := make([]net.Conn, 0, len(g.conns))
	for conn := range g.conns {
		conns = append(conns, conn)
	}
	g.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	g.wg.Wait()
}

func (g *sshTunnelGateway) acceptLoop() {
	defer g.wg.Done()
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			if !g.closing.Load() {
				g.logger.Printf("ssh tunnel gateway: accept error: %v", err)
			}
			return
		}
		// The connection is registered and counted under the lock stop takes for
		// its snapshot. Without that, a connection accepted just before the
		// listener closed was registered after the snapshot, so stop never closed
		// it and its handler kept Wait — and the whole shutdown — blocked on a
		// socket nobody would end.
		g.mu.Lock()
		if g.closing.Load() {
			g.mu.Unlock()
			_ = conn.Close()
			return
		}
		g.conns[conn] = struct{}{}
		g.wg.Add(1)
		g.mu.Unlock()
		go func() {
			defer g.wg.Done()
			defer func() {
				g.mu.Lock()
				delete(g.conns, conn)
				g.mu.Unlock()
			}()
			g.handleConn(conn)
		}()
	}
}

func (g *sshTunnelGateway) serverConfig(nConn net.Conn) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-AetherTunnel",
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if len(g.authorized) == 0 {
				// Nothing was listed, so a key is accepted only when the operator
				// configured no shared password either. A gateway that asks for a
				// password is not also open to any key that turns up: the two
				// credential styles are exclusive, which is what the key's own
				// documentation promises. Publishing still needs --token.
				if g.cfg.User == "" && g.cfg.Password == "" {
					return nil, nil
				}
				return nil, errors.New("this gateway authenticates with the configured password, not with a key")
			}
			if g.authorized[string(key.Marshal())] {
				return nil, nil
			}
			return nil, fmt.Errorf("public key %s is not listed in authorized_keys_file", ssh.FingerprintSHA256(key))
		},
	}
	cfg.AddHostKey(g.signer)
	if g.cfg.User != "" && g.cfg.Password != "" {
		cfg.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == g.cfg.User && crypto.EqualTokens(string(password), g.cfg.Password) {
				return nil, nil
			}
			// A rejected password is an authentication failure like any
			// other, and recordAuthFailure is the only place a ban is
			// imposed and says every way of failing to authenticate counts:
			// the gateway is otherwise a password oracle the ban ladder and
			// the auth-failure counter never hear about.
			g.srv.metrics.authFailures.Add(1)
			g.srv.auditor.Record(AuditEvent{
				Event:   EventAuthFailed,
				Remote:  nConn.RemoteAddr().String(),
				Outcome: "denied",
				Detail:  "ssh gateway password rejected",
			})
			g.srv.recordAuthFailure(nConn)
			return nil, errors.New("password authentication failed")
		}
	}
	return cfg
}

// tokenAccepted checks the command's --token against the server's auth_token in
// constant time. The offered token is never logged.
func (g *sshTunnelGateway) tokenAccepted(token string) bool {
	return crypto.EqualTokens(token, g.srv.cfg.Server.AuthToken)
}

// closeSession tears down the internal session the way a client disconnect does:
// every proxy it published is withdrawn, then the session is removed and closed.
func (g *sshTunnelGateway) closeSession(session *Session, reason string) {
	g.srv.tunnels.RemoveSessionTunnels(session, reason)
	g.srv.sessions.Remove(session.ID)
	session.Close(reason)
	if g.srv.ledger != nil {
		g.srv.waitForStreamsToFinish(session, ledgerSettle)
	}
	g.srv.recordSessionUsage(session)
}

func (g *sshTunnelGateway) handleConn(nConn net.Conn) {
	// The gateway is an entry point like the control port, so the same
	// admission applies before the handshake: a banned source, the
	// allow/deny lists and the per-IP rate limit refuse the connection
	// here the way admit refuses it everywhere else. The gateway section
	// of the configuration documentation promises that publishing rides
	// the same path as a client's proxies; its connections ride the same
	// admission too.
	if !g.srv.admit(nConn) {
		return
	}
	// The SSH handshake is bounded like every other one on this server: without a
	// deadline a peer that connects and says nothing holds a goroutine and an fd
	// for as long as it likes, and enough of them exhaust the gateway before any
	// authentication happens.
	if secs := g.srv.cfg.Server.HandshakeTimeoutSecs; secs > 0 {
		_ = nConn.SetDeadline(time.Now().Add(time.Duration(secs) * time.Second))
	}
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, g.serverConfig(nConn))
	if err != nil {
		logging.Warnf(g.logger, "ssh tunnel gateway: handshake from %s failed: %v", nConn.RemoteAddr(), err)
		return
	}
	// Authentication succeeded, so the source's earlier failed attempts no
	// longer count towards a ban — the control path clears the count the
	// same way once its handshake completes.
	g.srv.recordAuthSuccess(nConn)
	// The session that follows is bounded by its own idle timeout, not by the
	// handshake deadline.
	_ = nConn.SetDeadline(time.Time{})

	c := &sshGatewayConn{gateway: g, conn: sshConn, netConn: nConn, done: make(chan struct{})}
	session, err := g.newSession(sshConn, c)
	if err != nil {
		logging.Warnf(g.logger, "ssh tunnel gateway: cannot open a session for %s: %v", nConn.RemoteAddr(), err)
		_ = sshConn.Close()
		return
	}
	c.session = session
	c.touch()

	defer func() {
		close(c.done)
		g.closeSession(session, "ssh session ended")
		_ = sshConn.Close()
	}()

	g.logger.Printf("ssh tunnel gateway: %s authenticated as %q", nConn.RemoteAddr(), sshConn.User())

	if g.cfg.IdleTimeoutSeconds > 0 {
		go c.idleWatch()
	}
	go c.handleGlobalRequests(reqs)

	for newChannel := range chans {
		c.touch()
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "the ssh tunnel gateway serves session channels only")
			continue
		}
		// A session channel that never runs a command parks its goroutine on
		// the request loop until the whole connection ends, so an
		// authenticated peer could grow the process without bound by opening
		// channels and leaving them idle. The cap sits far above what
		// forwarding needs — each exec carries one proxy — and the peer is
		// told why.
		if !c.channels.reserve(maxSSHSessionChannels) {
			_ = newChannel.Reject(ssh.Prohibited, "too many concurrent session channels on this connection")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			c.channels.release()
			continue
		}
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			defer c.channels.release()
			c.handleSessionChannel(channel, channelRequests)
		}()
	}
}

// publishedSlot is the slots entry of a name whose proxy this connection went
// on to publish: the slot is the published proxy's own, held until the
// connection ends, and no command's failure can release it.
const publishedSlot = -1

// sshGatewayConn is one authenticated SSH connection and the frp session that
// owns the proxies its commands publish.
type sshGatewayConn struct {
	gateway *sshTunnelGateway
	conn    *ssh.ServerConn
	// netConn is the connection under the SSH layer, for the bookkeeping
	// that names the source the way the control and visitor paths do
	// (recordAuthFailure takes a net.Conn).
	netConn net.Conn
	session *Session

	mu       sync.Mutex
	forwards []sshForward
	channels channelGate
	// pendingForwards caps the forwarded-tcpip channels this connection is
	// still waiting for the client to answer — the gate the client-side
	// openChannel has no deadline for. See sshDataResponder.serve.
	pendingForwards channelGate
	// slots records, per name, how the one max_proxies slot that name answers
	// for is held: a positive count is the shares the commands still
	// registering it have pinned between them, and publishedSlot is the slot
	// of the proxy the name has published, which no later failure of its twin
	// commands can release. Names rather than a bare count, because a second
	// exec channel naming a proxy this one is still publishing has to be
	// recognized as a replacement; a counter rather than a set, because two
	// concurrent commands naming a proxy that is still registering pin that
	// slot between them — with a set, the first failure deleted it and the
	// surviving command went on to publish a proxy that held no slot at all.
	// Guarded by mu.
	slots map[string]int
	last  atomic.Int64
	done  chan struct{}
}

// sshForward is one tcpip-forward a client asked for. The address and port are
// echoed on the forwarded-tcpip channels opened for it, which is how the ssh
// client matches the channel to the -R rule that says where to dial.
type sshForward struct {
	addr string
	port uint32
}

func (c *sshGatewayConn) touch() { c.last.Store(time.Now().UnixNano()) }

func (c *sshGatewayConn) forward() (string, uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.forwards) == 0 {
		return "", 0, errors.New("the ssh client did not request a remote forwarding")
	}
	return c.forwards[0].addr, c.forwards[0].port, nil
}

// idleWatch closes a quiet SSH connection after idle_timeout_seconds.
func (c *sshGatewayConn) idleWatch() {
	idle := time.Duration(c.gateway.cfg.IdleTimeoutSeconds) * time.Second
	interval := idle / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, c.last.Load())) >= idle {
				c.gateway.logger.Printf("ssh tunnel gateway: closing %s after %s of inactivity",
					c.conn.RemoteAddr(), idle)
				_ = c.conn.Close()
				return
			}
		}
	}
}

// handleGlobalRequests answers the SSH global requests. tcpip-forward is the
// one that matters: the client refuses to run its command unless it is accepted.
func (c *sshGatewayConn) handleGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		c.touch()
		switch req.Type {
		case "tcpip-forward":
			c.handleTCPIPForward(req)
		case "cancel-tcpip-forward":
			c.cancelTCPIPForward(req)
		case "keepalive@openssh.com":
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func (c *sshGatewayConn) handleTCPIPForward(req *ssh.Request) {
	var requested struct {
		Addr string
		Port uint32
	}
	if err := ssh.Unmarshal(req.Payload, &requested); err != nil {
		_ = req.Reply(false, nil)
		return
	}

	port := requested.Port
	var reply []byte
	if port == 0 {
		// RFC 4254 requires the server to pick a port and return it when the
		// client asks for 0. Nothing is bound here (the published port comes
		// from the command line), so a free number is reserved and released;
		// only its value is used, to match the channel back to the rule.
		reserved, err := reserveEphemeralPort()
		if err != nil {
			_ = req.Reply(false, nil)
			return
		}
		port = uint32(reserved)
		reply = ssh.Marshal(struct{ Port uint32 }{port})
	}

	c.mu.Lock()
	// Only the first rule is kept: forward() publishes exactly that one, and a
	// peer that can complete the SSH handshake must not be able to grow this
	// slice — and with it the server's heap — with an unbounded stream of
	// tcpip-forward requests that carry an address of its choosing.
	ignored := len(c.forwards) > 0
	if !ignored {
		c.forwards = append(c.forwards, sshForward{addr: requested.Addr, port: port})
	}
	c.mu.Unlock()
	if ignored {
		// The address is the peer's own bytes, so it is quoted: a raw NUL
		// inside it would end up in the log line, where the level writer
		// would have to trust it as data (the marker position decides, but
		// a quoted rendering is what a log owes an arbitrary string).
		c.gateway.logger.Printf("ssh tunnel gateway: %s asked to forward %q:%d, which is not the rule this session publishes; the request is accepted and ignored",
			c.conn.RemoteAddr(), requested.Addr, port)
	}
	_ = req.Reply(true, reply)
}

// cancelTCPIPForward drops the rule the request names. The published port comes
// from the command line, so a cancelled rule means this session has nothing left
// to publish and forward() reports that.
func (c *sshGatewayConn) cancelTCPIPForward(req *ssh.Request) {
	var requested struct {
		Addr string
		Port uint32
	}
	if err := ssh.Unmarshal(req.Payload, &requested); err != nil {
		_ = req.Reply(false, nil)
		return
	}
	c.mu.Lock()
	kept := c.forwards[:0]
	for _, forward := range c.forwards {
		if forward.addr == requested.Addr && forward.port == requested.Port {
			continue
		}
		kept = append(kept, forward)
	}
	c.forwards = kept
	c.mu.Unlock()
	_ = req.Reply(true, nil)
}

func reserveEphemeralPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (c *sshGatewayConn) handleSessionChannel(channel ssh.Channel, reqs <-chan *ssh.Request) {
	defer channel.Close()
	for req := range reqs {
		c.touch()
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)
			c.runCommand(channel, payload.Command)
			return
		case "shell":
			_ = req.Reply(true, nil)
			fmt.Fprint(channel, sshGatewayUsage)
			_ = c.conn.Close()
			return
		case "pty-req", "env", "window-change", "signal":
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// runCommand parses the command the SSH client exec'd and publishes the proxy it
// names. A parse error, a token mismatch or a refused registration is written
// back and the session is left usable: the connection and its remote forwarding
// stay up, so the operator can read the reason and try again.
func (c *sshGatewayConn) runCommand(channel ssh.Channel, command string) {
	cmd, err := parseSSHGatewayCommand(command)
	if errors.Is(err, errSSHGatewayUsage) {
		fmt.Fprint(channel, sshGatewayUsage)
		_ = channel.Close()
		_ = c.conn.Close()
		return
	}
	if err != nil {
		fmt.Fprintf(channel, "%v\n", err)
		c.waitDone()
		return
	}

	if !c.gateway.tokenAccepted(cmd.flag["token"]) {
		// A wrong --token is a failed credential check after the SSH auth,
		// and it is one the peer can repeat within the session, so it feeds
		// the ban ladder like every other way of failing to authenticate —
		// the auth-failure counter's own text names "a wrong token". The
		// session and its forwards stay up, as the documentation promises.
		c.gateway.srv.metrics.authFailures.Add(1)
		c.gateway.srv.auditor.Record(AuditEvent{
			Event:   EventAuthFailed,
			Remote:  c.netConn.RemoteAddr().String(),
			Outcome: "denied",
			Detail:  "ssh gateway exec token rejected",
		})
		c.gateway.srv.recordAuthFailure(c.netConn)
		fmt.Fprintln(channel, "authentication failed: --token does not match the server's auth_token")
		c.waitDone()
		return
	}

	spec, err := cmd.proxySpec()
	if err != nil {
		fmt.Fprintf(channel, "%v\n", err)
		c.waitDone()
		return
	}

	// reserved records that this attempt took one of the session's max_proxies
	// slots, so the failure branch below can give it back.
	reserved := false
	if max := c.gateway.cfg.MaxProxies; max > 0 {
		var over bool
		over, reserved = c.reserveProxySlot(spec.Name, max)
		if over {
			fmt.Fprintf(channel, "this ssh session has published its maximum of %d proxies\n", max)
			c.waitDone()
			return
		}
	}

	tunnel, err := c.gateway.srv.registerSSHProxy(c.session, spec)
	if err != nil {
		if reserved {
			// The slot goes back here, not from a deferred call: waitDone holds
			// this function open until the connection ends, so a defer kept the
			// slot of a registration that never happened for the session's whole
			// life — with max_proxies = 1 every later attempt was refused as over
			// the cap, including the retry the refusal invites. Nothing else
			// releases one: a registration that succeeded holds its slot until
			// the connection ends, when the counter dies with the connection.
			c.releaseProxySlot(spec.Name)
		}
		fmt.Fprintf(channel, "%v\n", err)
		c.waitDone()
		return
	}

	// The proxy is published: its slot is its own from here on, whatever the
	// twin that named it while both were still registering ends up doing.
	c.holdPublishedSlot(spec.Name)

	fmt.Fprintf(channel, "User: %s\nProxyName: %s\nType: %s\nRemoteAddress: %s\n",
		c.conn.User(), tunnel.Name, tunnel.Type, remoteAddressOf(tunnel))
	c.waitDone()
}

// reserveProxySlot takes a share of the connection's max_proxies budget for
// name. over reports that the cap refuses the command; taken reports that this
// call holds a share it must give back if its registration fails. A command
// naming a proxy this connection already publishes replaces that registration
// and takes nothing; a second concurrent command naming a proxy that is still
// registering pins the slot the first command took — either one failing gives
// back only its own pin, and the slot stays with the command that finally
// publishes the name (or is still registering it).
//
// The name check and the count share one critical section. Apart, two exec
// channels on the same connection naming the same proxy each saw it as a new
// registration before either had reserved anything, and the one proxy they went
// on to publish between them held two slots until the connection ended.
func (c *sshGatewayConn) reserveProxySlot(name string, max int) (over, taken bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sessionHoldsName(c.session, name) {
		return false, false
	}
	if n := c.slots[name]; n > 0 {
		c.slots[name] = n + 1
		return false, true
	}
	// A name this connection has published never reaches the pin branch — it
	// returned at sessionHoldsName above — so a share is only ever pinned
	// while the proxy is still registering. The cap counts distinct names,
	// not pins: a published proxy's publishedSlot entry is one name like any
	// other and holds its slot.
	if len(c.slots) >= max {
		return true, false
	}
	if c.slots == nil {
		c.slots = make(map[string]int)
	}
	c.slots[name] = 1
	return false, true
}

// releaseProxySlot gives back the share of name's slot that a failed
// registration held. While other pins remain it only lowers the count; the
// last pin deletes the entry; a published slot is left untouched, because the
// twin's failure must not release the slot of the proxy that did publish —
// that is exactly the leak the counter exists to close.
func (c *sshGatewayConn) releaseProxySlot(name string) {
	c.mu.Lock()
	if n := c.slots[name]; n > 1 {
		c.slots[name] = n - 1
	} else if n == 1 {
		delete(c.slots, name)
	}
	c.mu.Unlock()
}

// holdPublishedSlot turns the name's slot into the published proxy's own. A
// success is a publication: whatever shares twin commands pinned while the
// registration was in flight stop counting, because the slot now belongs to a
// proxy that exists — a twin that fails afterwards releases at most its own
// share, and the entry it finds is no longer a count to lower.
func (c *sshGatewayConn) holdPublishedSlot(name string) {
	c.mu.Lock()
	// The map exists only once a reserve ran; with no max_proxies configured
	// no command ever pinned anything, and this is the first write the
	// connection makes — the lazy init mirrors reserveProxySlot's.
	if c.slots == nil {
		c.slots = make(map[string]int)
	}
	c.slots[name] = publishedSlot
	c.mu.Unlock()
}

// waitDone holds an exec channel open until the SSH connection ends, which is
// what keeps the remote forwarding alive for as long as the client wants it.
func (c *sshGatewayConn) waitDone() { <-c.done }

// remoteAddressOf names what a visitor dials for a published proxy: the bound
// address, or the hostnames an http/https proxy answers on.
func remoteAddressOf(tunnel *Tunnel) string {
	if addr := tunnel.Addr(); addr != "" {
		return addr
	}
	if tunnel.group != nil {
		return strings.Join(tunnel.group.Domains, ",")
	}
	return ""
}

// registerSSHProxy publishes one proxy for an SSH gateway session through the
// same path a client's register-proxy frame takes. It repeats the pre-checks the
// control loop does before TunnelManager.Register (subdomain composition and
// allow_ports) so every rule sees the request; the registration itself is that
// one call.
func (s *Server) registerSSHProxy(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	// Every refusal is written to the server log and the audit trail before it
	// is returned, the way the client path's register-proxy branch does: an SSH
	// client publishing through the gateway meets the same rules as a
	// [[proxies]] entry, and an operator has to be able to see who tried what.
	refuse := func(err error) (*Tunnel, error) {
		logging.Warnf(s.logger, "client %s: cannot register proxy %q over the ssh gateway: %v", session.ID, spec.Name, err)
		s.auditor.Record(AuditEvent{
			Event: EventProxyRejected, ClientID: session.ClientName(), Proxy: spec.Name,
			Outcome: "denied", Detail: err.Error(),
		})
		return nil, err
	}
	if spec.Subdomain != "" {
		if s.cfg.Server.SubdomainHost == "" {
			return refuse(fmt.Errorf("proxy %q: a subdomain needs [server].subdomain_host on the server", spec.Name))
		}
		label := strings.ToLower(spec.Subdomain)
		if err := config.ValidateDNSLabel(label); err != nil {
			return refuse(fmt.Errorf("proxy %q: subdomain %q: %v", spec.Name, spec.Subdomain, err))
		}
		spec.Domains = append(spec.Domains, label+"."+s.cfg.Server.SubdomainHost)
	}
	if bad := firstBadDomain(spec); bad != "" {
		return refuse(fmt.Errorf("proxy %q: domain %q is not a usable hostname or \"*.suffix\" wildcard", spec.Name, bad))
	}
	if spec.RemotePort != 0 && !s.remotePorts.Contains(spec.RemotePort) {
		return refuse(fmt.Errorf("remote port %d is outside allow_ports", spec.RemotePort))
	}

	tunnel, err := s.tunnels.Register(session, spec)
	if err != nil {
		return refuse(err)
	}
	if err := session.RegisterProxy(tunnel); err != nil {
		s.tunnels.Unregister(spec.Name, session, "session closed during registration")
		if tunnel.createdUsage && !tunnel.reachable.Load() {
			// The registration created the session's counter for this name and
			// its endpoint never came up, so the ledger must not bill an empty
			// entry for it at teardown. A name that was reachable keeps its
			// counter: a stream may still be holding it. The client path does
			// the same in handleControl's register-proxy branch.
			session.forgetUsage(spec.Name)
		}
		return refuse(err)
	}
	s.auditor.Record(AuditEvent{
		Event: EventProxyRegistered, ClientID: session.ID, Proxy: spec.Name,
		Outcome: "ok",
		Detail:  fmt.Sprintf("type=%s local=%s remote_port=%d via ssh gateway", spec.Type, spec.LocalAddr, spec.RemotePort),
	})
	return tunnel, nil
}

// --- the host key -------------------------------------------------------------

// loadOrCreateHostKey returns the SSH host key at path, generating one when the
// file is absent or empty. A generated key is written where KeyPath() points so
// it survives a restart and a client that pinned it keeps working. The parent
// directory is created when it is missing, so a configured path under a
// directory of the operator's own works on first use; the directory is 0700 and
// the key file is 0600.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil && len(data) > 0:
			signer, parseErr := ssh.ParsePrivateKey(data)
			if parseErr != nil {
				return nil, fmt.Errorf("ssh gateway host key %s cannot be parsed: %w", path, parseErr)
			}
			return signer, nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("ssh gateway host key %s cannot be read: %w", path, err)
		}
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ssh gateway: cannot generate a host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("ssh gateway: cannot use the generated host key: %w", err)
	}
	if path == "" {
		return signer, nil
	}

	block, err := ssh.MarshalPrivateKey(private, "aethertunnel ssh tunnel gateway host key")
	if err != nil {
		return nil, fmt.Errorf("ssh gateway: cannot encode the generated host key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ssh gateway: cannot create %s for the host key: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("ssh gateway: cannot write the host key to %s: %w", path, err)
	}
	return signer, nil
}

// loadAuthorizedKeys reads an authorized_keys file. Malformed lines are skipped,
// so one bad line does not lock an operator out; a file with no usable key is an
// error rather than a gateway that accepts every key.
func loadAuthorizedKeys(path string) (map[string]bool, error) {
	keys := map[string]bool{}
	if path == "" {
		return keys, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh gateway authorized_keys_file %s cannot be read: %w", path, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		keys[string(key.Marshal())] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ssh gateway authorized_keys_file %s cannot be read: %w", path, err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ssh gateway authorized_keys_file %s holds no usable public key", path)
	}
	return keys, nil
}

// --- the command line ---------------------------------------------------------

// errSSHGatewayUsage reports that the command asked for the help text (or named
// no proxy type), which prints the accepted flags and ends the session.
var errSSHGatewayUsage = errors.New("usage")

// sshGatewayUsage is the help text a gateway session prints. tcpmux and socks5 are
// not among the types: each needs a key this command line does not carry
// (--multiplexer, --allow_targets), so the registration is refused and advertising
// them would send an operator looking for the mistake in their command.
const sshGatewayUsage = `Usage: ssh -R :80:127.0.0.1:8080 <user>@<host> -p <port> <type> [flags]

Publish a service through the ssh tunnel gateway. <type> is one of:
tcp, udp, http, https, stcp, sudp, xtcp.

tcpmux and socks5 are not accepted here: publish them from a client
configuration file, which carries the keys they need.

Flags:
  --proxy_name NAME        the name the proxy is published under
  --remote_port PORT       the public port to publish on (tcp, udp)
  --local_ip IP            address of the local service (default 127.0.0.1)
  --local_port PORT        port of the local service
  --custom_domain LIST     comma-separated hostnames (http, https)
  --custom_domains LIST    the same as --custom_domain
  --subdomain NAME         subdomain of server.subdomain_host (http, https)
  --secret_key KEY         secret of a private proxy (stcp, sudp, xtcp)
  --auth_method METHOD     how a visitor proves the secret: secret, nizk, snark
  --group NAME             pool with other clients publishing the same name
  --token TOKEN            the server's auth_token
  --allow_users LIST       identities allowed to visit a private proxy
  --bandwidth LIMIT        accepted for compatibility with frp's command line
  --help                   print this help and close the session
`

// sshGatewayAcceptedFlags is every flag the gateway understands. It is both the
// parser's allowlist and the text an unknown flag is refused with.
var sshGatewayAcceptedFlags = []string{
	"proxy_name", "remote_port", "local_ip", "local_port",
	"custom_domain", "custom_domains", "subdomain",
	"secret_key", "auth_method", "group", "token", "allow_users", "bandwidth",
}

func sshGatewayAcceptedText() string {
	out := make([]string, 0, len(sshGatewayAcceptedFlags))
	for _, name := range sshGatewayAcceptedFlags {
		out = append(out, "--"+name)
	}
	return strings.Join(out, ", ")
}

var sshGatewayFlagSet = func() map[string]bool {
	set := make(map[string]bool, len(sshGatewayAcceptedFlags))
	for _, name := range sshGatewayAcceptedFlags {
		set[name] = true
	}
	return set
}()

// sshGatewayCommand is one parsed command line.
type sshGatewayCommand struct {
	proxyType string
	flag      map[string]string
}

// parseSSHGatewayCommand reads the command the client sent on its session
// channel: the first word is the proxy type, the rest are --flag value pairs
// (--flag=value is accepted too).
func parseSSHGatewayCommand(command string) (*sshGatewayCommand, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, errSSHGatewayUsage
	}
	name := fields[0]
	if name == "--help" || name == "-h" || name == "help" {
		return nil, errSSHGatewayUsage
	}
	if !config.IsProxyType(name) {
		return nil, fmt.Errorf("proxy type %q is not one of %s", name, strings.Join(config.ProxyTypes, ", "))
	}

	cmd := &sshGatewayCommand{proxyType: name, flag: map[string]string{}}
	rest := fields[1:]
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if !strings.HasPrefix(token, "--") {
			return nil, fmt.Errorf("unexpected argument %q: flags are written --name value (accepted: %s)",
				token, sshGatewayAcceptedText())
		}
		flagName := strings.TrimPrefix(token, "--")
		value := ""
		hasValue := false
		if eq := strings.IndexByte(flagName, '='); eq >= 0 {
			value, flagName, hasValue = flagName[eq+1:], flagName[:eq], true
		}
		if flagName == "help" {
			return nil, errSSHGatewayUsage
		}
		if !sshGatewayFlagSet[flagName] {
			return nil, fmt.Errorf("unknown flag \"--%s\" (accepted: %s)", flagName, sshGatewayAcceptedText())
		}
		if !hasValue {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("flag \"--%s\" needs a value (accepted: %s)", flagName, sshGatewayAcceptedText())
			}
			value = rest[i+1]
			i++
		}
		cmd.flag[flagName] = value
	}
	return cmd, nil
}

// proxySpec turns a parsed command into the ProxySpec a client's register frame
// would carry. The local address is informational for the stream types: the ssh
// client dials the target named on its own -R rule.
func (c *sshGatewayCommand) proxySpec() (protocol.ProxySpec, error) {
	spec := protocol.ProxySpec{
		Type:       c.proxyType,
		Name:       c.flag["proxy_name"],
		SecretKey:  c.flag["secret_key"],
		AuthMethod: c.flag["auth_method"],
		Group:      c.flag["group"],
		Subdomain:  c.flag["subdomain"],
	}

	if value := c.flag["remote_port"]; value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 0 || port > 65535 {
			return spec, fmt.Errorf("--remote_port %q is not a port number (accepted: %s)", value, sshGatewayAcceptedText())
		}
		spec.RemotePort = port
	}

	localIP := c.flag["local_ip"]
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	if localPort := c.flag["local_port"]; localPort != "" {
		if _, err := strconv.Atoi(localPort); err != nil {
			return spec, fmt.Errorf("--local_port %q is not a port number (accepted: %s)", localPort, sshGatewayAcceptedText())
		}
		spec.LocalAddr = net.JoinHostPort(localIP, localPort)
	} else if c.flag["local_ip"] != "" {
		spec.LocalAddr = localIP
	}

	domains := c.flag["custom_domains"]
	if domains == "" {
		domains = c.flag["custom_domain"]
	}
	for _, domain := range strings.Split(domains, ",") {
		if domain = strings.TrimSpace(domain); domain != "" {
			spec.Domains = append(spec.Domains, domain)
		}
	}
	for _, user := range strings.Split(c.flag["allow_users"], ",") {
		if user = strings.TrimSpace(user); user != "" {
			spec.AllowUsers = append(spec.AllowUsers, user)
		}
	}
	// --bandwidth is accepted so an frp command line parses unchanged; the
	// server's own metering and the bandwidth ledger account for the tunnel.
	return spec, nil
}

// maxSSHSessionChannels caps the session channels one SSH connection may hold
// open at once.
const maxSSHSessionChannels = 256

// channelGate caps how much one connection may have in flight.
type channelGate struct {
	live atomic.Int64
}

// reserve takes one slot, reporting false when the limit is reached (the
// failed attempt does not hold a slot).
func (g *channelGate) reserve(limit int64) bool {
	if g.live.Add(1) > limit {
		g.live.Add(-1)
		return false
	}
	return true
}

// release returns one slot.
func (g *channelGate) release() { g.live.Add(-1) }
