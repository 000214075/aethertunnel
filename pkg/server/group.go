package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// ProxyGroup is the published endpoint for one proxy name.
//
// It holds every client that registered that name. One member is the common case
// and behaves exactly like a single tunnel; several members, which requires each
// of them to declare the same group, are balanced according to
// [server].load_balance. Keeping the endpoint and its members in separate types
// is what lets a listener outlive the client that first created it.
type ProxyGroup struct {
	Name    string
	Type    string
	Private bool

	Domains         []string
	SecretKey       string
	AuthMethod      string
	SecretPublicKey []byte
	RemotePort      int
	Group           string
	Multipath       int

	manager     *TunnelManager
	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration
	strategy    string

	mu      sync.RWMutex
	members []*Tunnel

	listener net.Listener
	packet   net.PacketConn
	pump     *flynet.DatagramPump
	vhost    *vhostBinding

	proxyOnce sync.Once
	proxy     *httputil.ReverseProxy

	// Active, Total, BytesIn and BytesOut aggregate the members; the http path
	// updates them directly because its traffic is not attributed to one member.
	Active   atomic.Int64
	Total    atomic.Int64
	BytesIn  atomic.Int64
	BytesOut atomic.Int64

	counter atomic.Uint64
	once    sync.Once
	done    chan struct{}
}

// Tunnel is one client's registration of a proxy: a member of a group.
type Tunnel struct {
	Name       string
	Spec       protocol.ProxySpec
	Type       string
	RemotePort int
	Session    *Session

	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration

	group *ProxyGroup

	// dialLatency is an exponentially weighted average of how long this member
	// took to answer a request for a stream, in nanoseconds. The latency
	// strategy picks the smallest.
	dialLatency atomic.Int64

	Active   atomic.Int64
	Total    atomic.Int64
	BytesIn  atomic.Int64 // received from the client (its service's replies)
	BytesOut atomic.Int64 // sent to the client (what the visitor sent)

	closed atomic.Bool
}

// Closed reports whether this member has been removed from its group.
func (t *Tunnel) Closed() bool { return t.closed.Load() }

// healthy reports whether this member can still provide a stream.
func (t *Tunnel) healthy() bool {
	if t.closed.Load() || t.Session == nil || t.Session.IsClosed() {
		return false
	}
	if t.group != nil && t.group.manager != nil && t.group.manager.sessions != nil {
		_, ok := t.group.manager.sessions.Get(t.Session.ID)
		return ok
	}
	return true
}

// Addr reports the address the group is published on, or "" when it has none.
func (t *Tunnel) Addr() string {
	if t.group == nil {
		return ""
	}
	return t.group.Addr()
}

// Latency reports the member's observed response time.
func (t *Tunnel) Latency() time.Duration { return time.Duration(t.dialLatency.Load()) }

// --- group lifecycle ----------------------------------------------------------

func newProxyGroup(spec protocol.ProxySpec, manager *TunnelManager) *ProxyGroup {
	return &ProxyGroup{
		Name:        spec.Name,
		Type:        spec.Type,
		Private:     config.IsPrivateProxyType(spec.Type),
		Domains:     append([]string(nil), spec.Domains...),
		SecretKey:   spec.SecretKey,
		AuthMethod:  spec.AuthMethod,
		RemotePort:  spec.RemotePort,
		Group:       spec.Group,
		Multipath:   spec.Multipath,
		manager:     manager,
		logger:      manager.logger,
		cipher:      manager.cipher,
		metrics:     manager.metrics,
		idleTimeout: time.Duration(manager.cfg.Server.ReadTimeoutSecs) * time.Second,
		dialTimeout: time.Duration(manager.cfg.Server.DialTimeoutSecs) * time.Second,
		strategy:    manager.cfg.Server.LoadBalance,
		done:        make(chan struct{}),
	}
}

// Addr returns the address the group is published on.
func (g *ProxyGroup) Addr() string {
	switch {
	case g.listener != nil:
		return g.listener.Addr().String()
	case g.packet != nil:
		return g.packet.LocalAddr().String()
	default:
		return ""
	}
}

