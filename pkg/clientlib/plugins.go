package clientlib

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
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
	default:
		return nil, errors.New("unsupported plugin")
	}
}
