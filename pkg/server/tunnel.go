package server

import (
	"errors"
	"fmt"
	"log"
	"net"
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
// When the client asks for a remote port the server binds it here; every visit to
// that port is matched with a fresh data connection from the client, which is what
// makes a client behind NAT reachable. The v1 code never bound the remote port and
// dialled the server's own loopback instead, so no traffic could ever flow.
type Tunnel struct {
	Name       string
	Spec       protocol.ProxySpec
	RemotePort int
	Session    *Session

	logger      *log.Logger
	cipher      *crypto.Cipher
	listener    net.Listener
	idleTimeout time.Duration
	dialTimeout time.Duration

	Active   atomic.Int64
	Total    atomic.Int64
	BytesIn  atomic.Int64 // received from the client (its service's replies)
	BytesOut atomic.Int64 // sent to the client (what the visitor sent)

	once sync.Once
	done chan struct{}
}

// Addr returns the address the tunnel is published on, or "" when it has no
// public port.
func (t *Tunnel) Addr() string {
	if t.listener == nil {
		return ""
	}
	return t.listener.Addr().String()
}

// Close stops accepting new visits and unblocks anything waiting on this tunnel.
func (t *Tunnel) Close(reason string) {
	t.once.Do(func() {
		close(t.done)
		if t.listener != nil {
			_ = t.listener.Close()
		}
		t.logger.Printf("tunnel %q closed (%s)", t.Name, reason)
	})
}

// acceptLoop publishes the tunnel until it is closed.
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
// owning client.
func (t *Tunnel) handleVisit(public net.Conn) {
	streamID := newID(8)

	waiting, err := t.Session.AddPending(streamID, public)
	if err != nil {
		t.logger.Printf("tunnel %q: dropping visit from %s: %v", t.Name, public.RemoteAddr(), err)
		_ = public.Close()
		return
	}

	request := protocol.DataRequest{Proxy: t.Name, StreamID: streamID}
	if err := t.Session.Framer().WriteJSON(protocol.TypeDataRequest, request); err != nil {
		t.Session.DropPending(streamID)
		_ = public.Close()
		t.logger.Printf("tunnel %q: cannot ask the client for a stream: %v", t.Name, err)
		return
	}

	t.Active.Add(1)
	defer t.Active.Add(-1)

	var data net.Conn
	select {
	case data = <-waiting:
	case <-time.After(t.dialTimeout):
		t.Session.DropPending(streamID)
	case <-t.done:
		t.Session.DropPending(streamID)
	}

	if data == nil {
		_ = public.Close()
		t.logger.Printf("tunnel %q: client did not provide a stream for %s within %s",
			t.Name, public.RemoteAddr(), t.dialTimeout)
		return
	}

	// From here the visitor and the client's local service exchange raw bytes,
	// wrapped by the record layer when encryption is enabled.
	clientSide := &cryptoStreamConn{Stream: crypto.NewStream(data, t.cipher), conn: data}
	toClient, fromClient := flynet.Pipe(public, clientSide, t.idleTimeout)

	t.BytesOut.Add(toClient)
	t.BytesIn.Add(fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)

	t.logger.Printf("tunnel %q: stream from %s finished (%d bytes out, %d bytes in)",
		t.Name, public.RemoteAddr(), toClient, fromClient)
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
}

func newTunnelManager(cfg *config.Config, logger *log.Logger, cipher *crypto.Cipher, sessions *SessionManager) *TunnelManager {
	return &TunnelManager{
		tunnels:  make(map[string]*Tunnel),
		cfg:      cfg,
		logger:   logger,
		cipher:   cipher,
		sessions: sessions,
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
		spec.Type = "tcp"
	}
	if spec.Type != "tcp" {
		return nil, fmt.Errorf("proxy type %q is not implemented; only \"tcp\" is supported", spec.Type)
	}
	if spec.RemotePort < 0 || spec.RemotePort > 65535 {
		return nil, fmt.Errorf("remote_port %d is out of range", spec.RemotePort)
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
		RemotePort:  spec.RemotePort,
		Session:     session,
		logger:      m.logger,
		cipher:      m.cipher,
		idleTimeout: time.Duration(m.cfg.Server.ReadTimeoutSecs) * time.Second,
		dialTimeout: time.Duration(m.cfg.Server.DialTimeoutSecs) * time.Second,
		done:        make(chan struct{}),
	}

	if spec.RemotePort > 0 {
		addr := net.JoinHostPort(m.cfg.Server.BindAddr, strconv.Itoa(spec.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("cannot publish %s on %s: %w", spec.Name, addr, err)
		}
		tunnel.listener = listener
		go tunnel.acceptLoop()
		m.logger.Printf("tunnel %q published on %s -> client %s", spec.Name, addr, session.ID)
	} else {
		m.logger.Printf("tunnel %q registered with no public port (client %s)", spec.Name, session.ID)
	}

	m.tunnels[spec.Name] = tunnel
	return tunnel, nil
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
