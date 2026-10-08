package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tokenServerBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
`

const tokenClientBase = `
[client]
server_addr = "127.0.0.1:7001"
`

// A token file supplies the secret without the configuration carrying it, and
// the newline an editor leaves behind is not part of the secret.
func TestAuthTokenFileSuppliesTheToken(t *testing.T) {
	// The path goes into the configuration as a TOML literal string: a Windows
	// path holds backslashes, which a basic string would read as escapes.
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("file-token-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("write the token: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
		load func(string) (*Config, error)
		get  func(*Config) string
	}{
		{"server", tokenServerBase + "auth_token_file = '" + tokenPath + "'\n", LoadServer,
			func(c *Config) string { return c.Server.AuthToken }},
		{"client", tokenClientBase + "auth_token_file = '" + tokenPath + "'\n", LoadClient,
			func(c *Config) string { return c.Client.AuthToken }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := tc.load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := tc.get(cfg); got != "file-token-0123456789abcdef" {
				t.Errorf("the token is %q, want the file's line without its newline", got)
			}
		})
	}
}

// The file wins over an inline token, and the operator is told so: a file that
// carries both is usually a half-finished migration.
func TestAuthTokenFileOverridesTheInlineToken(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write the token: %v", err)
	}

	body := tokenServerBase + "auth_token = \"from-config\"\nauth_token_file = '" + tokenPath + "'\n"
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.AuthToken != "from-file" {
		t.Errorf("the token is %q, want the file's value", cfg.Server.AuthToken)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "auth_token_file overrides the inline") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning mentions the override, got: %v", cfg.Warnings)
	}
}

// An unreadable or empty file is a hard error: a token that silently became
// empty would fail much later, as an authentication error that names nothing.
func TestAuthTokenFileProblemsAreFatal(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write the empty token: %v", err)
	}
	missing := filepath.Join(dir, "missing")

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"unreadable", missing, "auth_token_file"},
		{"empty", empty, "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tokenServerBase + "auth_token_file = '" + tc.path + "'\n"
			_, err := LoadServer(writeConfig(t, body))
			if err == nil {
				t.Fatalf("a %s token file was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// Without a token file nothing changes: the inline token is the token.
func TestWithoutATokenFileTheInlineTokenStands(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, tokenServerBase+"auth_token = \"inline\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.AuthToken != "inline" {
		t.Errorf("the token is %q, want the inline one", cfg.Server.AuthToken)
	}
}

// The source address has to be an address; a hostname here would be resolved
// by the kernel at bind time with a message nobody expects.
func TestConnectServerLocalIPMustBeAnIP(t *testing.T) {
	body := tokenClientBase + "auth_token = \"t\"\nconnect_server_local_ip = \"eth0\"\n"
	if _, err := LoadClient(writeConfig(t, body)); err == nil {
		t.Fatal("LoadClient accepted a non-IP client.connect_server_local_ip")
	}

	body = tokenClientBase + "auth_token = \"t\"\nconnect_server_local_ip = \"10.1.2.3\"\n"
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient rejected a valid source address: %v", err)
	}
	if cfg.Client.ConnectServerLocalIP != "10.1.2.3" {
		t.Errorf("client.connect_server_local_ip is %q", cfg.Client.ConnectServerLocalIP)
	}
}
