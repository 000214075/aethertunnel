// Package reliable carries an ordered byte stream over a UDP socket so two NATed
// peers can tunnel traffic to each other directly, after a rendezvous server has
// told each of them the other's address.
//
// The socket is hole punched with a simultaneous open: each side sends an
// authenticated hello towards the address it learned, about every 100 ms, until
// it receives an authenticated hello from the peer. Receiving one also reveals
// the address the peer's NAT mapped for it, which is how Accept can start
// without knowing the peer beforehand. The established stream then speaks a
// byte-oriented protocol with cumulative acknowledgements, timer-based
// retransmission and flow control, and implements net.Conn.
package reliable

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// Defaults used for any zero field of Config.
const (
	DefaultMTU              = 1200
	DefaultHandshakeTimeout = 10 * time.Second
	DefaultIdleTimeout      = 120 * time.Second
	DefaultRetransmitLimit  = 12
	DefaultSendWindow       = 64
)

// punchInterval is how often the handshake sends its hello and how often the
// established connection repeats the acknowledgement that confirms it.
const punchInterval = 100 * time.Millisecond

// minMTU leaves room for the header, the MAC and a useful payload.
const minMTU = 128

// Config tunes the transport. The zero value is usable.
type Config struct {
	// Token authenticates the handshake. Both peers must use the same value.
	// A nil or empty token makes Punch and Accept return an error.
	Token []byte
	// MTU is the largest datagram this side will send, including headers.
	// Zero selects DefaultMTU (1200).
	MTU int
	// HandshakeTimeout bounds hole punching. Zero selects DefaultHandshakeTimeout (10s).
	HandshakeTimeout time.Duration
	// IdleTimeout closes a connection that has neither sent nor received a
	// segment for this long. Zero selects DefaultIdleTimeout (120s).
	IdleTimeout time.Duration
	// RetransmitLimit is how many times one segment is retransmitted before
	// the connection fails. Zero selects DefaultRetransmitLimit (12).
	RetransmitLimit int
	// SendWindow is the number of unacknowledged segments the sender may
	// have in flight. Zero selects DefaultSendWindow (64).
	SendWindow int
	// Logger receives diagnostic lines. nil disables logging.
	Logger *log.Logger
}

