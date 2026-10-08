package clientlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/snarkauth"
	"github.com/aethertunnel/aethertunnel/pkg/webrtcvisitor"
)

// visitorPath is one way into a private proxy: either a relayed stream through
// the server or a direct path punched to the owner.
type visitorPath struct {
	// conn is the connection that carries the session. For a direct path it is
	// the punched stream; otherwise the server connection.
	conn net.Conn
	// stream is the byte-stream view of conn with the record layer applied. It
	// is used for stcp and xtcp.
	stream net.Conn
	// framer carries datagrams for sudp.
	framer    *protocol.Framer
	datagrams bool
	direct    bool
	// teardown releases what the path holds beyond its connection, which is the
	// peer connection behind a WebRTC data channel. It runs when the session ends:
	// the channel is alive for as long as the caller is.
	teardown func()

	once sync.Once
}

func (p *visitorPath) close() {
	p.once.Do(func() {
		if p.conn != nil {
			_ = p.conn.Close()
		}
		if p.teardown != nil {
			p.teardown()
		}
	})
}

// runVisitors starts the local listener for every [[visitors]] entry. Visitor
// listeners are independent of the control session: each visiting connection
// opens its own connection to the server, so a visitor needs no session of its
// own and reconnects per connection.
func (c *client) runVisitors(ctx context.Context) {
	for _, visitor := range c.visitorList() {
		visitor := visitor
		go c.serveVisitor(ctx, visitor)
	}
}

// startVisitors starts the visitor listeners on the context a reload can
// replace: cancelling it closes every listener and lets a changed set come up
// in its place.
//
// The listeners are started once per context. A reload that lands before the
// first session — the admin server and a DHT bootstrap can hold Run up for
// seconds — has already started that context's set, and starting it again would
// bind every visitor port a second time, which fails on all of them.
func (c *client) startVisitors() {
	c.visitorsMu.Lock()
	if c.visitorsCtx == nil {
		c.visitorsCtx, c.visitorsCancel = context.WithCancel(c.baseCtx)
	}
	if c.visitorsStarted {
		c.visitorsMu.Unlock()
		return
	}
	c.visitorsStarted = true
	ctx := c.visitorsCtx
	c.visitorsMu.Unlock()
	c.runVisitors(ctx)
}

