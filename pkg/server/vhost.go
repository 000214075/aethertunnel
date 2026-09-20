package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// vhostRouter publishes every http or https tunnel on one shared listener. The
// request's Host header selects the tunnel, so a single TLS certificate and a
// single port serve any number of local web servers.
type vhostRouter struct {
	kind    string // "http" or "https"
	cfg     *config.Config
	logger  *log.Logger
	metrics *Metrics

	mu        sync.RWMutex
	exact     map[string]*Tunnel
	wildcard  map[string]*Tunnel // key is the suffix including the leading dot
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

// start binds every configured listener. The certificate is loaded first so a
// bad path is reported before any port is taken.
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

func (v *vhostSet) routerFor(t *Tunnel) *vhostRouter {
	switch t.Type {
	case protocol.ProxyTypeHTTP:
		return v.http
	case protocol.ProxyTypeHTTPS:
		return v.https
	default:
		return nil
	}
}

func (v *vhostSet) add(t *Tunnel) (*vhostBinding, error) {
	router := v.routerFor(t)
	if router == nil {
		port := "http_port"
		if t.Type == protocol.ProxyTypeHTTPS {
			port = "https_port"
		}
		return nil, fmt.Errorf("proxy %q is type %s but server.%s is not configured", t.Name, t.Type, port)
	}
	return router.add(t)
}

func newVhostRouter(kind string, cfg *config.Config, logger *log.Logger, metrics *Metrics) *vhostRouter {
	return &vhostRouter{
		kind:      kind,
		cfg:       cfg,
		logger:    logger,
		metrics:   metrics,
		exact:     make(map[string]*Tunnel),
		wildcard:  make(map[string]*Tunnel),
		subdomain: strings.ToLower(cfg.Server.SubdomainHost),
	}
}

// vhostBinding is one tunnel's registration on a router.
type vhostBinding struct {
	router  *vhostRouter
	tunnel  *Tunnel
	domains []string
	once    sync.Once
}

func (b *vhostBinding) remove() {
	b.once.Do(func() { b.router.remove(b) })
}

// add registers a tunnel's domains, refusing a domain that is already taken by
// another tunnel so two clients cannot silently share a hostname.
func (v *vhostRouter) add(t *Tunnel) (*vhostBinding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	binding := &vhostBinding{router: v, tunnel: t}
	registered := make([]string, 0, len(t.Domains))

	rollback := func() {
		for _, domain := range registered {
			if strings.HasPrefix(domain, "*.") {
				delete(v.wildcard, domain[1:])
			} else {
				delete(v.exact, domain)
			}
		}
	}

	for _, raw := range t.Domains {
		domain := strings.ToLower(strings.TrimSuffix(raw, "."))
		if err := v.checkFreeLocked(domain, t); err != nil {
			rollback()
			return nil, err
		}
		if strings.HasPrefix(domain, "*.") {
			v.wildcard[domain[1:]] = t
		} else {
			v.exact[domain] = t
		}
		registered = append(registered, domain)
	}

	if len(registered) == 0 {
		if v.subdomain == "" {
			rollback()
			return nil, fmt.Errorf("proxy %q has no domains and server.subdomain_host is not set", t.Name)
		}
		if _, taken := v.exact[t.Name]; taken {
			return nil, fmt.Errorf("the subdomain %q is already published by another tunnel", t.Name)
		}
		v.exact[t.Name] = t
		registered = append(registered, t.Name)
	}

	binding.domains = registered
	return binding, nil
}

func (v *vhostRouter) checkFreeLocked(domain string, t *Tunnel) error {
	lookup := domain
	if strings.HasPrefix(domain, "*.") {
		lookup = domain[1:]
		if existing, ok := v.wildcard[lookup]; ok && existing != t {
			return fmt.Errorf("the domain %q is already published by tunnel %q", domain, existing.Name)
		}
		return nil
	}
	if existing, ok := v.exact[domain]; ok && existing != t {
		return fmt.Errorf("the domain %q is already published by tunnel %q", domain, existing.Name)
	}
	return nil
}

func (v *vhostRouter) remove(b *vhostBinding) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, domain := range b.domains {
		if strings.HasPrefix(domain, "*.") {
			if current, ok := v.wildcard[domain[1:]]; ok && current == b.tunnel {
				delete(v.wildcard, domain[1:])
			}
			continue
		}
		if current, ok := v.exact[domain]; ok && current == b.tunnel {
			delete(v.exact, domain)
		}
	}
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

// lookup finds the tunnel that owns a hostname: an exact match first, then the
// longest matching wildcard, then the subdomain-host convention.
func (v *vhostRouter) lookup(host string) *Tunnel {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	v.mu.RLock()
	defer v.mu.RUnlock()

	if tunnel, ok := v.exact[host]; ok {
		return tunnel
	}

	best := ""
	var match *Tunnel
	for suffix, tunnel := range v.wildcard {
		if len(host) <= len(suffix) || !strings.HasSuffix(host, suffix) {
			continue
		}
		if len(suffix) > len(best) {
			best, match = suffix, tunnel
		}
	}
	if match != nil {
		return match
	}

	if v.subdomain != "" && strings.HasSuffix(host, "."+v.subdomain) {
		name := strings.TrimSuffix(host, "."+v.subdomain)
		if tunnel, ok := v.exact[name]; ok {
			return tunnel
		}
	}
	return nil
}

// handler dispatches one request to the tunnel that owns its Host header.
func (v *vhostRouter) handler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	tunnel := v.lookup(host)
	if tunnel == nil {
		v.logger.Printf("%s: no tunnel is registered for host %q", v.kind, host)
		http.Error(w, fmt.Sprintf("no tunnel is registered for %s", host), http.StatusNotFound)
		return
	}
	tunnel.serveHTTP(w, r)
}

