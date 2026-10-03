package config

import (
	"strings"
	"testing"
)

// TestHTTPPluginsValidation covers the [[http_plugins]] section: the address
// and the ops are what make a webhook answerable, and the section belongs to a
// server.
func TestHTTPPluginsValidation(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[[http_plugins]]
name = "gate"
addr = "http://127.0.0.1:9000"
path = "/hook"
ops = ["login", "kick"]
`)
	_, err := LoadServer(path)
	if err == nil || !strings.Contains(err.Error(), `"kick" is not one of`) {
		t.Fatalf("expected an unknown-op error, got %v", err)
	}

	path = writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[[http_plugins]]
name = "gate"
path = "/hook"
ops = ["login"]
`)
	_, err = LoadServer(path)
	if err == nil || !strings.Contains(err.Error(), "addr is required") {
		t.Fatalf("expected a missing-addr error, got %v", err)
	}

	path = writeConfig(t, `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef0123456789abcdef"

[[http_plugins]]
name = "gate"
addr = "127.0.0.1:9000"
path = "/hook"
ops = ["newProxy"]
timeout_secs = 3
tls_verify = true
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("a valid webhook config was refused: %v", err)
	}
	if len(cfg.HTTPPlugins) != 1 || cfg.HTTPPlugins[0].Addr != "127.0.0.1:9000" ||
		cfg.HTTPPlugins[0].TimeoutSecs != 3 || !cfg.HTTPPlugins[0].TLSVerify {
		t.Fatalf("the plugin round-tripped as %+v", cfg.HTTPPlugins)
	}

	path = writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[http_plugins]]
name = "gate"
addr = "http://127.0.0.1:9000"
ops = ["login"]
`)
	cfg, err = LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "http_plugins has no effect in a client configuration") {
		t.Fatalf("expected a client-config warning, got %v", cfg.Warnings)
	}
}

// TestMetasAndHeadersRoundTrip covers the round-eight client keys: [metas]
// rides the client section, and the header maps ride a proxy.
func TestMetasAndHeadersRoundTrip(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[client.metas]
team = "ops"
rack = "b42"

[[proxies]]
name = "web"
type = "http"
local_port = 8080
remote_port = 0
domains = ["web.example.com"]
request_headers = { X-Aether-Stamp = "from-the-client" }
response_headers = { X-Aether-Back = "server-says" }
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Client.Metas["team"] != "ops" || cfg.Client.Metas["rack"] != "b42" {
		t.Fatalf("metas round-tripped as %v", cfg.Client.Metas)
	}
	proxy := cfg.Proxies[0]
	if proxy.RequestHeaders["X-Aether-Stamp"] != "from-the-client" ||
		proxy.ResponseHeaders["X-Aether-Back"] != "server-says" {
		t.Fatalf("headers round-tripped as %v / %v", proxy.RequestHeaders, proxy.ResponseHeaders)
	}
}