// Members returns a snapshot of the group's members, oldest first.
func (g *ProxyGroup) Members() []*Tunnel {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]*Tunnel(nil), g.members...)
}

func (g *ProxyGroup) memberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.members)
}

// add registers a member, binding the group's public endpoint the first time.
func (g *ProxyGroup) add(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	member := &Tunnel{
		Name:        spec.Name,
		Spec:        spec,
		Type:        spec.Type,
		RemotePort:  spec.RemotePort,
		Session:     session,
		logger:      g.logger,
		cipher:      g.cipher,
		metrics:     g.metrics,
		idleTimeout: g.idleTimeout,
		dialTimeout: g.dialTimeout,
		group:       g,
	}

	g.mu.Lock()

	// A session re-registering its own proxy replaces its previous member; that
	// is what a reconnecting client does.
	for index, existing := range g.members {
		if existing.Session != session {
			continue
		}
		existing.closed.Store(true)
		g.members[index] = member
		g.mu.Unlock()
		g.logger.Printf("proxy %q: session %s replaced its own registration", g.Name, session.ID)
		return member, nil
	}

	if len(g.members) > 0 {
		first := g.members[0]
		if g.Group == "" || spec.Group != g.Group {
			g.mu.Unlock()
			return nil, fmt.Errorf(
				"proxy %q is already published by session %s; set the same group on both clients to pool them",
				g.Name, first.Session.ID)
		}
		if spec.SecretKey == "" && g.SecretKey != "" {
			g.mu.Unlock()
			return nil, errors.New("a pool of private proxies requires the same secret_key from every member")
		}
		if g.SecretKey != "" && !g.matchesSecret(spec.SecretKey) {
			g.mu.Unlock()
			return nil, errors.New("a pool of private proxies requires the same secret_key from every member")
		}
		if spec.LocalAddr == "" {
			g.mu.Unlock()
			return nil, errors.New("a pool member needs a local_addr")
		}
	}

	g.members = append(g.members, member)
	first := len(g.members) == 1
	g.mu.Unlock()

	if first {
		if err := g.bind(); err != nil {
			g.mu.Lock()
			g.members = g.members[:0]
			g.mu.Unlock()
			return nil, err
		}
	} else if g.vhost != nil {
		// A pooled http tunnel may add hostnames the first member did not use.
		if err := g.vhost.extend(spec.Domains); err != nil {
			g.mu.Lock()
			g.members = g.members[:len(g.members)-1]
			g.mu.Unlock()
			return nil, err
		}
	}

	return member, nil
}

// remove drops one member and closes the endpoint once the last one is gone.
func (g *ProxyGroup) remove(member *Tunnel) bool {
	member.closed.Store(true)

	g.mu.Lock()
	for index, existing := range g.members {
		if existing != member {
			continue
		}
		g.members = append(g.members[:index], g.members[index+1:]...)
		break
	}
	empty := len(g.members) == 0
	g.mu.Unlock()

	if empty {
		g.close("no member is left")
	}
	return empty
}

// close releases the group's endpoint exactly once.
func (g *ProxyGroup) close(reason string) {
	g.once.Do(func() {
		close(g.done)
		if g.listener != nil {
			_ = g.listener.Close()
		}
		if g.packet != nil {
			_ = g.packet.Close()
		}
		if g.pump != nil {
			g.pump.Shutdown()
		}
		if g.vhost != nil {
			g.vhost.remove()
		}
		g.logger.Printf("proxy %q closed (%s)", g.Name, reason)
	})
}

