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

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// Session is one authenticated client control connection.
//
// dropLog counts dropped packets and writes the line they stood for at most
// once per dropLogInterval, so a client emitting packets at line rate cannot
// flood the log with one line per packet. The first drop still logs at once.
// add's check-store-swap is not atomic across callers: one dropLog belongs to
// the single goroutine that owns the drop (the read loop that sees it), and
// two goroutines sharing one would split the count and log a zero total.
type dropLog struct {
	at    atomic.Int64 // unix second of the last line
	count atomic.Uint64
}

const dropLogInterval = 30 // seconds between two lines about the same drop

// add records n more dropped packets and, when the last line about this
// counter is older than the interval, writes one carrying the total since the
// previous one.
func (d *dropLog) add(n uint64, write func(total uint64)) {
	d.count.Add(n)
	now := time.Now().Unix()
	if now-d.at.Load() < dropLogInterval {
		return
	}
	d.at.Store(now)
	write(d.count.Swap(0))
}

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
	// ClientID is the stable name the client sent in its auth request
	// (client.client_id, the host name by default). Empty from an older client.
	ClientID string
	// Metas carries the client's [metas] pairs; empty when it sent none.
	Metas map[string]string

	// srv is the server this session belongs to. A session outlives the handler
	// that accepted it — its tunnels ask it for streams from their own goroutines —
	// and putting a parked connection to use is the server's job.
	srv *Server

	conn net.Conn
	// framer is replaced when a post-quantum key agreement completes, and tunnel
	// goroutines write through it meanwhile, so it is read through Framer.
	framerMu sync.RWMutex
	framer   *protocol.Framer
	done     chan struct{}
	once     sync.Once

	// streamKey is the post-quantum session key, empty when the session agreed
	// none. It is the input for every data connection's own key.
	streamKey []byte

	heartbeatAt atomic.Int64 // unix nanoseconds

	// vpnPacketDrops and vpnPacketsWithoutAddress rate-limit the per-packet log
	// lines in sessionLoop's TypeVPNPacket case: an authenticated client can emit
	// tunnel packets at line rate, and one line per packet would flood the log.
	// Same pattern as clientlib's deliverVPN.
	vpnPacketDrops        dropLog
	vpnPacketsWithoutAddr dropLog
	// errorReports throttles the log line about TypeError frames: their
	// payload is the client's own text and can arrive at line rate.
	errorReports dropLog
	// unknownFrames throttles the log line about a control frame whose type the
	// loop does not handle: an authenticated client can repeat one at line rate.
	unknownFrames dropLog

	mu      sync.Mutex
	tunnels map[string]*Tunnel
	pending map[string]*pendingStream
	closed  bool
	// usageCounters holds the bytes each proxy name this session published has
	// carried. The counters belong to the name rather than to the member that is
	// current for it: a client that re-registers a name replaces its member and a
	// withdrawal detaches one, while the streams that started on it keep finishing
	// on another goroutine. The ledger bills these totals at teardown, so none of
	// those transitions may lose what the name already carried.
	usageCounters map[string]*trafficTotals

	// parked holds the data connections the client opened in advance so a
	// stream does not have to wait for a dial (client.pool_count). They are
	// taken in the order they arrived, and a connection the client has since
	// dropped is discovered when it is put to use, which falls back to asking
	// over the control connection.
	parked []parkedConn

	// streams holds the data connections that are serving a visitor right now.
	// They are sockets of their own, separate from the control connection, so a
	// session that ends has to close them: the shutdown log says the streams still
	// running when the grace ran out are disconnected, and the client that owns
	// them only finds out about the control connection closing.
	streams map[*dataConn]struct{}

	activeStreams   atomic.Int64
	totalStreams    atomic.Int64
	bytesFromClient atomic.Int64
	bytesToClient   atomic.Int64

	// draining is set while the server winds down. A draining session still
	// carries the streams it already opened, but refuses to open another one.
	draining atomic.Bool

	// authSecret is the credential this session authenticated with: the static
	// token for token auth, or the access token for [oidc] auth. It is what
	// the client presented, so per-proxy stream keys derived from it match the
	// client's side under either authentication; cfg.Server.AuthToken would
	// only match the static case.
	authSecret string

	// vpnPeer and vpnTransport are set when the session was given a tunnel
	// address. The transport is what the control loop hands received packets to.
	vpnMu        sync.Mutex
	vpnPeer      *vpn.Peer
	vpnTransport *vpn.ChannelTransport
	vpnAddress   string
	// vpnOutReady gates the transport's outbound path. attachVPN wires the
	// transport — and the router starts serving it — before the auth response
	// is written and the framer switched, so a packet for the freshly leased
	// address can be ready in that window; see writeVPNPacket.
	vpnOutReady atomic.Bool
}

