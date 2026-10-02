package clientlib

import (
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
)

// pluginListener is a net.Listener that hands out connections the proxy data
// path creates for it: dialForProxy makes a net.Pipe pair, feeds one end here,
// and returns the other to the stream — which then carries HTTP into the
// in-process server exactly as it would carry it to a local service.
type pluginListener struct {
	conns chan net.Conn
	done  sync.Once
	addr  pluginAddr
}

func (l *pluginListener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		return nil, errors.New("the plugin listener is closed")
	}
	return conn, nil
}

// serveHTTPS2HTTP terminates the visitor's TLS on the tunnel end with the
// configured certificate and relays the plaintext to the plain HTTP service
// named by local_port. The certificate loads once per proxy and is cached for
// the client's lifetime, so a connection costs no file reads.
func (c *client) serveHTTPS2HTTP(server net.Conn, proxy config.ProxyConfig) {
	tlsConfig, err := c.https2HTTPConfig(proxy)
	if err != nil {
		c.logger.Printf("plugin https2http for %q: %v", proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Now().Add(10 * time.Second))
	tlsConn := tls.Server(server, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		// A plain-HTTP visitor on a TLS endpoint ends here; that is the
		// visitor's mistake to see, not a tunnel fault.
		c.logger.Printf("plugin https2http for %q: the visitor's TLS handshake failed: %v", proxy.Name, err)
		_ = server.Close()
		return
	}
	_ = server.SetDeadline(time.Time{})
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	local, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
	if err != nil {
		c.logger.Printf("plugin https2http for %q: cannot reach the local service %s: %v",
			proxy.Name, proxy.LocalAddr(), err)
		_ = tlsConn.Close()
		return
	}
	flynet.Pipe(local, tlsConn, time.Duration(c.cfg.Client.IdleTimeoutSecs)*time.Second)
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
	l.done.Do(func() { close(l.conns) })
	return nil
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
	defer c.staticFilesMu.Unlock()
	delete(c.staticFiles, name)
}

func (c *client) staticFileServerFor(proxy config.ProxyConfig) *pluginListener {
	c.staticFilesMu.Lock()
	defer c.staticFilesMu.Unlock()
	if c.staticFiles == nil {
		c.staticFiles = map[string]*staticFileServer{}
	}
	if entry, ok := c.staticFiles[proxy.Name]; ok {
		return entry.listener
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
	return listener
}

// dialForPlugin opens the in-process connection a plugin-backed proxy carries.
func (c *client) dialForPlugin(proxy config.ProxyConfig) (net.Conn, error) {
	switch proxy.Plugin {
	case config.PluginStaticFile:
		listener := c.staticFileServerFor(proxy)
		client, server := net.Pipe()
		select {
		case listener.conns <- server:
			return client, nil
		default:
			return nil, errors.New("the plugin server is not accepting")
		}
	case config.PluginUnixSocket:
		return net.Dial("unix", proxy.PluginLocalPath)
	case config.PluginHTTPS2HTTP:
		client, server := net.Pipe()
		go c.serveHTTPS2HTTP(server, proxy)
		return client, nil
	default:
		return nil, errors.New("unsupported plugin")
	}
}
