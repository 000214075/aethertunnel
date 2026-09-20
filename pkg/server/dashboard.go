package server

import (
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/web"
)

// Dashboard serves the embedded web UI and the JSON API it polls.
//
// Every number it reports is read from live server state. The endpoints are:
//
//	GET    /api/health          always public
//	GET    /api/status          version, uptime, counters, encryption state
//	GET    /api/clients         connected clients
//	GET    /api/proxies         published tunnels
//	GET    /api/config          redacted configuration summary
//	DELETE /api/clients/{id}    disconnect one client
//
// When [dashboard].token is set, everything except /api/health requires
// "Authorization: Bearer <token>".
type Dashboard struct {
	server *Server
	cfg    *config.Config
	logger *log.Logger

	mux        *http.ServeMux
	httpServer *http.Server
	listener   net.Listener
	startedAt  time.Time
}

// NewDashboard prepares the dashboard routes.
func NewDashboard(srv *Server, logger *log.Logger) (*Dashboard, error) {
	static, err := fs.Sub(web.FS, web.Dashboard)
	if err != nil {
		return nil, err
	}

	d := &Dashboard{
		server:    srv,
		cfg:       srv.cfg,
		logger:    logger,
		mux:       http.NewServeMux(),
		startedAt: time.Now(),
	}

	d.mux.Handle("/", http.FileServer(http.FS(static)))
	d.handle("GET /api/health", d.withAuth(false, d.apiHealth))
	d.handle("GET /api/status", d.withAuth(true, d.apiStatus))
	d.handle("GET /api/clients", d.withAuth(true, d.apiClients))
	d.handle("GET /api/proxies", d.withAuth(true, d.apiProxies))
	d.handle("GET /api/config", d.withAuth(true, d.apiConfig))
	d.handle("DELETE /api/clients/", d.withAuth(true, d.apiDisconnect))
	// Health probes are always public and cheap: orchestrators poll them often.
	d.handle("GET /healthz", d.withAuth(false, d.healthz))
	d.handle("GET /readyz", d.withAuth(false, d.readyz))
	if srv.cfg.Metrics.Enabled {
		d.handle("GET /metrics", d.withMetricsAuth(d.metrics))
	}
	return d, nil
}

// withMetricsAuth protects /metrics. When [metrics].token is set, either that token
// or the dashboard token is accepted: a Prometheus scraper should not need the
// dashboard credential, and an operator who already holds the dashboard token
// should not need a second one. With no metrics token, the dashboard rule applies.
func (d *Dashboard) withMetricsAuth(next http.HandlerFunc) http.HandlerFunc {
	metricsToken := d.cfg.Metrics.Token
	dashboardToken := d.cfg.Dashboard.Token

	if metricsToken == "" {
		return d.withAuth(true, next)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		presented := strings.TrimPrefix(header, "Bearer ")
		if presented == header ||
			(!crypto.EqualTokens(presented, metricsToken) && !crypto.EqualTokens(presented, dashboardToken)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// healthz reports that the process is running and serving.
func (d *Dashboard) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        d.server.version,
		"protocol":       protocol.ProtocolVersion,
		"uptime_seconds": int64(time.Since(d.server.startedAt).Seconds()),
	})
}

// readyz reports that the server is listening and accepting control connections.
func (d *Dashboard) readyz(w http.ResponseWriter, r *http.Request) {
	ready := d.server.listener != nil && !d.server.closing.Load()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":        ready,
		"listening":    d.server.listener != nil,
		"shuttingDown": d.server.closing.Load(),
	})
}

// metrics serves the Prometheus text exposition format.
func (d *Dashboard) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, d.server.metrics.Render())
}

// handle registers a pattern exactly as written, reusing the path from the
// pattern string (Go 1.22 method patterns are matched by ServeMux directly).
func (d *Dashboard) handle(pattern string, handler http.HandlerFunc) {
	d.mux.HandleFunc(pattern, handler)
}

// Start binds the dashboard port and serves in the background.
func (d *Dashboard) Start() error {
	listener, err := net.Listen("tcp", d.cfg.DashboardAddr())
	if err != nil {
		return err
	}
	d.listener = listener
	d.httpServer = &http.Server{
		Handler:           d.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	auth := "disabled"
	if d.cfg.Dashboard.Token != "" {
		auth = "required"
	}
	d.logger.Printf("dashboard on http://%s (auth %s)", listener.Addr(), auth)

	go func() {
		if err := d.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			d.logger.Printf("dashboard stopped: %v", err)
		}
	}()
	return nil
}

// Stop shuts the dashboard down.
func (d *Dashboard) Stop() {
	if d.httpServer != nil {
		_ = d.httpServer.Close()
	}
}

// --- middleware ---------------------------------------------------------------

