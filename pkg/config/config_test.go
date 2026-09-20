package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/dht"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestServerDefaultsAreApplied(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	if cfg.Server.MaxConnections != 512 {
		t.Errorf("MaxConnections = %d, want 512", cfg.Server.MaxConnections)
	}
	if cfg.Server.HeartbeatSeconds != 30 {
		t.Errorf("HeartbeatSeconds = %d, want 30", cfg.Server.HeartbeatSeconds)
	}
	if cfg.Server.HandshakeTimeoutSecs != 10 {
		t.Errorf("HandshakeTimeoutSecs = %d, want 10", cfg.Server.HandshakeTimeoutSecs)
	}
	if cfg.Encryption.Algorithm != "xchacha20-poly1305" {
		t.Errorf("Algorithm = %q, want the default cipher", cfg.Encryption.Algorithm)
	}
	if cfg.ListenAddr() != "127.0.0.1:7001" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr())
	}
}

func TestValidationRejectsBrokenServers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing token",
			body: "[server]\nbind_addr = \"127.0.0.1\"\nbind_port = 7001\n",
			want: "auth_token is required",
		},
		{
			name: "port out of range",
			body: "[server]\nbind_addr = \"127.0.0.1\"\nbind_port = 70000\nauth_token = \"0123456789abcdef0123456789abcdef\"\n",
			want: "bind_port must be 1-65535",
		},
		{
			name: "missing bind address",
			body: "[server]\nbind_port = 7001\nauth_token = \"0123456789abcdef0123456789abcdef\"\n",
			want: "bind_addr is required",
		},
		{
			name: "unsupported proxy type",
			body: `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "web"
type = "carrier-pigeon"
local_port = 80
`,
			want: `type "carrier-pigeon" is not supported`,
		},
		{
			name: "duplicate proxy names",
			body: `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "web"
local_port = 80

[[proxies]]
name = "web"
local_port = 81
`,
			want: `duplicate proxy name "web"`,
		},
		{
			name: "unsupported cipher",
			body: `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[encryption]
enabled = true
algorithm = "rot13"
`,
			want: `algorithm "rot13" is not supported`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadServer(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestUnknownKeysAreReportedNotIgnored(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[webrtc]
enabled = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("unknown keys should warn, not fail: %v", err)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "webrtc") {
		t.Fatalf("expected a warning naming the unknown key, got %q", joined)
	}

	_, err = Load(path, ValidateOptions{Role: RoleServer, RejectUnknownKeys: true})
	if err == nil || !strings.Contains(err.Error(), "does not understand") {
		t.Fatalf("RejectUnknownKeys should fail, got %v", err)
	}
}

func TestWeakTokenWarns(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "change-me"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "auth_token is short") {
		t.Fatalf("expected a weak-token warning, got %v", cfg.Warnings)
	}
}

func TestExposedDashboardWithoutTokenWarns(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dashboard]
enabled = true
bind_addr = "0.0.0.0"
port = 7500
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "dashboard.token is empty") {
		t.Fatalf("expected a dashboard warning, got %v", cfg.Warnings)
	}
}

func TestEncryptionPassphraseFallsBackToToken(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[encryption]
enabled = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if got := cfg.EncryptionPassphrase(RoleServer); got != cfg.Server.AuthToken {
		t.Fatalf("passphrase %q should fall back to the auth token", got)
	}

	cipher, err := cfg.Cipher(RoleServer)
	if err != nil {
		t.Fatalf("Cipher: %v", err)
	}
	if !cipher.Enabled() || cipher.Algorithm() != "xchacha20-poly1305" {
		t.Fatalf("cipher = %v/%s, want an enabled xchacha20-poly1305", cipher.Enabled(), cipher.Algorithm())
	}
}

func TestDisabledEncryptionYieldsNoCipher(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	cipher, err := cfg.Cipher(RoleServer)
	if err != nil {
		t.Fatalf("Cipher: %v", err)
	}
	if cipher.Enabled() {
		t.Fatal("encryption is disabled in the file but the cipher is enabled")
	}
	if got := cipher.Algorithm(); got != "none" {
		t.Fatalf("Algorithm = %q, want none", got)
	}
}

func TestClientValidation(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "127.0.0.1"
auth_token = "0123456789abcdef0123456789abcdef"
`)
	_, err := LoadClient(path)
	if err == nil || !strings.Contains(err.Error(), "is not host:port") {
		t.Fatalf("expected a server_addr format error, got %v", err)
	}

	path = writeConfig(t, `
[client]
server_addr = "example.com:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "ssh"
local_port = 22
remote_port = 6022
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Proxies[0].LocalAddr() != "127.0.0.1:22" {
		t.Fatalf("LocalAddr = %q, want 127.0.0.1:22", cfg.Proxies[0].LocalAddr())
	}
	if cfg.Proxies[0].Type != "tcp" {
		t.Fatalf("proxy type default = %q, want tcp", cfg.Proxies[0].Type)
	}
}

// --- bandwidth ledger ---------------------------------------------------------