// start binds the listener and serves in the background.
func (v *vhostRouter) start(addr string, tlsCert *tls.Certificate) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
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

// --- one tunnel's reverse proxy ----------------------------------------------

// serveHTTP forwards one request to the client's local web server over a fresh
// tunnelled connection.
func (t *Tunnel) serveHTTP(w http.ResponseWriter, r *http.Request) {
	proxy := t.httpProxy()
	if proxy == nil {
		http.Error(w, "this tunnel has no http handler", http.StatusInternalServerError)
		return
	}

	t.metrics.httpRequests.Add(1)
	t.metrics.tunnel(t.Name).httpRequests.Add(1)
	t.Active.Add(1)
	t.metrics.streamOpened(t.Name)
	defer func() {
		t.Active.Add(-1)
		t.metrics.streamClosed(t.Name)
	}()

	counter := &countingResponseWriter{ResponseWriter: w}
	proxy.ServeHTTP(counter, r)

	toClient := r.ContentLength
	if toClient < 0 {
		toClient = 0
	}
	fromClient := counter.written
	t.BytesOut.Add(toClient)
	t.BytesIn.Add(fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)
	t.metrics.recordStream(t.Name, toClient, fromClient)
}

// httpProxy builds the tunnel's reverse proxy once.
func (t *Tunnel) httpProxy() *httputil.ReverseProxy {
	t.proxyOnce.Do(func() {
		if t.Type == protocol.ProxyTypeHTTP || t.Type == protocol.ProxyTypeHTTPS {
			t.proxy = t.buildHTTPProxy()
		}
	})
	return t.proxy
}

func (t *Tunnel) buildHTTPProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dc, release, err := t.openStream(false)
			if err != nil {
				return nil, err
			}
			return &tunnelHTTPConn{
				Stream:  crypto.NewStream(dc.conn, t.cipher),
				conn:    dc.conn,
				tunnel:  t,
				release: release,
			}, nil
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: time.Duration(t.dialTimeout) * time.Second,
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
		ErrorLog:  log.New(&prefixWriter{logger: t.logger, prefix: "http " + t.Name + ": "}, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			t.logger.Printf("tunnel %q: http request for %s failed: %v", t.Name, r.Host, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
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

// countingResponseWriter records how many bytes went back to the visitor.
type countingResponseWriter struct {
	http.ResponseWriter
	written int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

// Flush keeps streaming responses streaming through the wrapper.
func (w *countingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
