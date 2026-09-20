package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// matchesSecret compares a presented secret with an expected one in constant time.
func matchesSecret(expected, presented string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// openStream asks the member's client for a fresh data connection and waits for
// it to dial back.
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
	case <-t.Session.Done():
		t.Session.DropPending(streamID)
		release()
		return nil, nil, errors.New("the session ended while the stream was pending")
	}
}

// pipeStream copies raw bytes between a visitor-facing connection and a client
// data connection, recording the traffic. Both connections are closed when it
// returns.
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

	t.logger.Printf("proxy %q: stream for %s finished (%d bytes out, %d bytes in)",
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

	t.logger.Printf("proxy %q: datagram session for %s finished (%d bytes out, %d bytes in)",
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

// --- the registry -------------------------------------------------------------

// TunnelManager owns every published proxy on the server.
type TunnelManager struct {
	mu     sync.RWMutex
	groups map[string]*ProxyGroup

	cfg      *config.Config
	logger   *log.Logger
	cipher   *crypto.Cipher
	sessions *SessionManager
	metrics  *Metrics

	vhost     *vhostSet
	p2p       *p2pRendezvous
	directory *directory
}

func newTunnelManager(cfg *config.Config, logger *log.Logger, cipher *crypto.Cipher, sessions *SessionManager, metrics *Metrics) *TunnelManager {
	return &TunnelManager{
		groups:   make(map[string]*ProxyGroup),
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
	if spec.Multipath < 0 || spec.Multipath > config.MaxMultipath {
		return nil, fmt.Errorf("multipath %d is out of range (0-%d)", spec.Multipath, config.MaxMultipath)
	}
	if config.IsPrivateProxyType(spec.Type) {
		if spec.SecretKey == "" {
			return nil, fmt.Errorf("proxy type %q is private and needs a secret_key", spec.Type)
		}
		if spec.RemotePort != 0 {
			return nil, fmt.Errorf("proxy type %q is private and must not open a public port", spec.Type)
		}
		if spec.AuthMethod == "" {
			spec.AuthMethod = config.AuthMethodSecret
		}
	}

	m.mu.Lock()
	group, exists := m.groups[spec.Name]
	if !exists {
		group = newProxyGroup(spec, m)
		m.groups[spec.Name] = group
	} else if group.Type != spec.Type {
		m.mu.Unlock()
		return nil, fmt.Errorf("proxy %q is already published as type %q", spec.Name, group.Type)
	}
	m.mu.Unlock()

	member, err := group.add(session, spec)
	if err != nil {
		if group.memberCount() == 0 {
			m.mu.Lock()
			if current, ok := m.groups[spec.Name]; ok && current == group {
				delete(m.groups, spec.Name)
			}
			m.mu.Unlock()
		}
		return nil, err
	}

	if members := group.memberCount(); members > 1 {
		m.logger.Printf("proxy %q: session %s joined the pool (%d members, strategy %s)",
			spec.Name, session.ID, members, group.strategy)
	} else {
		// The record names the address a visitor dials, so it is written when the
		// proxy first appears rather than once per pool member. A DHT failure is
		// logged by the directory and is not fatal: the proxy still works for a
		// visitor that knows the address.
		_ = m.directory.publish(spec)
	}
	return member, nil
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

// Unregister removes the given session's registration of a proxy. Other members
// of the same pool keep the endpoint published.
func (m *TunnelManager) Unregister(name string, session *Session, reason string) {
	m.mu.RLock()
	group, ok := m.groups[name]
	m.mu.RUnlock()
	if !ok {
		return
	}

	for _, member := range group.Members() {
		if session != nil && member.Session != session {
			continue
		}
		if group.remove(member) {
			m.mu.Lock()
			if current, still := m.groups[name]; still && current == group {
				delete(m.groups, name)
			}
			m.mu.Unlock()
			// The last member is gone, so the name is no longer served here.
			// Copies already replicated to DHT peers lapse within one TTL.
			m.directory.withdraw(name)
		}
		if session != nil {
			return
		}
	}
}

// Get looks a proxy up by name.
func (m *TunnelManager) Get(name string) (*ProxyGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	group, ok := m.groups[name]
	if !ok {
		return nil, errNoSuchTunnel
	}
	return group, nil
}

// List returns every member of every group, ordered by proxy name and then by
// registration order.
func (m *TunnelManager) List() []*Tunnel {
	m.mu.RLock()
	groups := make([]*ProxyGroup, 0, len(m.groups))
	for _, group := range m.groups {
		groups = append(groups, group)
	}
	m.mu.RUnlock()

	out := make([]*Tunnel, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.Members()...)
	}
	return out
}

// Groups returns a snapshot of the published proxies, ordered by name.
func (m *TunnelManager) Groups() []*ProxyGroup {
	m.mu.RLock()
	out := make([]*ProxyGroup, 0, len(m.groups))
	for _, group := range m.groups {
		out = append(out, group)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RemoveSessionTunnels stops everything a disconnecting session published. Other
// members of a pool keep the endpoint alive.
func (m *TunnelManager) RemoveSessionTunnels(s *Session, reason string) {
	for _, name := range s.Proxies() {
		m.Unregister(name, s, reason)
	}
}