// writeVPNPacket writes one tunnel packet to the client, or drops it while the
// session is not yet ready. attachVPN attaches the transport before the auth
// response goes out, and the router's peer begins serving at once, so a packet
// for the new address can arrive in that window. Written then it would either
// precede the AuthResponse — the client's ReadJSON(TypeAuthResponse) refuses the
// unexpected type and the login fails — or be framed with the superseded cipher
// the client has already left. Dropping is right: the address is already
// routable, the frame is a tunnel packet TCP inside the tunnel will retransmit,
// and the response must not be reordered.
func (s *Session) writeVPNPacket(packet []byte) error {
	if !s.vpnOutReady.Load() {
		return nil
	}
	return s.Framer().WriteFrame(&protocol.Message{Type: protocol.TypeVPNPacket, Payload: packet})
}

// markVPNReady opens the tunnel transport's outbound path, once the auth response
// has been written and the session's framer has switched to the agreed key.
func (s *Session) markVPNReady() { s.vpnOutReady.Store(true) }

// setVPN records the tunnel peer and transport of a session that was given an
// address.
func (s *Session) setVPN(peer *vpn.Peer, transport *vpn.ChannelTransport) {
	s.vpnMu.Lock()
	defer s.vpnMu.Unlock()
	s.vpnPeer = peer
	s.vpnTransport = transport
	if peer != nil && peer.Address() != nil {
		s.vpnAddress = peer.Address().String()
	}
}

// vpnTransportFor returns the transport a received packet belongs to, or nil when
// the session has no tunnel.
func (s *Session) vpnTransportFor() *vpn.ChannelTransport {
	s.vpnMu.Lock()
	defer s.vpnMu.Unlock()
	return s.vpnTransport
}

// VPNAddress is the tunnel address the session holds, or an empty string.
func (s *Session) VPNAddress() string {
	s.vpnMu.Lock()
	defer s.vpnMu.Unlock()
	return s.vpnAddress
}

