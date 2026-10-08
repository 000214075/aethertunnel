package clientlib

import (
	"encoding/json"
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

// adminHarness starts a client shell with the management API on an ephemeral
// port and returns its base URL plus the client.
func adminHarness(t *testing.T, admin *config.AdminConfig) (*client, string) {
	t.Helper()
	port := freeAdminPort(t)
	admin.Enabled = true
	admin.BindAddr = "127.0.0.1"
	admin.Port = port
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.Client.Admin = admin
	c.baseCtx = testContext(t)
	if err := c.startAdminServer(); err != nil {
		t.Fatalf("start the admin server: %v", err)
	}
	base := "http://127.0.0.1:" + itoa(port)
	// The listener answers the moment startAdminServer returns, but give a
	// slow machine a moment before the first request.
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c, base
}

func freeAdminPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

func adminGet(t *testing.T, url string, withAuth bool, user, password string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if withAuth {
		request.SetBasicAuth(user, password)
	}
	answer, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = answer.Body.Close() })
	return answer
}

func TestAdminHealthzIsOpen(t *testing.T) {
	_, base := adminHarness(t, &config.AdminConfig{})

	answer := adminGet(t, base+"/healthz", false, "", "")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d, want 200", answer.StatusCode)
	}
	body, _ := io.ReadAll(answer.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz body %q, want ok", body)
	}
}

func TestAdminStatusReportsTheProxies(t *testing.T) {
	c, base := adminHarness(t, &config.AdminConfig{})
	c.mu.Lock()
	c.session = "sess-42"
	c.registeredNames = []string{"web"}
	c.mu.Unlock()
	c.setProxyList([]config.ProxyConfig{
		{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080, RemotePort: 6000},
		{Name: "pending", Type: config.ProxyTypeTCP, LocalPort: 8081, RemotePort: 6001},
	})

	answer := adminGet(t, base+"/api/status", false, "", "")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 without credentials", answer.StatusCode)
	}
	var report adminStatusReport
	if err := json.NewDecoder(answer.Body).Decode(&report); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if report.Session != "sess-42" || !report.Connected {
		t.Fatalf("session %q connected=%v, want sess-42/true", report.Session, report.Connected)
	}
	if len(report.Proxies) != 2 {
		t.Fatalf("%d proxies in the report, want 2", len(report.Proxies))
	}
	if !report.Proxies[0].Confirmed {
		t.Fatalf("%q is not confirmed though the server named it", report.Proxies[0].Name)
	}
	if report.Proxies[1].Confirmed {
		t.Fatalf("%q is confirmed though the server never named it", report.Proxies[1].Name)
	}
	if report.Proxies[0].Local != "127.0.0.1:8080" {
		t.Fatalf("local %q, want 127.0.0.1:8080", report.Proxies[0].Local)
	}
}

func TestAdminAuthGatesTheAPIRoutes(t *testing.T) {
	_, base := adminHarness(t, &config.AdminConfig{User: "ops", Password: "s3cret"})

	answer := adminGet(t, base+"/api/status", false, "", "")
	if answer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without credentials: %d, want 401", answer.StatusCode)
	}
	if challenge := answer.Header.Get("WWW-Authenticate"); !strings.Contains(challenge, "Basic") {
		t.Fatalf("the 401 carries %q, want a Basic challenge", challenge)
	}

	answer = adminGet(t, base+"/api/status", true, "ops", "wrong")
	if answer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with a wrong password: %d, want 401", answer.StatusCode)
	}

	answer = adminGet(t, base+"/api/status", true, "ops", "s3cret")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("status with the right credentials: %d, want 200", answer.StatusCode)
	}

	// healthz stays open for the process supervisor even when the API has
	// credentials.
	answer = adminGet(t, base+"/healthz", false, "", "")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("healthz with no credentials: %d, want 200", answer.StatusCode)
	}
}

func TestAdminReloadWithoutSourceFileIsRefused(t *testing.T) {
	_, base := adminHarness(t, &config.AdminConfig{})

	answer, err := http.Post(base+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/reload: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusBadRequest {
		t.Fatalf("reload without a source file: %d, want 400", answer.StatusCode)
	}
	var payload map[string]string
	if err := json.NewDecoder(answer.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the error: %v", err)
	}
	if payload["error"] == "" {
		t.Fatal("the refusal carries no explanation")
	}
}

func TestAdminReloadRereadsTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "client.toml")
	first := "[client]\nserver_addr = \"127.0.0.1:7000\"\nauth_token = \"reload-test-token-000\"\n\n[[proxies]]\nname = \"web\"\ntype = \"tcp\"\nlocal_port = 8080\nremote_port = 6000\n"
	if err := os.WriteFile(file, []byte(first), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	c, base := adminHarness(t, &config.AdminConfig{})
	c.cfg.SourceFile = file
	if _, err := config.LoadString(first, "client.toml", config.ValidateOptions{Role: config.RoleClient}); err != nil {
		t.Fatalf("the fixture config is invalid: %v", err)
	}

	answer, err := http.Post(base+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/reload: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("reload: %d, want 200", answer.StatusCode)
	}
}
