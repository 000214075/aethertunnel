package config

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/dht"
)

// discardConfigLogger silences the diagnostics DHTSettings emits.
func discardConfigLogger() *log.Logger { return log.New(io.Discard, "", 0) }

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

// TestAnHTTPProxyWithoutDomainsIsAcceptedWithAWarning covers the one way an http
// proxy can be published without the client naming its hostname: the server
// publishes it as <proxy-name>.<server.subdomain_host>. Only the server knows that
// setting, so refusing the configuration here would make the setting unusable —
// which is what used to happen.
func TestAnHTTPProxyWithoutDomainsIsAcceptedWithAWarning(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "example.com:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "web"
type = "http"
local_port = 8080
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("a domain-less http proxy must load: %v", err)
	}
	if len(cfg.Proxies) != 1 || len(cfg.Proxies[0].Domains) != 0 {
		t.Fatalf("the proxy was parsed into %+v", cfg.Proxies)
	}

	warnings := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(warnings, "subdomain_host") {
		t.Fatalf("no warning about how the proxy will be reached: %v", cfg.Warnings)
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

// --- announcement signing -----------------------------------------------------

func TestLocalAddrIsEmptyForASocks5Proxy(t *testing.T) {
	// A socks5 tunnel forwards to the address the visitor names, so there is no
	// local service to report; the dashboard shows a dash for it.
	socks := ProxyConfig{Name: "exit", Type: ProxyTypeSOCKS, LocalPort: 0}
	if got := socks.LocalAddr(); got != "" {
		t.Errorf("LocalAddr() for a socks5 proxy is %q, want an empty string", got)
	}

	tcp := ProxyConfig{Name: "ssh", Type: ProxyTypeTCP, LocalPort: 22}
	if got := tcp.LocalAddr(); got != "127.0.0.1:22" {
		t.Errorf("LocalAddr() for a tcp proxy is %q, want 127.0.0.1:22", got)
	}

	// An explicit local_ip still counts, including on a socks5 proxy whose address
	// is not used: only the type decides.
	explicit := ProxyConfig{Name: "web", Type: ProxyTypeTCP, LocalIP: "10.0.0.5", LocalPort: 8080}
	if got := explicit.LocalAddr(); got != "10.0.0.5:8080" {
		t.Errorf("LocalAddr() is %q, want 10.0.0.5:8080", got)
	}
}

// A key pair in the form identity.allowed_keys and dht.trusted_keys accept.
const dhtTestKey = "8d5c1b6a1f3f2a4b0c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3"

func TestDHTSignerAndReaderKeysLoad(t *testing.T) {
	serverPath := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
advertise_host = "tunnel.example"
signing_key_file = "dht.key"
`)
	cfg, err := LoadServer(serverPath)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.DHT.SigningKeyFile != "dht.key" {
		t.Errorf("DHT.SigningKeyFile = %q, want dht.key", cfg.DHT.SigningKeyFile)
	}
	if warnings := strings.Join(cfg.Warnings, "\n"); strings.Contains(warnings, "announcements are unsigned") {
		t.Errorf("a server with a signing key was warned about unsigned announcements: %v", cfg.Warnings)
	}

	clientPath := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
require_signed = true
trusted_keys = ["`+dhtTestKey+`"]
`)
	clientCfg, err := LoadClient(clientPath)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !clientCfg.DHT.RequireSigned {
		t.Error("DHT.RequireSigned was not loaded")
	}
	keys, err := clientCfg.DHTTrustedKeys()
	if err != nil {
		t.Fatalf("DHTTrustedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("DHTTrustedKeys returned %d keys, want 1", len(keys))
	}
	if warnings := strings.Join(clientCfg.Warnings, "\n"); strings.Contains(warnings, "dht.require_signed is false") {
		t.Errorf("a client with a reader policy was warned that it accepts anything: %v", clientCfg.Warnings)
	}

	settings := clientCfg.DHTSettings(discardConfigLogger())
	if !settings.RequireSigned {
		t.Error("DHTSettings dropped require_signed")
	}
	if len(settings.TrustedKeys) != 1 || !settings.TrustedKeys[0].Equal(keys[0]) {
		t.Errorf("DHTSettings carries %d trusted keys, want the one from the file", len(settings.TrustedKeys))
	}
}

func TestDHTTrustedKeysRejectAValueThatIsNotAKey(t *testing.T) {
	path := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
trusted_keys = ["not-hex"]
`)
	_, err := LoadClient(path)
	if err == nil || !strings.Contains(err.Error(), "dht.trusted_keys entry") {
		t.Fatalf("expected a trusted_keys error, got %v", err)
	}
}

func TestDHTTrustedKeysRejectAKeyOfTheWrongLength(t *testing.T) {
	path := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
trusted_keys = ["aabbcc"]
`)
	_, err := LoadClient(path)
	if err == nil {
		t.Fatal("a key that is not 32 bytes was accepted")
	}
}