func newSession(srv *Server, conn net.Conn, framer *protocol.Framer, req *protocol.AuthRequest, encrypted bool, heartbeatSeconds int) *Session {
	s := &Session{
		srv:              srv,
		ID:               newID(8),
		ClientID:         req.ClientID,
		ClientVersion:    req.ClientVersion,
		Metas:            req.Metas,
		authSecret:       req.Token,
		Protocol:         req.Protocol,
		Encrypted:        encrypted,
		RemoteAddr:       conn.RemoteAddr().String(),
		ConnectedAt:      time.Now(),
		HeartbeatSeconds: heartbeatSeconds,
		conn:             conn,
		framer:           framer,
		done:             make(chan struct{}),
		tunnels:          make(map[string]*Tunnel),
		pending:          make(map[string]*pendingStream),
	}
	s.touchHeartbeat()
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Framer returns the control framer. Writes are serialised inside the framer, so
// it is safe to send a DataRequest from a tunnel goroutine while the session loop
// sends a heartbeat ack.
func (s *Session) Framer() *protocol.Framer {
	s.framerMu.RLock()
	defer s.framerMu.RUnlock()
	return s.framer
}

// SetFramer replaces the control framer, which happens once, when a post-quantum
// key agreement completes.
func (s *Session) SetFramer(framer *protocol.Framer) {
	s.framerMu.Lock()
	defer s.framerMu.Unlock()
	s.framer = framer
}

func (s *Session) touchHeartbeat() { s.heartbeatAt.Store(time.Now().UnixNano()) }

// LastHeartbeat returns the time of the most recent heartbeat.
func (s *Session) LastHeartbeat() time.Time {
	return time.Unix(0, s.heartbeatAt.Load())
}

// Uptime is how long the session has been connected.
func (s *Session) Uptime() time.Duration { return time.Since(s.ConnectedAt) }

// ClientName is what the server calls this client in its log, its audit records
// and its dashboard: the stable client_id the auth request carried, or the
// session's own random identifier when the client sent none (an older client).
func (s *Session) ClientName() string {
	if s.ClientID != "" {
		return s.ClientID
	}
	return s.ID
}

// trafficTotals is what one proxy name carried on one session.
type trafficTotals struct {
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// usageOf returns the byte counters of one proxy name, creating them on first
// registration, and reports whether it created them. The same pointer is handed
// to every member that publishes the name, so a replacement continues where its
// predecessor left off. The created flag is what lets a registration that never
// published anything take its counter back again: a counter that was already
// there belongs to a name this session carried before, and its bytes are still
// owed to the ledger whether or not this attempt succeeds.
func (s *Session) usageOf(name string) (*trafficTotals, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageCounters == nil {
		s.usageCounters = make(map[string]*trafficTotals)
	}
	totals, ok := s.usageCounters[name]
	if !ok {
		totals = &trafficTotals{}
		s.usageCounters[name] = totals
	}
	return totals, !ok
}

// forgetUsage drops the counter of a name this session never published, so the
// ledger does not bill an entry for it. It is only safe for a counter usageOf
// has just created: once a member has been reachable, a stream may be holding
// the pointer and writing to it, and the name must stay.
func (s *Session) forgetUsage(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.usageCounters, name)
}

// Usage is the session's per-proxy byte totals, which is what the ledger bills and
// what the dashboard's unbilled figure sums. A withdrawn name is still here: its
// bytes were carried on this session and belong in its entry.
func (s *Session) Usage() map[string][2]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][2]int64, len(s.usageCounters))
	for name, totals := range s.usageCounters {
		out[name] = [2]int64{totals.bytesIn.Load(), totals.bytesOut.Load()}
	}
	return out
}

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

// dataConn is a client's data connection together with the framing that protects
// it. A byte-stream proxy uses conn directly; a datagram proxy exchanges
// TypeUDPPacket frames through the framer, whose frame boundaries preserve the
// datagram boundaries.
type dataConn struct {
	conn   net.Conn
	framer *protocol.Framer
	// cipher protects the raw bytes of a byte-stream proxy. For a datagram
	// proxy the framer already seals each frame, so it is unused.
	cipher *crypto.Cipher
}

// Close releases the underlying connection.
func (d *dataConn) Close() error { return d.conn.Close() }

// streamResult is what a pending stream resolves to: the client's data
// connection, or the reason the client could not provide one.
type streamResult struct {
	conn *dataConn
	err  error
}

// AddPending registers a stream that a public connection or a visitor is waiting
// to be paired with. The client answers with a DataOpen carrying the same id, and
// that answer either delivers the data connection or reports why it could not be
// made.
func (s *Session) AddPending(streamID string) (*pendingStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	pending := &pendingStream{ch: make(chan streamResult, 1)}
	s.pending[streamID] = pending
	return pending, nil
}

// pendingStream is one stream waiting for the client's data connection. The
// abandoned flag is what makes giving up exact: the waiter and the deliverer
// coordinate on it under the session's lock, so a connection that arrives after the
// visitor gave up is closed by whoever holds it instead of being queued for nobody.
type pendingStream struct {
	ch        chan streamResult
	abandoned bool
}

