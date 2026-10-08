package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
)

// tcpmuxSet serves every tcpmux proxy on one listener. A visitor opens the
// connection with an HTTP CONNECT whose authority names the hostname — the
// same request a browser or curl sends when it tunnels through a proxy — the
// server routes by that hostname, answers 200, and everything after it belongs
// to the tunnel. One port can carry any number of TCP services this way.
type tcpmuxSet struct {
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
	// passthrough forwards the CONNECT instead of answering it, so the service
	// behind the tunnel sees the request as the visitor wrote it.
	passthrough bool
}

// newTCPMuxSet builds the multiplexer for server.tcpmux_port; a port of zero
// returns nil, and tcpmux registrations are refused at bind time.
func newTCPMuxSet(cfg *config.Config, logger *log.Logger) *tcpmuxSet {
	if cfg.Server.TCPMuxPort <= 0 {
		return nil
	}
	return &tcpmuxSet{
		exact:       make(map[string]*ProxyGroup),
		wildcard:    make(map[string]*ProxyGroup),
		done:        make(chan struct{}),
		addr:        net.JoinHostPort(cfg.ProxyHost(), strconv.Itoa(cfg.Server.TCPMuxPort)),
		timeout:     time.Duration(cfg.Server.HandshakeTimeoutSecs) * time.Second,
		logger:      logger,
		passthrough: cfg.Server.TCPMuxPassthrough,
	}
}

// tcpmuxBinding is one proxy's registration on the multiplexer.
type tcpmuxBinding struct {
	set     *tcpmuxSet
	group   *ProxyGroup
	domains []string
	once    sync.Once
}

func (b *tcpmuxBinding) remove() {
	b.once.Do(func() { b.set.removeBinding(b) })
}

// add registers the group's hostnames, refusing one another proxy already owns.
func (m *tcpmuxSet) add(group *ProxyGroup) (*tcpmuxBinding, error) {
	if len(group.Domains) == 0 {
		return nil, fmt.Errorf("proxy %q is type %s but carries no domains", group.Name, group.Type)
	}
	binding := &tcpmuxBinding{set: m, group: group}
	if err := m.register(binding, group.Domains); err != nil {
		return nil, err
	}
	return binding, nil
}

// extend registers hostnames a second member of a pool brought with it. Without
// it those names were dropped: the binding held the first member's list, so the
// joining member's own hostnames never resolved, and were never checked against
// another proxy already owning them either.
func (m *tcpmuxSet) extend(binding *tcpmuxBinding, domains []string) error {
	return m.register(binding, domains)
}

// register adds hostnames to a binding, undoing the ones it added when another
// proxy already owns one of them.
func (m *tcpmuxSet) register(binding *tcpmuxBinding, domains []string) error {
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
		// Undo the domains registered before the conflict, so a failed
		// registration leaves nothing squatting on a hostname: the owner
		// check would refuse every other group until the process restarts.
		m.removeDomainsLocked(group, added)
	} else {
		binding.domains = append(binding.domains, added...)
	}
	m.mu.Unlock()
	return conflict
}

func (m *tcpmuxSet) removeBinding(b *tcpmuxBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeDomainsLocked(b.group, b.domains)
}