// listenForVisitor binds a visitor's listen address. A reload that restarts
// the set races its own old listeners: each one closes on cancellation, but
// the close lands a moment after the new bind attempt, so a few short retries
// absorb the overlap instead of failing the visitor.
func (c *client) listenForVisitor(cfg config.VisitorConfig) (net.Listener, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-c.baseCtx.Done():
				return nil, c.baseCtx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		listener, err := net.Listen("tcp", cfg.ListenAddr())
		if err == nil {
			return listener, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// listenPacketForVisitor is listenForVisitor for a datagram visitor's UDP
// socket, with the same reload-rebind tolerance.
func (c *client) listenPacketForVisitor(cfg config.VisitorConfig) (net.PacketConn, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-c.baseCtx.Done():
				return nil, c.baseCtx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		socket, err := net.ListenPacket("udp", cfg.ListenAddr())
		if err == nil {
			return socket, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *client) serveVisitor(ctx context.Context, cfg config.VisitorConfig) {
	if config.IsDatagramProxyType(cfg.Type) {
		c.serveUDPVisitor(ctx, cfg)
		return
	}
	c.serveStreamVisitor(ctx, cfg)
}

func (c *client) serveStreamVisitor(ctx context.Context, cfg config.VisitorConfig) {
	listener, err := c.listenForVisitor(cfg)
	if err != nil {
		c.logger.Printf("visitor %q: %v", cfg.Name, flynet.ListenError(cfg.ListenAddr(), err))
		return
	}
	c.logger.Printf("visitor %q (%s) listening on %s -> %s/%s",
		cfg.Name, cfg.Type, listener.Addr(), c.serverAddr(), cfg.ServerName)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			// A closed listener is the normal end (reload or shutdown). Any
			// other failure is logged and retried after a short pause: a
			// single transient Accept error, an fd limit under load for
			// example, must not kill the visitor silently and leave a port
			// that keeps completing TCP handshakes into a backlog nothing
			// ever services.
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			c.logger.Printf("visitor %q: accept failed, retrying: %v", cfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		go c.handleVisitorTCP(ctx, cfg, conn)
	}
}

func (c *client) handleVisitorTCP(ctx context.Context, cfg config.VisitorConfig, local net.Conn) {
	// visitor.fallback_timeout_ms bounds the tunnel attempt when a fallback is
	// configured, so a caller is not left waiting for a tunnel that is not
	// coming before it is sent straight to the fallback address.
	openCtx := ctx
	if cfg.FallbackTo != "" && cfg.FallbackTimeoutMs > 0 {
		var cancel context.CancelFunc
		openCtx, cancel = context.WithTimeout(ctx, time.Duration(cfg.FallbackTimeoutMs)*time.Millisecond)
		defer cancel()
	}

	path, err := c.openVisitorPath(openCtx, cfg)
	if err != nil {
		if cfg.FallbackTo == "" {
			c.logger.Printf("visitor %q: %v", cfg.Name, err)
			_ = local.Close()
			return
		}
		c.serveVisitorFallback(ctx, cfg, local, err)
		return
	}
	defer path.close()

	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toRemote, fromRemote := flynet.Pipe(local, path.stream, idle)
	c.logger.Printf("visitor %q: session finished (sent %d, received %d, direct %v)",
		cfg.Name, toRemote, fromRemote, path.direct)
}

// fallbackDialer dials visitor.fallback_to. The fallback is a local service, so
// the dial carries none of the server-facing dialer's source-address or resolver
// settings: those describe how to reach the server, and applying them here made
// the fallback unreachable whenever connect_server_local_ip named an address that
// cannot reach the fallback.
func (c *client) fallbackDialer() *net.Dialer {
	return &net.Dialer{Timeout: time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second}
}

// serveVisitorFallback connects an accepted caller straight to
// visitor.fallback_to after the tunnel could not be opened, which is frp's
// fallbackTo. The caller is told nothing about the change of path: from its
// side this is the visitor it dialled.
func (c *client) serveVisitorFallback(ctx context.Context, cfg config.VisitorConfig, local net.Conn, tunnelErr error) {
	// The fallback is a local service address, so it is dialled the plain way
	// dialForProxy reaches a proxy's local service. The client's server-facing
	// dialer bound the source to connect_server_local_ip — an address chosen for
	// reaching the server — and the fallback then failed on a machine where that
	// address cannot reach the fallback's address, which is exactly the caller
	// this path exists to serve.
	dialer := c.fallbackDialer()
	fallback, err := dialer.DialContext(ctx, "tcp", cfg.FallbackTo)
	if err != nil {
		logging.Warnf(c.logger, "visitor %q: %v; the fallback %s did not answer either: %v",
			cfg.Name, tunnelErr, cfg.FallbackTo, err)
		_ = local.Close()
		return
	}
	c.logger.Printf("visitor %q: %v; connecting this caller to %s instead",
		cfg.Name, tunnelErr, cfg.FallbackTo)

	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toFallback, fromFallback := flynet.Pipe(local, fallback, idle)
	c.logger.Printf("visitor %q: fallback session finished (sent %d, received %d)",
		cfg.Name, toFallback, fromFallback)
}

// serveUDPVisitor listens for datagrams and gives every source address its own
// visitor connection, so the far end sees one stream per address the same way a
// udp proxy on the server does.
func (c *client) serveUDPVisitor(ctx context.Context, cfg config.VisitorConfig) {
	socket, err := c.listenPacketForVisitor(cfg)
	if err != nil {
		c.logger.Printf("visitor %q: %v", cfg.Name, flynet.ListenError(cfg.ListenAddr(), err))
		return
	}
	c.logger.Printf("visitor %q (%s) listening on %s -> %s/%s",
		cfg.Name, cfg.Type, socket.LocalAddr(), c.serverAddr(), cfg.ServerName)

	pump := &flynet.DatagramPump{
		Socket: socket,
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			path, err := c.openVisitorPath(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return path.framer, path.close, nil
		},
		IdleTimeout: time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second,
		MaxDatagram: c.cfg.Client.UDPPacketSize,
		Logger:      c.logger,
	}
	pump.Start()

	<-ctx.Done()
	_ = socket.Close()
	pump.Shutdown()
}

// openVisitorPath establishes one session into a private proxy.
// webrtcAnswerWait bounds the wait for the server's WebRTC answer: its own
// accept allows the ICE exchange 30 seconds, and the extra margin here keeps a
// slow gather from timing the visitor out first. A server that dies instead of
// answering must not leave the visitor blocked forever.
const webrtcAnswerWait = 45 * time.Second

func (c *client) openVisitorPath(ctx context.Context, cfg config.VisitorConfig) (*visitorPath, error) {
	conn, framer, kexState, presentedToken, err := c.dialVisitor(ctx, cfg)
	if err != nil {
		return nil, err
	}

	offer, ready, err := c.awaitVisitorReady(conn, framer, ctx, cfg, kexState, presentedToken, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	framer = ready.framer
	cipher := ready.cipher
	// dialVisitor set one deadline for the whole exchange, and only the read side
	// has been replaced since. A punch attempt can outlast the dial timeout, so
	// the fallback and the offer writes would fail with i/o timeout on the very
	// path that exists to recover from a failed punch.
	_ = conn.SetWriteDeadline(time.Time{})

	if offer != nil {
		direct, err := c.tryPunch(ctx, conn, framer, *offer)
		if err == nil {
			// The relayed connection is not needed once the direct path is up.
			// The server is told which path was taken, because from here on it
			// sees this visitor stop using the connection and nothing else.
			go c.reportPath(offer.Token, protocol.PunchPathDirect)
			_ = conn.Close()
			return &visitorPath{conn: direct, stream: c.wrapWith(direct, c.cipher.Load()), direct: true}, nil
		}
		c.logger.Printf("visitor %q: %v; falling back to the relayed path", cfg.Name, err)

		if err := framer.WriteJSON(protocol.TypeP2PFallback,
			protocol.P2PFallback{Proxy: cfg.ServerName}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("ask for the relayed path: %w", err)
		}
		// The cipher that first exchange agreed has to be carried over: the
		// server sends its key-agreement response in one frame only, so the ack
		// that answers the fallback carries none, and re-reading it from nothing
		// would wrap the relayed stream in the configured cipher while the server
		// uses the agreed key.
		if _, ready, err = c.awaitVisitorReady(conn, framer, ctx, cfg, kexState, presentedToken, ready.cipher); err != nil {
			_ = conn.Close()
			return nil, err
		}
		framer, cipher = ready.framer, ready.cipher
	}

	if cfg.Transport == config.TransportWebRTC {
		// The control connection carried the authentication and will carry the
		// signaling; the data itself moves onto a WebRTC DataChannel.
		offerSDP, accept, teardown, err := webrtcvisitor.VisitorOffer()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		// The peer connection has to outlive this function: it is what the data
		// channel rides on, and the caller keeps it until the session ends. A
		// deferred teardown here closed it on return, so the path handed back was
		// already dead — the server logged a channel established and not one byte
		// crossed it. The teardown travels with the path instead, and this flag is
		// what releases it on the error paths below.
		sessionTeardown := true
		defer func() {
			if sessionTeardown {
				teardown()
			}
		}()
		if err := framer.WriteJSON(protocol.TypeVisitorWebRTCOffer, protocol.VisitorWebRTCOffer{
			Proxy: cfg.ServerName, SDP: offerSDP,
		}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("send the WebRTC offer: %w", err)
		}
		// The server answers once its ICE negotiation finishes, which its own
		// accept bounds; a server that dies instead of answering must not
		// leave this read blocked forever.
		_ = conn.SetReadDeadline(time.Now().Add(webrtcAnswerWait))
		var answer protocol.VisitorWebRTCAnswer
		if err := framer.ReadJSON(protocol.TypeVisitorWebRTCAnswer, &answer); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("read the WebRTC answer: %w", err)
		}
		dataPath, err := accept(answer.SDP)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		sessionTeardown = false
		// The control connection carried only signaling from here on.
		_ = conn.Close()
		return &visitorPath{conn: dataPath, stream: dataPath, direct: true, teardown: teardown}, nil
	}

	return &visitorPath{
		conn:      conn,
		framer:    framer,
		stream:    c.wrapWith(conn, cipher),
		datagrams: config.IsDatagramProxyType(cfg.Type),
	}, nil
}

// visitorReadDeadline is how long one read of the visitor exchange may wait: the
// dial timeout, or the caller's own window when it is shorter. fallback_timeout_ms
// bounds the whole attempt, and a server that accepts the connection and then stalls
// the handshake used to hold the caller for the dial timeout past that window.
func (c *client) visitorReadDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second)
	if ctx != nil {
		if own, ok := ctx.Deadline(); ok && own.Before(deadline) {
			return own
		}
	}
	return deadline
}

// visitorReady is the state a visitor connection is in once the server has
// accepted it.
type visitorReady struct {
	framer *protocol.Framer
	cipher *crypto.Cipher
}

// awaitVisitorReady reads frames until the server accepts the visitor connection
// or offers a punch token, answering a proof-of-knowledge challenge on the way.
//
// It also performs the post-quantum key agreement: the server puts its response
// in the first frame it sends, and both ends then switch to the agreed key. The
// switch point is exactly here, which is what makes the two sides agree on it.
// current is the cipher the caller has already agreed on this connection, or nil
// on the first call. It is carried in rather than derived here because the key
// agreement happens once: a second call would otherwise fall back to the
// configured cipher and lose the agreed key.
func (c *client) awaitVisitorReady(conn net.Conn, framer *protocol.Framer, ctx context.Context, cfg config.VisitorConfig, kexState []byte, presentedToken string, current *crypto.Cipher) (*protocol.P2PPeer, visitorReady, error) {
	_ = conn.SetReadDeadline(c.visitorReadDeadline(ctx))

	cipher := current
	if cipher == nil {
		cipher = c.cipher.Load()
	}
	building := visitorReady{framer: framer, cipher: cipher}

	for attempt := 0; attempt < 4; attempt++ {
		msg, err := framer.ReadFrame()
		if err != nil {
			return nil, building, fmt.Errorf("read the server's answer: %w", err)
		}

		switch msg.Type {
		case protocol.TypeVisitorChallenge:
			var challenge protocol.VisitorChallenge
			if err := json.Unmarshal(msg.Payload, &challenge); err != nil {
				return nil, building, fmt.Errorf("malformed visitor challenge: %w", err)
			}

			// The challenge is the first frame, so it carries the key agreement
			// response; everything after it uses the agreed key. Without one, a
			// salt in the same frame derives this visitor session's own key.
			next, err := c.switchVisitorCipher(conn, framer, challenge.KEX, kexState)
			if err != nil {
				return nil, building, err
			}
			if next == nil {
				next, err = c.switchVisitorSessionCipher(conn, framer, challenge.SessionSalt, presentedToken)
				if err != nil {
					return nil, building, err
				}
			}
			if next != nil {
				framer, cipher = next.framer, next.cipher
				building = *next
			}

			proof, err := visitorProof(cfg, challenge.Nonce)
			if err != nil {
				return nil, building, fmt.Errorf("build the proof: %w", err)
			}
			// dialVisitor set one absolute deadline for the whole exchange and
			// only the read side has been refreshed since. A slow proof — the
			// Groth16 one takes real time to build — then made this write fail
			// with i/o timeout on a connection the server was still waiting on.
			_ = conn.SetWriteDeadline(c.visitorReadDeadline(ctx))
			if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
				return nil, building, fmt.Errorf("send the proof: %w", err)
			}
			_ = conn.SetReadDeadline(c.visitorReadDeadline(ctx))

		case protocol.TypeP2PPeer:
			var offer protocol.P2PPeer
			if err := json.Unmarshal(msg.Payload, &offer); err != nil {
				return nil, building, fmt.Errorf("malformed punch offer: %w", err)
			}
			next, err := c.switchVisitorCipher(conn, framer, offer.KEX, kexState)
			if err != nil {
				return nil, building, err
			}
			if next == nil {
				next, err = c.switchVisitorSessionCipher(conn, framer, offer.SessionSalt, presentedToken)
				if err != nil {
					return nil, building, err
				}
			}
			if next != nil {
				building = *next
			}
			return &offer, building, nil

		case protocol.TypeDataOpenAck:
			var ack protocol.DataOpenAck
			if err := json.Unmarshal(msg.Payload, &ack); err != nil {
				return nil, building, fmt.Errorf("malformed visitor ack: %w", err)
			}
			next, err := c.switchVisitorCipher(conn, framer, ack.KEX, kexState)
			if err != nil {
				return nil, building, err
			}
			if next == nil {
				next, err = c.switchVisitorSessionCipher(conn, framer, ack.SessionSalt, presentedToken)
				if err != nil {
					return nil, building, err
				}
			}
			if next != nil {
				building = *next
			}
			if !ack.OK {
				return nil, building, fmt.Errorf("the server refused the visitor connection: %s", ack.Error)
			}
			_ = conn.SetDeadline(time.Time{})
			return nil, building, nil

		default:
			return nil, building, fmt.Errorf("unexpected %s from the server", msg.Type)
		}
	}
	return nil, building, errors.New("the server kept asking for a proof")
}

