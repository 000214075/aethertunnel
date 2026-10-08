package net

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// maxUDPPayload is the largest payload a UDP datagram can carry, which is what
// an unset MaxDatagram relays.
const maxUDPPayload = 65535

// socketErrorLogLimit is how many socket errors — reads in Run, reply writes
// in the session readers — get logged one by one before the burst falls back
// to a once-a-minute summary.
const socketErrorLogLimit = 64

// DefaultMaxDatagramSessions caps how many source addresses one pump tracks when
// MaxSessions is unset. Each tracked address costs a goroutine and a stream to the
// tunnel's far end, and a source address is free to forge, so an untracked number
// of them turns a public udp port into an amplifier.
const DefaultMaxDatagramSessions = 4096

// DatagramPump relays datagrams between a packet socket and one framed stream per
// source address.
//
// UDP has no connections, so the pump invents a session on the first datagram
// from an address and releases it once that address has been quiet for the idle
// timeout. Each session carries its datagrams over a single stream whose frame
// boundaries preserve the datagram boundaries; the same code therefore serves a
// udp proxy on the server, a udp proxy on the client, and a sudp visitor
// listener.
type DatagramPump struct {
	// Socket is the public datagram socket the pump reads from and replies on.
	Socket net.PacketConn
	// Open opens the stream carrying one address's datagrams. The release
	// function is called when the session ends.
	Open func(addr net.Addr) (*protocol.Framer, func(), error)
	// Close is called when a session ends, with the bytes carried in each
	// direction: toPeer counts datagrams written towards the stream, fromPeer
	// counts datagrams read from it.
	Close func(addr net.Addr, toPeer, fromPeer int64)
	// OnDatagram, when set, is called for each relayed datagram.
	OnDatagram func(toPeer bool, n int)
	// MaxDatagram caps a relayed datagram's payload. A datagram over the cap
	// is dropped rather than truncated, and OnOversize hears about it; the
	// read buffer stays one byte past the cap so an oversized datagram is
	// detectable instead of silently shortened. Zero or a value above the
	// largest UDP payload selects that payload.
	MaxDatagram int
	// OnOversize, when set, is called for a datagram dropped for exceeding
	// MaxDatagram. The read buffer holds one byte past the cap, so n is the cap
	// plus one whatever the datagram's real size was: enough to count the drops,
	// not to learn how large they were.
	OnOversize func(n int)
	// OnSession, when set, is called with +1 when a source address starts being
	// tracked and -1 when it is released.
	OnSession func(delta int)
	// MaxSessions caps how many source addresses are tracked at once. A datagram
	// from an address not yet tracked when the cap is reached is dropped, and
	// OnSessionLimit hears about it; the addresses already tracked keep working.
	// Zero selects DefaultMaxDatagramSessions.
	MaxSessions int
	// OnSessionLimit, when set, is called with the source address of a datagram
	// dropped because MaxSessions addresses are already tracked.
	OnSessionLimit func(addr net.Addr)
	// IdleTimeout releases a session that has been quiet this long. A value of
	// zero selects 2 minutes.
	IdleTimeout time.Duration
	// Paths is how many parallel streams carry one address's datagrams. Values
	// below two select one path. Datagrams are spread over the paths in turn,
	// which is safe because a datagram is independent of every other one; replies
	// are accepted from any of them.
	Paths int
	// Logger receives diagnostics. It may be nil.
	Logger *log.Logger

	mu       sync.Mutex
	sessions map[string]*datagramSession
	closed   atomic.Bool
	// reapOnce starts the session reaper at most once, whichever entry point the
	// caller used.
	reapOnce sync.Once
}

// datagramSession is one remote address using a datagram tunnel.
type datagramSession struct {
	pump *DatagramPump
	addr net.Addr

	// framers holds one open stream per path; releases mirrors it.
	framers  []*protocol.Framer
	releases []func()
	next     atomic.Uint64

	lastSeen atomic.Int64
	toPeer   atomic.Int64
	fromPeer atomic.Int64

	mu sync.Mutex

	once sync.Once
	done chan struct{}
}

func (p *DatagramPump) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

func (s *datagramSession) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

func (s *datagramSession) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.lastSeen.Load()))
}

