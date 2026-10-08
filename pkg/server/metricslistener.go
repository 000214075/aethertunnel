package server

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// MetricsListener serves GET /metrics on its own address — frp's metrics.addr — so
// a Prometheus scraper reaches the counters without reaching the dashboard, and so
// a deployment that keeps [dashboard].enabled false can still be scraped. The
// credential rule is the dashboard's: [metrics].token when it is set (the dashboard
// token is accepted too, so one credential is enough), and no credential at all
// when neither is set, which is why the default address is the loopback one.
type MetricsListener struct {
	cfg    *config.Config
	srv    *Server
	logger *log.Logger

	// mu guards the two lifecycle fields: Stop reads them while Start writes
	// them, and Addr reads the listener besides.
	mu     sync.Mutex
	server *http.Server
	ln     net.Listener
}

// NewMetricsListener builds the listener. It does not bind anything until Start.
func NewMetricsListener(srv *Server, logger *log.Logger) *MetricsListener {
	return &MetricsListener{cfg: srv.cfg, srv: srv, logger: logger}
}

// Start binds the configured address and begins serving in the background.
func (m *MetricsListener) Start() error {
	ln, err := net.Listen("tcp", m.cfg.MetricsAddr())
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ln = ln
	m.mu.Unlock()
	// The same timeouts the dashboard's server carries. Without an idle cap a
	// keep-alive connection that goes quiet after one response pins its
	// goroutine and file descriptor forever: ReadHeaderTimeout only bounds the
	// first request, and a zero IdleTimeout falls back to a zero ReadTimeout.
	server := &http.Server{
		Handler:           http.HandlerFunc(m.handle),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	m.mu.Lock()
	m.server = server
	m.mu.Unlock()
	// The goroutine serves its own copy of the pointer: Stop nils the field
	// to make a second Stop a no-op, and a goroutine that read the field on
	// its way into Serve could see that nil and dereference it.
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			m.logger.Printf("metrics listener stopped: %v", err)
		}
	}()
	m.logger.Printf("metrics on http://%s/metrics%s", ln.Addr(), metricsAuthHint(m.cfg))
	return nil
}

// Stop closes the listener. It is safe to call on one that never started.
func (m *MetricsListener) Stop() {
	m.mu.Lock()
	server := m.server
	ln := m.ln
	m.server = nil
	m.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
	// Serve may never have registered the listener with the server — that is
	// the window between Start's two lock sections, and it is why m.server can
	// be nil here — so the listener has to be closed whatever the server field
	// says. Returning on a nil server skipped this and left the port bound for
	// the life of the process, on the one path that has no server to close it.
	if ln != nil {
		_ = ln.Close()
	}
}

// Addr is the address the listener bound, or nil before Start.
func (m *MetricsListener) Addr() net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return nil
	}
	return m.ln.Addr()
}

// handle answers the one endpoint this listener exists for. Everything else is a
// 404 rather than a redirect to the dashboard: the two listeners are separate
// surfaces on purpose.
func (m *MetricsListener) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !metricsCredentialAccepted(r, m.cfg) {
		m.srv.metrics.dashboardRefused.Add(1)
		w.Header().Set("WWW-Authenticate", `Bearer realm="aethertunnel metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(m.srv.metrics.Render()))
}

// metricsCredentialAccepted reports whether a request may read /metrics. With
// [metrics].token set, either that token or the dashboard token is accepted: a
// scraper should not need the dashboard credential, and an operator who already
// holds the dashboard token should not need a second one. With neither token set
// the endpoint needs no authentication at all, whatever header a scraper sends;
// with only the dashboard token set, that token is what it checks.
func metricsCredentialAccepted(r *http.Request, cfg *config.Config) bool {
	if cfg.Metrics.Token == "" && cfg.Dashboard.Token == "" {
		// Nothing is configured to check, so no credential is required: a scraper
		// that carries a stale bearer token is admitted like one that carries none.
		// Refusing the stale one left the endpoint reachable without a header and
		// unreachable with a placeholder, which is not what "no authentication" means.
		return true
	}
	header := r.Header.Get("Authorization")
	presented := strings.TrimPrefix(header, "Bearer ")
	if presented == header {
		// The header is missing or is not a bearer token, and a token is required.
		return false
	}
	if cfg.Metrics.Token != "" && crypto.EqualTokens(presented, cfg.Metrics.Token) {
		return true
	}
	if cfg.Dashboard.Token != "" && crypto.EqualTokens(presented, cfg.Dashboard.Token) {
		return true
	}
	return false
}

// metricsAuthHint says what a scraper has to present, for the line that announces
// the listener.
func metricsAuthHint(cfg *config.Config) string {
	switch {
	case cfg.Metrics.Token != "":
		return " (token required)"
	case cfg.Dashboard.Token != "":
		return " (the dashboard token is required)"
	default:
		return " (no token configured)"
	}
}
