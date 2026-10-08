package server

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// refusalLog throttles the one-line-per-request refusals of the name-routing
// entry points (vhost, tcpmux, TLS passthrough). None of the three sits behind
// an access list, so any scanner can produce one refusal per packet, and the
// first burst is all an operator can usefully read; past it the refusals are
// summarised once a minute, the way the datagram pump throttles its read
// errors. now is a field so tests can move the clock.
type refusalLog struct {
	mu      sync.Mutex
	burst   int       // refusals written one by one so far
	dropped int       // refusals suppressed since the last written line
	last    time.Time // when the last line went out
	now     func() time.Time
}

// refusalLogBurst is how many refusals are written one by one before the
// summary takes over.
const refusalLogBurst = 64

func (r *refusalLog) logf(logger *log.Logger, format string, args ...any) {
	// The nil check has to be inside the lock: every connection that hits an
	// unknown name runs logf from its own goroutine, so two concurrent
	// refusals on a fresh refusalLog would otherwise read nil and write the
	// function pointer at the same time.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.now == nil {
		r.now = time.Now
	}
	now := r.now()
	if r.burst < refusalLogBurst {
		r.burst++
		r.last = now
		logger.Printf(format, args...)
		return
	}
	r.dropped++
	if now.Sub(r.last) >= time.Minute {
		// This call's line is the summary, so the count names the refusals
		// that produced no line of their own.
		logger.Printf("%s (suppressed %d more refusals since the last line)", fmt.Sprintf(format, args...), r.dropped-1)
		r.last = now
		r.dropped = 0
	}
}

// normalizeDomain folds a configured hostname into the key every name-routing
// table publishes it under: surrounding whitespace dropped, one trailing root
// dot dropped, lowercased. vhost, tcpmux and the TLS passthrough route by
// hostname from the same spec, so one input cannot end up under different
// keys per proxy type.
func normalizeDomain(raw string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(raw, ".")))
}

// vhostRouter publishes every http or https proxy on one shared listener. The
// request's Host header selects the proxy, so a single TLS certificate and a
// single port serve any number of local web servers.
type vhostRouter struct {
	kind     string // "http" or "https"
	cfg      *config.Config
	logger   *log.Logger
	notFound refusalLog
	metrics  *Metrics

	mu        sync.RWMutex
	exact     map[string][]*ProxyGroup
	wildcard  map[string][]*ProxyGroup // key is the suffix including the leading dot
	subdomain string

	listener net.Listener
	server   *http.Server
	// done is closed by stop; start refuses to serve on a router that was
	// already stopped, the same guard the sni and tcpmux listeners carry.
	done     chan struct{}
	stopOnce sync.Once
}

// vhostSet holds one router per listener kind.
type vhostSet struct {
	http  *vhostRouter
	https *vhostRouter
}

func newVhostSet(cfg *config.Config, logger *log.Logger, metrics *Metrics) *vhostSet {
	set := &vhostSet{}
	if cfg.Server.HTTPPort > 0 {
		set.http = newVhostRouter("http", cfg, logger, metrics)
	}
	if cfg.Server.HTTPSPort > 0 {
		set.https = newVhostRouter("https", cfg, logger, metrics)
	}
	return set
}

// start binds every configured listener. The certificate is loaded first so a bad
// path is reported before any port is taken.
func (v *vhostSet) start() error {
	if v.https != nil {
		cert, err := tls.LoadX509KeyPair(v.https.cfg.Server.HTTPSCertFile, v.https.cfg.Server.HTTPSKeyFile)
		if err != nil {
			return fmt.Errorf("load the https certificate: %w", err)
		}
		if err := v.https.start(v.https.cfg.HTTPSAddr(), &cert); err != nil {
			return err
		}
	}
	if v.http != nil {
		if err := v.http.start(v.http.cfg.HTTPAddr(), nil); err != nil {
			// The HTTPS router may already be serving; leave nothing bound
			// behind a failed start.
			if v.https != nil {
				v.https.stop()
			}
			return err
		}
	}
	return nil
}

func (v *vhostSet) stop() {
	if v.http != nil {
		v.http.stop()
	}
	if v.https != nil {
		v.https.stop()
	}
}