// TakePending hands a waiting stream to the data connection that claimed it.
func (s *Session) TakePending(streamID string) (*pendingStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[streamID]
	if ok {
		delete(s.pending, streamID)
	}
	return pending, ok
}

// abandonPending marks a stream this side has given up on and drops it from the
// map, so a data connection that claims it afterwards is refused instead of queued
// for nobody. The waiter's own pending is what is marked: the deliverer may already
// have taken the entry out of the map, and marking the map's copy would then miss
// the one the deliverer is about to hand over. The caller drains what was already
// queued with closeLateStream.
func (s *Session) abandonPending(streamID string, pending *pendingStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, streamID)
	if pending != nil {
		pending.abandoned = true
	}
}

// resolveStream hands a result to the visitor waiting for this stream and reports
// whether anyone was still waiting.
//
// It and abandonPending run under the same lock, which is what closes the window
// between giving up and the client's answer arriving: either the result is queued
// here and the give-up drains it, or the stream was marked first and the caller
// closes the connection it was about to hand over. Without that, a connection that
// arrived in the window sat in the buffered channel with nobody to read it, holding
// the socket and the client's stream slot until the session ended.
func (s *Session) resolveStream(pending *pendingStream, result streamResult) bool {
	if pending == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending.abandoned {
		return false
	}
	select {
	case pending.ch <- result:
		return true
	default:
		return false
	}
}

// closeLateStream closes a data connection the client delivered in the instant this
// side gave up on the stream.
func closeLateStream(pending *pendingStream) {
	if pending == nil {
		return
	}
	select {
	case late := <-pending.ch:
		if late.conn != nil {
			_ = late.conn.Close()
		}
	default:
	}
}

// DropPending removes a pending stream that was never claimed.
func (s *Session) DropPending(streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, streamID)
}

// parkedConn is a data connection the client opened before it was needed. A
// connection enters the list before its confirmation has crossed the wire and
// only becomes visible to TakeParked once confirmed: the client's first read on
// a pooled connection must be the DataOpenAck, and handing the connection out
// before the ack is written would let a DataRequest arrive there first.
type parkedConn struct {
	conn      net.Conn
	framer    *protocol.Framer
	confirmed bool
}

// errPoolFull is returned by Park when the session already holds as many parked
// connections as server.max_pool_count allows.
var errPoolFull = errors.New("this session already holds the maximum number of parked connections")

// poolCap is how many connections this session may hold parked: the server's
// max_pool_count, which frp calls maxPoolCount. A configuration that predates the
// key keeps the built-in 64.
func (s *Session) poolCap() int {
	if s.srv == nil || s.srv.cfg.Server.MaxPoolCount <= 0 {
		return config.MaxPoolCount
	}
	return s.srv.cfg.Server.MaxPoolCount
}

// Park keeps a data connection until a stream needs it. It reports a full pool
// and a closed session, both of which the caller answers rather than ignores.
// The connection occupies a slot immediately but is not handed out until the
// caller records its confirmation with ConfirmParked, so the pool never offers
// a connection whose ack is still in flight.
func (s *Session) Park(conn net.Conn, framer *protocol.Framer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionClosed
	}
	if len(s.parked) >= s.poolCap() {
		return errPoolFull
	}
	s.parked = append(s.parked, parkedConn{conn: conn, framer: framer})
	return nil
}

// ConfirmParked marks a parked connection as usable by TakeParked. It reports
// whether the connection is still held: a session that closed in between has
// already closed the connection out from under the confirmation.
func (s *Session) ConfirmParked(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.parked {
		if s.parked[i].conn == conn {
			s.parked[i].confirmed = true
			return true
		}
	}
	return false
}

