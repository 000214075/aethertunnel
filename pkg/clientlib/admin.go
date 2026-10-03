package clientlib

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The admin API mirrors frp's client webServer: a small HTTP listener on the
// machine the client runs on that answers the operator's questions and applies
// a reload. /healthz is open so a process supervisor can probe it without
// credentials; the /api routes require basic auth when [client.admin] carries
// user and password.

// startAdminServer brings the management API up when the configuration asks
// for it. A listener that cannot bind is a configuration error the operator
// asked for by name, so it fails the client instead of vanishing into a log
// line.
func (c *client) startAdminServer() error {
	admin := c.cfg.Client.Admin
	if admin == nil || !admin.Enabled {
		return nil
	}
	addr := net.JoinHostPort(admin.BindAddr, strconv.Itoa(admin.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin api: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", c.adminHealthz)
	mux.HandleFunc("/api/status", c.adminAuth(c.adminStatus))
	mux.HandleFunc("/api/reload", c.adminAuth(c.adminReload))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-c.baseCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		defer listener.Close()
		c.logger.Printf("admin api on http://%s (/healthz /api/status /api/reload)", listener.Addr())
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			c.logger.Printf("admin api stopped: %v", err)
		}
	}()
	return nil
}

// adminAuth gates a handler behind basic auth when credentials are set.
func (c *client) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	admin := c.cfg.Client.Admin
	return func(w http.ResponseWriter, r *http.Request) {
		if admin.User == "" {
			next(w, r)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(admin.User)) != 1 ||
			subtle.ConstantTimeCompare([]byte(password), []byte(admin.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="aethertunnel admin"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (c *client) adminHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

// adminProxyStatus is one configured tunnel in /api/status. Confirmed reports
// that the server's latest proxy list named this proxy, so its endpoint is
// live on the current session.
type adminProxyStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Local      string `json:"local"`
	RemotePort int    `json:"remote_port"`
	Plugin     string `json:"plugin,omitempty"`
	Confirmed  bool   `json:"confirmed"`
	Healthy    bool   `json:"healthy"`
	Withdrawn  bool   `json:"withdrawn,omitempty"`
}

type adminVisitorStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	ServerName string `json:"server_name"`
	Listen     string `json:"listen"`
}

type adminStatusReport struct {
	Version   string               `json:"version"`
	Protocol  int                  `json:"protocol"`
	Session   string               `json:"session,omitempty"`
	Server    string               `json:"server"`
	Connected bool                 `json:"connected"`
	Proxies   []adminProxyStatus   `json:"proxies"`
	Visitors  []adminVisitorStatus `json:"visitors"`
}

func (c *client) adminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	confirmed := map[string]bool{}
	c.mu.Lock()
	session := c.session
	for _, name := range c.registeredNames {
		confirmed[name] = true
	}
	c.mu.Unlock()

	report := adminStatusReport{
		Version:   Version,
		Protocol:  protocol.ProtocolVersion,
		Session:   session,
		Server:    c.serverAddr(),
		Connected: session != "",
		Proxies:   []adminProxyStatus{},
		Visitors:  []adminVisitorStatus{},
	}
	for _, proxy := range c.proxyList() {
		status := adminProxyStatus{
			Name:       proxy.Name,
			Type:       proxy.Type,
			Local:      proxy.LocalAddr(),
			RemotePort: proxy.RemotePort,
			Plugin:     proxy.Plugin,
			Confirmed:  confirmed[proxy.Name],
			Healthy:    true,
		}
		if state := c.healthStateFor(proxy.Name); state != nil {
			status.Healthy = state.isHealthy()
			status.Withdrawn = state.isWithdrawn()
		}
		report.Proxies = append(report.Proxies, status)
	}
	for _, visitor := range c.visitorList() {
		report.Visitors = append(report.Visitors, adminVisitorStatus{
			Name:       visitor.Name,
			Type:       visitor.Type,
			ServerName: visitor.ServerName,
			Listen:     visitor.ListenAddr(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// adminReload re-reads the configuration file, the same path a SIGHUP takes.
// frp answers GET on /api/reload; this accepts GET and POST so either habit
// works.
func (c *client) adminReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if c.cfg.SourceFile == "" {
		writeAdminError(w, http.StatusBadRequest, "no configuration file to reload: the client was started from a string, so only SIGHUP-style reloads of a real file apply")
		return
	}
	c.reloadFromFile(c.cfg.SourceFile)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
}

func writeAdminError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
