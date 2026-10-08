package server

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
)

// sniSet is the TLS passthrough listener: a visitor opens a TLS connection
// whose ClientHello names a hostname, the server reads only that hello and
// routes by it, and every byte after it — the visitor's TLS session itself —
// is relayed untouched to the tunnel's end. The certificate the visitor sees
// is the one the client's side presents, so each hostname can carry its own.
type sniSet struct {
	exact    map[string]*ProxyGroup
	wildcard map[string]*ProxyGroup
	mu       sync.RWMutex

	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
	addr     string
	timeout  time.Duration
	logger   *log.Logger
	refusals refusalLog
}

// newSNISet builds the passthrough router for server.https_passthrough_port; a
// port of zero returns nil, and passthrough registrations are refused at bind
// time.
func newSNISet(cfg *config.Config, logger *log.Logger) *sniSet {
	if cfg.Server.HTTPSPassthroughPort <= 0 {
		return nil
	}
	return &sniSet{
		exact:    make(map[string]*ProxyGroup),
		wildcard: make(map[string]*ProxyGroup),
		done:     make(chan struct{}),
		addr:     net.JoinHostPort(cfg.ProxyHost(), fmt.Sprint(cfg.Server.HTTPSPassthroughPort)),
		timeout:  time.Duration(cfg.Server.HandshakeTimeoutSecs) * time.Second,
		logger:   logger,
	}
}

// sniBinding is one proxy's registration on the passthrough listener.
type sniBinding struct {
	set     *sniSet
	group   *ProxyGroup
	domains []string
	once    sync.Once
}

func (b *sniBinding) remove() {
	b.once.Do(func() { b.set.removeBinding(b) })
}

// add registers the group's hostnames, refusing one another proxy already owns.
func (m *sniSet) add(group *ProxyGroup) (*sniBinding, error) {
	if len(group.Domains) == 0 {
		return nil, fmt.Errorf("proxy %q is type %s but carries no domains", group.Name, group.Type)
	}
	binding := &sniBinding{set: m, group: group}
	if err := m.register(binding, group.Domains); err != nil {
		return nil, err
	}
	return binding, nil
}

// extend registers the server names a second member of a pool brought with it.
// Without it those names were dropped: the SNI listener chose a member by the
// binding built from the first member's list, so the joining member's own names
// were never routed — and were never checked against another proxy owning them.
func (m *sniSet) extend(binding *sniBinding, domains []string) error {
	return m.register(binding, domains)
}

// register adds server names to a binding, undoing the ones it added when another
// proxy already owns one of them.
func (m *sniSet) register(binding *sniBinding, domains []string) error {
	group := binding.group
	m.mu.Lock()
	var conflict error
	added := make([]string, 0, len(domains))
	for _, raw := range domains {
		domain := normalizeDomain(raw)
		if domain == "" {
			continue
		}
		if strings.HasPrefix(domain, "*.") {
			base := strings.TrimPrefix(domain, "*.")
			owner, taken := m.wildcard[base]
			if taken && owner != group {
				conflict = fmt.Errorf("proxy %q: wildcard %q is already published by proxy %q",
					group.Name, domain, owner.Name)
				break
			}
			if taken {
				// Already this group's, from the member that bound the group. It is not
				// one this call added, and counting it as added would make the rollback
				// below delete an entry that was published before this call.
				continue
			}
			m.wildcard[base] = group
		} else {
			owner, taken := m.exact[domain]
			if taken && owner != group {
				conflict = fmt.Errorf("proxy %q: hostname %q is already published by proxy %q",
					group.Name, domain, owner.Name)
				break
			}
			if taken {
				continue
			}
			m.exact[domain] = group
		}
		added = append(added, domain)
	}
	if conflict != nil {
		// Undo the domains registered before the conflict: a leftover entry
		// would keep that hostname unreachable for every later owner — it
		// accepts visitors only to disconnect them — until the process
		// restarts, because the owner check refuses every other group.
		m.removeDomainsLocked(group, added)
	} else {
		binding.domains = append(binding.domains, added...)
	}
	m.mu.Unlock()
	return conflict
}

func (m *sniSet) removeBinding(b *sniBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeDomainsLocked(b.group, b.domains)
}

// removeDomainsLocked drops the given server names while they are still owned by
// the group, so a name another proxy has since taken is left alone. mu must be
// held.
func (m *sniSet) removeDomainsLocked(group *ProxyGroup, domains []string) {
	for _, domain := range domains {
		if strings.HasPrefix(domain, "*.") {
			base := strings.TrimPrefix(domain, "*.")
			if m.wildcard[base] == group {
				delete(m.wildcard, base)
			}
		} else if m.exact[domain] == group {
			delete(m.exact, domain)
		}
	}
}

// start binds the listener and serves it until stop.
func (m *sniSet) start() error {
	listener, err := net.Listen("tcp", m.addr)
	if err != nil {
		return fmt.Errorf("cannot start the TLS passthrough listener: %w", flynet.ListenError(m.addr, err))
	}
	// The done check and the store share one critical section with stop, so
	// a stop that lands between them cannot leave this listener open with
	// nobody left to close it.
	m.mu.Lock()
	select {
	case <-m.done:
		m.mu.Unlock()
		_ = listener.Close()
		return fmt.Errorf("the tls passthrough listener was stopped before it started")
	default:
	}
	m.listener = listener
	m.mu.Unlock()
	m.logger.Printf("tls passthrough listener on %s", listener.Addr())
	go m.serve()
	return nil
}