// domains lists every hostname published across both listeners, for the startup
// banner and the dashboard.
func (v *vhostSet) domains() []string {
	var out []string
	if v.http != nil {
		out = append(out, v.http.Domains()...)
	}
	if v.https != nil {
		out = append(out, v.https.Domains()...)
	}
	sort.Strings(out)
	return out
}

func (v *vhostSet) add(group *ProxyGroup) (*vhostBinding, error) {
	router := v.routerFor(group)
	if router == nil {
		port := "http_port"
		if group.Type == protocol.ProxyTypeHTTPS {
			port = "https_port"
		}
		return nil, fmt.Errorf("proxy %q is type %s but server.%s is not configured",
			group.Name, group.Type, port)
	}
	return router.add(group)
}

func (v *vhostSet) routerFor(group *ProxyGroup) *vhostRouter {
	switch group.Type {
	case protocol.ProxyTypeHTTP:
		return v.http
	case protocol.ProxyTypeHTTPS:
		return v.https
	default:
		return nil
	}
}

func newVhostRouter(kind string, cfg *config.Config, logger *log.Logger, metrics *Metrics) *vhostRouter {
	return &vhostRouter{
		kind:      kind,
		cfg:       cfg,
		logger:    logger,
		metrics:   metrics,
		exact:     make(map[string][]*ProxyGroup),
		wildcard:  make(map[string][]*ProxyGroup),
		subdomain: strings.ToLower(cfg.Server.SubdomainHost),
		done:      make(chan struct{}),
	}
}

// vhostBinding is one proxy's registration on a router.
type vhostBinding struct {
	router  *vhostRouter
	group   *ProxyGroup
	domains []string
	once    sync.Once
}

func (b *vhostBinding) remove() {
	b.once.Do(func() { b.router.remove(b) })
}

// extend adds hostnames a later member of a pool asked for.
func (b *vhostBinding) extend(domains []string) error {
	if len(domains) == 0 {
		return nil
	}

	b.router.mu.Lock()
	defer b.router.mu.Unlock()

	added := make([]string, 0, len(domains))
	rollback := func() {
		for _, domain := range added {
			b.router.dropGroupLocked(domain, b.group)
		}
		b.domains = b.domains[:len(b.domains)-len(added)]
	}

	for _, raw := range domains {
		domain := normalizeDomain(raw)
		if domain == "" {
			// The tcpmux and sni tables skip what normalizes to empty, and
			// so does this one: a blank entry would otherwise be installed
			// under the empty key, which a request without a Host header
			// then matches.
			continue
		}
		if containsDomain(b.domains, domain) || containsDomain(added, domain) {
			// A pool member repeating a hostname this binding already publishes:
			// there is nothing to add, and adding it a second time would make the
			// rollback below delete the entry that was already there.
			continue
		}
		if err := b.router.checkFreeLocked(domain, b.group); err != nil {
			// The member this was for is detached by the caller, so the hostnames
			// already taken on its behalf have to go with it: otherwise the group
			// serves names only a registration that failed ever asked for, and no
			// other client can publish them.
			rollback()
			return err
		}
		if strings.HasPrefix(domain, "*.") {
			b.router.wildcard[domain[1:]] = append(b.router.wildcard[domain[1:]], b.group)
		} else {
			b.router.exact[domain] = append(b.router.exact[domain], b.group)
		}
		b.domains = append(b.domains, domain)
		added = append(added, domain)
	}
	return nil
}

// add registers a proxy's domains, refusing a domain that is already taken by
// another proxy so two clients cannot silently share a hostname.
func (v *vhostRouter) add(group *ProxyGroup) (*vhostBinding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	binding := &vhostBinding{router: v, group: group, domains: append([]string(nil), group.Domains...)}
	registered := make([]string, 0, len(group.Domains))

	rollback := func() {
		for _, domain := range registered {
			v.dropGroupLocked(domain, group)
		}
	}

	for _, raw := range group.Domains {
		domain := normalizeDomain(raw)
		if domain == "" {
			// The tcpmux and sni tables skip what normalizes to empty, and
			// so does this one: a blank entry would otherwise be installed
			// under the empty key, which a request without a Host header
			// then matches.
			continue
		}
		if err := v.checkFreeLocked(domain, group); err != nil {
			rollback()
			return nil, err
		}
		if strings.HasPrefix(domain, "*.") {
			v.wildcard[domain[1:]] = append(v.wildcard[domain[1:]], group)
		} else {
			v.exact[domain] = append(v.exact[domain], group)
		}
		registered = append(registered, domain)
	}

	if len(registered) == 0 {
		if v.subdomain == "" {
			rollback()
			return nil, fmt.Errorf("proxy %q has no domains and server.subdomain_host is not set", group.Name)
		}
		// The lookup lowercases the Host header and strips the configured suffix
		// before it looks the name up, so the key has to be lowercased here too:
		// a name with a capital letter was published and then unreachable.
		name := strings.ToLower(group.Name)
		if err := v.checkFreeLocked(name, group); err != nil {
			return nil, err
		}
		v.exact[name] = append(v.exact[name], group)
		registered = append(registered, name)
	}

	binding.domains = registered
	return binding, nil
}

