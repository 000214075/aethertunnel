package clientlib

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	bind := admin.BindAddr
	if bind == "" {
		// config.Load defaults this to loopback; Run accepts a configuration built
		// in code, where an empty address would bind every interface.
		bind = "127.0.0.1"
	}
	addr := net.JoinHostPort(bind, strconv.Itoa(admin.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin api: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", c.adminHealthz)
	mux.HandleFunc("/api/status", c.adminAuth(c.adminStatus))
	mux.HandleFunc("/api/reload", c.adminAuth(c.adminReload))
	// The store's endpoints follow frp's client API: one entry per name, read,
	// created or replaced, and deleted. They answer 404 when [store].path is
	// unset, so a deployment that has not asked for runtime entries cannot have
	// them by accident.
	mux.HandleFunc("GET /api/store", c.adminAuth(c.adminStoreList))
	mux.HandleFunc("PUT /api/store/proxy/{name}", c.adminAuth(c.adminStorePutProxy))
	mux.HandleFunc("GET /api/store/proxy/{name}", c.adminAuth(c.adminStoreGetProxy))
	mux.HandleFunc("DELETE /api/store/proxy/{name}", c.adminAuth(c.adminStoreDeleteProxy))
	mux.HandleFunc("PUT /api/store/visitor/{name}", c.adminAuth(c.adminStorePutVisitor))
	mux.HandleFunc("GET /api/store/visitor/{name}", c.adminAuth(c.adminStoreGetVisitor))
	mux.HandleFunc("DELETE /api/store/visitor/{name}", c.adminAuth(c.adminStoreDeleteVisitor))
	// The dashboard's server bounds the header, the body and the answer; this one
	// bounded only the header, so a client that opened a connection and then sent
	// its body a byte at a time held it (and a goroutine) forever.
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	c.adminServer = server

	go func() {
		// The service lifetime, not baseCtx: the embedder's context stays alive
		// after Run returns, and stopStartupServices cancels the service context so
		// this goroutine does not outlive the Run that started it.
		<-c.serviceLifetime().Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		defer listener.Close()
		endpoints := "/healthz /api/status /api/reload"
		if c.store != nil {
			endpoints += " /api/store"
		}
		c.logger.Printf("admin api on http://%s (%s)", listener.Addr(), endpoints)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			c.logger.Printf("admin api stopped: %v", err)
		}
	}()
	return nil
}

// stopAdminServer shuts the management API down. It is what a failed Run uses
// to undo startAdminServer, whose goroutines otherwise wait on the embedder's
// context — which stays alive after Run has returned an error.
func (c *client) stopAdminServer() {
	if c.adminServer == nil {
		return
	}
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.adminServer.Shutdown(shutdown)
	c.adminServer = nil
}

// adminAuth gates a handler behind basic auth when credentials are set.
func (c *client) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	admin := c.cfg.Client.Admin
	return func(w http.ResponseWriter, r *http.Request) {
		if admin.User == "" {
			if admin.Password != "" {
				// The mirror of the case below. Validate refuses the pairing for a
				// file-loaded configuration, but Run accepts a programmatic one as it
				// is, and serving here would expose the whole management API — the
				// store, the reload — without ever reading the header.
				writeAdminError(w, http.StatusUnauthorized, "the admin API is misconfigured: client.admin.password is set without client.admin.user")
				return
			}
			next(w, r)
			return
		}
		if admin.Password == "" {
			// Validate enforces the pairing on file-loaded configs, but Run
			// accepts programmatic ones as they are: an empty password with
			// a user set would authenticate the empty string, so refuse
			// everything instead of serving a passwordless API.
			writeAdminError(w, http.StatusUnauthorized, "the admin API is misconfigured: client.admin.user is set without client.admin.password")
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
	if err := c.reloadFromFile(c.cfg.SourceFile); err != nil {
		writeAdminError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
}

func writeAdminError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// adminStoreReport is GET /api/store: whether the store is on, where it lives
// and every entry it holds, as the bodies the operator supplied.
type adminStoreReport struct {
	Enabled  bool                       `json:"enabled"`
	Path     string                     `json:"path,omitempty"`
	Proxies  map[string]json.RawMessage `json:"proxies"`
	Visitors map[string]json.RawMessage `json:"visitors"`
	Note     string                     `json:"note,omitempty"`
}

// requireStore answers 404 when [store].path is unset. The endpoints exist in
// that case only to say why they cannot do anything, which is the difference
// between "this release has no such feature" and "this deployment turned it off".
func (c *client) requireStore(w http.ResponseWriter) bool {
	if c.store != nil {
		return true
	}
	writeAdminError(w, http.StatusNotFound,
		"the store is disabled: set [store].path in the client configuration to create tunnels at runtime")
	return false
}

func (c *client) adminStoreList(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	bodies := c.store.bodiesFor()
	report := adminStoreReport{Enabled: true, Path: c.cfg.Store.Path,
		Proxies: bodies["proxies"], Visitors: bodies["visitors"]}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// maxAdminBody bounds a store entry. One byte over is read so an oversized body
// can be told apart from one that happens to be exactly the limit: the callers
// used to read through a LimitReader, which cut the body off silently and left
// the JSON decoder to report a syntax error somewhere in the middle.
const maxAdminBody = 1 << 20

// adminStoreBody reads the request body, which is the entry in JSON form.
func (c *client) adminStoreBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBody+1))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("cannot read the body: %v", err))
		return nil, false
	}
	if len(body) > maxAdminBody {
		writeAdminError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the body is larger than %d bytes", maxAdminBody))
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeAdminError(w, http.StatusBadRequest, "the body is empty: send the entry as a JSON object")
		return nil, false
	}
	return body, true
}

// storeResult is what a successful write answers with: the entries the body
// defined, after the configuration decoder has filled in the defaults, so the
// caller sees what the client will actually publish.
func (c *client) adminStorePutProxy(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	body, ok := c.adminStoreBody(w, r)
	if !ok {
		return
	}
	proxies, err := c.store.putProxy(name, body)
	if err != nil {
		if errors.Is(err, errStoreWrite) {
			writeAdminError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	c.refreshDispatch()
	c.logger.Printf("store: proxy entry %q written to %s and applied", name, c.cfg.Store.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": "ok", "proxies": proxies})
}

func (c *client) adminStoreGetProxy(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	body, ok := c.store.bodiesFor()["proxies"][name]
	if !ok {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("no stored proxy entry named %q", name))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (c *client) adminStoreDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	existed, err := c.store.deleteProxy(name)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("no stored proxy entry named %q", name))
		return
	}
	c.refreshDispatch()
	c.logger.Printf("store: proxy entry %q removed from %s and withdrawn", name, c.cfg.Store.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
}

func (c *client) adminStorePutVisitor(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	body, ok := c.adminStoreBody(w, r)
	if !ok {
		return
	}
	visitors, err := c.store.putVisitor(name, body)
	if err != nil {
		if errors.Is(err, errStoreWrite) {
			writeAdminError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	c.refreshDispatch()
	c.logger.Printf("store: visitor entry %q written to %s and applied", name, c.cfg.Store.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": "ok", "visitors": visitors})
}

func (c *client) adminStoreGetVisitor(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	body, ok := c.store.bodiesFor()["visitors"][name]
	if !ok {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("no stored visitor entry named %q", name))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (c *client) adminStoreDeleteVisitor(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	name := r.PathValue("name")
	existed, err := c.store.deleteVisitor(name)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("no stored visitor entry named %q", name))
		return
	}
	c.refreshDispatch()
	c.logger.Printf("store: visitor entry %q removed from %s and stopped", name, c.cfg.Store.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
}
