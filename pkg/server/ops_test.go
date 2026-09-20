package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// --- metrics ------------------------------------------------------------------

func TestMetricsRenderExposesDocumentedSeries(t *testing.T) {
	m := newMetrics()
	m.controlAccepted.Add(3)
	m.authFailures.Add(1)
	m.bytesFromClients.Add(2048)
	m.streamOpened("ssh")
	m.recordStream("ssh", 100, 200)
	m.streamClosed("ssh")

	out := m.Render()

	for _, want := range []string{
		"# TYPE aethertunnel_control_connections_total counter",
		"aethertunnel_control_connections_total 3",
		"# TYPE aethertunnel_auth_failures_total counter",
		"aethertunnel_auth_failures_total 1",
		"# TYPE aethertunnel_bytes_from_clients_total counter",
		"aethertunnel_bytes_from_clients_total 2248",
		"# TYPE aethertunnel_streams_active gauge",
		"aethertunnel_streams_active 0",
		"aethertunnel_streams_total 1",
		`aethertunnel_tunnel_streams_total{tunnel="ssh"} 1`,
		`aethertunnel_tunnel_bytes_total{tunnel="ssh",direction="from_client"} 200`,
		`aethertunnel_tunnel_bytes_total{tunnel="ssh",direction="to_client"} 100`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q\n---\n%s", want, out)
		}
	}
}

func TestMetricsAreSortedAndPrometheusShaped(t *testing.T) {
	m := newMetrics()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		m.recordStream(name, 1, 1)
	}
	out := m.Render()

	alpha := strings.Index(out, `tunnel="alpha"`)
	mu := strings.Index(out, `tunnel="mu"`)
	zeta := strings.Index(out, `tunnel="zeta"`)
	if alpha < 0 || mu < 0 || zeta < 0 {
		t.Fatalf("expected all three tunnels in the output:\n%s", out)
	}
	if !(alpha < mu && mu < zeta) {
		t.Errorf("tunnel series are not sorted: alpha=%d mu=%d zeta=%d", alpha, mu, zeta)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "aethertunnel_") {
			t.Errorf("unexpected line in exposition format: %q", line)
		}
		if !strings.Contains(line, " ") {
			t.Errorf("line has no value: %q", line)
		}
	}
}

// --- access control -----------------------------------------------------------

func TestAccessControlDenyAndAllowLists(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		deny  []string
		ip    string
		want  Decision
	}{
		{"no rules allows everything", nil, nil, "203.0.113.7", Allow},
		{"deny list wins", []string{"0.0.0.0/0"}, []string{"203.0.113.0/24"}, "203.0.113.7", DenyCIDR},
		{"allow list excludes others", []string{"10.0.0.0/8"}, nil, "203.0.113.7", DenyCIDR},
		{"allow list includes match", []string{"10.0.0.0/8", "192.168.0.0/16"}, nil, "192.168.4.4", Allow},
		{"deny only blocks its range", []string{"0.0.0.0/0"}, []string{"198.51.100.0/24"}, "192.168.4.4", Allow},
		{"ipv6 allow list", []string{"2001:db8::/32"}, nil, "2001:db8::1", Allow},
		{"ipv6 outside allow list", []string{"2001:db8::/32"}, nil, "2001:dead::1", DenyCIDR},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac, err := NewAccessControl(tc.allow, tc.deny, 0, 0)
			if err != nil {
				t.Fatalf("NewAccessControl: %v", err)
			}
			if got := ac.CheckAddr(net.ParseIP(tc.ip)); got != tc.want {
				t.Fatalf("CheckAddr(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestAccessControlRejectsUnparseableSourceWhenRulesExist(t *testing.T) {
	ac, err := NewAccessControl([]string{"10.0.0.0/8"}, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ac.CheckAddr(nil); got != DenyCIDR {
		t.Fatalf("CheckAddr(nil) = %v, want DenyCIDR", got)
	}

	open, err := NewAccessControl(nil, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := open.CheckAddr(nil); got != Allow {
		t.Fatalf("CheckAddr(nil) with no rules = %v, want Allow", got)
	}
}

func TestAccessControlRejectsInvalidCIDR(t *testing.T) {
	if _, err := NewAccessControl([]string{"not-a-cidr"}, nil, 0, 0); err == nil {
		t.Fatal("expected an error for an invalid allow CIDR")
	}
	if _, err := NewAccessControl(nil, []string{"10.0.0.0/99"}, 0, 0); err == nil {
		t.Fatal("expected an error for an invalid deny CIDR")
	}
}

func TestRateLimiterAllowsBurstThenRefills(t *testing.T) {
	// 1000 tokens per second with a burst of 3: the first three calls pass, the
	// fourth is refused, and after 10 ms the bucket has refilled enough for one
	// more.
	limiter := newRateLimiter(1000, 3)

	for i := 0; i < 3; i++ {
		if !limiter.allow("198.51.100.9") {
			t.Fatalf("call %d should have been allowed within the burst", i+1)
		}
	}
	if limiter.allow("198.51.100.9") {
		t.Fatal("the fourth call should have been rate limited")
	}
	if !limiter.allow("203.0.113.1") {
		t.Fatal("a different source must have its own bucket")
	}

	time.Sleep(10 * time.Millisecond)
	if !limiter.allow("198.51.100.9") {
		t.Fatal("the bucket should have refilled after 10 ms at 1000 tokens/s")
	}
}

func TestRateLimiterDropsIdleBuckets(t *testing.T) {
	limiter := newRateLimiter(1, 1)
	limiter.allow("198.51.100.1")
	if limiter.Size() != 1 {
		t.Fatalf("expected one bucket, got %d", limiter.Size())
	}
	// Force the collection window to have elapsed.
	limiter.lastGC = time.Now().Add(-time.Hour)
	for _, b := range limiter.buckets {
		b.seen = time.Now().Add(-time.Hour)
	}
	limiter.allow("203.0.113.5")
	if limiter.Size() != 1 {
		t.Fatalf("the idle bucket should have been collected, size = %d", limiter.Size())
	}
}

// --- audit log ----------------------------------------------------------------

func TestAuditorWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	auditor.Record(AuditEvent{Event: EventControlAccepted, ClientID: "abc", Remote: "10.0.0.1:5", Outcome: "ok"})
	auditor.Record(AuditEvent{Event: EventAuthFailed, Remote: "10.0.0.2:6", Outcome: "denied", Detail: "invalid auth token"})
	if err := auditor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 audit lines, got %d:\n%s", len(lines), data)
	}

	var first AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if first.Event != EventControlAccepted || first.ClientID != "abc" {
		t.Fatalf("first event = %+v", first)
	}
	if first.Time == "" {
		t.Fatal("the auditor must stamp a time")
	}

	var second AuditEvent
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line is not JSON: %v", err)
	}
	if second.Outcome != "denied" {
		t.Fatalf("second event = %+v", second)
	}
}

func TestAuditorDisabledWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(false, path, 0)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	if auditor.Enabled() {
		t.Fatal("a disabled auditor must report Enabled() == false")
	}
	auditor.Record(AuditEvent{Event: EventControlAccepted})
	if err := auditor.Close(); err != nil {
		t.Fatalf("Close on a disabled auditor must be a no-op, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a disabled auditor created %s", path)
	}
}