// checkFreeLocked refuses a (domain, location) pair another proxy already
// claims. Same domain with different locations is the point of locations: the
// longest matching prefix on one hostname routes to different proxies.
func (v *vhostRouter) checkFreeLocked(domain string, group *ProxyGroup) error {
	var bucket map[string][]*ProxyGroup
	if strings.HasPrefix(domain, "*.") {
		bucket = v.wildcard
		domain = domain[1:]
	} else {
		bucket = v.exact
	}
	for _, existing := range bucket[domain] {
		if existing == group {
			continue
		}
		for _, location := range group.routeLocations() {
			for _, existingLocation := range existing.routeLocations() {
				if location == existingLocation && group.RouteByHTTPUser == existing.RouteByHTTPUser {
					return fmt.Errorf("the domain %q at location %q for http user %q is already published by proxy %q",
						domain, locationDisplay(location), userDisplay(group.RouteByHTTPUser), existing.Name)
				}
			}
		}
	}
	return nil
}

// routeLocations lists the path prefixes a proxy claims on its hostnames: one
// empty prefix when it publishes the whole host.
//
// An explicit "/" is that same claim and is normalized to the empty prefix. The
// two were compared as written, so a proxy with no locations and one with
// locations = ["/"] were admitted on one hostname and then the longer "/" won every
// request in selectByRoute, silently taking the hostname from the first proxy.
func (g *ProxyGroup) routeLocations() []string {
	if len(g.Locations) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(g.Locations))
	for _, location := range g.Locations {
		if location == "/" {
			location = ""
		}
		out = append(out, location)
	}
	return out
}

func locationDisplay(location string) string {
	if location == "" {
		return "/"
	}
	return location
}

func userDisplay(user string) string {
	if user == "" {
		return "(any)"
	}
	return user
}