// TakeParked returns the oldest confirmed parked connection, or false when none
// is held. Connections whose confirmation has not crossed yet are skipped and
// stay parked: they either confirm in a moment or are dropped with their slot.
// The caller owns the returned connection from here: it is no longer closed by
// the session's teardown, so a caller that does not put it to use has to close
// it.
func (s *Session) TakeParked() (net.Conn, *protocol.Framer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.parked {
		if !s.parked[i].confirmed {
			continue
		}
		parked := s.parked[i]
		s.parked = append(s.parked[:i], s.parked[i+1:]...)
		return parked.conn, parked.framer, true
	}
	return nil, nil, false
}

// DropParked removes conn from the parked pool when it is still held, so a
// parked connection that failed its confirmation stops occupying a slot the
// next TakeParked would hand out as a dead connection.
func (s *Session) DropParked(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, parked := range s.parked {
		if parked.conn == conn {
			s.parked = append(s.parked[:i], s.parked[i+1:]...)
			return true
		}
	}
	return false
}

// parkedCount reports how many connections are waiting, which the session's
// teardown needs to know and a test can read.
func (s *Session) parkedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.parked)
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
		s.pending = make(map[string]*pendingStream)
		parked := s.parked
		s.parked = nil
		tunnels := make([]*Tunnel, 0, len(s.tunnels))
		for _, t := range s.tunnels {
			tunnels = append(tunnels, t)
		}
		s.mu.Unlock()

		close(s.done)

		// A parked connection is one the client is holding open for nothing once
		// the session ends, so it is closed here rather than left to the client's
		// own read to discover.
		for _, p := range parked {
			_ = p.conn.Close()
		}

		for _, waiting := range pending {
			// The channel is buffered, so this never blocks: a waiting visitor
			// sees the session end instead of its dial timeout. A stream a data
			// connection already claimed is out of this map, so this and
			// resolveStream never race for one channel.
			waiting.ch <- streamResult{err: errSessionClosed}
		}
		for _, t := range tunnels {
			// Removing the member closes the endpoint only when it was the last
			// one, so a pool survives the loss of one client. A session that is
			// closed before its control handler tears it down — which is what a
			// shutdown does — is where its members actually leave, so the removal
			// carries the reason this session was closed for.
			t.group.remove(t, reason)
		}

		s.vpnMu.Lock()
		transport := s.vpnTransport
		s.vpnTransport, s.vpnPeer, s.vpnAddress = nil, nil, ""
		s.vpnMu.Unlock()
		if transport != nil {
			// Closing the transport stops the peer's serve loop, which detaches it
			// from the router.
			_ = transport.Close()
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

// beginDrain stops this session from opening further streams. The streams that are
// already running keep working until they finish or the server's grace period ends.
func (s *Session) beginDrain() { s.draining.Store(true) }

// isDraining reports whether the server is winding down and this session may no
// longer open a stream.
func (s *Session) isDraining() bool { return s.draining.Load() }

// ActiveStreams is the number of streams this session is carrying right now.
func (s *Session) ActiveStreams() int64 { return s.activeStreams.Load() }

// streamOpened books a stream this session is about to carry. It is paired with
// streamClosed, which the stream's release function calls.
func (s *Session) streamOpened() {
	s.activeStreams.Add(1)
	s.totalStreams.Add(1)
}

// streamClosed books the end of a stream.
func (s *Session) streamClosed() { s.activeStreams.Add(-1) }

// trackStream records a data connection that is serving a visitor, so Close can
// disconnect it. Every stream opened for a visitor goes through it, whatever the
// proxy type.
func (s *Session) trackStream(dc *dataConn) {
	s.mu.Lock()
	if s.streams == nil {
		s.streams = make(map[*dataConn]struct{})
	}
	s.streams[dc] = struct{}{}
	s.mu.Unlock()
}

// untrackStream drops a data connection whose stream has ended, whether it
// finished on its own or disconnectStreams ended it.
func (s *Session) untrackStream(dc *dataConn) {
	s.mu.Lock()
	delete(s.streams, dc)
	s.mu.Unlock()
}

// disconnectStreams closes the data connections still serving a visitor. The
// streams themselves end through their own teardown when a session is lost in the
// ordinary way, and their bytes are billed as they finish; this is for the one
// path that cannot wait: a shutdown whose grace period has run out. The
// connections are sockets of their own, separate from the control connection, so
// closing the control connection does not reach them.
func (s *Session) disconnectStreams() {
	s.mu.Lock()
	streams := make([]*dataConn, 0, len(s.streams))
	for dc := range s.streams {
		streams = append(streams, dc)
	}
	s.mu.Unlock()
	for _, dc := range streams {
		_ = dc.Close()
	}
}

// --- session registry ---------------------------------------------------------

var (
	errSessionClosed = errors.New("session is closed")
	errAtCapacity    = errors.New("server is at its connection limit")
	errShuttingDown  = errors.New("server is shutting down")
)

// SessionManager tracks live sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	max      int
	// closing is set once CloseAll has run. A handler whose control
	// connection was already sitting in the accept backlog can finish
	// authenticating after that, and a session registered then would miss
	// the CloseAll snapshot: it would run until the client disconnected on
	// its own, hold Run's WaitGroup open past the graceful-shutdown window,
	// and keep binding public ports.
	closing bool
}

