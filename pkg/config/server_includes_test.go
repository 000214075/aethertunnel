package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// includes is the client's mechanism for splitting its own proxies and visitors
// across files. A server's [[proxies]] is the operator's policy instead, so
// merging a fragment there silently turned it into one, and --check said nothing.
// A server configuration that names includes now gets a warning and keeps the
// fragments out, so the warning and the behavior agree.
func TestIncludesHaveNoEffectInAServerConfiguration(t *testing.T) {
	dir := t.TempDir()
	fragment := filepath.Join(dir, "frag.toml")
	if err := os.WriteFile(fragment, []byte(
		"[[proxies]]\nname = \"sneaky\"\ntype = \"tcp\"\nlocal_port = 8080\nremote_port = 7001\n"), 0o600); err != nil {
		t.Fatalf("write the fragment: %v", err)
	}
	main := filepath.Join(dir, "server.toml")
	if err := os.WriteFile(main, []byte(
		"includes = [\"frag.toml\"]\n\n[server]\nbind_addr = \"127.0.0.1\"\nbind_port = 7000\nauth_token = \"0123456789abcdef0123456789abcdef\"\n"), 0o600); err != nil {
		t.Fatalf("write the main file: %v", err)
	}

	cfg, err := Load(main, ValidateOptions{Role: RoleServer})
	if err != nil {
		t.Fatalf("load the server configuration: %v", err)
	}
	if len(cfg.Proxies) != 0 {
		t.Fatalf("a server configuration merged %d proxies from a fragment", len(cfg.Proxies))
	}
	warned := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "includes has no effect in a server configuration") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning about includes in a server configuration; warnings: %v", cfg.Warnings)
	}
}

// dashboard.bind_addr left empty is the documented loopback default. The
// non-loopback warning tested the raw string, and isLoopback("") is false, so
// every dashboard with a token and the default address drew a warning about a
// bind it had not asked for.
func TestADashboardWithNoBindAddrIsNotWarnedAboutAsNonLoopback(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Dashboard.Enabled = true
	cfg.Dashboard.Token = ""
	cfg.Dashboard.BindAddr = ""
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "dashboard.bind_addr is not a loopback address") {
			t.Fatalf("an empty bind_addr was warned about as non-loopback: %q", warning)
		}
	}
}