// selectByRoute picks one proxy from the host's candidates the way frp's
// router does: routes named for the request's basic-auth user come first, and
// a proxy that named no user catches whatever they miss; within each pass the
// longest declared location prefix that matches the request path wins, and a
// proxy with no locations catches whatever is left.
func selectByRoute(candidates []*ProxyGroup, path, user string) *ProxyGroup {
	for _, passUser := range []string{user, ""} {
		best := (*ProxyGroup)(nil)
		bestLen := -1
		for _, group := range candidates {
			if group.RouteByHTTPUser != passUser {
				continue
			}
			for _, location := range group.routeLocations() {
				if strings.HasPrefix(path, location) && len(location) > bestLen {
					best, bestLen = group, len(location)
				}
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

func (v *vhostRouter) remove(binding *vhostBinding) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, domain := range binding.domains {
		v.dropGroupLocked(domain, binding.group)
	}
}

// dropGroupLocked removes one proxy from one hostname's list, deleting the key when
// nothing is left. Storing the emptied slice back — which `bucket[domain] =
// dropGroup(...)` did — left the key present, and lookup treats a present key as a
// match: a withdrawn hostname then answered its own (empty) route forever instead of
// falling through to a wildcard or the subdomain convention, a stale wildcard could
// shadow a genuine shorter match, and every distinct hostname a client ever
// registered stayed in the map for the life of the process.
func (v *vhostRouter) dropGroupLocked(domain string, group *ProxyGroup) {
	if strings.HasPrefix(domain, "*.") {
		base := domain[1:]
		if rest := dropGroup(v.wildcard[base], group); len(rest) == 0 {
			delete(v.wildcard, base)
		} else {
			v.wildcard[base] = rest
		}
		return
	}
	if rest := dropGroup(v.exact[domain], group); len(rest) == 0 {
		delete(v.exact, domain)
	} else {
		v.exact[domain] = rest
	}
}

// dropGroup removes one proxy from a hostname's list.
func dropGroup(groups []*ProxyGroup, group *ProxyGroup) []*ProxyGroup {
	out := groups[:0]
	for _, candidate := range groups {
		if candidate != group {
			out = append(out, candidate)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Domains lists the hostnames currently published, sorted.
func (v *vhostRouter) Domains() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]string, 0, len(v.exact)+len(v.wildcard))
	for domain := range v.exact {
		out = append(out, domain)
	}
	for suffix := range v.wildcard {
		out = append(out, "*"+suffix)
	}
	sort.Strings(out)
	return out
}

// lookup lists the proxies a hostname can reach: the exact name first, then
// the longest matching wildcard, then the subdomain-host convention.
func (v *vhostRouter) lookup(host string) []*ProxyGroup {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	v.mu.RLock()
	defer v.mu.RUnlock()

	if groups, ok := v.exact[host]; ok && len(groups) > 0 {
		return copyGroups(groups)
	}

	best := ""
	var match []*ProxyGroup
	for suffix, groups := range v.wildcard {
		if len(groups) == 0 {
			continue
		}
		// A wildcard owns its base domain as well, the way tcpmux and the TLS
		// passthrough match: "*.example.com" answers example.com itself, not only
		// the subdomains. An exact entry for the base still wins, because exact
		// names are looked up first.
		if host != strings.TrimPrefix(suffix, ".") && (len(host) <= len(suffix) || !strings.HasSuffix(host, suffix)) {
			continue
		}
		if len(suffix) > len(best) {
			best, match = suffix, groups
		}
	}
	if match != nil {
		return copyGroups(match)
	}

	if v.subdomain != "" && strings.HasSuffix(host, "."+v.subdomain) {
		name := strings.TrimSuffix(host, "."+v.subdomain)
		if groups, ok := v.exact[name]; ok && len(groups) > 0 {
			return copyGroups(groups)
		}
	}
	return nil
}

// containsDomain reports whether a normalized domain is in the list.
func containsDomain(domains []string, domain string) bool {
	for _, existing := range domains {
		if existing == domain {
			return true
		}
	}
	return false
}

// copyGroups hands the caller a slice of its own. The router keeps the stored
// slice and compacts it in place when a proxy is removed, so a request that
// iterated it after the lock was dropped would race the removal of a client that
// is disconnecting.
func copyGroups(groups []*ProxyGroup) []*ProxyGroup {
	if groups == nil {
		return nil
	}
	out := make([]*ProxyGroup, len(groups))
	copy(out, groups)
	return out
}

// handler dispatches one request to the proxy that owns its Host header.
func (v *vhostRouter) handler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	candidates := v.lookup(host)
	// The routing user is the name the visitor presented, before any proxy's
	// own credential check: two proxies can share a hostname and split it by
	// who is asking.
	user, _, _ := r.BasicAuth()
	group := selectByRoute(candidates, r.URL.Path, user)
	if group == nil {
		v.notFound.logf(v.logger, "%s: no proxy is registered for host %q at %q", v.kind, host, r.URL.Path)
		v.serveNotFound(w, host)
		return
	}
	group.serveHTTP(w, r)
}

// serveNotFound answers a request for a hostname nobody published. A
// configured custom_404_page replaces the built-in text; the file is read at
// request time, so an edit appears without a restart, and a page that cannot
// be read falls back to the built-in answer.
func (v *vhostRouter) serveNotFound(w http.ResponseWriter, host string) {
	if path := v.cfg.Server.Custom404Page; path != "" {
		page, err := os.ReadFile(path)
		if err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(page)
			return
		}
		logging.Warnf(v.logger, "%s: custom_404_page %q cannot be read: %v", v.kind, path, err)
	}
	http.Error(w, fmt.Sprintf("no tunnel is registered for %s", host), http.StatusNotFound)
}

// start binds the listener and serves in the background.
func (v *vhostRouter) start(addr string, tlsCert *tls.Certificate) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return flynet.ListenError(addr, err)
	}
	if tlsCert != nil {
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{*tlsCert},
			MinVersion:   tls.VersionTLS12,
		})
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(v.handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          log.New(&prefixWriter{logger: v.logger, prefix: v.kind + ": "}, "", 0),
	}

	// The done check and the stores share one critical section with stop, so
	// a stop that lands between them cannot leave this listener serving with
	// nobody left to close it.
	v.mu.Lock()
	select {
	case <-v.done:
		v.mu.Unlock()
		_ = listener.Close()
		return fmt.Errorf("the %s virtual host listener was stopped before it started", v.kind)
	default:
	}
	v.listener = listener
	v.server = server
	v.mu.Unlock()

	v.logger.Printf("%s virtual host listener on %s", v.kind, listener.Addr())
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			v.logger.Printf("%s listener stopped: %v", v.kind, err)
		}
	}()
	return nil
}