// bind gives the group the endpoint its type calls for.
func (g *ProxyGroup) bind() error {
	switch g.Type {
	case protocol.ProxyTypeTCP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.Server.BindAddr, strconv.Itoa(g.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s: %w", g.Name, addr, err)
		}
		g.listener = listener
		go g.acceptLoop()
		g.logger.Printf("proxy %q (tcp) published on %s", g.Name, addr)

	case protocol.ProxyTypeUDP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.Server.BindAddr, strconv.Itoa(g.RemotePort))
		packet, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s/udp: %w", g.Name, addr, err)
		}
		g.packet = packet
		g.startUDP()
		g.logger.Printf("proxy %q (udp) published on %s", g.Name, addr)

	case protocol.ProxyTypeHTTP, protocol.ProxyTypeHTTPS:
		if g.manager.vhost == nil {
			return fmt.Errorf("proxy %q is type %s but server.http_port is not configured", g.Name, g.Type)
		}
		binding, err := g.manager.vhost.add(g)
		if err != nil {
			return err
		}
		g.vhost = binding

	case protocol.ProxyTypeSTCP, protocol.ProxyTypeSUDP, protocol.ProxyTypeXTCP:
		if g.AuthMethod == config.AuthMethodNIZK {
			public, err := crypto.SchnorrPublicKey([]byte(g.SecretKey))
			if err != nil {
				return fmt.Errorf("proxy %q: cannot derive the proof key: %w", g.Name, err)
			}
			g.SecretPublicKey = public
		}
		g.logger.Printf("proxy %q (%s) registered as private, reachable by visitors", g.Name, g.Type)

	default:
		return fmt.Errorf("proxy type %q has no binding", g.Type)
	}
	return nil
}

// matchesSecret compares a visitor's secret with the group's in constant time.
func (g *ProxyGroup) matchesSecret(presented string) bool {
	return matchesSecret(g.SecretKey, presented)
}

// --- member selection ---------------------------------------------------------

// pick chooses the member that should serve the next visitor.
func (g *ProxyGroup) pick() *Tunnel { return g.pickExcluding(nil) }

// pickExcluding chooses among the healthy members that have not been tried, and
// falls back to every member when nothing is healthy so that a failing pool still
// reports a real error instead of silently dropping the visitor.
func (g *ProxyGroup) pickExcluding(tried map[*Tunnel]bool) *Tunnel {
	g.mu.RLock()
	candidates := make([]*Tunnel, 0, len(g.members))
	fallback := make([]*Tunnel, 0, len(g.members))
	for _, member := range g.members {
		if tried != nil && tried[member] {
			continue
		}
		fallback = append(fallback, member)
		if member.healthy() {
			candidates = append(candidates, member)
		}
	}
	g.mu.RUnlock()

	if len(candidates) == 0 {
		candidates = fallback
	}
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	switch g.strategy {
	case config.LoadBalanceRandom:
		return candidates[rand.Intn(len(candidates))]

	case config.LoadBalanceLatency:
		best := candidates[0]
		for _, member := range candidates[1:] {
			if member.dialLatency.Load() < best.dialLatency.Load() {
				best = member
			}
		}
		return best

	case config.LoadBalanceFailover:
		// Members are kept in registration order, so the first one wins until
		// it stops being healthy.
		return candidates[0]

	default: // round-robin
		n := g.counter.Add(1) - 1
		return candidates[int(n%uint64(len(candidates)))]
	}
}

// --- tcp ----------------------------------------------------------------------

func (g *ProxyGroup) acceptLoop() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			select {
			case <-g.done:
				return
			default:
			}
			g.logger.Printf("proxy %q: accept error: %v", g.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go g.serveVisit(conn)
	}
}

// serveVisit matches one public connection with a member, moving on to the next
// member when one cannot provide a stream.
func (g *ProxyGroup) serveVisit(public net.Conn) {
	tried := make(map[*Tunnel]bool)
	attempts := g.memberCount()
	if attempts == 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		member := g.pickExcluding(tried)
		if member == nil {
			break
		}
		tried[member] = true

		stream, err := g.openStream(member, false)
		if err != nil {
			lastErr = err
			g.logger.Printf("proxy %q: member %s did not answer: %v", g.Name, member.Session.ID, err)
			continue
		}

		if len(tried) > 1 {
			g.logger.Printf("proxy %q: served %s from member %s after %d attempt(s)",
				g.Name, public.RemoteAddr(), member.Session.ID, len(tried))
		}
		_ = member.pipeStream(public, stream.dc, public.RemoteAddr().String())
		return
	}

	_ = public.Close()
	if lastErr != nil {
		g.logger.Printf("proxy %q: no member could serve %s: %v", g.Name, public.RemoteAddr(), lastErr)
	}
}

