package config

import (
	"strings"
	"testing"
)

func clientBase() *Config {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7001"
	cfg.Client.AuthToken = "client-token-0123456"
	cfg.ApplyClientDefaults()
	return cfg
}

// The server-side [oidc] keys placed in a client configuration used to be
// silently ignored — every other section warns when its keys sit on the wrong
// side, and a client that sets skip_expiry_check believes it turned a check
// off.
func TestMisplacedOidcServerKeysAreNamedInTheClientWarning(t *testing.T) {
	cfg := clientBase()
	cfg.OIDC = &OIDCConfig{
		TokenEndpointURL: "https://issuer.example/token",
		ClientID:         "panel",
		ClientSecret:     "secret",
		TimeoutSecs:      60,
		SkipExpiryCheck:  true,
	}
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, key := range []string{"oidc.timeout_secs", "oidc.skip_expiry_check", "oidc.skip_issuer_check"} {
		found := false
		for _, warning := range cfg.Warnings {
			if strings.Contains(warning, key) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the misplaced-key warning does not name %s; warnings: %v", key, cfg.Warnings)
		}
	}
}

// An [oidc] client no longer needs its auth_token to equal the server's: both
// ends derive the per-proxy stream key from the credential the session
// authenticated with. The old warning told operators to keep the server's
// token there and is gone.
func TestUseEncryptionBesideOidcNeedsNoWarningAnyMore(t *testing.T) {
	cfg := clientBase()
	cfg.Client.AuthToken = "a-placeholder-not-the-servers"
	cfg.OIDC = &OIDCConfig{
		TokenEndpointURL: "https://issuer.example/token",
		ClientID:         "panel",
		ClientSecret:     "secret",
	}
	cfg.Proxies = []ProxyConfig{{
		Name:          "sealed",
		Type:          "tcp",
		LocalIP:       "127.0.0.1",
		LocalPort:     8080,
		UseEncryption: true,
	}}
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "use_encryption") && strings.Contains(warning, "[oidc]") {
			t.Fatalf("the stale per-proxy-key warning is still produced: %s", warning)
		}
	}
}

// The server refuses a client_id longer than 128 bytes at authentication —
// the ledger keys its per-client totals by the name — so a configuration
// carrying a longer one would connect nowhere. --check names it instead.
func TestAnOverlongClientIdIsRefusedByCheck(t *testing.T) {
	cfg := clientBase()
	cfg.Client.ClientID = strings.Repeat("x", 129)
	err := cfg.Validate(RoleClient)
	if err == nil {
		t.Fatal("a 129-byte client_id passed --check")
	}
	if !strings.Contains(err.Error(), "client.client_id") {
		t.Fatalf("the problem does not name client.client_id: %v", err)
	}

	cfg.Client.ClientID = strings.Repeat("x", 128)
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a 128-byte client_id was refused: %v", err)
	}
}

// A single-value range check is a statement about the file itself, so it runs
// whether or not the section is enabled — the policy the top of
// docs/CONFIGURATION.md states, and metrics.port has always followed. A
// dashboard port that is out of range behind enabled = false used to pass
// --check and surface only when the operator turned the panel on.
func TestADashboardPortOutOfRangeIsRefusedWhileThePanelIsOff(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef"

[dashboard]
enabled = false
port = 70000
`
	_, err := LoadString(body, "server.toml", ValidateOptions{Role: RoleServer})
	if err == nil {
		t.Fatal("a dashboard.port of 70000 behind enabled = false passed --check")
	}
	if !strings.Contains(err.Error(), "dashboard.port") {
		t.Fatalf("the refusal does not name dashboard.port: %v", err)
	}

	negative := strings.Replace(body, "port = 70000", "port = -1", 1)
	if _, err := LoadString(negative, "server.toml", ValidateOptions{Role: RoleServer}); err == nil {
		t.Fatal("a negative dashboard.port behind enabled = false passed --check")
	}
}

// The conflict check reads the address each listener really binds. An empty
// dashboard or metrics bind means loopback — DashboardAddr and MetricsAddr say
// so — and reading it as the wildcard it never binds as made a configuration
// built in code report a conflict between a loopback panel and a listener on a
// concrete address that shares only the port number.
func TestAnUnboundDashboardDoesNotConflictWithAListenerOnAConcreteAddress(t *testing.T) {
	cfg := &Config{}
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.BindPort = 7001
	cfg.Server.AuthToken = "0123456789abcdef"
	cfg.Dashboard.Enabled = true
	cfg.Dashboard.Port = 7500
	cfg.Metrics.Enabled = true
	cfg.Metrics.Port = 7500
	cfg.Metrics.BindAddr = "192.0.2.10"

	found := make(map[string]boundListener)
	for _, l := range cfg.listeners(RoleServer) {
		found[l.key] = l
	}
	dashboard, ok := found["dashboard.port"]
	if !ok {
		t.Fatal("the dashboard listener is missing from the conflict table")
	}
	if dashboard.host != "127.0.0.1" {
		t.Fatalf("an empty dashboard.bind_addr reads as %q, want the loopback it really binds", dashboard.host)
	}
	metrics, ok := found["metrics.port"]
	if !ok {
		t.Fatal("the metrics listener is missing from the conflict table")
	}
	if metrics.host != "192.0.2.10" {
		t.Fatalf("the metrics bind reads as %q, want the address it really binds", metrics.host)
	}
	if _, conflict := listenerConflict([]boundListener{dashboard, metrics}); conflict {
		t.Fatal("a loopback panel and a metrics listener on a concrete address were reported as a conflict")
	}
}
