package config

import (
	"strconv"
	"strings"
	"testing"
)

// A role-less load — what --dht-lookup uses, since a lookup only needs [dht] —
// must not judge a server file's [[proxies]] as client services. It did, so a
// policy entry such as server.toml.example's was refused for having no
// local_port even though the same binary accepts it for --check.
func TestARoleLessLoadLeavesServerPolicyEntriesAlone(t *testing.T) {
	const doc = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef"

[[proxies]]
name = "ssh"
remote_port = 6022
allow_cidrs = ["10.0.0.0/8"]
`
	if _, err := LoadString(doc, "server.toml", ValidateOptions{Role: ""}); err != nil {
		t.Fatalf("a role-less load refused a server policy entry: %v", err)
	}
	if _, err := LoadString(doc, "server.toml", ValidateOptions{Role: RoleServer}); err != nil {
		t.Fatalf("the server role refused the same file: %v", err)
	}
	// The client role is the one that does need a local service.
	if _, err := LoadString(doc, "server.toml", ValidateOptions{Role: RoleClient}); err == nil {
		t.Fatal("the client role accepted a policy entry with no local_port")
	}
}

// visitor.fallback_timeout_ms is multiplied by a millisecond, so a value that
// overflows the conversion makes the context WithTimeout builds already expired
// and the fallback is used at once instead of after the window.
func TestAFallbackTimeoutThatWouldOverflowIsRefused(t *testing.T) {
	if strconv.IntSize == 32 {
		// The field is an int, so on a 32-bit target the value that would overflow a
		// duration cannot be represented at all: the ceiling is beyond int there and
		// int(MaxDurationMillis) would not compile.
		t.Skip("fallback_timeout_ms cannot reach the millisecond ceiling on a 32-bit int")
	}
	// A variable, so the conversion to int is a runtime one: int() applied to the
	// typed constant does not compile on a 32-bit target even though the skip above
	// means it is never evaluated there.
	ceiling := MaxDurationMillis
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7001"
	cfg.Client.AuthToken = "0123456789abcdef"
	cfg.Visitors = []VisitorConfig{{
		Name: "v", Type: ProxyTypeSTCP, ServerName: "svc", SecretKey: "s3cret",
		AuthMethod: AuthMethodSecret, BindPort: 7100,
		FallbackTo: "127.0.0.1:9000", FallbackTimeoutMs: int(ceiling) + 1,
	}}
	err := cfg.Validate(RoleClient)
	if err == nil {
		t.Fatal("a fallback timeout that overflows a duration was accepted")
	}
	if !strings.Contains(err.Error(), "fallback_timeout_ms") {
		t.Fatalf("the refusal does not name the setting: %v", err)
	}
	cfg.Visitors[0].FallbackTimeoutMs = int(ceiling)
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("the largest value that still fits was refused: %v", err)
	}
}

// The same ceiling for server.vhost_http_timeout: it is only checked for being
// negative, and a wrapped value is read as "unset", so the bound the operator
// wrote silently became the dial timeout.
func TestAVhostHTTPTimeoutThatWouldOverflowIsRefused(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Server.VhostHTTPTimeout = MaxDurationSeconds + 1
	err := cfg.Validate(RoleServer)
	if err == nil {
		t.Fatal("a vhost http timeout that overflows a duration was accepted")
	}
	if !strings.Contains(err.Error(), "server.vhost_http_timeout") {
		t.Fatalf("the refusal does not name the setting: %v", err)
	}
}

// The dashboard's bind address defaults to loopback, so an empty one in a
// configuration built in code must not bind every interface — the dashboard
// carries the server's control surface.
func TestAnEmptyDashboardBindDefaultsToLoopback(t *testing.T) {
	cfg := &Config{}
	cfg.Dashboard.Enabled = true
	cfg.Dashboard.Port = 7500
	if got := cfg.DashboardAddr(); got != "127.0.0.1:7500" {
		t.Fatalf("DashboardAddr = %q, want the loopback default", got)
	}
	cfg.Dashboard.BindAddr = "0.0.0.0"
	if got := cfg.DashboardAddr(); got != "0.0.0.0:7500" {
		t.Fatalf("DashboardAddr = %q, want the address that was set", got)
	}
}

// metrics.bind_addr defaults to loopback too, so the warning about an
// unauthenticated listener must not fire for an empty address.
func TestNoMetricsWarningForAnAddressThatDefaultsToLoopback(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Metrics.Enabled = true
	cfg.Metrics.Port = 9100
	cfg.Metrics.Token = ""
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "metrics.bind_addr") {
			t.Fatalf("an empty bind address that defaults to loopback drew a warning: %s", warning)
		}
	}

	cfg.Warnings = nil
	cfg.Metrics.BindAddr = "0.0.0.0"
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "metrics.bind_addr") {
			found = true
		}
	}
	if !found {
		t.Fatal("a metrics listener on every interface with no token drew no warning")
	}
}

// A probe connects to local_ip:local_port over TCP, and a datagram proxy's local
// service is a UDP port, so the probe would fail every time and the client would
// withdraw a working tunnel for good.
func TestHealthCheckOnADatagramProxyIsRefused(t *testing.T) {
	const doc = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef"

[[proxies]]
name = "dns"
type = "udp"
local_ip = "127.0.0.1"
local_port = 5353
remote_port = 5353

[proxies.health_check]
type = "tcp"
`
	_, err := LoadString(doc, "client.toml", ValidateOptions{Role: RoleClient})
	if err == nil {
		t.Fatal("health_check on a udp proxy was accepted")
	}
	if !strings.Contains(err.Error(), "UDP port") {
		t.Fatalf("the refusal does not explain why: %v", err)
	}
}
