package server

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// metricsListenerServer starts a server whose [metrics] section carries its own
// address, and returns the address it bound.
func metricsListenerServer(t *testing.T, cfg *config.Config) (*runningServer, string) {
	t.Helper()
	rs := startServer(t, cfg)
	listener := NewMetricsListener(rs.server, log.New(io.Discard, "", 0))
	if err := listener.Start(); err != nil {
		t.Fatalf("start the metrics listener: %v", err)
	}
	t.Cleanup(listener.Stop)
	return rs, listener.Addr().String()
}

func getMetricsFrom(t *testing.T, addr, path, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return response.StatusCode, string(body)
}

// The endpoint can be served on an address of its own, which is what a deployment
// that keeps the dashboard off uses: the counters are reachable, the panel is not.
func TestMetricsCanBeServedOnTheirOwnAddress(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Metrics.Enabled = true
	cfg.Metrics.BindAddr = "127.0.0.1"
	cfg.Metrics.Port = freePort(t)
	cfg.Metrics.Token = "scrape-token"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	_, addr := metricsListenerServer(t, cfg)

	status, body := getMetricsFrom(t, addr, "/metrics", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("a scrape without a token answered %d", status)
	}
	status, body = getMetricsFrom(t, addr, "/metrics", "wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("a scrape with a wrong token answered %d", status)
	}
	status, body = getMetricsFrom(t, addr, "/metrics", "scrape-token")
	if status != http.StatusOK {
		t.Fatalf("a scrape with the token answered %d", status)
	}
	if !strings.Contains(body, "aethertunnel_uptime_seconds") {
		t.Fatalf("the exposition does not carry the counters: %q", body[:min(len(body), 200)])
	}

	// The listener is one endpoint: everything else is a 404, not a redirect to
	// the panel.
	if status, _ := getMetricsFrom(t, addr, "/api/status", "scrape-token"); status != http.StatusNotFound {
		t.Fatalf("/api/status on the metrics listener answered %d", status)
	}
}

// The dashboard token is accepted as well, which is the rule the dashboard's own
// /metrics has: one credential is enough.
func TestTheDashboardTokenScrapesTheMetricsListener(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Metrics.Enabled = true
	cfg.Metrics.Port = freePort(t)
	cfg.Metrics.Token = "scrape-token"
	cfg.Dashboard.Token = "panel-token"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	_, addr := metricsListenerServer(t, cfg)

	if status, _ := getMetricsFrom(t, addr, "/metrics", "panel-token"); status != http.StatusOK {
		t.Fatalf("the dashboard token was refused with %d", status)
	}
	if status, _ := getMetricsFrom(t, addr, "/metrics", "scrape-token"); status != http.StatusOK {
		t.Fatalf("the metrics token was refused with %d", status)
	}
}

// With no token at all the listener is open, which the default loopback address
// makes a local matter; the line the server logs says so.
func TestAnOpenMetricsListenerIsAnnouncedAsOpen(t *testing.T) {
	hint := metricsAuthHint(&config.Config{})
	if !strings.Contains(hint, "no token") {
		t.Fatalf("the hint for an open listener is %q", hint)
	}
	cfg := &config.Config{}
	cfg.Metrics.Token = "scrape-token"
	if hint := metricsAuthHint(cfg); !strings.Contains(hint, "token required") {
		t.Fatalf("the hint for a token-protected listener is %q", hint)
	}
	both := &config.Config{}
	both.Dashboard.Token = "panel-token"
	if hint := metricsAuthHint(both); !strings.Contains(hint, "dashboard token") {
		t.Fatalf("the hint for a dashboard-token listener is %q", hint)
	}
}
