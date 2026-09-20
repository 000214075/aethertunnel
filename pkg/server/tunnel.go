package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http/httputil"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Tunnel is one proxy published by a client.
//
// What the server binds depends on the proxy type: a tcp tunnel gets its own
// listening port, a udp tunnel its own datagram socket, an http/https tunnel a
// share of the shared virtual-host listener, and a private tunnel (stcp, sudp,
// xtcp) nothing at all — it is reachable only by a visitor that presents the
// secret key.
type Tunnel struct {
	Name       string
	Spec       protocol.ProxySpec
	Type       string
	RemotePort int
	Domains    []string
	SecretKey  string
	// AuthMethod is "secret" or "nizk".
	AuthMethod string
	// SecretPublicKey is g^x for the proxy's secret key, published so a visitor
	// can prove knowledge of x without sending it.
	SecretPublicKey []byte
	Session         *Session

	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration

	listener net.Listener // tcp
	packet   net.PacketConn
	pump     *flynet.DatagramPump
	vhost    *vhostBinding

	proxyOnce sync.Once
	proxy     *httputil.ReverseProxy

	Active   atomic.Int64
	Total    atomic.Int64
	BytesIn  atomic.Int64 // received from the client (its service's replies)
	BytesOut atomic.Int64 // sent to the client (what the visitor sent)

	once sync.Once
	done chan struct{}
}

// Private reports whether the tunnel is reached through a visitor connection
// instead of a public port.
func (t *Tunnel) Private() bool { return config.IsPrivateProxyType(t.Type) }

// Addr returns the address the tunnel is published on, or "" when it has no
// public address.
func (t *Tunnel) Addr() string {
	switch {
	case t.listener != nil:
		return t.listener.Addr().String()
	case t.packet != nil:
		return t.packet.LocalAddr().String()
	default:
		return ""
	}
}

// Close stops accepting new visits and unblocks anything waiting on this tunnel.
func (t *Tunnel) Close(reason string) {
	t.once.Do(func() {
		close(t.done)
		if t.listener != nil {
			_ = t.listener.Close()
		}
		if t.packet != nil {
			_ = t.packet.Close()
		}
		if t.pump != nil {
			t.pump.Shutdown()
		}
		if t.vhost != nil {
			t.vhost.remove()
		}
		t.logger.Printf("tunnel %q closed (%s)", t.Name, reason)
	})
}

// matchesSecret compares a visitor's secret with the tunnel's in constant time.
func (t *Tunnel) matchesSecret(presented string) bool {
	if t.SecretKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(t.SecretKey)) == 1
}

// openStream asks the owning client for a fresh data connection and waits for it
// to dial back.
//
// The returned release function accounts for the stream's lifetime and must be
// called once the stream has ended.
func (t *Tunnel) openStream(visitor bool) (*dataConn, func(), error) {
	streamID := newID(8)

	waiting, err := t.Session.AddPending(streamID)
	if err != nil {
		return nil, nil, err
	}

	request := protocol.DataRequest{Proxy: t.Name, StreamID: streamID, Visitor: visitor}
	if err := t.Session.Framer().WriteJSON(protocol.TypeDataRequest, request); err != nil {
		t.Session.DropPending(streamID)
		return nil, nil, fmt.Errorf("cannot ask the client for a stream: %w", err)
	}

	t.Active.Add(1)
	t.metrics.streamOpened(t.Name)
	release := func() {
		t.Active.Add(-1)
		t.metrics.streamClosed(t.Name)
	}

	timer := time.NewTimer(t.dialTimeout)
	defer timer.Stop()

	select {
	case dc := <-waiting:
		if dc == nil {
			release()
			return nil, nil, errors.New("the client disconnected while the stream was pending")
		}
		return dc, release, nil
	case <-timer.C:
		t.Session.DropPending(streamID)
		release()
		return nil, nil, fmt.Errorf("the client did not provide a stream within %s", t.dialTimeout)
	case <-t.done:
		t.Session.DropPending(streamID)
		release()
		return nil, nil, errors.New("the tunnel was closed")
	}
}

// acceptLoop publishes a tcp tunnel until it is closed.
func (t *Tunnel) acceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.done:
				return
			default:
			}
			t.logger.Printf("tunnel %q: accept error: %v", t.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go t.handleVisit(conn)
	}
}

// handleVisit matches one public connection with a data connection opened by the
// owning client and copies bytes between them.
func (t *Tunnel) handleVisit(public net.Conn) {
	dc, release, err := t.openStream(false)
	if err != nil {
		t.logger.Printf("tunnel %q: dropping visit from %s: %v", t.Name, public.RemoteAddr(), err)
		_ = public.Close()
		return
	}
	defer release()

	if err := t.pipeStream(public, dc, public.RemoteAddr().String()); err != nil {
		t.logger.Printf("tunnel %q: %v", t.Name, err)
	}
}

