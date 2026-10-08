package config

import (
	"strings"
	"testing"
)

// TestOIDCValidation covers the [oidc] section: the server needs an issuer,
// the client needs an endpoint with paired credentials, and each side warns
// about the other side's keys.
func TestOIDCValidation(t *testing.T) {
	t.Run("a server without an issuer", func(t *testing.T) {
		path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
audience = "aethertunnel"
`)
		_, err := LoadServer(path)
		if err == nil || !strings.Contains(err.Error(), "oidc.issuer is required") {
			t.Fatalf("err = %v, want a missing-issuer error", err)
		}
	})

	t.Run("a server with client-side keys draws a warning", func(t *testing.T) {
		path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
issuer = "https://issuer.example"
audience = "aethertunnel"
token_endpoint_url = "https://issuer.example/token"
`)
		cfg, err := LoadServer(path)
		if err != nil {
			t.Fatalf("LoadServer: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "no effect in a server configuration") {
			t.Fatalf("expected a server-config warning, got %v", cfg.Warnings)
		}
	})

	t.Run("a client needs its credentials together", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
token_endpoint_url = "https://issuer.example/token"
client_id = "aethertunnel"
`)
		_, err := LoadClient(path)
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("err = %v, want a both-or-none error", err)
		}
	})

	t.Run("a client with only server keys draws a warning", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
issuer = "https://issuer.example"
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "no effect in a client configuration") {
			t.Fatalf("expected a client-config warning, got %v", cfg.Warnings)
		}
	})

	t.Run("a full client and server pair validates", func(t *testing.T) {
		serverPath := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
issuer = "https://issuer.example"
audience = "aethertunnel"
skip_expiry_check = true
`)
		serverCfg, err := LoadServer(serverPath)
		if err != nil {
			t.Fatalf("LoadServer: %v", err)
		}
		if serverCfg.OIDC.Issuer != "https://issuer.example" || !serverCfg.OIDC.SkipExpiryCheck {
			t.Fatalf("the server oidc round-tripped as %+v", serverCfg.OIDC)
		}

		clientPath := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[oidc]
token_endpoint_url = "https://issuer.example/token"
client_id = "aethertunnel"
client_secret = "secret-1"
audience = "aethertunnel"
scope = "tunnels"
`)
		clientCfg, err := LoadClient(clientPath)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if clientCfg.OIDC.TokenEndpointURL != "https://issuer.example/token" || clientCfg.OIDC.Scope != "tunnels" {
			t.Fatalf("the client oidc round-tripped as %+v", clientCfg.OIDC)
		}
	})
}