// openedStream carries a data connection together with the bookkeeping that
// releases it.
type openedStream struct {
	dc      *dataConn
	release func()
}

// openStream asks one member for a data connection and records how long it took,
// which is what the latency strategy compares.
func (g *ProxyGroup) openStream(member *Tunnel, visitor bool) (*openedStream, error) {
	started := time.Now()
	dc, release, err := member.openStream(visitor)
	if err != nil {
		return nil, err
	}

	elapsed := time.Since(started).Nanoseconds()
	if previous := member.dialLatency.Load(); previous == 0 {
		member.dialLatency.Store(elapsed)
	} else {
		// A quarter-weight moving average reacts to a change without letting one
		// slow connection redefine the member.
		member.dialLatency.Store(previous - previous/4 + elapsed/4)
	}
	return &openedStream{dc: dc, release: release}, nil
}

// --- udp ----------------------------------------------------------------------

// startUDP publishes a udp proxy. Each visitor address gets its own datagram
// session, and a session may be spread over several members when the proxy asks
// for more than one path.
func (g *ProxyGroup) startUDP() {
	paths := g.Multipath
	if paths < 1 {
		paths = 1
	}

	g.pump = &flynet.DatagramPump{
		Socket: g.packet,
		Paths:  paths,
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			member := g.pick()
			if member == nil {
				return nil, nil, errors.New("no member is available")
			}
			stream, err := g.openStream(member, false)
			if err != nil {
				return nil, nil, err
			}
			return stream.dc.framer, func() {
				_ = stream.dc.Close()
				stream.release()
			}, nil
		},
		Close: func(addr net.Addr, toPeer, fromPeer int64) {
			g.recordSession(toPeer, fromPeer)
		},
		OnDatagram:  func(toPeer bool, n int) { g.metrics.udpDatagrams.Add(1) },
		OnSession:   func(delta int) { g.metrics.udpSessions.Add(int64(delta)) },
		IdleTimeout: g.idleTimeout,
		Logger:      g.logger,
	}
	g.pump.Start()
}

// recordSession books the traffic of one finished datagram session against the
// group's members in proportion, so the dashboard and the metrics stay consistent
// without attributing one session to a single member it may not have used alone.
func (g *ProxyGroup) recordSession(toPeer, fromPeer int64) {
	members := g.Members()
	if len(members) == 0 {
		return
	}
	shareOut, shareIn := toPeer/int64(len(members)), fromPeer/int64(len(members))
	for _, member := range members {
		member.BytesOut.Add(shareOut)
		member.BytesIn.Add(shareIn)
		member.Total.Add(1)
		member.Session.RecordTraffic(shareOut, shareIn)
	}
	g.metrics.recordStream(g.Name, toPeer, fromPeer)
}

// openForVisitor asks one member for a data connection, trying the others when
// one cannot answer.
func (g *ProxyGroup) openForVisitor() (*Tunnel, *openedStream, error) {
	tried := make(map[*Tunnel]bool)
	attempts := g.memberCount()
	if attempts == 0 {
		return nil, nil, errors.New("no client is publishing this proxy")
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		member := g.pickExcluding(tried)
		if member == nil {
			break
		}
		tried[member] = true

		stream, err := g.openStream(member, true)
		if err != nil {
			lastErr = err
			continue
		}
		return member, stream, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no client could provide a stream")
	}
	return nil, nil, lastErr
}

// --- http ---------------------------------------------------------------------

