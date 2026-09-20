package net

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

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
	// OnSession, when set, is called with +1 when a source address starts being
	// tracked and -1 when it is released.
	OnSession func(delta int)
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

// Run reads datagrams until the socket is closed. It blocks.
func (p *DatagramPump) Run() {
	if p.sessions == nil {
		p.sessions = make(map[string]*datagramSession)
	}
	buf := make([]byte, 65535)
	for {
		n, addr, err := p.Socket.ReadFrom(buf)
		if err != nil {
			if p.closed.Load() {
				return
			}
			p.logf("datagram pump: read error: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		datagram := make([]byte, n)
		copy(datagram, buf[:n])
		p.deliver(addr, datagram)
	}
}

// Start runs the pump and its session reaper in the background.
func (p *DatagramPump) Start() {
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 2 * time.Minute
	}
	go p.Run()
	go p.reap()
}

// deliver routes one datagram, creating a session when the address is new.
func (p *DatagramPump) deliver(addr net.Addr, datagram []byte) {
	key := addr.String()

	p.mu.Lock()
	session, known := p.sessions[key]
	if !known {
		session = &datagramSession{pump: p, addr: addr, done: make(chan struct{})}
		session.touch()
		p.sessions[key] = session
	}
	p.mu.Unlock()

	session.touch()
	if !known {
		if p.OnSession != nil {
			p.OnSession(1)
		}
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
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.framers[0]
	}
	return s.pathAt(int(s.next.Add(1)-1) % count)
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

func (s *datagramSession) setFramers(framers []*protocol.Framer, releases []func()) {
	s.mu.Lock()
	s.framers = framers
	s.releases = releases
	s.mu.Unlock()
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
	s.setFramers(framers, releases)

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
// path to fail ends the whole session, because a half-open multipath session
// would silently drop the datagrams that were spread onto the failed path.
func (p *DatagramPump) readReplies(s *datagramSession, framer *protocol.Framer) {
	for {
		msg, err := framer.ReadFrame()
		if err != nil {
			break
		}
		if msg.Type != protocol.TypeUDPPacket {
			continue
		}
		if _, err := p.Socket.WriteTo(msg.Payload, s.addr); err != nil {
			break
		}
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
