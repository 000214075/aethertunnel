package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// client.client_id is the client's stable name. It rides the auth request so the
// server can name the client by something that outlives the session's random
// identifier — in its log, its audit records, the dashboard and the ledger — and
// a client that sends none keeps that identifier.
func TestTheAuthRequestNamesTheClientStably(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Dashboard.Enabled = true
	cfg.Dashboard.BindAddr = "127.0.0.1"
	cfg.Dashboard.Port = freePort(t)
	cfg.Dashboard.Token = "dashboard-secret"

	rs := startServer(t, cfg)
	dashboard, err := NewDashboard(rs.server, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}
	if err := dashboard.Start(); err != nil {
		t.Fatalf("dashboard start: %v", err)
	}
	t.Cleanup(dashboard.Stop)

	named, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer named.close()
	response, err := named.authenticateAs("test", testToken, "edge-01")
	if err != nil {
		t.Fatalf("authenticate as edge-01: %v", err)
	}

	anonymous, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer anonymous.close()
	anonymousResponse, err := anonymous.authenticate("test", testToken)
	if err != nil {
		t.Fatalf("authenticate without a name: %v", err)
	}

	byName := waitForSession(t, rs, func(s *Session) bool { return s.ClientName() == "edge-01" })
	if byName.ID == "" {
		t.Fatal("the named session was not found")
	}
	if byName.ID != response.Session {
		t.Fatalf("the session that carries edge-01 is %s, want %s", byName.ID, response.Session)
	}

	unnamed := waitForSession(t, rs, func(s *Session) bool { return s.ID == anonymousResponse.Session })
	if got := unnamed.ClientName(); got != anonymousResponse.Session {
		t.Fatalf("a client that sent no client_id is named %q, want its session id %q", got, anonymousResponse.Session)
	}
	if unnamed.ClientID != "" {
		t.Fatalf("a client that sent no client_id recorded %q", unnamed.ClientID)
	}

	// The dashboard names the client by the same stable value it uses for a
	// proxy's member and for a ledger entry.
	body := fetchClients(t, cfg)
	for _, row := range body {
		if row["id"] == response.Session {
			if row["client_id"] != "edge-01" {
				t.Fatalf("the dashboard names the session %v, want edge-01", row["client_id"])
			}
			return
		}
	}
	t.Fatalf("the dashboard does not list the named session %s: %v", response.Session, body)
}

// waitForSession waits for a session the predicate accepts, which is how a test
// avoids racing the accept path.
func waitForSession(t *testing.T, rs *runningServer, accept func(*Session) bool) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, session := range rs.server.sessions.List() {
			if accept(session) {
				return session
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no session matched within 5s")
	return nil
}

// fetchClients reads /api/clients from the dashboard this test started.
func fetchClients(t *testing.T, cfg *config.Config) []map[string]any {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://"+cfg.DashboardAddr()+"/api/clients", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer dashboard-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /api/clients: %v", err)
	}
	defer response.Body.Close()
	var payload struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode /api/clients: %v", err)
	}
	return payload.Clients
}
