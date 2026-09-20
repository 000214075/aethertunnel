package main

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
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
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

	once sync.Once
}

func (p *visitorPath) close() {
	p.once.Do(func() {
		if p.conn != nil {
			_ = p.conn.Close()
		}
	})
}

// runVisitors starts the local listener for every [[visitors]] entry. Visitor
// listeners are independent of the control session: each visiting connection
// opens its own connection to the server, so a visitor needs no session of its
// own and reconnects per connection.
func (c *client) runVisitors(ctx context.Context) {
	for _, visitor := range c.cfg.Visitors {
		visitor := visitor
		go c.serveVisitor(ctx, visitor)
	}
}

func (c *client) serveVisitor(ctx context.Context, cfg config.VisitorConfig) {
	if config.IsDatagramProxyType(cfg.Type) {
		c.serveUDPVisitor(ctx, cfg)
		return
	}
	c.serveStreamVisitor(ctx, cfg)
}

func (c *client) serveStreamVisitor(ctx context.Context, cfg config.VisitorConfig) {
	listener, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		c.logger.Printf("visitor %q: cannot listen on %s: %v", cfg.Name, cfg.ListenAddr(), err)
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
			return
		}
		go c.handleVisitorTCP(ctx, cfg, conn)
	}
}

func (c *client) handleVisitorTCP(ctx context.Context, cfg config.VisitorConfig, local net.Conn) {
	path, err := c.openVisitorPath(ctx, cfg)
	if err != nil {
		c.logger.Printf("visitor %q: %v", cfg.Name, err)
		_ = local.Close()
		return
	}
	defer path.close()

	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toRemote, fromRemote := flynet.Pipe(local, path.stream, idle)
	c.logger.Printf("visitor %q: session finished (sent %d, received %d, direct %v)",
		cfg.Name, toRemote, fromRemote, path.direct)
}

// serveUDPVisitor listens for datagrams and gives every source address its own
// visitor connection, so the far end sees one stream per address the same way a
// udp proxy on the server does.
func (c *client) serveUDPVisitor(ctx context.Context, cfg config.VisitorConfig) {
	socket, err := net.ListenPacket("udp", cfg.ListenAddr())
	if err != nil {
		c.logger.Printf("visitor %q: cannot listen on %s: %v", cfg.Name, cfg.ListenAddr(), err)
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
		Logger:      c.logger,
	}
	pump.Start()

	<-ctx.Done()
	_ = socket.Close()
	pump.Shutdown()
}

// openVisitorPath establishes one session into a private proxy.
func (c *client) openVisitorPath(ctx context.Context, cfg config.VisitorConfig) (*visitorPath, error) {
	conn, framer, kexState, err := c.dialVisitor(cfg)
	if err != nil {
		return nil, err
	}

	offer, ready, err := c.awaitVisitorReady(conn, framer, cfg, kexState)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	framer = ready.framer
	cipher := ready.cipher

	if offer != nil {
		direct, err := c.tryPunch(ctx, conn, framer, *offer)
		if err == nil {
			// The relayed connection is not needed once the direct path is up.
			_ = conn.Close()
			return &visitorPath{conn: direct, stream: c.wrapWith(direct, c.cipher), direct: true}, nil
		}
		c.logger.Printf("visitor %q: %v; falling back to the relayed path", cfg.Name, err)

		if err := framer.WriteJSON(protocol.TypeP2PFallback,
			protocol.P2PFallback{Proxy: cfg.ServerName}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("ask for the relayed path: %w", err)
		}
		if _, ready, err = c.awaitVisitorReady(conn, framer, cfg, kexState); err != nil {
			_ = conn.Close()
			return nil, err
		}
		framer, cipher = ready.framer, ready.cipher
	}

	return &visitorPath{
		conn:      conn,
		framer:    framer,
		stream:    c.wrapWith(conn, cipher),
		datagrams: config.IsDatagramProxyType(cfg.Type),
	}, nil
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
func (c *client) awaitVisitorReady(conn net.Conn, framer *protocol.Framer, cfg config.VisitorConfig, kexState []byte) (*protocol.P2PPeer, visitorReady, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(dialTimeout))

	cipher := c.cipher
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
			// response; everything after it uses the agreed key.
			next, err := c.switchVisitorCipher(conn, framer, challenge.KEX, kexState)
			if err != nil {
				return nil, building, err
			}
			if next != nil {
				framer, cipher = next.framer, next.cipher
				building = *next
			}

			proof, err := crypto.SchnorrProve([]byte(cfg.SecretKey),
				protocol.VisitorProofContext(cfg.ServerName, challenge.Nonce))
			if err != nil {
				return nil, building, fmt.Errorf("build the proof: %w", err)
			}
			if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
				return nil, building, fmt.Errorf("send the proof: %w", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(dialTimeout))

		case protocol.TypeP2PPeer:
			var offer protocol.P2PPeer
			if err := json.Unmarshal(msg.Payload, &offer); err != nil {
				return nil, building, fmt.Errorf("malformed punch offer: %w", err)
			}
			next, err := c.switchVisitorCipher(conn, framer, offer.KEX, kexState)
			if err != nil {
				return nil, building, err
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

// dialVisitor opens the visitor connection and sends the visitor-connect frame.
func (c *client) dialVisitor(cfg config.VisitorConfig) (net.Conn, *protocol.Framer, []byte, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	conn, err := c.dialServer()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial %s: %w", c.serverAddr(), err)
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))

	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())
	request := protocol.VisitorConnect{
		Proxy:     cfg.ServerName,
		Type:      cfg.Type,
		AuthToken: c.cfg.Client.AuthToken,
	}
	if cfg.AuthMethod == config.AuthMethodSecret {
		request.Secret = cfg.SecretKey
	}
	if err := c.attachVisitorIdentity(&request); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}

	var kexState []byte
	if c.cfg.PostQuantum() {
		public, state, err := crypto.HybridClientInit()
		if err != nil {
			_ = conn.Close()
			return nil, nil, nil, fmt.Errorf("post-quantum key agreement: %w", err)
		}
		request.KEX, kexState = public, state
	}

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, request); err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("send visitor-connect: %w", err)
	}
	return conn, framer, kexState, nil
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