func TestDHTUnsignedDeploymentWarnsOnTheServer(t *testing.T) {
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
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "dht.signing_key_file is empty") {
		t.Fatalf("expected a warning that announcements are unsigned, got %v", cfg.Warnings)
	}
}

func TestDHTReaderSettingsOnTheServerRoleWarn(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
signing_key_file = "dht.key"
require_signed = true
trusted_keys = ["`+dhtTestKey+`"]
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	warnings := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(warnings, "dht.require_signed has no effect in a server configuration") {
		t.Errorf("expected a warning about require_signed on the server role, got %v", cfg.Warnings)
	}
	if !strings.Contains(warnings, "dht.trusted_keys has no effect in a server configuration") {
		t.Errorf("expected a warning about trusted_keys on the server role, got %v", cfg.Warnings)
	}
}

func TestDHTReaderWithoutAPolicyWarns(t *testing.T) {
	path := writeConfig(t, `
[client]
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
discover = "ssh"
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "dht.trusted_keys is empty") {
		t.Fatalf("expected a warning that any record is accepted, got %v", cfg.Warnings)
	}
}

// --- environment overrides ----------------------------------------------------

func TestEnvironmentSuppliesTheCredentialsTheFileOmits(t *testing.T) {
	// A container configuration: the file holds no secret at all, because the
	// ConfigMap it is stored in is readable by anyone with access to the namespace.
	path := writeConfig(t, `
[server]
bind_addr = "0.0.0.0"
bind_port = 7001

[dashboard]
enabled = true
bind_addr = "0.0.0.0"
port = 7500
`)
	t.Setenv(EnvAuthToken, "from-the-environment-0123456789")
	t.Setenv(EnvDashboardToken, "dashboard-token-0123456789")

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Server.AuthToken != "from-the-environment-0123456789" {
		t.Errorf("auth_token is %q, want the value from the environment", cfg.Server.AuthToken)
	}
	if cfg.Dashboard.Token != "dashboard-token-0123456789" {
		t.Errorf("dashboard.token is %q, want the value from the environment", cfg.Dashboard.Token)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), EnvAuthToken) {
		t.Errorf("expected a warning naming the overriding variable, got %v", cfg.Warnings)
	}
}

func TestEnvironmentPassphraseOverridesTheFile(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[encryption]
enabled = true
passphrase = "from-the-file"
`)
	t.Setenv(EnvEncryptionPassphrase, "from-the-environment")

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if got := cfg.EncryptionPassphrase(RoleServer); got != "from-the-environment" {
		t.Fatalf("the passphrase is %q, want the value from the environment", got)
	}
}

func TestAnUnsetEnvironmentLeavesTheFileAlone(t *testing.T) {
	t.Setenv(EnvAuthToken, "")
	t.Setenv(EnvDashboardToken, "")
	t.Setenv(EnvEncryptionPassphrase, "")

	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[encryption]
enabled = true
passphrase = "from-the-file"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Server.AuthToken != "0123456789abcdef0123456789abcdef" {
		t.Errorf("auth_token is %q, want the file's value", cfg.Server.AuthToken)
	}
	if got := cfg.EncryptionPassphrase(RoleServer); got != "from-the-file" {
		t.Errorf("the passphrase is %q, want the file's value", got)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "overrode the configuration file") {
			t.Errorf("an unset variable reported an override: %q", warning)
		}
	}
}

func TestEnvironmentAppliesToAClientAsWell(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "example.com:7001"
`)
	t.Setenv(EnvAuthToken, "client-token-0123456789ab")

	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Client.AuthToken != "client-token-0123456789ab" {
		t.Fatalf("client.auth_token is %q, want the value from the environment", cfg.Client.AuthToken)
	}
}
