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
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// vhostRouter publishes every http or https proxy on one shared listener. The
// request's Host header selects the proxy, so a single TLS certificate and a
// single port serve any number of local web servers.
type vhostRouter struct {
	kind    string // "http" or "https"
	cfg     *config.Config
	logger  *log.Logger
	metrics *Metrics

	mu        sync.RWMutex
	exact     map[string][]*ProxyGroup
	wildcard  map[string][]*ProxyGroup // key is the suffix including the leading dot
	subdomain string

	listener net.Listener
	server   *http.Server
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

	for _, raw := range domains {
		domain := strings.ToLower(strings.TrimSuffix(raw, "."))
		if err := b.router.checkFreeLocked(domain, b.group); err != nil {
			return err
		}
		if strings.HasPrefix(domain, "*.") {
			b.router.wildcard[domain[1:]] = append(b.router.wildcard[domain[1:]], b.group)
		} else {
			b.router.exact[domain] = append(b.router.exact[domain], b.group)
		}
		b.domains = append(b.domains, domain)
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
			if strings.HasPrefix(domain, "*.") {
				v.wildcard[domain[1:]] = dropGroup(v.wildcard[domain[1:]], group)
			} else {
				v.exact[domain] = dropGroup(v.exact[domain], group)
			}
		}
	}

	for _, raw := range group.Domains {
		domain := strings.ToLower(strings.TrimSuffix(raw, "."))
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
		if err := v.checkFreeLocked(group.Name, group); err != nil {
			return nil, err
		}
		v.exact[group.Name] = append(v.exact[group.Name], group)
		registered = append(registered, group.Name)
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
func (g *ProxyGroup) routeLocations() []string {
	if len(g.Locations) == 0 {
		return []string{""}
	}
	return g.Locations
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
		if strings.HasPrefix(domain, "*.") {
			v.wildcard[domain[1:]] = dropGroup(v.wildcard[domain[1:]], binding.group)
			continue
		}
		v.exact[domain] = dropGroup(v.exact[domain], binding.group)
	}
}

// dropGroup removes one proxy from a hostname's list, deleting the entry when
// the list empties.
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

	if groups, ok := v.exact[host]; ok {
		return groups
	}

	best := ""
	var match []*ProxyGroup
	for suffix, groups := range v.wildcard {
		if len(host) <= len(suffix) || !strings.HasSuffix(host, suffix) {
			continue
		}
		if len(suffix) > len(best) {
			best, match = suffix, groups
		}
	}
	if match != nil {
		return match
	}

	if v.subdomain != "" && strings.HasSuffix(host, "."+v.subdomain) {
		name := strings.TrimSuffix(host, "."+v.subdomain)
		if groups, ok := v.exact[name]; ok {
			return groups
		}
	}
	return nil
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
		v.logger.Printf("%s: no proxy is registered for host %q at %q", v.kind, host, r.URL.Path)
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
		v.logger.Printf("%s: custom_404_page %q cannot be read: %v", v.kind, path, err)
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
	v.listener = listener

	v.server = &http.Server{
		Handler:           http.HandlerFunc(v.handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          log.New(&prefixWriter{logger: v.logger, prefix: v.kind + ": "}, "", 0),
	}

	v.logger.Printf("%s virtual host listener on %s", v.kind, listener.Addr())
	go func() {
		if err := v.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			v.logger.Printf("%s listener stopped: %v", v.kind, err)
		}
	}()
	return nil
}

func (v *vhostRouter) stop() {
	if v.server != nil {
		_ = v.server.Close()
	}
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
	*crypto.Stream
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