// visitorProof answers the server's challenge with the proof the visitor's
// auth_method calls for: a Schnorr proof of knowledge of the secret for "nizk", a
// Groth16 proof of the same knowledge for "snark". Both are bound to the proxy name
// and the server's nonce, so the choice is a setting rather than a difference on
// the wire. A visitor whose method is "secret" never reaches here against a proof
// proxy: it sends the secret in its visitor-connect and the server refuses the
// mismatch before it would issue a challenge, so this path only ever produces a
// proof for the method the visitor itself configured.
func visitorProof(cfg config.VisitorConfig, nonce []byte) ([]byte, error) {
	context := protocol.VisitorProofContext(cfg.ServerName, nonce)
	if cfg.AuthMethod == config.AuthMethodSNARK {
		return snarkauth.Prove([]byte(cfg.SecretKey), context)
	}
	return crypto.SchnorrProve([]byte(cfg.SecretKey), context)
}

// switchVisitorCipher completes the post-quantum key agreement when the server
// answered one, and returns the framing to use from then on. It returns nil when
// no agreement is in play, leaving the caller's framing untouched.
func (c *client) switchVisitorCipher(conn net.Conn, framer *protocol.Framer, response, state []byte) (*visitorReady, error) {
	if len(response) == 0 {
		return nil, nil
	}
	if len(state) == 0 {
		return nil, errors.New("the server answered a key exchange this client did not start")
	}

	key, err := crypto.HybridClientFinish(state, response)
	if err != nil {
		return nil, fmt.Errorf("post-quantum key agreement: %w", err)
	}
	cipher, err := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, key)
	if err != nil {
		return nil, fmt.Errorf("post-quantum key agreement: %w", err)
	}
	return &visitorReady{
		framer: protocol.NewFramerWithOptions(conn, cipher, c.framerOptions()),
		cipher: cipher,
	}, nil
}

