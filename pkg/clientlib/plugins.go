package clientlib

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	aecrypto "github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// pluginListener is a net.Listener that hands out connections the proxy data
// path creates for it: dialForProxy makes a net.Pipe pair, feeds one end here,
// and returns the other to the stream — which then carries HTTP into the
// in-process server exactly as it would carry it to a local service.
type pluginListener struct {
	conns chan net.Conn
	done  sync.Once
	// closed guards the channel: a Close followed by a concurrent feed
	// would otherwise panic on a send to the closed channel. The mutex is
	// safe to hold across the feed because the send is non-blocking.
	mu     sync.Mutex
	closed bool
	addr   pluginAddr
}

func (l *pluginListener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		return nil, errors.New("the plugin listener is closed")
	}
	return conn, nil
}

// feedPlugin hands one pipe end to an in-process server and returns the end
// the stream carries.
func (c *client) feedPlugin(listener *pluginListener) (net.Conn, error) {
	client, server := net.Pipe()
	if !listener.feed(server) {
		return nil, errors.New("the plugin server is not accepting")
	}
	return client, nil
}

// errPluginServersStopped reports an in-process plugin server asked for after the
// client stopped them. Refusing is the only safe answer: the alternative is a
// server the stop snapshot has already passed, which nothing would ever shut down.
var errPluginServersStopped = errors.New("the client has stopped its plugin servers")

// proxyAuthorized checks the Proxy-Authorization header against the proxy's
// plugin_http_user/plugin_http_password; a proxy without either is open. The
// header is read by hand: r.BasicAuth only looks at Authorization, which a
// proxy never sees.
func proxyAuthorized(r *http.Request, proxy config.ProxyConfig) bool {
	user := proxy.PluginHTTPUser
	password := proxy.PluginHTTPPassword
	if user == "" && password == "" {
		return true
	}
	header := r.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	index := strings.IndexByte(string(decoded), ':')
	if index < 0 {
		return false
	}
	return aecrypto.EqualTokens(string(decoded[:index]), user) && aecrypto.EqualTokens(string(decoded[index+1:]), password)
}

// forgetPluginCaches drops the per-proxy caches the plugin entry points keep.
// They are keyed by name, so a reload that narrows allow_targets or rotates a
// plugin's certificate would otherwise keep serving the old, more permissive
// policy until the process restarted.
func (c *client) forgetPluginCaches(name string) {
	c.staticFilesMu.Lock()
	delete(c.pluginPolicies, name)
	c.staticFilesMu.Unlock()
	c.pluginTLSMu.Lock()
	delete(c.pluginTLS, name)
	c.pluginTLSMu.Unlock()
}

// targetPolicyFor builds and caches the allow_targets matcher for one proxy.
func (c *client) targetPolicyFor(proxy config.ProxyConfig) (*socks.TargetPolicy, error) {
	c.staticFilesMu.Lock()
	defer c.staticFilesMu.Unlock()
	if c.pluginPolicies == nil {
		c.pluginPolicies = map[string]*socks.TargetPolicy{}
	}
	if cached, ok := c.pluginPolicies[proxy.Name]; ok {
		return cached, nil
	}
	policy, err := socks.NewTargetPolicy(proxy.AllowTargets)
	if err != nil {
		return nil, err
	}
	c.pluginPolicies[proxy.Name] = policy
	return policy, nil
}

// checkProxyTarget enforces the http_proxy plugin's allow_targets; an empty
// list lets the proxy dial anything this client can reach, which the
// configuration layer refuses up front.
func (c *client) checkProxyTarget(proxy config.ProxyConfig, target string) error {
	if len(proxy.AllowTargets) == 0 {
		return nil
	}
	policy, err := c.targetPolicyFor(proxy)
	if err != nil {
		return err
	}
	_, err = policy.Check(target, 5*time.Second)
	return err
}

// proxyTargetDialCtx dials a target through the allow list when one is set,
// honoring ctx so a visitor that goes away stops the connect.
func (c *client) proxyTargetDialCtx(ctx context.Context, proxy config.ProxyConfig, target string) (net.Conn, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	dialer := &net.Dialer{Timeout: dialTimeout}
	if len(proxy.AllowTargets) == 0 {
		return dialer.DialContext(ctx, "tcp", target)
	}
	policy, err := c.targetPolicyFor(proxy)
	if err != nil {
		return nil, err
	}
	address, err := policy.Check(target, dialTimeout)
	if err != nil {
		return nil, err
	}
	return dialer.DialContext(ctx, "tcp", address)
}