// Run reads datagrams until the socket is closed. It blocks. Start runs it in the
// background; calling it directly is supported too, and it fills in the same
// defaults and session table that Start does.
func (p *DatagramPump) Run() {
	p.prepare()
	limit := p.MaxDatagram
	if limit <= 0 || limit > maxUDPPayload {
		limit = maxUDPPayload
	}
	// One byte past the cap tells a datagram that overflows it apart from one
	// that exactly fills it: the read returns the extra byte, and the session
	// sees nothing.
	buf := make([]byte, limit+1)
	// readErrors counts the read failures of the current burst, and lastSummary
	// is when the burst last earned its one-line summary. The retry loop spins
	// every 50 ms, so a socket that fails for hours — an interface that went
	// down, a tun device in a bad state — would otherwise write one line per
	// spin for the whole time and bury everything else the process logs.
	var readErrors int
	var lastSummary time.Time
	for {
		n, addr, err := p.Socket.ReadFrom(buf)
		if err != nil {
			if p.closed.Load() || errors.Is(err, net.ErrClosed) {
				// The socket going away is how this ends, whether the caller shut
				// the pump down or closed the socket underneath it: retrying a
				// closed socket spun this loop, and its log line, forever.
				//
				// The same teardown Shutdown performs, because Run can be called
				// on its own: without it the reaper kept ticking on a pump that
				// will never read again, and the sessions it had to release were
				// left in the table for the idle timeout. Shutdown is idempotent
				// (datagramSession.finish is once-guarded), so a caller that
				// called it before closing the socket is unaffected.
				p.Shutdown()
				return
			}
			readErrors++
			if readErrors <= socketErrorLogLimit {
				p.logf("datagram pump: read error: %v", err)
			} else if lastSummary.IsZero() || time.Since(lastSummary) >= time.Minute {
				lastSummary = time.Now()
				p.logf("datagram pump: %d read error(s) so far: %v", readErrors, err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// A successful read ends the burst: the next failure starts a fresh
		// count, so a recovered-then-broken socket is logged in detail again.
		readErrors = 0
		lastSummary = time.Time{}
		if n > limit {
			if p.OnOversize != nil {
				p.OnOversize(n)
			}
			continue
		}
		datagram := make([]byte, n)
		copy(datagram, buf[:n])
		p.deliver(addr, datagram)
	}
}

// prepare fills the defaults, creates the session table and starts the reaper. It
// is idempotent and runs before any goroutine reads the pump, so a caller that
// asks Sessions() or shuts the pump down straight after Start never reads the map
// while Run is assigning it. Run calls it too: Run is exported and can be called
// on its own, where a nil table meant a panic on the first new address and a zero
// MaxSessions meant every datagram was dropped.
func (p *DatagramPump) prepare() {
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 2 * time.Minute
	}
	if p.MaxSessions <= 0 {
		p.MaxSessions = DefaultMaxDatagramSessions
	}
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = make(map[string]*datagramSession)
	}
	p.mu.Unlock()
	p.reapOnce.Do(func() { go p.reap() })
}

// Start runs the pump and its session reaper in the background.
func (p *DatagramPump) Start() {
	p.prepare()
	go p.Run()
}

// deliver routes one datagram, creating a session when the address is new.
func (p *DatagramPump) deliver(addr net.Addr, datagram []byte) {
	key := addr.String()

	p.mu.Lock()
	// A datagram that arrives as the pump shuts down has no session to join and no
	// client left to carry it, so it is dropped rather than opening one that only
	// the shutdown that is already running could have closed.
	if p.closed.Load() {
		p.mu.Unlock()
		return
	}
	session, known := p.sessions[key]
	if !known {
		if len(p.sessions) >= p.MaxSessions {
			// The cap bounds what one public datagram port can make this process
			// hold: every tracked address keeps a goroutine and a stream to the
			// tunnel's far end, and the address is the sender's to forge.
			p.mu.Unlock()
			if p.OnSessionLimit != nil {
				p.OnSessionLimit(addr)
			}
			return
		}
		session = &datagramSession{pump: p, addr: addr, done: make(chan struct{})}
		session.touch()
		p.sessions[key] = session
		// Counting the session under the same lock that publishes it keeps the gauge
		// right when a shutdown races this datagram: the shutdown takes the lock to
		// list the sessions, so it either misses this one or sees it after the +1 and
		// balances it with its own -1. Counting after the unlock left an extra +1
		// behind, because the shutdown's -1 and the release both ran first.
		if p.OnSession != nil {
			p.OnSession(1)
		}
	}
	p.mu.Unlock()

	session.touch()
	if !known {
		go p.establish(session, datagram)
		return
	}
	if framer := session.pick(); framer != nil {
		session.send(framer, datagram)
	}
	// While a session is still being established the datagram is dropped: UDP is
	// lossy by contract, and blocking this loop on a round trip would stall
	// every other address of the same tunnel.
}

// pick returns the path that should carry the next datagram, or nil while the
// session is still being established.
func (s *datagramSession) pick() *protocol.Framer {
	s.mu.Lock()
	count := len(s.framers)
	s.mu.Unlock()

	if count == 0 {
		return nil
	}
	if count == 1 {
		// Through the bounds-checked accessor like the multi-path case: the count
		// was read before the lock was taken again, and closeFramers replaces the
		// slice with nil in between (a reaper, a shutdown or a failed write runs
		// on another goroutine), so indexing here panicked the process.
		return s.pathAt(0)
	}
	// The modulo runs in the unsigned domain: on a 32-bit build, int of a
	// counter past 2^31 wraps negative, a negative Go modulo stays negative,
	// and pathAt would silently drop every datagram until the counter wrapped.
	return s.pathAt(int((s.next.Add(1) - 1) % uint64(count)))
}

func (s *datagramSession) pathAt(index int) *protocol.Framer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.framers) {
		return nil
	}
	return s.framers[index]
}