// switchVisitorSessionCipher derives the per-session key a server without a
// post-quantum agreement sends as a salt in its first frame — crypto.SessionKey
// over the token this visitor presented — and returns the framing to use from
// then on. It returns nil when the frame carries no salt, leaving the caller's
// framing untouched: that is the shape a server older than the salt speaks.
func (c *client) switchVisitorSessionCipher(conn net.Conn, framer *protocol.Framer, salt []byte, presentedToken string) (*visitorReady, error) {
	if len(salt) == 0 {
		return nil, nil
	}
	if presentedToken == "" {
		return nil, errors.New("the server sent a session salt but this visitor presented no token")
	}
	configured := c.cipher.Load()
	if configured == nil || !configured.Enabled() {
		// The salt only ever rides an encrypting server's first frame, so a
		// visitor that configured no encryption is looking at a mismatched
		// peer; switching its framer to the derived key would seal in one
		// direction only.
		return nil, errors.New("the server sent a session salt but this visitor configured no encryption")
	}
	key, err := crypto.SessionKey(presentedToken, salt)
	if err != nil {
		return nil, fmt.Errorf("session key derivation: %w", err)
	}
	cipher, err := crypto.NewCipherFromKey(c.cfg.Encryption.Algorithm, key)
	if err != nil {
		return nil, fmt.Errorf("session key derivation: %w", err)
	}
	return &visitorReady{
		framer: protocol.NewFramerWithOptions(conn, cipher, c.framerOptions()),
		cipher: cipher,
	}, nil
}

