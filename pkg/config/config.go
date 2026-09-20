// Package config loads the server and client configuration and validates it.
//
// Unknown keys are reported rather than silently ignored: the v1 configuration
// examples shipped hundreds of keys for features that did not exist, and because
// toml decoding ignored them, users had no way to notice that, for example,
// enable_tls had no effect. Set ValidateOptions{RejectUnknownKeys:true} to make
// them fatal.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// ServerConfig is the [server] section.
type ServerConfig struct {
	BindAddr string `toml:"bind_addr"`
	BindPort int    `toml:"bind_port"`

	AuthToken string `toml:"auth_token"`

	MaxConnections       int `toml:"max_connections"`
	HandshakeTimeoutSecs int `toml:"handshake_timeout_seconds"`
	ReadTimeoutSecs      int `toml:"read_timeout_seconds"`
	HeartbeatSeconds     int `toml:"heartbeat_seconds"`
	DialTimeoutSecs      int `toml:"dial_timeout_seconds"`
	GracefulShutdownSecs int `toml:"graceful_shutdown_seconds"`
}

// ClientConfig is the [client] section.
type ClientConfig struct {
	ServerAddr string `toml:"server_addr"`
	AuthToken  string `toml:"auth_token"`

	ReconnectSeconds    int `toml:"reconnect_seconds"`
	MaxReconnectSeconds int `toml:"max_reconnect_seconds"`
	HeartbeatSeconds    int `toml:"heartbeat_seconds"`
	DialTimeoutSecs     int `toml:"dial_timeout_seconds"`
	IdleTimeoutSecs     int `toml:"idle_timeout_seconds"`
}

// ProxyConfig is one [[proxies]] entry.
type ProxyConfig struct {
	Name       string `toml:"name"`
	Type       string `toml:"type"`
	LocalIP    string `toml:"local_ip"`
	LocalPort  int    `toml:"local_port"`
	RemotePort int    `toml:"remote_port"`
}

// LocalAddr returns the address the client forwards to.
func (p ProxyConfig) LocalAddr() string {
	host := p.LocalIP
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, p.LocalPort)
}

// DashboardConfig is the [dashboard] section.
type DashboardConfig struct {
	Enabled  bool   `toml:"enabled"`
	BindAddr string `toml:"bind_addr"`
	Port     int    `toml:"port"`
	// Token protects /api/*. Leave empty only when BindAddr is a loopback
	// address: the dashboard exposes connection and traffic metadata.
	Token string `toml:"token"`
}

// EncryptionConfig is the [encryption] section. Encryption is off by default so
// that an existing deployment keeps working after upgrading; when it is on, both
// peers must use the same algorithm, passphrase and salt.
type EncryptionConfig struct {
	Enabled    bool   `toml:"enabled"`
	Algorithm  string `toml:"algorithm"`
	Passphrase string `toml:"passphrase"`
	Salt       string `toml:"salt"`
}

// ObfuscationConfig is kept so old configuration files still parse. The packet
// obfuscator is not implemented in this version; see docs/SECURITY.md.
type ObfuscationConfig struct {
	Enabled     bool   `toml:"enabled"`
	DefaultType string `toml:"default_type"`
}

// VPNConfig is kept so old configuration files still parse. The VPN data path is
// not implemented in this version; see docs/ARCHITECTURE.md.
type VPNConfig struct {
	Enabled   bool   `toml:"enabled"`
	BindAddr  string `toml:"bind_addr"`
	Port      int    `toml:"port"`
	AuthToken string `toml:"auth_token"`
	Protocol  string `toml:"protocol"`
}

// Config is the whole file.
type Config struct {
	Server      ServerConfig      `toml:"server"`
	Client      ClientConfig      `toml:"client"`
	Dashboard   DashboardConfig   `toml:"dashboard"`
	Encryption  EncryptionConfig  `toml:"encryption"`
	Obfuscation ObfuscationConfig `toml:"obfuscation"`
	VPN         VPNConfig         `toml:"vpn"`
	Proxies     []ProxyConfig     `toml:"proxies"`

	// Warnings collects non-fatal problems found while loading (unknown keys,
	// settings that will be ignored). Callers are expected to log them.
	Warnings []string `toml:"-"`
}

