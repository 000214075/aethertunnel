package config

import (
	"strings"
	"testing"
)

// TestTransportProtocolValidation covers [transport].protocol: only the
// websocket upgrade exists, and the key is a client's concern.
func TestTransportProtocolValidation(t *testing.T) {
	base := `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"
`
	path := writeConfig(t, base+`
[transport]
protocol = "kcp"
`)
	if _, err := LoadClient(path); err == nil || !strings.Contains(err.Error(), "transport.protocol") {
		t.Fatalf("expected a transport.protocol error, got %v", err)
	}

	path = writeConfig(t, base+`
[transport]
protocol = "websocket"
`)
	if _, err := LoadClient(path); err != nil {
		t.Fatalf("websocket was refused: %v", err)
	}

	path = writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[transport]
protocol = "websocket"
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("the server refused the config: %v", err)
	}
	warned := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "transport.protocol has no effect in a server configuration") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the server did not warn about transport.protocol: %v", cfg.Warnings)
	}
}

// TestNewPluginValidation covers the round-seven plugin family: socks5 needs
// its target list and paired credentials, the TLS bridges need their
// certificates.
func TestNewPluginValidation(t *testing.T) {
	cases := []struct {
		name    string
		plugin  string
		body    string
		wantErr string
	}{
		{
			name:   "socks5 without allow_targets",
			plugin: "socks5",
			body: `
[[proxies]]
name = "exit"
type = "tcp"
remote_port = 6005
plugin = "socks5"
`,
			wantErr: "needs allow_targets",
		},
		{
			name:   "socks5 with one-sided credentials",
			plugin: "socks5",
			body: `
[[proxies]]
name = "exit"
type = "tcp"
remote_port = 6005
plugin = "socks5"
allow_targets = ["127.0.0.0/8"]
plugin_user = "ops"
`,
			wantErr: "both or none",
		},
		{
			name:   "https2https without a certificate",
			plugin: "https2https",
			body: `
[[proxies]]
name = "bridge"
type = "tcp"
local_port = 8443
remote_port = 6006
plugin = "https2https"
`,
			wantErr: "plugin_cert_file",
		},
		{
			name:   "tls2raw without a certificate",
			plugin: "tls2raw",
			body: `
[[proxies]]
name = "raw"
type = "tcp"
local_port = 8443
remote_port = 6007
plugin = "tls2raw"
`,
			wantErr: "plugin_cert_file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"
`+tc.body)
			_, err := LoadClient(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