// dialVisitor opens the visitor connection and sends the visitor-connect frame.
func (c *client) dialVisitor(ctx context.Context, cfg config.VisitorConfig) (net.Conn, *protocol.Framer, []byte, string, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	conn, err := c.dialServer(ctx)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("dial %s: %w", c.serverAddr(), err)
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))

	framer := protocol.NewFramerWithOptions(conn, c.cipher.Load(), c.framerOptions())
	c.cfgMu.Lock()
	token := c.cfg.Client.AuthToken
	c.cfgMu.Unlock()
	if c.cfg.OIDC != nil && c.cfg.OIDC.TokenEndpointURL != "" {
		// The server holds a visitor to the same credentials as the control
		// connection, so an [oidc] client whose auth_token is a placeholder
		// has to present the access token here too, or every visitor is
		// refused with "invalid auth token" while the session looks healthy.
		access, err := c.oidcAccessToken(ctx)
		if err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", err
		}
		token = access
		// The fetch rides its own HTTP budget — up to ten seconds — and does
		// not touch this connection, but the absolute deadline armed at dial
		// time has been running the whole while: a token endpoint near its
		// budget left the visitor-connect write below failing with i/o
		// timeout while the server was still waiting for the frame. The
		// exchange restarts its clock here, the way the slow proof write in
		// awaitVisitorReady re-arms its deadline.
		_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	}
	request := protocol.VisitorConnect{
		Proxy:     cfg.ServerName,
		Type:      cfg.Type,
		AuthToken: token,
		// The server decides the data path from this field: without it a visitor
		// whose configuration asks for a WebRTC data channel was relayed anyway,
		// and the offer the client sent next came back to it through the relay as
		// though it were stream data.
		Transport: cfg.Transport,
		// The visitor's identity is what the proxy's allow_users is checked
		// against; a private proxy without a list admits this identity alone.
		User: c.cfg.Client.User,
	}
	if cfg.AuthMethod == config.AuthMethodSecret {
		request.Secret = cfg.SecretKey
	}
	if err := c.attachVisitorIdentity(&request); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", err
	}

	var kexState []byte
	if c.cfg.PostQuantum() {
		public, state, err := crypto.HybridClientInit()
		if err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", fmt.Errorf("post-quantum key agreement: %w", err)
		}
		request.KEX, kexState = public, state
	}

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, request); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", fmt.Errorf("send visitor-connect: %w", err)
	}
	return conn, framer, kexState, token, nil
}

// wrapWith applies the cipher's record layer to a byte-stream connection.
func (c *client) wrapWith(conn net.Conn, cipher *crypto.Cipher) net.Conn {
	return &cryptoStreamConn{Stream: crypto.NewStream(conn, cipher), conn: conn}
}

// attachVisitorIdentity adds the same Ed25519 assertion a control connection
// carries, so a server that requires an identity requires it of visitors too.
func (c *client) attachVisitorIdentity(request *protocol.VisitorConnect) error {
	if c.identity == nil {
		return nil
	}
	nonce, err := crypto.Nonce()
	if err != nil {
		return fmt.Errorf("identity assertion: %w", err)
	}
	now := time.Now().Unix()

	request.Identity = c.identity.PublicKey()
	request.IdentityNonce = nonce
	request.IdentityTime = now
	request.IdentitySignature = c.identity.SignChallenge(nonce, now)
	return nil
}
