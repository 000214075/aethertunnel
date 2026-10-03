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
	addr     string
	timeout  time.Duration
	logger   *log.Logger
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
		addr:     net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(cfg.Server.HTTPSPassthroughPort)),
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, raw := range group.Domains {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if domain == "" {
			continue
		}
		if strings.HasPrefix(domain, "*.") {
			base := strings.TrimPrefix(domain, "*.")
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

func (m *sniSet) removeBinding(b *sniBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, domain := range b.domains {
		if strings.HasPrefix(domain, "*.") {
			if m.wildcard[strings.TrimPrefix(domain, "*.")] == b.group {
				delete(m.wildcard, strings.TrimPrefix(domain, "*."))
			}
		} else if m.exact[domain] == b.group {
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
	m.listener = listener
	m.logger.Printf("tls passthrough listener on %s", listener.Addr())
	go m.serve()
	return nil
}

func (m *sniSet) stop() {
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
		conn, err := m.listener.Accept()
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

	group := lookupHostname(m.exact, m.wildcard, serverName)
	if group == nil {
		m.logger.Printf("tls passthrough: no tunnel publishes %q (from %s)", serverName, conn.RemoteAddr())
		// Answer the way frp does: a real TLS server that owns no certificate
		// for the name, which tells the visitor their hostname was wrong in a
		// language their TLS stack understands. The hello is replayed from the
		// buffer — it was already read off the wire — and under a deadline, so
		// a visitor that sent no hello does not hold the goroutine.
		if m.timeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(m.timeout))
		}
		_ = tls.Server(&replayConn{Conn: conn, reader: recorder.replay(buffered)}, &tls.Config{}).Handshake()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if allowed, reason := group.visitorAllowed(conn.RemoteAddr()); !allowed {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		return
	}
	m.logger.Printf("tls passthrough: %s opens %q on %q", conn.RemoteAddr(), serverName, group.Name)
	group.serveStream(&replayConn{Conn: conn, reader: recorder.replay(buffered)})
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

// replayConn reads the sniffed bytes back first, then the connection itself —
// the tunnel's end receives the visitor's handshake as written. CloseWrite is
// forwarded by hand: net.Conn does not carry it, and a wrapper that hid the
// TCP connection's half-close would turn it into a full close, cutting off a
// reply the far side only sends after the end of its request.
type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *replayConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}