// proxyTargetDial dials a CONNECT target through the allow list when one is set.
func (c *client) proxyTargetDial(proxy config.ProxyConfig, target string) (net.Conn, error) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	if len(proxy.AllowTargets) == 0 {
		return net.DialTimeout("tcp", target, dialTimeout)
	}
	policy, err := c.targetPolicyFor(proxy)
	if err != nil {
		return nil, err
	}
	return policy.Dial(target, dialTimeout)
}

// httpProxyHandler serves the HTTP proxy protocol the way a visitor speaks it
// to a proxy port: absolute-form requests name their target in the URL, and
// CONNECT opens a raw tunnel to the named authority. Both dial from this
// client's network, bounded by allow_targets, and both may ask for the
// http_user/http_password credentials.
func (c *client) httpProxyHandler(proxy config.ProxyConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !proxyAuthorized(r, proxy) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="aethertunnel http_proxy"`)
			http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			c.handleProxyConnect(w, r, proxy)
			return
		}
		target := r.Host
		if target == "" {
			http.Error(w, "the request names no target", http.StatusBadRequest)
			return
		}
		// An absolute-form request may leave the default port implicit, while the
		// allow list matches host:port pairs: name the port the request will use
		// before asking the list about it, and use the same name to dial.
		if _, _, err := net.SplitHostPort(target); err != nil {
			target = net.JoinHostPort(target, "80")
		}
		if err := c.checkProxyTarget(proxy, target); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		r.Header.Del("Proxy-Authorization")
		r.Header.Del("Proxy-Connection")
		r.Host = target
		r.URL.Host = target
		// The default transport dials the name it is given, so a name that resolved
		// to an allowed address while the list was checked could resolve elsewhere by
		// the time the connection is made. Dial through the list instead: it resolves
		// again and connects to an address it has just allowed.
		transport := &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return c.proxyTargetDialCtx(ctx, proxy, address)
			},
		}
		defer transport.CloseIdleConnections()
		resp, err := transport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// handleProxyConnect opens the raw tunnel a CONNECT request asks for.
func (c *client) handleProxyConnect(w http.ResponseWriter, r *http.Request, proxy config.ProxyConfig) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking is not supported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		// The visitor is left without an answer because the connection cannot be
		// taken over at all, so this line is the only record of why: the same
		// reason the other failures in this file are logged.
		logging.Warnf(c.logger, "plugin %s for %q: cannot take over the visitor's connection: %v",
			proxy.Plugin, proxy.Name, err)
		return
	}
	targetConn, err := c.proxyTargetDial(proxy, target)
	if err != nil {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	// The answer goes through the hijacked writer, not the socket: it is the
	// writer the server's buffering belongs to, and the reader below is what the
	// visitor's pipelined bytes are sitting in.
	if _, err := rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		_ = targetConn.Close()
		_ = conn.Close()
		return
	}
	// A visitor may send the first tunnelled bytes right behind the CONNECT
	// headers. The server has already read those into rw.Reader, so the tunnel
	// has to read through it: reading the raw connection would drop them.
	flynet.Pipe(targetConn, &hijackedConn{Conn: conn, reader: rw.Reader}, time.Duration(c.cfg.Client.IdleTimeoutSecs)*time.Second)
}

// hijackedConn reads through whatever the HTTP server buffered before the
// connection was hijacked, and forwards everything else to it.
type hijackedConn struct {
	net.Conn
	reader io.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// CloseWrite is forwarded by hand, like the other connection wrappers here:
// net.Conn does not carry it, and hiding the TCP connection's half-close would
// turn it into a full close, cutting off a reply the target only sends after the
// visitor's end of the request.
func (c *hijackedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}

// http2HTTPSHandler answers plain HTTP and forwards it to the local HTTPS
// service named by local_port, wrapping the local leg in TLS with the
// request's host as SNI — the certificate of a home NAS with a self-signed
// certificate, for example, needs no trusting.
func (c *client) http2HTTPSHandler(proxy config.ProxyConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := r.Host
		if host, _, err := net.SplitHostPort(sni); err == nil {
			sni = host
		}
		if proxy.HostHeaderRewrite != "" {
			r.Host = proxy.HostHeaderRewrite
		}
		dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
		raw, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// The local leg is bounded until the response header is in: a local
		// service that accepts and then never answers would otherwise hold the
		// visitor's request open forever. The bound is generous - a loaded
		// machine's post-quantum TLS handshake can take seconds - and is lifted
		// before the body streams. One connection per request: whatever the
		// path out of this handler does with the handshake, the write or the
		// response, the local leg has to be closed again —
		// http.ReadResponse's Body wrapper does not close the underlying conn
		// for it.
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(localLegBound(dialTimeout)))
		tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: sni})
		if err := tlsConn.HandshakeContext(r.Context()); err != nil {
			http.Error(w, "the local service's TLS handshake failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := r.Write(tlsConn); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Re-arm the bound for the response. The single deadline set before the
		// handshake also covered the request write, so a request whose body streamed
		// for a while left the response on an already-spent deadline and answered 502.
		_ = raw.SetDeadline(time.Now().Add(localLegBound(dialTimeout)))
		resp, err := http.ReadResponse(bufio.NewReader(tlsConn), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		_ = raw.SetDeadline(time.Time{})
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// localLegBound is how long a local leg may take from dial to response
// header: the dial timeout the operator configured, but never under thirty
// seconds, because a busy machine's post-quantum TLS handshake alone can eat
// several.
func localLegBound(dialTimeout time.Duration) time.Duration {
	if dialTimeout < 30*time.Second {
		return 30 * time.Second
	}
	return dialTimeout
}

// httpBridgeFor returns the cached listener behind an HTTP-flavoured plugin:
// the first call builds the in-process server and serves it for the client's
// lifetime, like the static_file bridge does.
func (c *client) httpBridgeFor(proxy config.ProxyConfig, handler http.Handler) (*pluginListener, error) {
	c.staticFilesMu.Lock()
	defer c.staticFilesMu.Unlock()
	if c.pluginsStopped {
		return nil, errPluginServersStopped
	}
	if c.staticFiles == nil {
		c.staticFiles = map[string]*staticFileServer{}
	}
	if entry, ok := c.staticFiles[proxy.Name]; ok {
		return entry.listener, nil
	}
	listener := &pluginListener{conns: make(chan net.Conn, 16), addr: pluginAddr{}}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = server.Serve(listener) }()
	c.staticFiles[proxy.Name] = &staticFileServer{listener: listener, server: server}
	return listener, nil
}

// serveHTTPS2HTTP terminates the visitor's TLS on the tunnel end with the
// configured certificate and relays the plaintext to the plain TCP service
// named by local_port — the https2http and tls2raw plugins share this bridge,
// one because the backend speaks HTTP, the other because it speaks its own
// protocol. The certificate loads once per proxy and is cached for the
// client's lifetime, so a connection costs no file reads.
func (c *client) serveHTTPS2HTTP(server net.Conn, proxy config.ProxyConfig) {
	tlsConfig, err := c.https2HTTPConfig(proxy)
	if err != nil {
		c.logger.Printf("plugin %s for %q: %v", proxy.Plugin, proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Now().Add(10 * time.Second))
	tlsConn := tls.Server(server, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		// A plain-HTTP visitor on a TLS endpoint ends here; that is the
		// visitor's mistake to see, not a tunnel fault.
		logging.Warnf(c.logger, "plugin %s for %q: the visitor's TLS handshake failed: %v", proxy.Plugin, proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Time{})
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	local, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
	if err != nil {
		logging.Warnf(c.logger, "plugin %s for %q: cannot reach the local service %s: %v",
			proxy.Plugin, proxy.Name, proxy.LocalAddr(), err)
		_ = tlsConn.Close()
		return
	}
	flynet.Pipe(local, tlsConn, time.Duration(c.cfg.Client.IdleTimeoutSecs)*time.Second)
}

// serveHTTPS2HTTPS terminates the visitor's TLS with the configured
// certificate and wraps the local leg in TLS as well: both legs encrypted, for
// a local service that already speaks HTTPS. The local certificate is not
// verified — the leg is inside this machine — with the local host as SNI.
func (c *client) serveHTTPS2HTTPS(server net.Conn, proxy config.ProxyConfig) {
	tlsConfig, err := c.https2HTTPConfig(proxy)
	if err != nil {
		c.logger.Printf("plugin %s for %q: %v", proxy.Plugin, proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Now().Add(10 * time.Second))
	tlsConn := tls.Server(server, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		logging.Warnf(c.logger, "plugin %s for %q: the visitor's TLS handshake failed: %v", proxy.Plugin, proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Time{})
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	local, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
	if err != nil {
		logging.Warnf(c.logger, "plugin %s for %q: cannot reach the local service %s: %v",
			proxy.Plugin, proxy.Name, proxy.LocalAddr(), err)
		_ = tlsConn.Close()
		return
	}
	sni, _, err := net.SplitHostPort(proxy.LocalAddr())
	if err != nil {
		sni = proxy.LocalAddr()
	}
	localTLS := tls.Client(local, &tls.Config{InsecureSkipVerify: true, ServerName: sni})
	_ = local.SetDeadline(time.Now().Add(localLegBound(dialTimeout)))
	if err := localTLS.Handshake(); err != nil {
		logging.Warnf(c.logger, "plugin %s for %q: the local TLS handshake failed: %v", proxy.Plugin, proxy.Name, err)
		_ = tlsConn.Close()
		_ = local.Close()
		return
	}
	_ = local.SetDeadline(time.Time{})
	flynet.Pipe(localTLS, tlsConn, time.Duration(c.cfg.Client.IdleTimeoutSecs)*time.Second)
}

// serveSocks5 runs the SOCKS5 server the socks5 plugin promises: the visitor's
// SOCKS5 bytes arrive through the tunnel, this end answers the handshake —
// with username/password when plugin_user and plugin_password say so — dials
// the named target through allow_targets and relays. UDP ASSOCIATE is refused:
// the tunneled form has no UDP relay to offer.
func (c *client) serveSocks5(server net.Conn, proxy config.ProxyConfig) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	target, err := socks.ServeConn(server, socks.ServerOptions{
		Username:  proxy.PluginUser,
		Password:  proxy.PluginPassword,
		Handshake: dialTimeout,
		Dial: func(target string) (net.Conn, error) {
			return c.proxyTargetDial(proxy, target)
		},
	})
	if err != nil {
		logging.Warnf(c.logger, "plugin socks5 for %q: the visitor's session failed: %v", proxy.Name, err)
		return
	}
	defer target.Close()
	flynet.Pipe(target, server, time.Duration(c.cfg.Client.IdleTimeoutSecs)*time.Second)
}

// https2HTTPConfig loads and caches the certificate for one proxy.
func (c *client) https2HTTPConfig(proxy config.ProxyConfig) (*tls.Config, error) {
	c.pluginTLSMu.Lock()
	defer c.pluginTLSMu.Unlock()
	if c.pluginTLS == nil {
		c.pluginTLS = map[string]*tls.Config{}
	}
	if cached, ok := c.pluginTLS[proxy.Name]; ok {
		return cached, nil
	}
	cert, err := tls.LoadX509KeyPair(proxy.PluginCertFile, proxy.PluginKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the certificate: %w", err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	c.pluginTLS[proxy.Name] = config
	return config, nil
}

func (l *pluginListener) Close() error {
	l.done.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.conns)
		l.mu.Unlock()
	})
	return nil
}

// feed hands a connection to the plugin server's accept loop. It reports
// false when the listener is closed (or its buffer is full): a reload that
// shut this server down must not turn into a send on a closed channel.
func (l *pluginListener) feed(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	select {
	case l.conns <- conn:
		return true
	default:
		return false
	}
}

func (l *pluginListener) Addr() net.Addr { return l.addr }

type pluginAddr struct{}

func (pluginAddr) Network() string { return "plugin" }
func (pluginAddr) String() string  { return "in-process" }

// staticFileServer is the lazily built HTTP server behind a static_file
// plugin. It lives for the client's lifetime: the listener is cheap, and the
// file server reads the directory at request time, so files added later
// appear without a reload.
type staticFileServer struct {
	listener *pluginListener
	server   *http.Server
}

// staticFileServerFor returns (building on first use) the server behind one
// static_file proxy, keyed by proxy name.
// forgetStaticFile drops a cached plugin server, so a reload that replaces the
// proxy rebuilds it from the new settings.
func (c *client) forgetStaticFile(name string) {
	c.staticFilesMu.Lock()
	entry, ok := c.staticFiles[name]
	if ok {
		delete(c.staticFiles, name)
	}
	c.staticFilesMu.Unlock()
	if ok {
		// Stop the old in-process server: its Serve goroutine blocks on the
		// in-memory listener forever, so without a shutdown every reload that
		// replaces or removes the proxy would abandon one goroutine and its
		// old handler for the life of the process. The shutdown runs outside
		// the mutex because it waits for handlers to finish.
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = entry.server.Shutdown(shutdown)
		_ = entry.listener.Close()
	}
}

// stopPluginServers shuts down every cached in-process plugin server. Each one is
// an *http.Server on an in-memory listener, and an http.Server does not watch the
// client's context: a Run that returns has to stop them, or an embedding process
// keeps the servers, their handlers and their goroutines for the rest of its life.
// A reload stops the entries it replaces through forgetStaticFile; this is the rest.
func (c *client) stopPluginServers() {
	c.staticFilesMu.Lock()
	// Set before the snapshot: a stream that reaches a helper from here on has to
	// be refused rather than given a server the snapshot below cannot include.
	c.pluginsStopped = true
	names := make([]string, 0, len(c.staticFiles))
	for name := range c.staticFiles {
		names = append(names, name)
	}
	c.staticFilesMu.Unlock()
	for _, name := range names {
		c.forgetStaticFile(name)
	}
}

func (c *client) staticFileServerFor(proxy config.ProxyConfig) (*pluginListener, error) {
	c.staticFilesMu.Lock()
	defer c.staticFilesMu.Unlock()
	if c.pluginsStopped {
		return nil, errPluginServersStopped
	}
	if c.staticFiles == nil {
		c.staticFiles = map[string]*staticFileServer{}
	}
	if entry, ok := c.staticFiles[proxy.Name]; ok {
		return entry.listener, nil
	}

	listener := &pluginListener{conns: make(chan net.Conn, 16), addr: pluginAddr{}}
	fileServer := http.FileServer(http.Dir(proxy.PluginLocalPath))
	var handler http.Handler = fileServer
	user, password := proxy.PluginHTTPUser, proxy.PluginHTTPPassword
	if user != "" || password != "" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, pass, ok := r.BasicAuth()
			userOK := subtle.ConstantTimeCompare([]byte(name), []byte(user)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(password)) == 1
			if !ok || !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="aethertunnel static_file"`)
				http.Error(w, "401 unauthorized", http.StatusUnauthorized)
				return
			}
			fileServer.ServeHTTP(w, r)
		})
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = server.Serve(listener) }()

	entry := &staticFileServer{listener: listener, server: server}
	c.staticFiles[proxy.Name] = entry
	return listener, nil
}