func (d *Dashboard) withAuth(required bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if required && d.cfg.Dashboard.Token != "" {
			header := r.Header.Get("Authorization")
			token := strings.TrimPrefix(header, "Bearer ")
			if token == header || !crypto.EqualTokens(token, d.cfg.Dashboard.Token) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// --- handlers -----------------------------------------------------------------

func (d *Dashboard) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        d.server.version,
		"protocol":       protocol.ProtocolVersion,
		"uptime_seconds": int64(time.Since(d.server.startedAt).Seconds()),
	})
}

func (d *Dashboard) apiStatus(w http.ResponseWriter, r *http.Request) {
	in, out := d.server.totalBytes()
	activeStreams := int64(0)
	registered := 0
	for _, tunnel := range d.server.tunnels.List() {
		activeStreams += tunnel.Active.Load()
		registered++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":        d.server.version,
		"build_time":     d.server.buildTime,
		"git_commit":     d.server.gitCommit,
		"protocol":       protocol.ProtocolVersion,
		"uptime_seconds": int64(time.Since(d.server.startedAt).Seconds()),
		"started_at":     d.server.startedAt.UTC().Format(time.RFC3339),
		"encryption":     d.server.Cipher(),
		"connections": map[string]any{
			"active": d.server.sessions.Count(),
			"total":  d.server.totalConnections.Load(),
			"max":    d.cfg.Server.MaxConnections,
		},
		"proxies": map[string]any{
			"registered":     registered,
			"active_streams": activeStreams,
		},
		"traffic": map[string]any{
			"bytes_in":  in,
			"bytes_out": out,
		},
		"auth_required": d.cfg.Dashboard.Token != "",
	})
}

func (d *Dashboard) apiClients(w http.ResponseWriter, r *http.Request) {
	sessions := d.server.sessions.List()
	clients := make([]map[string]any, 0, len(sessions))

	for _, s := range sessions {
		proxies := s.Proxies()
		if proxies == nil {
			proxies = []string{}
		}
		clients = append(clients, map[string]any{
			"id":                s.ID,
			"remote_addr":       s.RemoteAddr,
			"client_version":    s.ClientVersion,
			"connected_at":      s.ConnectedAt.UTC().Format(time.RFC3339),
			"last_heartbeat":    s.LastHeartbeat().UTC().Format(time.RFC3339),
			"heartbeat_seconds": s.HeartbeatSeconds,
			"encrypted":         s.Encrypted,
			"proxies":           proxies,
			"active_streams":    s.activeStreams.Load(),
			"total_streams":     s.totalStreams.Load(),
			"bytes_in":          s.bytesFromClient.Load(),
			"bytes_out":         s.bytesToClient.Load(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

func (d *Dashboard) apiProxies(w http.ResponseWriter, r *http.Request) {
	tunnels := d.server.tunnels.List()
	proxies := make([]map[string]any, 0, len(tunnels))

	for _, t := range tunnels {
		proxies = append(proxies, map[string]any{
			"name":               t.Name,
			"type":               t.Spec.Type,
			"local_addr":         t.Spec.LocalAddr,
			"remote_port":        t.RemotePort,
			"client_id":          t.Session.ID,
			"active_connections": t.Active.Load(),
			"total_connections":  t.Total.Load(),
			"bytes_in":           t.BytesIn.Load(),
			"bytes_out":          t.BytesOut.Load(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"proxies": proxies})
}

// apiConfig reports the effective settings that are safe to show. It never
// returns the auth token, the dashboard token or the encryption passphrase.
func (d *Dashboard) apiConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server": map[string]any{
			"bind_addr":            d.cfg.Server.BindAddr,
			"bind_port":            d.cfg.Server.BindPort,
			"max_connections":      d.cfg.Server.MaxConnections,
			"heartbeat_seconds":    d.cfg.Server.HeartbeatSeconds,
			"idle_timeout_seconds": d.cfg.Server.ReadTimeoutSecs,
		},
		"encryption": map[string]any{
			"enabled":   d.cfg.Encryption.Enabled,
			"algorithm": d.server.Cipher(),
		},
		"dashboard": map[string]any{
			"bind_addr":     d.cfg.Dashboard.BindAddr,
			"port":          d.cfg.Dashboard.Port,
			"auth_required": d.cfg.Dashboard.Token != "",
		},
		"proxies_configured": len(d.cfg.Proxies),
	})
}

// apiDisconnect closes one client. Closing the session also stops its tunnels.
func (d *Dashboard) apiDisconnect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/clients/")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client id is required"})
		return
	}
	session, ok := d.server.sessions.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such client"})
		return
	}
	d.logger.Printf("dashboard: disconnecting client %s on request", id)
	go session.Close("disconnected from the dashboard")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
