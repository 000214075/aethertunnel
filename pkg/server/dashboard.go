package server

import (
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
//	GET    /api/ledger          signed bandwidth ledger and its public key
//	GET    /api/dht             DHT node identity and the proxies it announces
//	GET    /api/vpn             layer-3 tunnel state and packet counters
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
	startedAt  time.Time

	// listenerMu guards the bound listener, which Start writes while the rest of the
	// process (the tests, and anything that wants the address) may read it.
	listenerMu sync.Mutex
	listener   net.Listener
}

// Listener returns the bound listener, or nil before Start has bound one.
func (d *Dashboard) Listener() net.Listener {
	d.listenerMu.Lock()
	defer d.listenerMu.Unlock()
	return d.listener
}

// setListener records the bound listener.
func (d *Dashboard) setListener(listener net.Listener) {
	d.listenerMu.Lock()
	defer d.listenerMu.Unlock()
	d.listener = listener
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
	d.handle("GET /api/ledger", d.withAuth(true, d.apiLedger))
	d.handle("GET /api/dht", d.withAuth(true, d.apiDHT))
	d.handle("GET /api/vpn", d.withAuth(true, d.apiVPN))
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
	ready := d.server.Listener() != nil && !d.server.closing.Load()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":        ready,
		"listening":    d.server.Listener() != nil,
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
	d.setListener(listener)
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
	for _, group := range d.server.tunnels.Groups() {
		active, _, _, _ := group.Totals()
		activeStreams += active
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
	groups := d.server.tunnels.Groups()
	proxies := make([]map[string]any, 0, len(groups))

	for _, group := range groups {
		active, total, in, out := group.Totals()
		members := group.Summary()

		row := map[string]any{
			"name":               group.Name,
			"type":               group.Type,
			"remote_port":        group.RemotePort,
			"domains":            group.Domains,
			"group":              group.Group,
			"load_balance":       group.strategy,
			"multipath":          group.Multipath,
			"addr":               group.Addr(),
			"member_count":       len(members),
			"members":            members,
			"active_connections": active,
			"total_connections":  total,
			"bytes_in":           in,
			"bytes_out":          out,
		}

		// The single-member fields keep the original shape of the response so an
		// existing client of this API does not have to know about pools.
		if len(members) > 0 {
			row["local_addr"] = members[0].LocalAddr
			row["client_id"] = members[0].ClientID
		}
		proxies = append(proxies, row)
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
		"ledger": map[string]any{
			"enabled": d.cfg.Ledger.Enabled,
			"path":    d.cfg.Ledger.Path,
		},
		"dht":                d.server.directory.summary(),
		"vpn":                d.server.vpn.summary(),
		"proxies_configured": len(d.cfg.Proxies),
	})
}

// apiVPN reports the layer-3 tunnel: the interface it runs on, the subnet and
// pool the server hands addresses from, and the packet counters. The interface
// lives inside this process, so this endpoint is the only place an operator can
// see whether the tunnel is up and carrying packets.
func (d *Dashboard) apiVPN(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.server.vpn.summary())
}

// apiLedger publishes the bandwidth ledger: the signing public key an auditor
// needs, the chain head, and the most recent entries. The entries are already
// signed, so a reader can verify the response without any further access to the
// server. ?limit= caps how many of the most recent entries are returned.
func (d *Dashboard) apiLedger(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a non-negative integer"})
			return
		}
		if parsed > 5000 {
			parsed = 5000
		}
		limit = parsed
	}
	writeJSON(w, http.StatusOK, d.server.renderLedger(limit))
}

// apiDHT reports the DHT node this server runs and the proxies it announces, so an
// operator can see whether a proxy is findable by name.
func (d *Dashboard) apiDHT(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.server.directory.summary())
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
	// Who asked is only known to the extent the request carried a token, but that
	// a client was disconnected from the dashboard is an administrative action and
	// belongs in the audit log next to everything else that happened to it.
	d.server.auditor.Record(AuditEvent{
		Event: EventDashboardAction, ClientID: id, Remote: r.RemoteAddr,
		Outcome: "ok", Detail: "disconnect requested through DELETE /api/clients/{id}",
	})
	go session.Close("disconnected from the dashboard")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