// removeDomainsLocked drops the given hostnames while they are still owned by the
// group, so a name another proxy has since taken is left alone. mu must be held.
func (m *tcpmuxSet) removeDomainsLocked(group *ProxyGroup, domains []string) {
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
func (m *tcpmuxSet) start() error {
	listener, err := net.Listen("tcp", m.addr)
	if err != nil {
		return fmt.Errorf("cannot start the tcpmux listener: %w", flynet.ListenError(m.addr, err))
	}
	// The done check and the store share one critical section with stop, so
	// a stop that lands between them cannot leave this listener open with
	// nobody left to close it.
	m.mu.Lock()
	select {
	case <-m.done:
		m.mu.Unlock()
		_ = listener.Close()
		return fmt.Errorf("the tcpmux listener was stopped before it started")
	default:
	}
	m.listener = listener
	m.mu.Unlock()
	go m.serve()
	return nil
}

func (m *tcpmuxSet) stop() {
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

// domains lists every hostname published on the multiplexer, for the startup
// banner and the dashboard.
func (m *tcpmuxSet) domains() []string {
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

// serve accepts CONNECT requests until the listener closes.
func (m *tcpmuxSet) serve() {
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
			m.logger.Printf("tcpmux: accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go m.handle(conn)
	}
}

// handle reads one CONNECT request and hands the connection to the tunnel the
// authority names. Everything the server writes before the 200 is a plain HTTP
// answer, so a client that typoed a hostname sees a real status instead of a
// hang or a closed socket.
func (m *tcpmuxSet) handle(conn net.Conn) {
	defer conn.Close()
	if m.timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(m.timeout))
	}

	// In passthrough the request itself is relayed, so what the parser reads is
	// recorded and put back in front of the stream.
	var relaySource io.Reader = conn
	var recorder *helloRecorder
	if m.passthrough {
		recorder = &helloRecorder{source: conn}
		relaySource = recorder
	}
	// net/http's request reader has no header bound of its own, and this listener
	// answers anybody: an unauthenticated peer that streams one endless header line
	// would have it allocated here (and recorded, in passthrough) until the
	// handshake deadline. The cap covers the request only, and it is released the
	// moment the request has been parsed rather than deferred: handle stays on this
	// goroutine for the whole stream, so a deferred release would count every
	// relayed byte against the cap and end a tunnel past 64 KiB.
	limited := &requestLimitReader{reader: relaySource, left: maxConnectRequestBytes, limited: true}
	buffered := bufio.NewReader(limited)
	request, err := http.ReadRequest(buffered)
	limited.release()
	if err != nil {
		m.logger.Printf("tcpmux: %s sent no request: %v", conn.RemoteAddr(), err)
		return
	}
	if request.Method != http.MethodConnect {
		// Only CONNECT multiplexes here: anything else is asking for a plain
		// HTTP answer, which would leak which hostnames exist behind the
		// listener if the server answered it honestly.
		_, _ = conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n"))
		m.logger.Printf("tcpmux: %s sent %s; only CONNECT is served", conn.RemoteAddr(), request.Method)
		return
	}
	authority := request.Host
	if authority == "" {
		authority = request.URL.Host
	}
	host := authority
	if _, _, err := net.SplitHostPort(authority); err == nil {
		host, _, _ = net.SplitHostPort(authority)
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))

	group := m.lookup(host)
	if group == nil {
		m.refusals.logf(m.logger, "tcpmux: no tunnel publishes %q (from %s)", host, conn.RemoteAddr())
		_, _ = conn.Write([]byte("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n"))
		return
	}
	if allowed, reason := group.visitorAllowed(conn.RemoteAddr()); !allowed {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n"))
		return
	}

	if m.passthrough {
		// The CONNECT is the backend's to answer: the relay starts with the
		// request exactly as it arrived, headers and all, and this side says
		// nothing of its own.
		_ = conn.SetDeadline(time.Time{})
		m.logger.Printf("tcpmux: %s opens %q on %q (passthrough)", conn.RemoteAddr(), host, group.Name)
		// The recorded bytes are everything bufio pulled off the socket, and that
		// already includes the payload it read ahead of the request. The remainder is
		// therefore the socket itself: replaying `buffered` here would hand that tail
		// to the backend a second time.
		group.serveStream(&readerConn{Conn: conn, reader: recorder.replay(conn)})
		return
	}

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	m.logger.Printf("tcpmux: %s opens %q on %q", conn.RemoteAddr(), host, group.Name)
	// The request line and headers were read through a buffer, and a client is
	// free to put its first payload bytes in the same write: hand the relay a
	// connection that drains that buffer first (and that forwards CloseWrite,
	// so a visitor's half-close still reaches the tunnel), or those bytes are
	// read out of the socket by nothing and the tunnel starts one packet short.
	group.serveStream(&readerConn{Conn: conn, reader: buffered})
}

// maxConnectRequestBytes bounds one CONNECT request. A real one is a few hundred
// bytes; anything larger is a peer writing a header line that never ends.
const maxConnectRequestBytes = 64 << 10

// errConnectRequestTooLarge ends a request that outgrew the bound.
var errConnectRequestTooLarge = errors.New("tcpmux: the CONNECT request is too large")

// requestLimitReader bounds how much may be read while one CONNECT is parsed. After
// release it is a pass-through, so the relayed stream is not limited by it.
type requestLimitReader struct {
	reader  io.Reader
	left    int
	limited bool
}

func (r *requestLimitReader) Read(p []byte) (int, error) {
	if !r.limited {
		return r.reader.Read(p)
	}
	if r.left <= 0 {
		return 0, errConnectRequestTooLarge
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	r.left -= n
	return n, err
}

func (r *requestLimitReader) release() { r.limited = false }

// lookup matches a hostname against the exact names and the wildcards. A
// wildcard "*.example" owns the base domain itself and every subdomain of it,
// which is what a certificate for "*.example" covers.
func (m *tcpmuxSet) lookup(host string) *ProxyGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return lookupHostname(m.exact, m.wildcard, host)
}

// lookupHostname matches a lowercased host against the exact names and the
// wildcards of a hostname table: a wildcard "*.example" owns the base domain
// itself and every subdomain of it, which is what a certificate for
// "*.example" covers. Shared by the tcpmux and SNI passthrough tables.
func lookupHostname(exact, wildcard map[string]*ProxyGroup, host string) *ProxyGroup {
	if group, ok := exact[host]; ok {
		return group
	}
	// The longest matching suffix, not the first the map happens to yield: with
	// "*.com" and "*.a.com" published, a map iteration order would otherwise decide
	// which tunnel answers x.a.com, and the answer could differ between requests.
	best := ""
	var match *ProxyGroup
	for base, group := range wildcard {
		if host != base && !strings.HasSuffix(host, "."+base) {
			continue
		}
		if len(base) > len(best) {
			best, match = base, group
		}
	}
	return match
}