func (s *datagramSession) pathCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.framers)
}

// setFramers installs the paths of a new session and reports whether the session
// took them.
//
// It reports false when the session ended while its paths were being opened: the
// shutdown, the reaper or a failed write can release a session before its first
// path comes up, and closeFramers has already run by then — it saw no paths and
// will not look again. Reporting that here is what lets establish close the paths
// it just opened instead of leaking the data connections and the goroutines that
// would read them.
func (s *datagramSession) setFramers(framers []*protocol.Framer, releases []func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
		return false
	default:
	}

	s.framers = framers
	s.releases = releases
	return true
}

// send forwards one datagram towards the stream.
func (s *datagramSession) send(framer *protocol.Framer, datagram []byte) {
	if err := framer.WriteFrame(&protocol.Message{
		Type:    protocol.TypeUDPPacket,
		Payload: datagram,
	}); err != nil {
		s.finish("write failed")
		return
	}
	s.toPeer.Add(int64(len(datagram)))
	if s.pump.OnDatagram != nil {
		s.pump.OnDatagram(true, len(datagram))
	}
}

// establish opens the streams for a new session and starts carrying the peer's
// replies back to the source address.
//
// A session needs at least one path; when some of the requested paths fail, the
// session runs on the ones that came up rather than refusing the visitor
// outright.
func (p *DatagramPump) establish(s *datagramSession, first []byte) {
	paths := p.Paths
	if paths < 1 {
		paths = 1
	}

	framers := make([]*protocol.Framer, 0, paths)
	releases := make([]func(), 0, paths)
	var lastErr error

	for i := 0; i < paths; i++ {
		framer, release, err := p.Open(s.addr)
		if err != nil {
			lastErr = err
			continue
		}
		framers = append(framers, framer)
		releases = append(releases, release)
	}

	if len(framers) == 0 {
		p.logf("datagram pump: cannot open a session for %s: %v", s.addr, lastErr)
		s.fail()
		return
	}
	if !s.setFramers(framers, releases) {
		// The session ended while its paths were being opened, and closeFramers has
		// already run without seeing them, so they are closed here: otherwise the
		// data connections stay open and the reader goroutines below never return.
		for _, framer := range framers {
			_ = framer.Close()
		}
		for _, release := range releases {
			if release != nil {
				release()
			}
		}
		return
	}

	// Replies may arrive on any path, so every one of them is read.
	for _, framer := range framers {
		go p.readReplies(s, framer)
	}

	s.send(framers[0], first)

	if len(framers) > 1 {
		p.logf("datagram session for %s is spread over %d paths", s.addr, len(framers))
	}
}

