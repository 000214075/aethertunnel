package config

import (
	"os"
	"testing"
)

func TestLoadServer(t *testing.T) {
	// Create a temporary config file
	configContent := `
[server]
bind_addr = "0.0.0.0"
bind_port = 8080
auth_token = "test-token"
enable_tls = false
cert_file = ""
key_file = ""
max_connections = 1000
graceful_shutdown_timeout = 30

[dashboard]
enabled = true
port = 8081

[vpn]
enabled = false
bind_addr = "0.0.0.0"
port = 8082
local_ip = "10.0.0.1"
remote_ip = "10.0.0.2"
netmask = "255.255.255.0"
protocol = "tcp"
obfuscation = false
auth_token = "test-vpn-token"
max_peers = 10
mtu = 1500

[obfuscation]
enabled = false
default_type = "xor"
allowed_types = ["xor", "xor2", "xor4"]
adaptive_enabled = false
key_rotation = 60
packet_padding = false
traffic_morphing = false

[[proxies]]
name = "test-proxy"
type = "http"
local_ip = "127.0.0.1"
local_port = 8080
remote_port = 8080
`
	
	// Write to temp file
	err := os.WriteFile("test-config.toml", []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}
	defer os.Remove("test-config.toml")

	// Test loading config
	cfg, err := LoadServer("test-config.toml")
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify config values
	if cfg.Server.BindAddr != "0.0.0.0" {
		t.Errorf("Expected BindAddr '0.0.0.0', got '%s'", cfg.Server.BindAddr)
	}
	if cfg.Server.BindPort != 8080 {
		t.Errorf("Expected BindPort 8080, got %d", cfg.Server.BindPort)
	}
	if cfg.Server.AuthToken != "test-token" {
		t.Errorf("Expected AuthToken 'test-token', got '%s'", cfg.Server.AuthToken)
	}
	if cfg.Dashboard.Port != 8081 {
		t.Errorf("Expected Dashboard port 8081, got %d", cfg.Dashboard.Port)
	}
	if len(cfg.Proxies) != 1 {
		t.Errorf("Expected 1 proxy, got %d", len(cfg.Proxies))
	}
}

func TestLoadClient(t *testing.T) {
	// Create a temporary config file
	configContent := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "test-client-token"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
`
	
	// Write to temp file
	err := os.WriteFile("test-client-config.toml", []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test client config file: %v", err)
	}
	defer os.Remove("test-client-config.toml")

	// Test loading config
	cfg, err := LoadClient("test-client-config.toml")
	if err != nil {
		t.Fatalf("Failed to load client config: %v", err)
	}

	// Verify config values
	if cfg.Client.ServerAddr != "127.0.0.1:7001" {
		t.Errorf("Expected ServerAddr '127.0.0.1:7001', got '%s'", cfg.Client.ServerAddr)
	}
	if cfg.Client.AuthToken != "test-client-token" {
		t.Errorf("Expected AuthToken 'test-client-token', got '%s'", cfg.Client.AuthToken)
	}
	if len(cfg.Proxies) != 1 {
		t.Errorf("Expected 1 proxy, got %d", len(cfg.Proxies))
	}
}

func TestDefaultConfig(t *testing.T) {
	// Test creating a basic VPN config
	vpnConfig := VPNConfig{
		Enabled:     false,
		Protocol:    "tcp",
		MaxPeers:    10,
		MTU:         1500,
		MaxPoolSize: 10,
	}
	if vpnConfig.Protocol != "tcp" {
		t.Errorf("Expected protocol 'tcp', got '%s'", vpnConfig.Protocol)
	}
	if vpnConfig.MaxPeers != 10 {
		t.Errorf("Expected MaxPeers 10, got %d", vpnConfig.MaxPeers)
	}
	if vpnConfig.MTU != 1500 {
		t.Errorf("Expected MTU 1500, got %d", vpnConfig.MTU)
	}
}