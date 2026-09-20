package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
type = "udp"
local_port = 80
`,
			want: `type "udp" is not implemented`,
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