func TestAuditorRotatesWhenTheFileGrows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	auditor, err := NewAuditor(true, path, 256)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer auditor.Close()

	for i := 0; i < 20; i++ {
		auditor.Record(AuditEvent{Event: EventProxyRegistered, Proxy: fmt.Sprintf("proxy-%02d", i), Outcome: "ok"})
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotated file at %s.1: %v", path, err)
	}
}

// --- dashboard endpoints ------------------------------------------------------

func newTestDashboard(t *testing.T, mutate func(*config.Config)) (*Dashboard, string) {
	t.Helper()
	cfg := testConfig(t, false)
	cfg.Dashboard.Enabled = true
	cfg.Dashboard.BindAddr = "127.0.0.1"
	cfg.Dashboard.Port = freePort(t)
	cfg.Dashboard.Token = "dashboard-secret"
	cfg.Metrics.Enabled = true
	cfg.Metrics.Token = "metrics-secret"
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	srv, err := New(cfg, Options{Version: "test", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dashboard, err := NewDashboard(srv, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}
	if err := dashboard.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(dashboard.Stop)
	t.Cleanup(func() { srv.auditor.Close() })

	return dashboard, "http://" + cfg.DashboardAddr()
}

func TestHealthProbesArePublic(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", resp.StatusCode)
	}
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if health["status"] != "ok" {
		t.Fatalf("/healthz = %v", health)
	}

	// The server is not listening yet, so readiness must say so.
	ready, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 before the tunnel listener is up", ready.StatusCode)
	}
}

func TestMetricsEndpointRequiresAToken(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	noToken, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	noToken.Body.Close()
	if noToken.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/metrics without a token = %d, want 401", noToken.StatusCode)
	}

	wrong, err := http.Get(base + "/metrics" + "?x=1")
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics with the metrics token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics with the metrics token = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "aethertunnel_uptime_seconds") {
		t.Fatalf("the metrics body has no uptime series:\n%s", body)
	}

	// The dashboard token is accepted too, so an operator does not need a second
	// credential to read the same counters.
	dashReq, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	dashReq.Header.Set("Authorization", "Bearer dashboard-secret")
	dashResp, err := http.DefaultClient.Do(dashReq)
	if err != nil {
		t.Fatalf("GET /metrics with the dashboard token: %v", err)
	}
	defer dashResp.Body.Close()
	if dashResp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics with the dashboard token = %d, want 200", dashResp.StatusCode)
	}
}

func TestMetricsEndpointIsAbsentWhenDisabled(t *testing.T) {
	_, base := newTestDashboard(t, func(cfg *config.Config) { cfg.Metrics.Enabled = false })

	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	// The static file handler answers unknown paths, so the assertion is that the
	// Prometheus body is not served, whatever the status code is.
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "aethertunnel_uptime_seconds") {
		t.Fatal("/metrics served metrics even though [metrics].enabled is false")
	}
}
