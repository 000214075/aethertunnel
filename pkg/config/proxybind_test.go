package config

import (
	"strings"
	"testing"
)

const proxyBindBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`

// Published listeners follow the control address unless the operator moves
// them, which is how frp's proxyBindAddr defaults.
func TestProxyBindAddrDefaultsToTheControlAddress(t *testing.T) {
	path := writeConfig(t, proxyBindBase+"http_port = 8080\n")
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Server.ProxyBindAddr != "127.0.0.1" {
		t.Fatalf("server.proxy_bind_addr is %q, want the control address 127.0.0.1", cfg.Server.ProxyBindAddr)
	}
	if want := "127.0.0.1:8080"; cfg.HTTPAddr() != want {
		t.Errorf("HTTPAddr() = %q, want %q", cfg.HTTPAddr(), want)
	}
}

// An explicit proxy_bind_addr moves every published listener, leaving the
// control port where it was.
func TestProxyBindAddrMovesThePublishedListeners(t *testing.T) {
	body := `
[server]
bind_addr = "10.0.0.1"
bind_port = 7001
proxy_bind_addr = "192.168.1.9"
auth_token = "0123456789abcdef0123456789abcdef"
http_port = 8080
tcpmux_port = 8444
https_passthrough_port = 8445
`
	path := writeConfig(t, body)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if got := cfg.ListenAddr(); got != "10.0.0.1:7001" {
		t.Errorf("ListenAddr() = %q, want the control address unchanged", got)
	}
	if want := "192.168.1.9:8080"; cfg.HTTPAddr() != want {
		t.Errorf("HTTPAddr() = %q, want %q", cfg.HTTPAddr(), want)
	}
	if cfg.ProxyHost() != "192.168.1.9" {
		t.Errorf("ProxyHost() = %q, want 192.168.1.9", cfg.ProxyHost())
	}
}

// The address is bound directly, so a name that is not an IP is refused before
// anything tries to listen on it.
func TestProxyBindAddrMustBeAnIP(t *testing.T) {
	path := writeConfig(t, proxyBindBase+"proxy_bind_addr = \"published.example\"\n")
	_, err := LoadServer(path)
	if err == nil {
		t.Fatal("LoadServer accepted a non-IP server.proxy_bind_addr")
	}
	if !strings.Contains(err.Error(), "server.proxy_bind_addr") {
		t.Errorf("error %q does not name the offending key", err.Error())
	}
}

// Two proxy listeners on one address still collide, and the check runs on the
// address they actually bind, not on the control address.
func TestProxyBindAddrIsWhatTheListenerConflictChecks(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
proxy_bind_addr = "10.0.0.5"
auth_token = "0123456789abcdef0123456789abcdef"
http_port = 7001
https_port = 7001
`
	path := writeConfig(t, body)
	if _, err := LoadServer(path); err == nil {
		t.Fatal("two proxy listeners on 10.0.0.5:7001 were accepted")
	}

	// The same port on the control address and on the proxy address is two
	// different addresses, so this one is legal: without using the proxy host
	// the check would report a collision that cannot happen.
	body = `
[server]
bind_addr = "10.0.0.5"
bind_port = 7001
proxy_bind_addr = "127.0.0.1"
auth_token = "0123456789abcdef0123456789abcdef"
http_port = 7001
`
	path = writeConfig(t, body)
	if _, err := LoadServer(path); err != nil {
		t.Fatalf("the same port on two different addresses was refused: %v", err)
	}
}

// A keep-alive interval is a duration: a negative one would mean a probe
// before the connection exists.
func TestTCPKeepAliveRejectsNegativeIntervals(t *testing.T) {
	path := writeConfig(t, proxyBindBase+"tcp_keepalive_seconds = -1\n")
	if _, err := LoadServer(path); err == nil {
		t.Fatal("LoadServer accepted a negative server.tcp_keepalive_seconds")
	}

	clientBody := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
tcp_keepalive_seconds = -1
`
	path = writeConfig(t, clientBody)
	if _, err := LoadClient(path); err == nil {
		t.Fatal("LoadClient accepted a negative client.tcp_keepalive_seconds")
	}
}
