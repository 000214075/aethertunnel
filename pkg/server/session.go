package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Session is one authenticated client control connection.
//
// Everything a session owns — its tunnels and the public connections waiting for
// a data stream — is registered here so that a disconnect can tear all of it down
// in one place. The v1 code added connections to a map and never removed them,
// which made the server refuse every client after the 100th connection it had
// ever seen.
type Session struct {
	ID               string
	ClientVersion    string
	Protocol         int
	Encrypted        bool
	RemoteAddr       string
	ConnectedAt      time.Time
	HeartbeatSeconds int

	conn   net.Conn
	framer *protocol.Framer
	done   chan struct{}
	once   sync.Once

	heartbeatAt atomic.Int64 // unix nanoseconds

	mu      sync.Mutex
	tunnels map[string]*Tunnel
	pending map[string]chan net.Conn
	closed  bool

	activeStreams   atomic.Int64
	totalStreams    atomic.Int64
	bytesFromClient atomic.Int64
	bytesToClient   atomic.Int64
}

func newSession(conn net.Conn, framer *protocol.Framer, req *protocol.AuthRequest, encrypted bool, heartbeatSeconds int) *Session {
	s := &Session{
		ID:               newID(8),
		ClientVersion:    req.ClientVersion,
		Protocol:         req.Protocol,
		Encrypted:        encrypted,
		RemoteAddr:       conn.RemoteAddr().String(),
		ConnectedAt:      time.Now(),
		HeartbeatSeconds: heartbeatSeconds,
		conn:             conn,
		framer:           framer,
		done:             make(chan struct{}),
		tunnels:          make(map[string]*Tunnel),
		pending:          make(map[string]chan net.Conn),
	}
	s.touchHeartbeat()
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Framer returns the control framer. Writes are serialised inside the framer, so
// it is safe to send a DataRequest from a tunnel goroutine while the session loop
// sends a heartbeat ack.
func (s *Session) Framer() *protocol.Framer { return s.framer }

func (s *Session) touchHeartbeat() { s.heartbeatAt.Store(time.Now().UnixNano()) }

// LastHeartbeat returns the time of the most recent heartbeat.
func (s *Session) LastHeartbeat() time.Time {
	return time.Unix(0, s.heartbeatAt.Load())
}

// Uptime is how long the session has been connected.
func (s *Session) Uptime() time.Duration { return time.Since(s.ConnectedAt) }

// RegisterProxy records a tunnel owned by this session.
func (s *Session) RegisterProxy(t *Tunnel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionClosed
	}
	s.tunnels[t.Name] = t
	return nil
}

// UnregisterProxy forgets a tunnel.
func (s *Session) UnregisterProxy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tunnels, name)
}

// Proxies lists the tunnel names this session owns, sorted for stable output.
func (s *Session) Proxies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tunnels))
	for name := range s.tunnels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Tunnels returns a snapshot of the session's tunnels.
func (s *Session) Tunnels() []*Tunnel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tunnel, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		out = append(out, t)
	}
	return out
}

// AddPending registers a public connection that is waiting for the client to dial
// back with a matching stream id.
func (s *Session) AddPending(streamID string, conn net.Conn) (<-chan net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	ch := make(chan net.Conn, 1)
	s.pending[streamID] = ch
	return ch, nil
}

// TakePending hands a waiting stream to the data connection that claimed it.
func (s *Session) TakePending(streamID string) (chan net.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.pending[streamID]
	if ok {
		delete(s.pending, streamID)
	}
	return ch, ok
}

// DropPending removes a pending stream that was never claimed.
func (s *Session) DropPending(streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, streamID)
}

// Close tears the session down exactly once: it notifies the session loop, fails
// every waiting public connection, closes the client's tunnel listeners and
// closes the control socket.
//
// The tunnel map is deliberately left in place: the session's owner unregisters
// them from the TunnelManager afterwards, and clearing the map here would make
// that unregistration a no-op — which is exactly how a stale tunnel entry kept a
// reconnecting client from re-registering its proxy.
func (s *Session) Close(reason string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		pending := s.pending
		s.pending = make(map[string]chan net.Conn)
		tunnels := make([]*Tunnel, 0, len(s.tunnels))
		for _, t := range s.tunnels {
			tunnels = append(tunnels, t)
		}
		s.mu.Unlock()

		close(s.done)

		for _, ch := range pending {
			close(ch)
		}
		for _, t := range tunnels {
			t.Close(reason)
		}
		_ = s.conn.Close()
	})
}

// IsClosed reports whether the session has been torn down. A tunnel whose owning
// session is closed is stale and may be replaced by a reconnecting client.
func (s *Session) IsClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// --- session registry ---------------------------------------------------------

var (
	errSessionClosed = errors.New("session is closed")
	errAtCapacity    = errors.New("server is at its connection limit")
)

// SessionManager tracks live sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	max      int
}

func newSessionManager(max int) *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session), max: max}
}

// Add registers a session, refusing it when the server is at capacity.
func (m *SessionManager) Add(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.max > 0 && len(m.sessions) >= m.max {
		return errAtCapacity
	}
	m.sessions[s.ID] = s
	return nil
}

// Remove deregisters a session.
func (m *SessionManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// Get looks a session up by id (used by data connections).
func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns a snapshot, newest first.
func (m *SessionManager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.After(out[j].ConnectedAt) })
	return out
}

// Count reports the number of live sessions.
func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CloseAll disconnects every session (used on shutdown).
func (m *SessionManager) CloseAll(reason string) {
	for _, s := range m.List() {
		s.Close(reason)
	}
}

// Pipe copies between the public connection and the data connection and records
// the byte counts on the session.
func (s *Session) RecordTraffic(toClient, fromClient int64) {
	s.bytesToClient.Add(toClient)
	s.bytesFromClient.Add(fromClient)
}

// newID returns n random bytes as hex.
func newID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not recoverable in a meaningful way here; fall
		// back to a timestamp so we at least stay unique within a process.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:n*2]
	}
	return hex.EncodeToString(buf)
}