func (c Config) withDefaults() Config {
	if c.MTU == 0 {
		c.MTU = DefaultMTU
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.RetransmitLimit <= 0 {
		c.RetransmitLimit = DefaultRetransmitLimit
	}
	if c.SendWindow <= 0 {
		c.SendWindow = DefaultSendWindow
	}
	return c
}

var errNoToken = errors.New("reliable: Config.Token is empty")

// packetConn is the datagram transport under a Socket. *net.UDPConn is the
// production implementation; tests replace it to inject loss and duplicates.
type packetConn interface {
	ReadFrom(b []byte) (int, net.Addr, error)
	WriteTo(b []byte, addr net.Addr) (int, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// sender serialises writes to the transport. The handshake goroutine and the
// established stream both write, and only *net.UDPConn is documented to be safe
// for that on its own.
type sender struct {
	mu sync.Mutex
	pc packetConn
}

func (s *sender) writeTo(b []byte, addr net.Addr) {
	if len(b) == 0 || addr == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.pc.WriteTo(b, addr)
}

// Socket is a UDP socket that can be hole punched and then upgraded to a
// reliable stream.
type Socket struct {
	cfg   Config
	tx    *sender
	local net.Addr

	mu   sync.Mutex
	used bool
	dead bool
}

// Listen binds a UDP socket on laddr. laddr may be ":0" to let the kernel
// choose the port; LocalAddr then reports the chosen address.
func Listen(laddr string) (*Socket, error) { return ListenConfig(laddr, Config{}) }

// ListenConfig binds a UDP socket using cfg.
func ListenConfig(laddr string, cfg Config) (*Socket, error) {
	return newSocket(laddr, cfg, nil)
}

// newSocket binds a UDP socket and passes its datagrams through wrap, which
// tests use to inject loss and duplicates. wrap may be nil.
func newSocket(laddr string, cfg Config, wrap func(packetConn) packetConn) (*Socket, error) {
	cfg = cfg.withDefaults()
	if cfg.MTU < minMTU {
		return nil, fmt.Errorf("reliable: MTU %d is below the minimum of %d", cfg.MTU, minMTU)
	}
	raddr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, fmt.Errorf("reliable: resolve %q: %w", laddr, err)
	}
	uc, err := net.ListenUDP("udp", raddr)
	if err != nil {
		return nil, fmt.Errorf("reliable: listen %q: %w", laddr, err)
	}
	var pc packetConn = uc
	if wrap != nil {
		pc = wrap(pc)
	}
	return &Socket{cfg: cfg, tx: &sender{pc: pc}, local: uc.LocalAddr()}, nil
}

// LocalAddr reports the address the socket is bound to.
func (s *Socket) LocalAddr() net.Addr { return s.local }

// Close releases the socket. A stream created from it is not affected other
// than losing its transport.
func (s *Socket) Close() error {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return nil
	}
	s.dead = true
	s.mu.Unlock()
	return s.tx.pc.Close()
}

// Punch performs the simultaneous-open handshake towards peer. Both peers
// call Punch simultaneously (the peer address is what each side learned
// from the rendezvous server); the handshake succeeds when each side has
// received the other's authenticated hello. It returns a reliable, ordered,
// net.Conn-speaking stream. The socket is consumed and must not be reused.
func (s *Socket) Punch(ctx context.Context, peer net.Addr) (net.Conn, error) {
	if peer == nil {
		return nil, errors.New("reliable: punch needs a peer address")
	}
	return s.establish(ctx, peer)
}

// Accept waits for an incoming punch from any address. token is taken from
// Config.Token. It returns the established stream.
func (s *Socket) Accept(ctx context.Context) (net.Conn, error) {
	return s.establish(ctx, nil)
}

func (s *Socket) establish(ctx context.Context, peer net.Addr) (net.Conn, error) {
	if len(s.cfg.Token) == 0 {
		return nil, errNoToken
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	switch {
	case s.dead:
		s.mu.Unlock()
		return nil, net.ErrClosed
	case s.used:
		s.mu.Unlock()
		return nil, errors.New("reliable: socket is already carrying a connection")
	}
	s.used = true
	s.mu.Unlock()

	c, err := s.handshake(ctx, peer)
	if err != nil {
		// The socket saw no valid peer, so it is still usable.
		s.mu.Lock()
		s.used = false
		s.mu.Unlock()
		return nil, err
	}
	return c, nil
}

// handshake runs the simultaneous open. peer may be nil, in which case the
// remote address is learned from the first authenticated hello.
func (s *Socket) handshake(ctx context.Context, peer net.Addr) (*stream, error) {
	cfg := s.cfg
	localNonce := make([]byte, nonceSize)
	if _, err := rand.Read(localNonce); err != nil {
		return nil, fmt.Errorf("reliable: nonce: %w", err)
	}
	hello := buildHello(cfg.Token, localNonce)

	// The punching goroutine and the reader below share where to send and
	// whether the hello has been answered yet.
	var (
		hsMu   sync.Mutex
		dst    = peer
		answer []byte
	)
	setPeer := func(addr net.Addr, ack []byte) {
		hsMu.Lock()
		dst, answer = addr, ack
		hsMu.Unlock()
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(punchInterval)
		defer ticker.Stop()
		for {
			hsMu.Lock()
			to, pkt := dst, answer
			if pkt == nil {
				pkt = hello
			}
			hsMu.Unlock()
			s.tx.writeTo(pkt, to)
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	deadline := time.Now().Add(cfg.HandshakeTimeout)
	buf := make([]byte, cfg.MTU)
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, fmt.Errorf("reliable: handshake with %s timed out", addrName(peer))
		}
		if wait > punchInterval {
			wait = punchInterval
		}
		_ = s.tx.pc.SetReadDeadline(time.Now().Add(wait))
		n, addr, err := s.tx.pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if s.isDead() {
				return nil, net.ErrClosed
			}
			// A datagram too large for the buffer is an error on some platforms
			// and junk from an unrelated host must not end the punch, so a
			// bounded run of read errors is tolerated here as well.
			if failures++; failures <= readFailLimit {
				continue
			}
			return nil, fmt.Errorf("reliable: handshake read: %w", err)
		}
		failures = 0

		seg, ok := parseSegment(cfg.Token, buf[:n])
		if !ok {
			continue
		}
		// Only these two types can establish the session: a data or fin segment
		// carries nothing but the session it was built with, so it cannot be
		// authenticated before the peer's nonce is known. A peer that has
		// established keeps repeating its hello-ack, which covers the case where
		// a segment arrives first.
		switch seg.typ {
		case typeHello:
			// The peer's nonce is in the payload, so both sides can derive the
			// session without an initiator role.
			if len(seg.payload) != nonceSize {
				continue
			}
			peerNonce := append([]byte(nil), seg.payload...)
			session := sessionID(localNonce, peerNonce)
			ack := buildHelloAck(cfg.Token, localNonce, peerNonce, session)
			setPeer(addr, ack)
			s.tx.writeTo(ack, addr)
			return s.newStream(addr, session, ack), nil
		case typeHelloAck:
			// The acknowledgement echoes our nonce, so a lost hello does not
			// leave this side waiting for the whole handshake timeout.
			if len(seg.payload) != 2*nonceSize {
				continue
			}
			peerNonce := append([]byte(nil), seg.payload[:nonceSize]...)
			if !bytes.Equal(seg.payload[nonceSize:], localNonce) {
				continue
			}
			session := sessionID(localNonce, peerNonce)
			if session != seg.session {
				continue
			}
			ack := buildHelloAck(cfg.Token, localNonce, peerNonce, session)
			setPeer(addr, ack)
			s.tx.writeTo(ack, addr)
			return s.newStream(addr, session, ack), nil
		}
	}
}

func (s *Socket) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// newStream takes ownership of the transport. The handshake goroutine is still
// writing until handshake returns, but the read side is free.
func (s *Socket) newStream(peer net.Addr, session uint32, helloAck []byte) *stream {
	cfg := s.cfg
	c := &stream{
		tx:           s.tx,
		peer:         peer,
		local:        s.local,
		token:        cfg.Token,
		logger:       cfg.Logger,
		cfg:          cfg,
		segSize:      cfg.MTU - headerSize - macSize,
		session:      session,
		helloAck:     helloAck,
		done:         make(chan struct{}),
		wake:         make(chan struct{}),
		ooo:          make(map[uint32][]byte),
		peerWindow:   maxWindow * windowScale,
		finRTO:       baseRTO,
		lastHelloAck: time.Now(),
		lastActivity: time.Now(),
	}
	if cfg.Logger != nil {
		cfg.Logger.Printf("reliable: %s established with %s", c.local, peer)
	}
	go c.readLoop()
	go c.timerLoop()
	return c
}

// addrName renders an address for an error message, tolerating a nil peer.
func addrName(addr net.Addr) string {
	if addr == nil {
		return "any peer"
	}
	return addr.String()
}