// ValidateOptions controls how strict loading is.
type ValidateOptions struct {
	// RejectUnknownKeys turns unknown TOML keys into an error instead of a
	// warning.
	RejectUnknownKeys bool
	// Role is "server" or "client"; it selects which sections are required.
	Role string
}

const (
	RoleServer = "server"
	RoleClient = "client"
)

// defaults fills in the values that make a minimal config usable.
func (c *Config) applyDefaults() {
	if c.Server.MaxConnections == 0 {
		c.Server.MaxConnections = 512
	}
	if c.Server.HandshakeTimeoutSecs == 0 {
		c.Server.HandshakeTimeoutSecs = 10
	}
	if c.Server.ReadTimeoutSecs == 0 {
		c.Server.ReadTimeoutSecs = 120
	}
	if c.Server.HeartbeatSeconds == 0 {
		c.Server.HeartbeatSeconds = 30
	}
	if c.Server.DialTimeoutSecs == 0 {
		c.Server.DialTimeoutSecs = 10
	}
	if c.Server.GracefulShutdownSecs == 0 {
		c.Server.GracefulShutdownSecs = 5
	}
	if c.Client.ReconnectSeconds == 0 {
		c.Client.ReconnectSeconds = 3
	}
	if c.Client.MaxReconnectSeconds == 0 {
		c.Client.MaxReconnectSeconds = 60
	}
	if c.Client.HeartbeatSeconds == 0 {
		c.Client.HeartbeatSeconds = 30
	}
	if c.Client.DialTimeoutSecs == 0 {
		c.Client.DialTimeoutSecs = 10
	}
	if c.Client.IdleTimeoutSecs == 0 {
		c.Client.IdleTimeoutSecs = 300
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = 7500
	}
	if c.Dashboard.BindAddr == "" {
		c.Dashboard.BindAddr = "127.0.0.1"
	}
	if c.Encryption.Algorithm == "" {
		c.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
	}
	if c.Encryption.Salt == "" {
		c.Encryption.Salt = "aethertunnel"
	}
	for i := range c.Proxies {
		if c.Proxies[i].Type == "" {
			c.Proxies[i].Type = "tcp"
		}
	}
}

// EncryptionPassphrase returns the passphrase used for key derivation. An empty
// [encryption].passphrase falls back to the auth token, so enabling encryption
// does not require distributing a second secret.
func (c *Config) EncryptionPassphrase(role string) string {
	if c.Encryption.Passphrase != "" {
		return c.Encryption.Passphrase
	}
	if role == RoleServer {
		return c.Server.AuthToken
	}
	return c.Client.AuthToken
}

// Cipher builds the configured Cipher. A disabled [encryption] section yields a
// nil Cipher, which every call site treats as "no encryption".
func (c *Config) Cipher(role string) (*crypto.Cipher, error) {
	if !c.Encryption.Enabled {
		return nil, nil
	}
	return crypto.NewCipher(c.Encryption.Algorithm, c.EncryptionPassphrase(role), c.Encryption.Salt)
}

// ListenAddr is the address the server binds.
func (c *Config) ListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.BindAddr, c.Server.BindPort)
}

// DashboardAddr is the address the dashboard binds.
func (c *Config) DashboardAddr() string {
	return fmt.Sprintf("%s:%d", c.Dashboard.BindAddr, c.Dashboard.Port)
}

