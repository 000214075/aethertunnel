package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// The dashboard endpoints that read server state are all behind the dashboard
// token. A missing or wrong token has to be refused before the handler runs.
// DELETE /api/clients/{id} matters most of all: it closes a live session, so an
// unauthenticated caller must not reach it.
func TestDashboardProtectedEndpointsRequireTheToken(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	cases := []struct {
		method string
		path   string
		key    string // a field the authorized response must carry
		authed int    // status once the token is presented
	}{
		{http.MethodGet, "/api/status", "connections", http.StatusOK},
		{http.MethodGet, "/api/clients", "clients", http.StatusOK},
		{http.MethodGet, "/api/proxies", "proxies", http.StatusOK},
		{http.MethodGet, "/api/config", "server", http.StatusOK},
		{http.MethodGet, "/api/ledger", "enabled", http.StatusOK},
		{http.MethodGet, "/api/dht", "enabled", http.StatusOK},
		{http.MethodGet, "/api/vpn", "enabled", http.StatusOK},
		// There is no such client, so the handler answers 404. Reaching the
		// handler at all is the point: 404 rather than 401 proves the token
		// opened the gate and the lookup ran.
		{http.MethodDelete, "/api/clients/no-such-client", "", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			noToken := doDashboardRequest(t, tc.method, base+tc.path, "")
			noToken.Body.Close()
			if noToken.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s without a token = %d, want 401", tc.method, tc.path, noToken.StatusCode)
			}

			wrong := doDashboardRequest(t, tc.method, base+tc.path, "not-the-dashboard-token")
			wrong.Body.Close()
			if wrong.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with a wrong token = %d, want 401", tc.method, tc.path, wrong.StatusCode)
			}

			authorized := doDashboardRequest(t, tc.method, base+tc.path, "dashboard-secret")
			defer authorized.Body.Close()
			if authorized.StatusCode != tc.authed {
				t.Fatalf("%s %s with the token = %d, want %d", tc.method, tc.path, authorized.StatusCode, tc.authed)
			}
			if tc.key == "" {
				return
			}
			var body map[string]any
			if err := json.NewDecoder(authorized.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s: %v", tc.path, err)
			}
			if _, ok := body[tc.key]; !ok {
				t.Fatalf("%s = %v, want a %q field", tc.path, body, tc.key)
			}
		})
	}
}

// A configuration with none of the optional subsystems enabled must say so on
// the endpoints that report them, rather than reporting an empty success. The
// panel renders these payloads directly, so "absent" and "disabled" have to be
// distinguishable.
func TestDashboardReportsDisabledSubsystems(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	for _, path := range []string{"/api/ledger", "/api/dht", "/api/vpn"} {
		resp := doDashboardRequest(t, http.MethodGet, base+path, "dashboard-secret")
		var body map[string]any
		err := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if body["enabled"] != false {
			t.Errorf("%s reports enabled=%v although the subsystem is off", path, body["enabled"])
		}
	}

	// The VPN endpoint carries the reason, because "vpn.enabled is false" and
	// "the platform cannot build a TUN device" are different problems for an
	// operator to chase.
	resp := doDashboardRequest(t, http.MethodGet, base+"/api/vpn", "dashboard-secret")
	var vpn map[string]any
	err := json.NewDecoder(resp.Body).Decode(&vpn)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decode /api/vpn: %v", err)
	}
	if reason, _ := vpn["reason"].(string); reason == "" {
		t.Errorf("/api/vpn = %v, want a reason the tunnel is off", vpn)
	}
}

// /api/config is the one endpoint that republishes the operator's own settings,
// so it is the one place a credential would leak. It must expose the shape of
// the configuration without the tokens or the passphrase in it.
func TestDashboardConfigNeverExposesCredentials(t *testing.T) {
	_, base := newTestDashboard(t, func(cfg *config.Config) {
		cfg.Server.AuthToken = "server-secret"
		cfg.Encryption.Enabled = true
		cfg.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
		cfg.Encryption.Salt = "test-salt"
		cfg.Encryption.Passphrase = "super-secret-passphrase"
	})

	resp := doDashboardRequest(t, http.MethodGet, base+"/api/config", "dashboard-secret")
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read /api/config: %v", err)
	}
	body := string(raw)

	for _, secret := range []string{"super-secret-passphrase", "server-secret", "metrics-secret", "dashboard-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("/api/config leaked %q: %s", secret, body)
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	for _, section := range []string{"server", "encryption", "dashboard", "ledger", "dht", "vpn", "proxies_configured"} {
		if _, ok := parsed[section]; !ok {
			t.Errorf("/api/config is missing the %q section: %s", section, body)
		}
	}
	dashboard, _ := parsed["dashboard"].(map[string]any)
	if dashboard["auth_required"] != true {
		t.Errorf("/api/config reports dashboard auth_required=%v although a token is set", dashboard["auth_required"])
	}
	encryption, _ := parsed["encryption"].(map[string]any)
	if encryption["enabled"] != true {
		t.Errorf("/api/config reports encryption.enabled=%v although encryption is on", encryption["enabled"])
	}
}

// ?limit= is operator input that reaches the ledger slice arithmetic, so it is
// rejected when it is not a non-negative integer instead of being clamped into
// a surprising window.
func TestLedgerEndpointValidatesTheLimit(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	for _, bad := range []string{"abc", "-1", "1.5"} {
		path := "/api/ledger?limit=" + bad
		resp := doDashboardRequest(t, http.MethodGet, base+path, "dashboard-secret")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, resp.StatusCode)
		}
	}

	for _, good := range []string{"0", "1", "200", "999999"} {
		path := "/api/ledger?limit=" + good
		resp := doDashboardRequest(t, http.MethodGet, base+path, "dashboard-secret")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func doDashboardRequest(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}