// dialForPlugin opens the in-process connection a plugin-backed proxy carries.
func (c *client) dialForPlugin(proxy config.ProxyConfig) (net.Conn, error) {
	switch proxy.Plugin {
	case config.PluginStaticFile:
		listener, err := c.staticFileServerFor(proxy)
		if err != nil {
			return nil, err
		}
		client, server := net.Pipe()
		if !listener.feed(server) {
			return nil, errors.New("the plugin server is not accepting")
		}
		return client, nil
	case config.PluginUnixSocket:
		// Bounded like every other dial: connect(2) on an AF_UNIX stream socket
		// blocks while the listener's backlog is full, and a stalled service left
		// the stream goroutine parked with no answer and no failure.
		return net.DialTimeout("unix", proxy.PluginLocalPath, time.Duration(c.cfg.Client.DialTimeoutSecs)*time.Second)
	case config.PluginHTTPS2HTTP, config.PluginTLS2Raw:
		client, server := net.Pipe()
		go c.serveHTTPS2HTTP(server, proxy)
		return client, nil
	case config.PluginHTTPS2HTTPS:
		client, server := net.Pipe()
		go c.serveHTTPS2HTTPS(server, proxy)
		return client, nil
	case config.PluginSocks5:
		client, server := net.Pipe()
		go c.serveSocks5(server, proxy)
		return client, nil
	case config.PluginHTTPProxy:
		listener, err := c.httpBridgeFor(proxy, c.httpProxyHandler(proxy))
		if err != nil {
			return nil, err
		}
		return c.feedPlugin(listener)
	case config.PluginHTTP2HTTPS:
		listener, err := c.httpBridgeFor(proxy, c.http2HTTPSHandler(proxy))
		if err != nil {
			return nil, err
		}
		return c.feedPlugin(listener)
	default:
		return nil, errors.New("unsupported plugin")
	}
}
