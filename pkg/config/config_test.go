package config

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/dht"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
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

// A negative duration or count is not a smaller setting, and the program does not
// treat it as one: -1 second of dial timeout is an immediate timeout, so a client
// configured with it never reaches its server, and a negative reconnect interval
// collapses the backoff into a one-second retry loop. On the server side each of
// these keys stops being enforced the moment it is negative, because every use site
// guards with "if the value is positive". All of them used to load as "valid".
func TestNegativeDurationsAndCountsAreRejected(t *testing.T) {
	serverBase := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`
	clientBase := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`
	cases := []struct {
		name   string
		body   string
		client bool
		want   string
	}{
		{"max_connections", serverBase + "max_connections = -5\n",
			false, "server.max_connections cannot be negative"},
		{"handshake_timeout_seconds", serverBase + "handshake_timeout_seconds = -1\n",
			false, "server.handshake_timeout_seconds cannot be negative"},
		{"read_timeout_seconds", serverBase + "read_timeout_seconds = -1\n",
			false, "server.read_timeout_seconds cannot be negative"},
		{"server heartbeat_seconds", serverBase + "heartbeat_seconds = -1\n",
			false, "server.heartbeat_seconds cannot be negative"},
		{"server dial_timeout_seconds", serverBase + "dial_timeout_seconds = -1\n",
			false, "server.dial_timeout_seconds cannot be negative"},
		{"rate_limit_burst", serverBase + "rate_limit_burst = -1\n",
			false, "server.rate_limit_burst cannot be negative"},
		{"reconnect_seconds", clientBase + "reconnect_seconds = -1\n",
			true, "client.reconnect_seconds cannot be negative"},
		{"max_reconnect_seconds", clientBase + "max_reconnect_seconds = -1\n",
			true, "client.max_reconnect_seconds cannot be negative"},
		{"client heartbeat_seconds", clientBase + "heartbeat_seconds = -1\n",
			true, "client.heartbeat_seconds cannot be negative"},
		{"client dial_timeout_seconds", clientBase + "dial_timeout_seconds = -1\n",
			true, "client.dial_timeout_seconds cannot be negative"},
		{"idle_timeout_seconds", clientBase + "idle_timeout_seconds = -1\n",
			true, "client.idle_timeout_seconds cannot be negative"},
		{"dht lookup_timeout_seconds", serverBase + "[dht]\nenabled = true\nlookup_timeout_seconds = -1\n",
			false, "dht.lookup_timeout_seconds cannot be negative"},
		{"a ceiling below the starting value",
			clientBase + "reconnect_seconds = 30\nmax_reconnect_seconds = 5\n",
			true, "is smaller than client.reconnect_seconds"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			var err error
			if tc.client {
				_, err = LoadClient(path)
			} else {
				_, err = LoadServer(path)
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// Padding happens before the frame is written and a frame over the limit is refused by
// its own sender, so a pad_to above the limit refuses every frame: measured with real
// processes, a client configured with pad_to = 1500000 never sent its authentication
// request ("refusing to send 1500000 byte frame (limit 1048576)") and retried forever.
func TestAPaddingTargetAboveTheFrameLimitIsRejected(t *testing.T) {
	body := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[obfuscation]
enabled = true
pad_to = %d
`
	_, err := LoadClient(writeConfig(t, fmt.Sprintf(body, protocol.DefaultMaxPayload+1)))
	if err == nil || !strings.Contains(err.Error(), "obfuscation.pad_to must not exceed") {
		t.Fatalf("expected a pad_to limit error, got %v", err)
	}

	// The limit itself is usable: a payload below it pads up to exactly the limit, which
	// the frame check accepts (it refuses only what is larger).
	if _, err := LoadClient(writeConfig(t, fmt.Sprintf(body, protocol.DefaultMaxPayload))); err != nil {
		t.Fatalf("pad_to at the frame limit was refused: %v", err)
	}
}

// Two listeners of one process cannot share an address, and the failure arrives late and
// beside the point: the server binds what it can, prints that it is listening, and then
// exits with "bind: address already in use" naming an address rather than the two keys
// that disagree. Every one of these pairs is visible in the file, so it is decided before
// anything is bound.
func TestTwoListenersOnOneAddressAreRejected(t *testing.T) {
	serverHead := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`
	clientHead := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "private"
type = "stcp"
local_port = 9
secret_key = "s3cret"
`
	cases := []struct {
		name   string
		body   string
		client bool
		want   string
	}{
		{"http_port on the control port", serverHead + "http_port = 7001\n",
			false, "server.bind_port and server.http_port"},
		{"https_port on the control port", serverHead + "https_port = 7001\nhttps_cert_file = \"/tmp/x.crt\"\nhttps_key_file = \"/tmp/x.key\"\n",
			false, "server.bind_port and server.https_port"},
		{"the dashboard on the control port",
			serverHead + "\n[dashboard]\nenabled = true\nbind_addr = \"127.0.0.1\"\nport = 7001\n",
			false, "server.bind_port and dashboard.port"},
		{"the two shared listeners on one port",
			serverHead + "http_port = 7002\nhttps_port = 7002\nhttps_cert_file = \"/tmp/x.crt\"\nhttps_key_file = \"/tmp/x.key\"\n",
			false, "server.http_port and server.https_port"},
		{"a wildcard control port against a specific dashboard",
			"[server]\nbind_addr = \"0.0.0.0\"\nbind_port = 7001\nauth_token = \"0123456789abcdef0123456789abcdef\"\n" +
				"\n[dashboard]\nenabled = true\nbind_addr = \"127.0.0.1\"\nport = 7001\n",
			false, "server.bind_port and dashboard.port"},
		{"the punching port on the DHT port",
			serverHead + "p2p_port = 7003\n\n[dht]\nenabled = true\nlisten_addr = \"0.0.0.0:7003\"\n",
			false, "dht.listen_addr and server.p2p_port"},
		{"two visitors on one port",
			clientHead + "\n[[visitors]]\nname = \"one\"\ntype = \"stcp\"\nserver_name = \"private\"\nsecret_key = \"s3cret\"\nbind_port = 7004\n" +
				"\n[[visitors]]\nname = \"two\"\ntype = \"stcp\"\nserver_name = \"private\"\nsecret_key = \"s3cret\"\nbind_port = 7004\n",
			true, `visitor "one" bind_port and visitor "two" bind_port`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			var err error
			if tc.client {
				_, err = LoadClient(path)
			} else {
				_, err = LoadServer(path)
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// The rule is about the same address, not about the same number: two listeners on one
// port are fine when they bind different local addresses, which is how a dashboard is
// served on a second interface while the tunnel stays on the first. The shared http and
// https listeners cannot be used as the example: they bind server.bind_addr, so a
// matching http_port is a real conflict.
func TestTheSamePortOnDifferentAddressesIsAccepted(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dashboard]
enabled = true
bind_addr = "192.0.2.1"
port = 7001
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("two listeners on one port but different addresses were refused: %v", err)
	}
	if cfg.Server.BindPort != 7001 || cfg.Dashboard.Port != 7001 {
		t.Fatalf("the ports came out as %d and %d", cfg.Server.BindPort, cfg.Dashboard.Port)
	}

	// And the shared http listener on its own port, next to the control port, is of
	// course fine.
	path = writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
http_port = 7002
`)
	if _, err := LoadServer(path); err != nil {
		t.Fatalf("a control port and a shared http port on different ports were refused: %v", err)
	}
}

// Two tunnels of one client cannot share a public port: the server binds it for whichever
// registers first, and the second is refused with "address already in use" — which names
// the port but not the proxy holding it. Measured with real processes: the client logs the
// refusal and only the first proxy is published. A tcp and a udp tunnel on one port number
// are two sockets, and both work (checked the same way), so they only share the number.
func TestTwoProxiesCannotAskForOnePublicPort(t *testing.T) {
	head := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`
	proxy := func(name, kind string, local, remote int, extra string) string {
		return fmt.Sprintf(`
[[proxies]]
name = %q
type = %q
local_port = %d
remote_port = %d
%s`, name, kind, local, remote, extra)
	}
	socks := "allow_targets = [\"127.0.0.1/32\"]\n"

	cases := []struct {
		name string
		body string
		want string
	}{
		{"two tcp tunnels", head + proxy("first", "tcp", 22, 7002, "") + proxy("second", "tcp", 22, 7002, ""),
			`proxies "first" and "second" both ask for tcp port 7002`},
		{"a tcp and a socks5 tunnel", head + proxy("first", "tcp", 22, 7002, "") +
			proxy("second", "socks5", 0, 7002, socks),
			`proxies "first" and "second" both ask for tcp port 7002`},
		{"two udp tunnels", head + proxy("first", "udp", 53, 7002, "") + proxy("second", "udp", 53, 7002, ""),
			`proxies "first" and "second" both ask for udp port 7002`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadClient(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}

	// The number can be shared across protocols.
	if _, err := LoadClient(writeConfig(t, head+
		proxy("first", "tcp", 22, 7002, "")+proxy("second", "udp", 53, 7002, ""))); err != nil {
		t.Fatalf("a tcp and a udp tunnel on one port number were refused: %v", err)
	}
}

// The rule is "not negative", not "must be positive": zero keeps selecting the
// documented default for every one of these keys.
func TestZeroDurationsAndCountsStillSelectTheDefaults(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
max_connections = 0
handshake_timeout_seconds = 0
read_timeout_seconds = 0
heartbeat_seconds = 0
dial_timeout_seconds = 0
rate_limit_burst = 0
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	for _, tc := range []struct {
		key  string
		got  int
		want int
	}{
		{"server.max_connections", cfg.Server.MaxConnections, 512},
		{"server.handshake_timeout_seconds", cfg.Server.HandshakeTimeoutSecs, 10},
		{"server.read_timeout_seconds", cfg.Server.ReadTimeoutSecs, 120},
		{"server.heartbeat_seconds", cfg.Server.HeartbeatSeconds, 30},
		{"server.dial_timeout_seconds", cfg.Server.DialTimeoutSecs, 10},
		{"server.rate_limit_burst", cfg.Server.RateLimitBurst, 20},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d with a zero in the file, want the default %d", tc.key, tc.got, tc.want)
		}
	}

	clientPath := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
reconnect_seconds = 0
max_reconnect_seconds = 0
heartbeat_seconds = 0
dial_timeout_seconds = 0
idle_timeout_seconds = 0
`)
	client, err := LoadClient(clientPath)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	for _, tc := range []struct {
		key  string
		got  int
		want int
	}{
		{"client.reconnect_seconds", client.Client.ReconnectSeconds, 3},
		{"client.max_reconnect_seconds", client.Client.MaxReconnectSeconds, 60},
		{"client.heartbeat_seconds", client.Client.HeartbeatSeconds, 30},
		{"client.dial_timeout_seconds", client.Client.DialTimeoutSecs, 10},
		{"client.idle_timeout_seconds", client.Client.IdleTimeoutSecs, 300},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d with a zero in the file, want the default %d", tc.key, tc.got, tc.want)
		}
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

// --- [[proxies]] as a server-side policy ---------------------------------------

// TestAServerProxyPolicyNeedsNoLocalService covers what an entry means in a server
// configuration: the policy for a name a client will publish, so the keys that
// describe the client's own service are not required and the type stays unset unless
// the operator pins one.
func TestAServerProxyPolicyNeedsNoLocalService(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "ssh"
remote_port = 6022
allow_cidrs = ["10.0.0.0/8"]
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("a policy without a local service must load: %v", err)
	}
	if cfg.Proxies[0].Type != "" {
		t.Fatalf("the policy pinned type %q; an entry that names no type accepts any", cfg.Proxies[0].Type)
	}
	if cfg.Proxies[0].RemotePort != 6022 {
		t.Fatalf("the policy pins remote_port %d, want 6022", cfg.Proxies[0].RemotePort)
	}
}

// TestAServerProxyPolicyReportsTheKeysItIgnores covers the other half of the same
// rule: a key that describes the client's service is accepted so one file can be
// used for both roles, and it is reported because the server does not act on it.
func TestAServerProxyPolicyReportsTheKeysItIgnores(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "ssh"
local_port = 22
group = "pool"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	warnings := strings.Join(cfg.Warnings, "\n")
	for _, key := range []string{"local_port", "group"} {
		if !strings.Contains(warnings, key+" has no effect in a server configuration") {
			t.Errorf("no warning for %s: %v", key, cfg.Warnings)
		}
	}
}

// TestAServerProxyPolicyIsChecked covers the values a policy does have to get right.
func TestAServerProxyPolicyIsChecked(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`
	for _, tc := range []struct {
		name     string
		policy   string
		problems []string
	}{
		{
			name:     "an unknown type",
			policy:   "[[proxies]]\nname = \"ssh\"\ntype = \"gopher\"\n",
			problems: []string{"type \"gopher\" is not supported"},
		},
		{
			name:     "a visitor list that is not a CIDR",
			policy:   "[[proxies]]\nname = \"ssh\"\nallow_cidrs = [\"10.0.0.0\"]\n",
			problems: []string{"is not a CIDR"},
		},
		{
			name:     "a private type with a public port",
			policy:   "[[proxies]]\nname = \"ssh\"\ntype = \"stcp\"\nremote_port = 6022\n",
			problems: []string{"has no public port"},
		},
		{
			name:     "a multipath setting out of range",
			policy:   "[[proxies]]\nname = \"ssh\"\nmultipath = 99\n",
			problems: []string{"multipath must be 0-"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadServer(writeConfig(t, body+tc.policy))
			if err == nil {
				t.Fatalf("the policy was accepted: %+v", cfg.Proxies)
			}
			for _, problem := range tc.problems {
				if !strings.Contains(err.Error(), problem) {
					t.Errorf("the refusal does not mention %q: %v", problem, err)
				}
			}
		})
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

func TestAuditRetentionDefaultsAndValidation(t *testing.T) {
	base := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`
	cfg, err := LoadServer(writeConfig(t, base+`
[audit]
enabled = true
`))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Audit.Keep != 1 {
		t.Errorf("Audit.Keep = %d, want the default of one generation", cfg.Audit.Keep)
	}

	cfg, err = LoadServer(writeConfig(t, base+`
[audit]
enabled = true
max_bytes = 4096
keep = 5
`))
	if err != nil {
		t.Fatalf("LoadServer with keep = 5: %v", err)
	}
	if cfg.Audit.Keep != 5 {
		t.Errorf("Audit.Keep = %d, want 5", cfg.Audit.Keep)
	}

	// An explicit zero is the default like every other key with a non-zero default,
	// and anything outside the documented range is refused rather than clamped: a
	// typo must not turn into a silent retention of a hundred files.
	cfg, err = LoadServer(writeConfig(t, base+`
[audit]
enabled = true
keep = 0
`))
	if err != nil {
		t.Fatalf("LoadServer with keep = 0: %v", err)
	}
	if cfg.Audit.Keep != 1 {
		t.Errorf("Audit.Keep = %d after an explicit zero, want the default of one", cfg.Audit.Keep)
	}

	if _, err = LoadServer(writeConfig(t, base+`
[audit]
enabled = true
keep = 101
`)); err == nil {
		t.Fatal("keep = 101 was accepted")
	} else if !strings.Contains(err.Error(), "audit.keep must be 0-100") {
		t.Errorf("the refusal does not name the key and its range: %v", err)
	}

	if _, err = LoadServer(writeConfig(t, base+`
[audit]
enabled = true
keep = -1
`)); err == nil {
		t.Fatal("keep = -1 was accepted")
	} else if !strings.Contains(err.Error(), "audit.keep must be 0-100") {
		t.Errorf("the refusal does not name the key and its range: %v", err)
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

// A third of a short announce TTL is zero seconds, and a zero interval means "the
// caller chose nothing" to the discovery node, which would then use its own
// 30-second default: longer than the TTL, so the announcement would lapse before
// it was rewritten and the name would stop resolving while the server was running.
func TestDHTRepublishIntervalStaysBelowAShortAnnounceTTL(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
announce_ttl_seconds = 2
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.DHT.RepublishSeconds != 1 {
		t.Errorf("DHT.RepublishSeconds = %d, want 1 for a 2-second announce TTL",
			cfg.DHT.RepublishSeconds)
	}
	if cfg.DHT.RepublishSeconds >= cfg.DHT.AnnounceTTLSeconds {
		t.Fatalf("the derived republish interval %ds does not fit inside the announce TTL %ds",
			cfg.DHT.RepublishSeconds, cfg.DHT.AnnounceTTLSeconds)
	}
	// The interval the node is actually given, not just the number in the file.
	if interval := cfg.DHTSettings(discardConfigLogger()).RepublishInterval; interval >= time.Duration(cfg.DHT.AnnounceTTLSeconds)*time.Second {
		t.Errorf("the node republishes every %s, which is not inside the %ds announce TTL",
			interval, cfg.DHT.AnnounceTTLSeconds)
	}
}

// Every [dht] duration is handed to the node. A key that is parsed and then dropped
// would leave the node on its own defaults, and the default that matters is the
// republish interval: too long an interval lets an announcement lapse while the server
// that published it is still running.
func TestDHTDurationsReachTheNode(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[dht]
enabled = true
ttl_seconds = 300
announce_ttl_seconds = 120
republish_seconds = 30
lookup_timeout_seconds = 7
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	settings := cfg.DHTSettings(discardConfigLogger())
	for _, tc := range []struct {
		key  string
		got  time.Duration
		want time.Duration
	}{
		{"dht.ttl_seconds", settings.TTL, 300 * time.Second},
		{"dht.announce_ttl_seconds", settings.AnnounceTTL, 120 * time.Second},
		{"dht.republish_seconds", settings.RepublishInterval, 30 * time.Second},
		{"dht.lookup_timeout_seconds", settings.LookupTimeout, 7 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s reached the node as %s, want %s", tc.key, tc.got, tc.want)
		}
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
			name: "announce ttl too short to republish within",
			body: base + "\n[dht]\nenabled = true\nannounce_ttl_seconds = 1\n",
			want: "dht.announce_ttl_seconds (1) is too short",
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
