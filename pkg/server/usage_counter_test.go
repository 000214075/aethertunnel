package server

import (
	"net/http"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A session's per-name usage counter is what the ledger bills at teardown, so a
// registration that never published its name has to drop the counter it created.
// Telling that counter apart from one an earlier registration of the same name
// left behind — whose streams may still be writing to it — is what the flag this
// test checks is for.
func TestARegistrationRemembersWhetherItCreatedTheUsageCounter(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	spec := protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP,
		LocalAddr: "127.0.0.1:1", RemotePort: freePort(t),
	}
	agent.register(spec)

	group, err := rs.server.tunnels.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	members := group.Members()
	if len(members) != 1 {
		t.Fatalf("%d members are registered, want one", len(members))
	}
	if !members[0].createdUsage {
		t.Fatal("the first registration did not record that it created the usage counter")
	}

	// A re-registration by the same session replaces its own member and reuses the
	// counter the name already has.
	agent.register(spec)
	members = group.Members()
	if len(members) != 1 {
		t.Fatalf("%d members after the re-registration, want one", len(members))
	}
	if members[0].createdUsage {
		t.Fatal("a replacement claimed to have created the usage counter, so a rollback would drop one an earlier registration still bills")
	}
}

// With neither token configured, /metrics needs no authentication — including for
// a scraper that carries a stale bearer token, which used to be refused while a
// request with no header at all was admitted.
func TestMetricsNeedsNoCredentialWhenNoTokenIsConfigured(t *testing.T) {
	_, base := newTestDashboard(t, func(cfg *config.Config) {
		cfg.Dashboard.Token = ""
		cfg.Metrics.Token = ""
	})

	for _, token := range []string{"", "a-stale-placeholder"} {
		req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /metrics with %q: %v", token, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/metrics with %q = %d, want 200 with no token configured", token, resp.StatusCode)
		}
	}
}
