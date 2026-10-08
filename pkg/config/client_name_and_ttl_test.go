package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// client.start is expanded against the proxies as written, and an included
// fragment's proxies are not in that list when the main file is decoded: the
// start name stayed as it was written, named nothing, and the client published
// no tunnel at all while the warning blamed a name that does exist.
func TestAClientStartNameFollowsAnIncludedRemotePortsRange(t *testing.T) {
	dir := t.TempDir()
	fragment := filepath.Join(dir, "extra.toml")
	if err := os.WriteFile(fragment, []byte(`
[[proxies]]
name = "web"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 8080
remote_ports = "7000-7002"
`), 0o600); err != nil {
		t.Fatalf("write fragment: %v", err)
	}
	main := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(main, []byte(`
includes = ["extra.toml"]

[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
start = ["web"]
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadClient(main)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	names := make([]string, 0, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		names = append(names, p.Name)
	}
	set := cfg.Client.StartSet()
	if len(set) != 3 {
		t.Fatalf("client.start selects %v, want the three names the range expands to (%v)", set, names)
	}
	for _, want := range []string{"web-7000", "web-7001", "web-7002"} {
		if _, ok := set[want]; !ok {
			t.Errorf("client.start does not select %q (proxies: %v)", want, names)
		}
	}
	if missing := cfg.missingStartNames(); len(missing) != 0 {
		t.Errorf("start names reported missing: %v", missing)
	}
}

// The TTL a DHT node really uses is the package default of an hour when the key
// is absent, and the check that compares it with announce_ttl_seconds skipped
// zero: an announce TTL of a day then passed with the node still expiring its own
// record after an hour, so the name stopped resolving between two republishes.
func TestAnAnnounceTTLLongerThanTheDHTTTLIsRefused(t *testing.T) {
	const serverBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef"
`
	_, err := LoadString(serverBase+`
[dht]
enabled = true
announce_ttl_seconds = 86400
`, "embedded", ValidateOptions{Role: RoleServer})
	if err == nil {
		t.Fatal("an announce TTL of a day passed with the default one-hour TTL")
	}
	if !strings.Contains(err.Error(), "dht.ttl_seconds") {
		t.Fatalf("the refusal does not name the TTL it is about: %v", err)
	}

	// Raising the TTL is the fix the message asks for.
	if _, err := LoadString(serverBase+`
[dht]
enabled = true
ttl_seconds = 86400
announce_ttl_seconds = 86400
`, "embedded", ValidateOptions{Role: RoleServer}); err != nil {
		t.Fatalf("a TTL at least as long as the announce TTL was refused: %v", err)
	}
}

// A value multiplied by a second wraps inside the int64 a duration is, and the
// wrapped value is read as "no limit" at every use site, so an absurd timeout
// silently removed the bound it named instead of setting it.
func TestADurationThatWouldOverflowIsRefused(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Server.HandshakeTimeoutSecs = MaxDurationSeconds + 1
	err := cfg.Validate(RoleServer)
	if err == nil {
		t.Fatal("a handshake timeout that overflows a duration was accepted")
	}
	if !strings.Contains(err.Error(), "server.handshake_timeout_seconds") {
		t.Fatalf("the refusal does not name the setting: %v", err)
	}

	cfg.Server.HandshakeTimeoutSecs = MaxDurationSeconds
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("the largest value that still fits was refused: %v", err)
	}

	client := &Config{}
	client.Client.ServerAddr = "127.0.0.1:7001"
	client.Client.AuthToken = "0123456789abcdef"
	client.Client.DialTimeoutSecs = MaxDurationSeconds + 1
	if err := client.Validate(RoleClient); err == nil {
		t.Fatal("a client dial timeout that overflows a duration was accepted")
	}
	client.Client.DialTimeoutSecs = 0
	client.HTTPPlugins = []HTTPPluginConfig{{
		Name: "gate", Addr: "http://127.0.0.1:1", Ops: []string{"login"},
		TimeoutSecs: MaxDurationSeconds + 1,
	}}
	if err := client.Validate(RoleClient); err == nil {
		t.Fatal("a plugin timeout that overflows a duration was accepted")
	}
}

// The management API warning tested the bind address with net.ParseIP, so a
// hostname — which the client really listens on — parsed to a nil IP and drew no
// warning while the same address written as an IP did.
func TestANonLoopbackAdminBindAddressIsWarnedAboutWhateverItsForm(t *testing.T) {
	const base = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[client.admin]
enabled = true
port = 7400
`
	for _, address := range []string{"0.0.0.0", "10.0.0.7", "admin.example", "myhost.lan"} {
		cfg, err := LoadString(base+"bind_addr = \""+address+"\"\n", "embedded", ValidateOptions{Role: RoleClient})
		if err != nil {
			t.Fatalf("LoadString(%q): %v", address, err)
		}
		if warnings := strings.Join(cfg.Warnings, "\n"); !strings.Contains(warnings, "client.admin binds ") {
			t.Errorf("binding %q drew no warning; warnings: %v", address, cfg.Warnings)
		}
	}
	for _, address := range []string{"127.0.0.1", "localhost", "::1"} {
		cfg, err := LoadString(base+"bind_addr = \""+address+"\"\n", "embedded", ValidateOptions{Role: RoleClient})
		if err != nil {
			t.Fatalf("LoadString(%q): %v", address, err)
		}
		if warnings := strings.Join(cfg.Warnings, "\n"); strings.Contains(warnings, "client.admin binds ") {
			t.Errorf("binding %q drew the public-API warning; warnings: %v", address, cfg.Warnings)
		}
	}
}

// Run and RunWithShell accept a configuration built in code, which never went
// through Load's defaulting, and these client settings read a zero as an
// immediate or zero duration at some use sites: a zero dial timeout expired the
// deadline the moment it was set, so the client could never log in, and a zero
// reconnect interval turned the backoff into a retry loop.
func TestACodeBuiltConfigurationGetsTheClientDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyClientDefaults()
	if cfg.Client.DialTimeoutSecs != 10 {
		t.Errorf("client.dial_timeout_seconds = %d, want 10", cfg.Client.DialTimeoutSecs)
	}
	if cfg.Client.ReconnectSeconds != 3 {
		t.Errorf("client.reconnect_seconds = %d, want 3", cfg.Client.ReconnectSeconds)
	}
	if cfg.Client.MaxReconnectSeconds != 60 {
		t.Errorf("client.max_reconnect_seconds = %d, want 60", cfg.Client.MaxReconnectSeconds)
	}
	if cfg.Client.HeartbeatSeconds != 30 {
		t.Errorf("client.heartbeat_seconds = %d, want 30", cfg.Client.HeartbeatSeconds)
	}
	if cfg.Client.IdleTimeoutSecs != 300 {
		t.Errorf("client.idle_timeout_seconds = %d, want 300", cfg.Client.IdleTimeoutSecs)
	}
	if cfg.Client.ClientID == "" {
		t.Error("client.client_id was not filled in from the host")
	}

	// A value the operator set is left alone, and the server's section is not
	// touched: those defaults belong to the server's own load.
	set := &Config{}
	set.Client.DialTimeoutSecs = 3
	set.Client.MaxReconnectSeconds = 30
	set.Server.HandshakeTimeoutSecs = 5
	set.ApplyClientDefaults()
	if set.Client.DialTimeoutSecs != 3 || set.Client.MaxReconnectSeconds != 30 {
		t.Errorf("a configured value was overwritten: %+v", set.Client)
	}
	if set.Server.HandshakeTimeoutSecs != 5 {
		t.Errorf("the client defaults touched the server section: %d", set.Server.HandshakeTimeoutSecs)
	}
}