func (v *vhostRouter) stop() {
	// Once, not a check-then-act: a failing startup path and Shutdown can both
	// call this, and a double close would panic.
	v.stopOnce.Do(func() {
		close(v.done)
		v.mu.Lock()
		server := v.server
		listener := v.listener
		v.server = nil
		v.listener = nil
		v.mu.Unlock()
		if server != nil {
			// Close closes the listener this server serves.
			_ = server.Close()
		}
		if listener != nil {
			// Serve may never have registered the listener with the server:
			// a stop that lands between start's unlock and the Serve
			// goroutine is exactly that window, and closing only the server
			// would leave the port bound with nobody left to close it.
			_ = listener.Close()
		}
	})
}

// prefixWriter adapts the standard library's HTTP server error log onto the
// server logger.
type prefixWriter struct {
	logger *log.Logger
	prefix string
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.logger.Printf("%s%s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// tunnelHTTPConn presents a tunnelled data stream as a net.Conn for the HTTP
// transport. Closing it releases the stream slot.
type tunnelHTTPConn struct {
	// Conn is the view the transport reads and writes: the session cipher, and
	// on top of it the per-proxy cipher and compression the DataRequest asked
	// the client for.
	net.Conn
	conn    net.Conn
	tunnel  *Tunnel
	release func()

	closeOnce sync.Once
}

func (c *tunnelHTTPConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *tunnelHTTPConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *tunnelHTTPConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *tunnelHTTPConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *tunnelHTTPConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

func (c *tunnelHTTPConn) Close() error {
	err := c.conn.Close()
	c.closeOnce.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return err
}

// countingResponseWriter records how many bytes went back to the visitor. For a
// normal response that is the Write path; for a protocol switch the connection is
// hijacked, so the bytes moved after the switch are counted separately.
type countingResponseWriter struct {
	http.ResponseWriter
	written int64

	// hijackRead counts bytes read from the hijacked visitor connection, which is
	// the request direction. hijackSent counts bytes written to it, the response
	// direction. Both are atomic because the reverse proxy moves those bytes from
	// two copy goroutines, and the serving goroutine reads them once the request
	// is over.
	hijackRead atomic.Int64
	hijackSent atomic.Int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer so http.ResponseController can reach the
// interfaces this wrapper does not implement itself.
func (w *countingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack hands the connection over for a protocol switch, wrapping it so the
// bytes carried after the switch still reach the metrics and the ledger. Without
// this the switch works but its traffic disappears from every counter.
func (w *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the underlying response writer cannot be hijacked")
	}
	conn, brw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &hijackedConn{Conn: conn, counter: w}, brw, nil
}

// Flush keeps streaming responses streaming through the wrapper.
func (w *countingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// hijackedConn counts the bytes a protocol switch moves after the response has
// been handed over, on top of a raw net.Conn.
type hijackedConn struct {
	net.Conn
	counter *countingResponseWriter
}

func (c *hijackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.counter.hijackRead.Add(int64(n))
	return n, err
}

func (c *hijackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.counter.hijackSent.Add(int64(n))
	return n, err
}

// countingReadCloser records how many request body bytes were actually read, so a
// chunked body — whose ContentLength is -1 — is billed like a body of known
// length. The read counter is atomic because the HTTP transport drains the body
// from its own goroutine.
type countingReadCloser struct {
	io.ReadCloser
	read atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.read.Add(int64(n))
	return n, err
}