// Validate checks the configuration for the given role and returns every problem
// it finds, so the user can fix them in one pass.
func (c *Config) Validate(role string) error {
	var problems []string

	switch role {
	case RoleServer:
		if c.Server.BindAddr == "" {
			problems = append(problems, "server.bind_addr is required")
		}
		if c.Server.BindPort < 1 || c.Server.BindPort > 65535 {
			problems = append(problems, fmt.Sprintf("server.bind_port must be 1-65535, got %d", c.Server.BindPort))
		}
		if c.Server.AuthToken == "" {
			problems = append(problems, "server.auth_token is required")
		} else if isWeakToken(c.Server.AuthToken) {
			c.Warnings = append(c.Warnings, "server.auth_token is short or a well-known placeholder; use at least 16 random characters")
		}
	case RoleClient:
		if c.Client.ServerAddr == "" {
			problems = append(problems, "client.server_addr is required")
		} else if _, _, err := splitHostPort(c.Client.ServerAddr); err != nil {
			problems = append(problems, fmt.Sprintf("client.server_addr %q is not host:port", c.Client.ServerAddr))
		}
		if c.Client.AuthToken == "" {
			problems = append(problems, "client.auth_token is required")
		}
	}

	if c.Dashboard.Enabled {
		if c.Dashboard.Port < 1 || c.Dashboard.Port > 65535 {
			problems = append(problems, fmt.Sprintf("dashboard.port must be 1-65535, got %d", c.Dashboard.Port))
		}
		if c.Dashboard.Token == "" && !isLoopback(c.Dashboard.BindAddr) {
			c.Warnings = append(c.Warnings,
				"dashboard.bind_addr is not a loopback address but dashboard.token is empty: "+
					"anyone who can reach that port can read status and traffic metadata")
		}
	}

	if c.Encryption.Enabled {
		switch c.Encryption.Algorithm {
		case crypto.AlgorithmXChaCha20Poly1305, crypto.AlgorithmAES256GCM:
		default:
			problems = append(problems, fmt.Sprintf(
				"encryption.algorithm %q is not supported (use %s or %s)",
				c.Encryption.Algorithm, crypto.AlgorithmXChaCha20Poly1305, crypto.AlgorithmAES256GCM))
		}
		if c.EncryptionPassphrase(role) == "" {
			problems = append(problems, "encryption.enabled is true but neither encryption.passphrase nor auth_token is set")
		}
	}

	if c.Obfuscation.Enabled {
		c.Warnings = append(c.Warnings, "obfuscation.enabled is true but packet obfuscation is not implemented in this version; it will be ignored")
	}
	if c.VPN.Enabled {
		c.Warnings = append(c.Warnings, "vpn.enabled is true but the VPN data path is not implemented in this version; it will be ignored")
	}

	seen := map[string]bool{}
	for _, p := range c.Proxies {
		if p.Name == "" {
			problems = append(problems, "every [[proxies]] entry needs a name")
			continue
		}
		if seen[p.Name] {
			problems = append(problems, fmt.Sprintf("duplicate proxy name %q", p.Name))
		}
		seen[p.Name] = true
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			problems = append(problems, fmt.Sprintf("proxy %q: local_port must be 1-65535, got %d", p.Name, p.LocalPort))
		}
		if p.Type != "" && p.Type != "tcp" {
			problems = append(problems, fmt.Sprintf("proxy %q: type %q is not implemented; only \"tcp\" is supported", p.Name, p.Type))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func isLoopback(addr string) bool {
	return addr == "127.0.0.1" || addr == "::1" || addr == "localhost"
}

func isWeakToken(token string) bool {
	if len(token) < 16 {
		return true
	}
	lower := strings.ToLower(token)
	for _, bad := range []string{"change-me", "changeme", "your-auth-token", "password", "test", "secret", "example"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

func splitHostPort(addr string) (string, string, error) {
	idx := strings.LastIndex(addr, ":")
	if idx <= 0 || idx == len(addr)-1 {
		return "", "", fmt.Errorf("missing port")
	}
	return addr[:idx], addr[idx+1:], nil
}

// Load reads and validates a configuration file.
func Load(filename string, opts ValidateOptions) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", filename, err)
	}

	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", filename, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		message := fmt.Sprintf("config %s contains %d key(s) this version does not understand: %s",
			filename, len(keys), strings.Join(keys, ", "))
		if opts.RejectUnknownKeys {
			return nil, fmt.Errorf("%s", message)
		}
		cfg.Warnings = append(cfg.Warnings, message)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(opts.Role); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadServer loads a server configuration.
func LoadServer(filename string) (*Config, error) {
	return Load(filename, ValidateOptions{Role: RoleServer})
}

// LoadClient loads a client configuration.
func LoadClient(filename string) (*Config, error) {
	return Load(filename, ValidateOptions{Role: RoleClient})
}

// HeartbeatInterval returns the configured heartbeat period as a duration.
func (c *Config) ServerHeartbeatInterval() time.Duration {
	return time.Duration(c.Server.HeartbeatSeconds) * time.Second
}

// ClientHeartbeatInterval returns the configured client heartbeat period.
func (c *Config) ClientHeartbeatInterval() time.Duration {
	return time.Duration(c.Client.HeartbeatSeconds) * time.Second
}