// readReplies forwards one path's frames back to the source address. The first
// path to fail reading ends the whole session, because a half-open multipath
// session would silently drop the datagrams that were spread onto the failed
// path. A failed write back to the source address is a different thing — the
// path and the socket are fine, the kernel just refused one send — and used to
// end the session there, tearing down an established tunnel exactly when
// traffic peaked. It now gets the same burst policy the socket's reads in Run
// get: drop the datagram, back off, log the burst, and only a closed socket
// ends the loop.
func (p *DatagramPump) readReplies(s *datagramSession, framer *protocol.Framer) {
	var writeErrors int
	var lastWriteSummary time.Time
	for {
		msg, err := framer.ReadFrame()
		if err != nil {
			break
		}
		if msg.Type != protocol.TypeUDPPacket {
			continue
		}
		if _, err := p.Socket.WriteTo(msg.Payload, s.addr); err != nil {
			if p.closed.Load() || errors.Is(err, net.ErrClosed) {
				break
			}
			writeErrors++
			if writeErrors <= socketErrorLogLimit {
				p.logf("datagram pump: reply write error: %v", err)
			} else if lastWriteSummary.IsZero() || time.Since(lastWriteSummary) >= time.Minute {
				lastWriteSummary = time.Now()
				p.logf("datagram pump: %d reply write error(s) so far: %v", writeErrors, err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		writeErrors = 0
		lastWriteSummary = time.Time{}
		s.touch()
		s.fromPeer.Add(int64(len(msg.Payload)))
		if p.OnDatagram != nil {
			p.OnDatagram(false, len(msg.Payload))
		}
	}
	s.finish("session ended")
}

// finish ends a session that carried traffic.
func (s *datagramSession) finish(reason string) {
	s.once.Do(func() {
		close(s.done)
		s.closeFramers()
		s.pump.remove(s)
		if s.pump.OnSession != nil {
			s.pump.OnSession(-1)
		}
		if s.pump.Close != nil {
			s.pump.Close(s.addr, s.toPeer.Load(), s.fromPeer.Load())
		}
		s.pump.logf("datagram session for %s ended (%s)", s.addr, reason)
	})
}

// fail ends a session whose stream never came up.
func (s *datagramSession) fail() {
	s.once.Do(func() {
		close(s.done)
		s.pump.remove(s)
		if s.pump.OnSession != nil {
			s.pump.OnSession(-1)
		}
		if s.pump.Close != nil {
			// The paths never opened, so no bytes moved, but Close belongs to
			// every session's ending — the contract on the field says it is
			// called when a session ends — so callers that pair Open with
			// Close hear about this one too.
			s.pump.Close(s.addr, 0, 0)
		}
	})
}

func (s *datagramSession) closeFramers() {
	s.mu.Lock()
	framers, releases := s.framers, s.releases
	s.framers, s.releases = nil, nil
	s.mu.Unlock()

	for _, framer := range framers {
		_ = framer.Close()
	}
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

// CloseSession ends one session, used by the reaper and by shutdown.
func (p *DatagramPump) CloseSession(addr net.Addr) {
	p.mu.Lock()
	session, ok := p.sessions[addr.String()]
	p.mu.Unlock()
	if ok {
		session.finish("released")
	}
}

func (p *DatagramPump) remove(s *datagramSession) {
	p.mu.Lock()
	if current, ok := p.sessions[s.addr.String()]; ok && current == s {
		delete(p.sessions, s.addr.String())
	}
	p.mu.Unlock()
}

// Sessions reports how many source addresses are currently tracked.
func (p *DatagramPump) Sessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// Shutdown ends every session and stops accepting new datagrams.
func (p *DatagramPump) Shutdown() {
	p.closed.Store(true)
	p.mu.Lock()
	sessions := make([]*datagramSession, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.mu.Unlock()

	for _, s := range sessions {
		s.finish("shutdown")
	}
}

// reap releases sessions whose source address has stopped sending.
func (p *DatagramPump) reap() {
	timeout := p.IdleTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := timeout / 4
	if interval < time.Second {
		interval = time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		if p.closed.Load() {
			return
		}
		var stale []*datagramSession
		p.mu.Lock()
		for _, s := range p.sessions {
			if s.idleFor() > timeout {
				stale = append(stale, s)
			}
		}
		p.mu.Unlock()

		for _, s := range stale {
			s.finish("idle for " + timeout.String())
		}
	}
}