func TestLedgerDefaultsAreApplied(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[ledger]
enabled = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Ledger.Path != "aethertunnel-ledger.jsonl" {
		t.Errorf("Ledger.Path = %q, want the default file", cfg.Ledger.Path)
	}
	if cfg.Ledger.SigningKey != "aethertunnel-ledger.key" {
		t.Errorf("Ledger.SigningKey = %q, want the default key file", cfg.Ledger.SigningKey)
	}
}

func TestDisabledLedgerNeedsNoPaths(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Ledger.Enabled {
		t.Fatal("the ledger is enabled without a [ledger] section")
	}
	if cfg.Ledger.Path != "" {
		t.Errorf("a disabled ledger has path %q, want empty", cfg.Ledger.Path)
	}
}

func TestLedgerInAClientConfigurationWarns(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "example.com:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[ledger]
enabled = true
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "bandwidth ledger is kept by the server") {
		t.Fatalf("expected a warning that the ledger is server-side, got %v", cfg.Warnings)
	}
}

// --- distributed hash table ---------------------------------------------------

func TestDHTDefaultsAreApplied(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.DHT.ListenAddr != DefaultDHTAddr {
		t.Errorf("DHT.ListenAddr = %q, want %q", cfg.DHT.ListenAddr, DefaultDHTAddr)
	}
	if cfg.DHT.Namespace != DefaultDHTNamespace {
		t.Errorf("DHT.Namespace = %q, want %q", cfg.DHT.Namespace, DefaultDHTNamespace)
	}
	if cfg.DHT.AnnounceTTLSeconds != 90 {
		t.Errorf("DHT.AnnounceTTLSeconds = %d, want 90", cfg.DHT.AnnounceTTLSeconds)
	}
	if cfg.DHT.RepublishSeconds != 30 {
		t.Errorf("DHT.RepublishSeconds = %d, want a third of the announce TTL", cfg.DHT.RepublishSeconds)
	}
	if cfg.DHT.LookupTimeoutSeconds != 5 {
		t.Errorf("DHT.LookupTimeoutSeconds = %d, want 5", cfg.DHT.LookupTimeoutSeconds)
	}
}

func TestDHTValidation(t *testing.T) {
	base := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "listen address without a port",
			body: base + "\n[dht]\nenabled = true\nlisten_addr = \"127.0.0.1\"\n",
			want: "dht.listen_addr",
		},
		{
			name: "malformed node id",
			body: base + "\n[dht]\nenabled = true\nnode_id = \"zz\"\n",
			want: "dht.node_id",
		},
		{
			name: "malformed bootstrap entry",
			body: base + "\n[dht]\nenabled = true\nbootstrap = [\"example.com\"]\n",
			want: "dht.bootstrap entry",
		},
		{
			name: "republish loop slower than the announce TTL",
			body: base + "\n[dht]\nenabled = true\nannounce_ttl_seconds = 10\nrepublish_seconds = 30\n",
			want: "must be shorter than dht.announce_ttl_seconds",
		},
		{
			name: "dht ttl shorter than the announce ttl",
			body: base + "\n[dht]\nenabled = true\nannounce_ttl_seconds = 120\nttl_seconds = 60\n",
			want: "must not be shorter than dht.announce_ttl_seconds",
		},
		{
			name: "advertise host with a port",
			body: base + "\n[dht]\nenabled = true\nadvertise_host = \"example.com:7001\"\n",
			want: "contains a port",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadServer(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDHTWildcardBindAddressWarns(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "dht.advertise_host is empty") {
		t.Fatalf("expected a warning about the wildcard bind address, got %v", cfg.Warnings)
	}
}

func TestDHTDiscoverySatisfiesTheClientAddressRequirement(t *testing.T) {
	path := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("a client with discovery and no server_addr was rejected: %v", err)
	}
	if cfg.DHT.Discover != "ssh" {
		t.Errorf("DHT.Discover = %q, want ssh", cfg.DHT.Discover)
	}
}

func TestClientWithoutAnAddressOrDiscoveryIsRejected(t *testing.T) {
	path := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"
`)
	_, err := LoadClient(path)
	if err == nil || !strings.Contains(err.Error(), "client.server_addr is required") {
		t.Fatalf("expected a missing server_addr error, got %v", err)
	}
}

func TestDHTDiscoverAlongsideAConfiguredAddressWarns(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "example.com:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "the configured address is used") {
		t.Fatalf("expected a warning that client.server_addr wins, got %v", cfg.Warnings)
	}
}

func TestDHTNodeIDRoundTrip(t *testing.T) {
	cfg := &Config{}
	if id, err := cfg.DHTNodeID(); err != nil || id != (dht.ID{}) {
		t.Fatalf("an empty node id should select the zero ID, got %v/%v", id, err)
	}

	cfg.DHT.NodeID = "00000000000000000000000000000000000000ff"
	id, err := cfg.DHTNodeID()
	if err != nil {
		t.Fatalf("DHTNodeID: %v", err)
	}
	if id[19] != 0xff {
		t.Errorf("the parsed node id ends in %#x, want 0xff", id[19])
	}
}