// serveHTTP forwards one request to a member's local web server over a fresh
// tunnelled connection, moving on to another member if the first cannot answer.
func (g *ProxyGroup) serveHTTP(w http.ResponseWriter, r *http.Request) {
	proxy := g.httpProxy()
	if proxy == nil {
		http.Error(w, "this proxy has no http handler", http.StatusInternalServerError)
		return
	}

	g.metrics.httpRequests.Add(1)
	g.metrics.tunnel(g.Name).httpRequests.Add(1)
	g.Active.Add(1)
	g.metrics.streamOpened(g.Name)
	defer func() {
		g.Active.Add(-1)
		g.metrics.streamClosed(g.Name)
	}()

	counter := &countingResponseWriter{ResponseWriter: w}
	proxy.ServeHTTP(counter, r)

	toClient := r.ContentLength
	if toClient < 0 {
		toClient = 0
	}
	fromClient := counter.written
	g.BytesOut.Add(toClient)
	g.BytesIn.Add(fromClient)
	g.Total.Add(1)
	for _, member := range g.Members() {
		member.Session.RecordTraffic(toClient/int64(len(g.Members())), fromClient/int64(len(g.Members())))
	}
	g.metrics.recordStream(g.Name, toClient, fromClient)
}

// httpProxy builds the group's reverse proxy once.
func (g *ProxyGroup) httpProxy() *httputil.ReverseProxy {
	g.proxyOnce.Do(func() {
		if g.Type == protocol.ProxyTypeHTTP || g.Type == protocol.ProxyTypeHTTPS {
			g.proxy = g.buildHTTPProxy()
		}
	})
	return g.proxy
}

func (g *ProxyGroup) buildHTTPProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			tried := make(map[*Tunnel]bool)
			attempts := g.memberCount()
			if attempts == 0 {
				return nil, errors.New("no member is available")
			}

			var lastErr error
			for attempt := 0; attempt < attempts; attempt++ {
				member := g.pickExcluding(tried)
				if member == nil {
					break
				}
				tried[member] = true

				stream, err := g.openStream(member, false)
				if err != nil {
					lastErr = err
					continue
				}
				// stream.dc.cipher is the key derived for this stream: the
				// configured cipher unless the session agreed a post-quantum key.
				return &tunnelHTTPConn{
					Stream:  crypto.NewStream(stream.dc.conn, stream.dc.cipher),
					conn:    stream.dc.conn,
					tunnel:  member,
					release: stream.release,
				}, nil
			}
			if lastErr == nil {
				lastErr = errors.New("no member could provide a stream")
			}
			return nil, lastErr
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: g.dialTimeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
		// The client's web server chose the encoding; re-encoding here would
		// change the body the visitor receives.
		DisableCompression: true,
	}

	target := &url.URL{Scheme: "http", Host: "tunnel"}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorLog:  log.New(&prefixWriter{logger: g.logger, prefix: "http " + g.Name + ": "}, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.logger.Printf("proxy %q: http request for %s failed: %v", g.Name, r.Host, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// --- totals -------------------------------------------------------------------

// Totals aggregates the group's counters.
func (g *ProxyGroup) Totals() (active, total, in, out int64) {
	for _, member := range g.Members() {
		active += member.Active.Load()
		total += member.Total.Load()
		in += member.BytesIn.Load()
		out += member.BytesOut.Load()
	}
	return active, total, in, out
}

// MemberSummary describes one member for the dashboard and the API.
type MemberSummary struct {
	ClientID  string  `json:"client_id"`
	LocalAddr string  `json:"local_addr"`
	LatencyMS float64 `json:"latency_ms"`
	Active    int64   `json:"active_connections"`
	Total     int64   `json:"total_connections"`
	BytesIn   int64   `json:"bytes_in"`
	BytesOut  int64   `json:"bytes_out"`
	Healthy   bool    `json:"healthy"`
}

// Summary lists the group's members, oldest first.
func (g *ProxyGroup) Summary() []MemberSummary {
	out := make([]MemberSummary, 0, g.memberCount())
	for _, member := range g.Members() {
		out = append(out, MemberSummary{
			ClientID:  member.Session.ID,
			LocalAddr: member.Spec.LocalAddr,
			LatencyMS: float64(member.Latency().Microseconds()) / 1000,
			Active:    member.Active.Load(),
			Total:     member.Total.Load(),
			BytesIn:   member.BytesIn.Load(),
			BytesOut:  member.BytesOut.Load(),
			Healthy:   member.healthy(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out
}