// pipeStream copies raw bytes between a public connection and a client data
// connection, recording the traffic. Both connections are closed when it returns.
func (t *Tunnel) pipeStream(public net.Conn, dc *dataConn, label string) error {
	defer dc.Close()

	// dc.cipher is the key derived for this stream: the configured cipher when
	// the session agreed no post-quantum key, and a per-stream key when it did.
	clientSide := &cryptoStreamConn{Stream: crypto.NewStream(dc.conn, dc.cipher), conn: dc.conn}
	toClient, fromClient := flynet.Pipe(public, clientSide, t.idleTimeout)

	t.BytesOut.Add(toClient)
	t.BytesIn.Add(fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)
	t.metrics.recordStream(t.Name, toClient, fromClient)

	t.logger.Printf("tunnel %q: stream for %s finished (%d bytes out, %d bytes in)",
		t.Name, label, toClient, fromClient)
	return nil
}

// pipeDatagrams relays datagrams between a visitor connection and a client data
// connection. Both connections are closed when it returns.
func (t *Tunnel) pipeDatagrams(visitor *protocol.Framer, dc *dataConn, label string) error {
	defer dc.Close()

	toClient, fromClient := relayDatagrams(visitor, dc.framer)

	t.BytesOut.Add(toClient)
	t.BytesIn.Add(fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)

	t.logger.Printf("tunnel %q: datagram session for %s finished (%d bytes out, %d bytes in)",
		t.Name, label, toClient, fromClient)
	return nil
}

// relayDatagrams copies TypeUDPPacket frames between two framers until one of
// them fails. The frame boundary is what preserves the datagram boundary.
func relayDatagrams(a, b *protocol.Framer) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	copyFrames := func(dst, src *protocol.Framer) int64 {
		var total int64
		for {
			msg, err := src.ReadFrame()
			if err != nil {
				return total
			}
			if msg.Type != protocol.TypeUDPPacket {
				continue
			}
			if err := dst.WriteFrame(&protocol.Message{
				Type:    protocol.TypeUDPPacket,
				Payload: msg.Payload,
			}); err != nil {
				return total
			}
			total += int64(len(msg.Payload))
		}
	}

	go func() {
		defer wg.Done()
		aToB = copyFrames(b, a)
		_ = a.Close()
		_ = b.Close()
	}()
	go func() {
		defer wg.Done()
		bToA = copyFrames(a, b)
		_ = a.Close()
		_ = b.Close()
	}()

	wg.Wait()
	return aToB, bToA
}

// cryptoStreamConn adapts crypto.Stream (an io.ReadWriteCloser) to net.Conn so the
// pipe helper can use it interchangeably with a raw socket.
type cryptoStreamConn struct {
	*crypto.Stream
	conn net.Conn
}

func (c *cryptoStreamConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *cryptoStreamConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *cryptoStreamConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *cryptoStreamConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *cryptoStreamConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// --- tunnel registry ----------------------------------------------------------

// TunnelManager owns every published tunnel on the server.
type TunnelManager struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel

	cfg      *config.Config
	logger   *log.Logger
	cipher   *crypto.Cipher
	sessions *SessionManager
	metrics  *Metrics

	vhost *vhostSet
	p2p   *p2pRendezvous
}

func newTunnelManager(cfg *config.Config, logger *log.Logger, cipher *crypto.Cipher, sessions *SessionManager, metrics *Metrics) *TunnelManager {
	return &TunnelManager{
		tunnels:  make(map[string]*Tunnel),
		cfg:      cfg,
		logger:   logger,
		cipher:   cipher,
		sessions: sessions,
		metrics:  metrics,
	}
}

var (
	errTunnelExists = errors.New("a tunnel with that name is already registered")
	errNoSuchTunnel = errors.New("no such tunnel")
)