func (m *sniSet) stop() {
	// Once, not a check-then-act: Shutdown and a failing Run startup path can
	// both call this concurrently, and a double close would panic.
	m.stopOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		listener := m.listener
		m.listener = nil
		m.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
	})
}

// lookup matches a hostname against the exact names and the wildcards. It takes
// the lock the table is written under: a visitor's handle goroutine reads these
// maps while a control-connection goroutine registers or withdraws a
// passthrough proxy, and an unsynchronised read of a map being written is a
// fatal error rather than a race the runtime can report.
func (m *sniSet) lookup(host string) *ProxyGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return lookupHostname(m.exact, m.wildcard, host)
}

// domains lists every hostname published on the passthrough listener.
func (m *sniSet) domains() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.exact)+len(m.wildcard))
	for domain := range m.exact {
		out = append(out, domain)
	}
	for base := range m.wildcard {
		out = append(out, "*."+base)
	}
	sort.Strings(out)
	return out
}

// serve accepts TLS connections until the listener closes.
func (m *sniSet) serve() {
	for {
		m.mu.RLock()
		listener := m.listener
		m.mu.RUnlock()
		if listener == nil {
			return
		}
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			m.logger.Printf("tls passthrough: accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go m.handle(conn)
	}
}

// handle sniffs the ClientHello's server name and hands the connection — the
// visitor's TLS session itself, untouched — to the tunnel that owns it. The
// sniffed bytes are replayed to the tunnel first, so the client's side sees
// the handshake exactly as the visitor wrote it.
func (m *sniSet) handle(conn net.Conn) {
	defer conn.Close()
	if m.timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(m.timeout))
	}
	buffered := bufio.NewReader(conn)
	// crypto/tls consumes the hello's bytes while parsing them, so they have to
	// be captured on the way through — a plain buffer would only replay what the
	// handshake left behind, which is nothing.
	recorder := &helloRecorder{source: buffered}

	var serverName string
	tls.Server(readOnlyConn{reader: recorder}, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			serverName = hello.ServerName
			return nil, nil
		},
	}).Handshake()
	// The handshake always fails here — the read-only connection cannot write
	// — and that is fine: the hello has been read by the time it fails. A
	// visitor that sent no hello gets no route either.
	serverName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(serverName), "."))

	group := m.lookup(serverName)
	if group == nil {
		m.refusals.logf(m.logger, "tls passthrough: no tunnel publishes %q (from %s)", serverName, conn.RemoteAddr())
		// Answer the way frp does: a real TLS server that owns no certificate
		// for the name, which tells the visitor their hostname was wrong in a
		// language their TLS stack understands. The hello is replayed from the
		// buffer — it was already read off the wire — and under a deadline, so
		// a visitor that sent no hello does not hold the goroutine.
		if m.timeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(m.timeout))
		}
		_ = tls.Server(&readerConn{Conn: conn, reader: recorder.replay(buffered)}, &tls.Config{}).Handshake()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if allowed, reason := group.visitorAllowed(conn.RemoteAddr()); !allowed {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		return
	}
	m.logger.Printf("tls passthrough: %s opens %q on %q", conn.RemoteAddr(), serverName, group.Name)
	group.serveStream(&readerConn{Conn: conn, reader: recorder.replay(buffered)})
}

// helloRecorder keeps every byte that passes through it, so the bytes a failed
// sniff handshake consumed can be replayed to the tunnel.
type helloRecorder struct {
	source io.Reader
	seen   bytes.Buffer
}

func (c *helloRecorder) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	if n > 0 {
		c.seen.Write(p[:n])
	}
	return n, err
}

// replay yields the recorded hello first, then whatever the buffered reader
// still holds.
func (c *helloRecorder) replay(remainder io.Reader) io.Reader {
	return io.MultiReader(bytes.NewReader(c.seen.Bytes()), remainder)
}

// readOnlyConn is a net.Conn that can only be read: enough for
// crypto/tls to walk the ClientHello, whose capture is the point.
type readOnlyConn struct {
	reader io.Reader
}

func (c readOnlyConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }
func (c readOnlyConn) Close() error                { return nil }
func (c readOnlyConn) LocalAddr() net.Addr         { return nil }
func (c readOnlyConn) RemoteAddr() net.Addr        { return nil }
func (c readOnlyConn) SetDeadline(t time.Time) error {
	return nil
}
func (c readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (c readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

// readerConn reads through a reader before it reads the connection: whatever a
// sniffing parser already pulled out of the socket is delivered first, so the
// tunnel's end receives the visitor's bytes exactly as written. The SNI
// passthrough uses it to put the captured ClientHello back, and the tcpmux
// multiplexer to put back the payload that arrived with the CONNECT headers.
//
// CloseWrite is forwarded by hand: net.Conn does not carry it, and a wrapper
// that hid the TCP connection's half-close would turn it into a full close,
// cutting off a reply the far side only sends after the end of its request.
type readerConn struct {
	net.Conn
	reader io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *readerConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}
