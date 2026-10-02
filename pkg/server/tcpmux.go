package server

import (
	"bufio"
	"fmt"
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
	addr     string
	timeout  time.Duration
	logger   *log.Logger
}

// newTCPMuxSet builds the multiplexer for server.tcpmux_port; a port of zero
// returns nil, and tcpmux registrations are refused at bind time.
func newTCPMuxSet(cfg *config.Config, logger *log.Logger) *tcpmuxSet {
	if cfg.Server.TCPMuxPort <= 0 {
		return nil
	}
	return &tcpmuxSet{
		exact:    make(map[string]*ProxyGroup),
		wildcard: make(map[string]*ProxyGroup),
		done:     make(chan struct{}),
		addr:     net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.TCPMuxPort)),
		timeout:  time.Duration(cfg.Server.HandshakeTimeoutSecs) * time.Second,
		logger:   logger,
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, raw := range group.Domains {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if domain == "" {
			continue
		}
		if strings.HasPrefix(domain, "*.") {
			base := domain[1:]
			if owner, taken := m.wildcard[base]; taken && owner != group {
				return nil, fmt.Errorf("proxy %q: wildcard %q is already published by proxy %q",
					group.Name, domain, owner.Name)
			}
			m.wildcard[base] = group
		} else {
			if owner, taken := m.exact[domain]; taken && owner != group {
				return nil, fmt.Errorf("proxy %q: hostname %q is already published by proxy %q",
					group.Name, domain, owner.Name)
			}
			m.exact[domain] = group
		}
		binding.domains = append(binding.domains, domain)
	}
	return binding, nil
}

func (m *tcpmuxSet) removeBinding(b *tcpmuxBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, domain := range b.domains {
		if strings.HasPrefix(domain, "*.") {
			if m.wildcard[domain[1:]] == b.group {
				delete(m.wildcard, domain[1:])
			}
		} else if m.exact[domain] == b.group {
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
	m.listener = listener
	go m.serve()
	return nil
}

func (m *tcpmuxSet) stop() {
	select {
	case <-m.done:
		return
	default:
	}
	close(m.done)
	if m.listener != nil {
		_ = m.listener.Close()
	}
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
		conn, err := m.listener.Accept()
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

	request, err := http.ReadRequest(bufio.NewReader(conn))
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
		m.logger.Printf("tcpmux: no tunnel publishes %q (from %s)", host, conn.RemoteAddr())
		_, _ = conn.Write([]byte("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n"))
		return
	}
	if allowed, reason := group.visitorAllowed(conn.RemoteAddr()); !allowed {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n"))
		return
	}

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	m.logger.Printf("tcpmux: %s opens %q on %q", conn.RemoteAddr(), host, group.Name)
	group.serveStream(conn)
}

// lookup matches a hostname against the exact names and the wildcards. A
// wildcard "*.example" owns the base domain itself and every subdomain of it,
// which is what a certificate for "*.example" covers.
func (m *tcpmuxSet) lookup(host string) *ProxyGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if group, ok := m.exact[host]; ok {
		return group
	}
	for base, group := range m.wildcard {
		if host == base || strings.HasSuffix(host, "."+base) {
			return group
		}
	}
	return nil
}