func newSessionManager(max int) *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session), max: max}
}

// Add registers a session, refusing it when the server is at capacity or
// shutting down.
func (m *SessionManager) Add(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errShuttingDown
	}
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

// CloseAll disconnects every session (used on shutdown). It also closes the
// registry to new arrivals under the same lock that takes the snapshot, so a
// handler that authenticates around this call is refused instead of becoming
// a session nothing will ever close. The snapshot is returned because it is
// only complete here: once the control connections close, their handlers
// remove the sessions from the registry, so a list taken later would miss
// every session whose teardown already ran.
func (m *SessionManager) CloseAll(reason string) []*Session {
	m.mu.Lock()
	m.closing = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close(reason)
	}
	return sessions
}

// drainPollInterval is how often Drain looks at the active stream count. It trades
// a little latency at shutdown for not waking up per stream.
const drainPollInterval = 25 * time.Millisecond

// Drain stops every session from opening another stream and waits for the streams
// that are already running to finish, up to grace.
//
// It returns the number of streams that were still running when it stopped waiting,
// which is zero when everything finished inside the grace period. Sessions that are
// idle do not delay it: what it waits for is traffic in flight, not connections,
// because a control connection only ends when its client decides to leave.
func (m *SessionManager) Drain(grace time.Duration) int64 {
	sessions := m.List()
	for _, s := range sessions {
		s.beginDrain()
	}
	if len(sessions) == 0 {
		return 0
	}

	active := func() int64 {
		var n int64
		for _, s := range sessions {
			n += s.ActiveStreams()
		}
		return n
	}

	if grace <= 0 {
		return active()
	}

	deadline := time.Now().Add(grace)
	for {
		remaining := active()
		if remaining == 0 || !time.Now().Before(deadline) {
			return remaining
		}
		time.Sleep(drainPollInterval)
	}
}

// RecordTraffic books the bytes a finished stream carried, as the relay counted
// them: the copying happens in the pipe helper, and this records its result on the
// session for the ledger and the dashboard.
func (s *Session) RecordTraffic(toClient, fromClient int64) {
	s.bytesToClient.Add(toClient)
	s.bytesFromClient.Add(fromClient)
}

// idFallback orders the ids generated while crypto/rand is unusable.
var idFallback atomic.Uint64

// newID returns n random bytes as hex.
func newID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not recoverable in a meaningful way here. The
		// old fallback truncated a date-first timestamp, so every id generated
		// in the same minute was the same string — and session ids are what data
		// connections use to find their session. Mix a process-wide counter with
		// the clock instead, which stays unique within the process whatever the
		// clock's resolution.
		seed := uint64(time.Now().UnixNano()) + idFallback.Add(1)*0x9e3779b97f4a7c15
		for i := range buf {
			seed = seed*6364136223846793005 + 1442695040888963407
			buf[i] = byte(seed >> 56)
		}
	}
	return hex.EncodeToString(buf)
}