// Register publishes a tunnel for a session. When the spec asks for a remote port
// the port is bound here, so a failure (port in use, out of range) is reported to
// the client immediately instead of failing on the first visit.
func (m *TunnelManager) Register(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	if spec.Name == "" {
		return nil, errors.New("proxy name is required")
	}
	if spec.Type == "" {
		spec.Type = protocol.ProxyTypeTCP
	}
	if !config.IsProxyType(spec.Type) {
		return nil, fmt.Errorf("proxy type %q is not supported (use one of %s)",
			spec.Type, joinTypes(config.ProxyTypes))
	}
	if spec.RemotePort < 0 || spec.RemotePort > 65535 {
		return nil, fmt.Errorf("remote_port %d is out of range", spec.RemotePort)
	}
	if config.IsPrivateProxyType(spec.Type) {
		if spec.SecretKey == "" {
			return nil, fmt.Errorf("proxy type %q is private and needs a secret_key", spec.Type)
		}
		if spec.RemotePort != 0 {
			return nil, fmt.Errorf("proxy type %q is private and must not open a public port", spec.Type)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.tunnels[spec.Name]; ok {
		// A name may be re-registered when the previous owner is the same session
		// (a re-register) or when that session is gone or already closed (the
		// client reconnected faster than the old session was cleaned up).
		if existing.Session != session && m.isLive(existing.Session) {
			return nil, errTunnelExists
		}
		m.logger.Printf("tunnel %q: replacing the registration held by session %s",
			spec.Name, existing.Session.ID)
		existing.Close("replaced by a new registration")
		delete(m.tunnels, spec.Name)
	}

	tunnel := &Tunnel{
		Name:        spec.Name,
		Spec:        spec,
		Type:        spec.Type,
		RemotePort:  spec.RemotePort,
		Domains:     spec.Domains,
		SecretKey:   spec.SecretKey,
		AuthMethod:  spec.AuthMethod,
		Session:     session,
		logger:      m.logger,
		cipher:      m.cipher,
		metrics:     m.metrics,
		idleTimeout: time.Duration(m.cfg.Server.ReadTimeoutSecs) * time.Second,
		dialTimeout: time.Duration(m.cfg.Server.DialTimeoutSecs) * time.Second,
		done:        make(chan struct{}),
	}
	if tunnel.AuthMethod == "" {
		tunnel.AuthMethod = config.AuthMethodSecret
	}
	if tunnel.AuthMethod == config.AuthMethodNIZK {
		public, err := crypto.SchnorrPublicKey([]byte(spec.SecretKey))
		if err != nil {
			return nil, fmt.Errorf("proxy %q: cannot derive the proof key: %w", spec.Name, err)
		}
		tunnel.SecretPublicKey = public
	}

	if err := m.bind(tunnel); err != nil {
		tunnel.Close("registration failed")
		return nil, err
	}

	m.tunnels[spec.Name] = tunnel
	return tunnel, nil
}

// bind gives a tunnel the listener its type calls for.
func (m *TunnelManager) bind(t *Tunnel) error {
	switch t.Type {
	case protocol.ProxyTypeTCP:
		if t.RemotePort == 0 {
			m.logger.Printf("tunnel %q registered with no public port (client %s)", t.Name, t.Session.ID)
			return nil
		}
		addr := net.JoinHostPort(m.cfg.Server.BindAddr, strconv.Itoa(t.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s: %w", t.Name, addr, err)
		}
		t.listener = listener
		go t.acceptLoop()
		m.logger.Printf("tunnel %q (tcp) published on %s -> client %s", t.Name, addr, t.Session.ID)

	case protocol.ProxyTypeUDP:
		if t.RemotePort == 0 {
			m.logger.Printf("tunnel %q registered with no public port (client %s)", t.Name, t.Session.ID)
			return nil
		}
		addr := net.JoinHostPort(m.cfg.Server.BindAddr, strconv.Itoa(t.RemotePort))
		packet, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s/udp: %w", t.Name, addr, err)
		}
		t.packet = packet
		t.startUDP()
		m.logger.Printf("tunnel %q (udp) published on %s -> client %s", t.Name, addr, t.Session.ID)

	case protocol.ProxyTypeHTTP, protocol.ProxyTypeHTTPS:
		if m.vhost == nil {
			return fmt.Errorf("proxy %q is type %s but server.http_port is not configured", t.Name, t.Type)
		}
		binding, err := m.vhost.add(t)
		if err != nil {
			return err
		}
		t.vhost = binding
		m.logger.Printf("tunnel %q (%s) registered for %v via the shared listener (client %s)",
			t.Name, t.Type, t.Domains, t.Session.ID)

	case protocol.ProxyTypeSTCP, protocol.ProxyTypeSUDP, protocol.ProxyTypeXTCP:
		m.logger.Printf("tunnel %q (%s) registered as private, reachable by visitors (client %s)",
			t.Name, t.Type, t.Session.ID)

	default:
		return fmt.Errorf("proxy type %q has no binding", t.Type)
	}
	return nil
}

func joinTypes(types []string) string {
	out := ""
	for i, t := range types {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(t)
	}
	return out
}

// isLive reports whether a tunnel's owning session is still usable.
func (m *TunnelManager) isLive(s *Session) bool {
	if s == nil || s.IsClosed() {
		return false
	}
	if m.sessions == nil {
		return true
	}
	_, ok := m.sessions.Get(s.ID)
	return ok
}

// Unregister removes a tunnel and stops its public listener.
func (m *TunnelManager) Unregister(name, reason string) {
	m.mu.Lock()
	tunnel, ok := m.tunnels[name]
	delete(m.tunnels, name)
	m.mu.Unlock()

	if ok {
		tunnel.Close(reason)
	}
}

// Get looks a tunnel up by name.
func (m *TunnelManager) Get(name string) (*Tunnel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tunnel, ok := m.tunnels[name]
	if !ok {
		return nil, errNoSuchTunnel
	}
	return tunnel, nil
}

// List returns a snapshot ordered by name.
func (m *TunnelManager) List() []*Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, t)
	}
	return out
}

// RemoveSessionTunnels stops everything a disconnecting session published.
func (m *TunnelManager) RemoveSessionTunnels(s *Session, reason string) {
	for _, name := range s.Proxies() {
		m.Unregister(name, reason)
	}
}
