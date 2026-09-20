// Package config loads the server and client configuration and validates it.
//
// Unknown keys are reported rather than silently ignored: the v1 configuration
// examples shipped hundreds of keys for features that did not exist, and because
// toml decoding ignored them, users had no way to notice that, for example,
// enable_tls had no effect. Set ValidateOptions{RejectUnknownKeys:true} to make
// them fatal.
package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
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

	// Access control applied to every incoming connection before the handshake.
	AllowCIDRs []string `toml:"allow_cidrs"`
	DenyCIDRs  []string `toml:"deny_cidrs"`

	// Per-source-IP connection rate limit applied before the handshake. Zero
	// disables the limiter.
	RateLimitPerSecond float64 `toml:"rate_limit_per_second"`
	RateLimitBurst     int     `toml:"rate_limit_burst"`

	// LoadBalance selects how visitors are distributed when several clients
	// publish the same proxy name: "round-robin", "random", "latency" or
	// "failover" (first healthy client only).
	LoadBalance string `toml:"load_balance"`

	// HTTPPort publishes http proxies on one shared listener; the request's Host
	// header selects the tunnel. Zero disables HTTP virtual hosting.
	HTTPPort int `toml:"http_port"`
	// HTTPSPort does the same over TLS and requires both certificate files.
	HTTPSPort     int    `toml:"https_port"`
	HTTPSCertFile string `toml:"https_cert_file"`
	HTTPSKeyFile  string `toml:"https_key_file"`

	// SubdomainHost, when set, lets an http proxy without explicit domains be
	// reached at <proxy-name>.<subdomain_host>.
	SubdomainHost string `toml:"subdomain_host"`

	// P2PPort is the UDP rendezvous port used by xtcp hole punching. It is
	// served by the same process as the control port; zero disables xtcp.
	P2PPort int `toml:"p2p_port"`
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

	// Domains lists the Host values that reach an http/https proxy. An entry may
	// start with "*." to match any single- or multi-label subdomain.
	Domains []string `toml:"domains"`
	// SecretKey gates a private proxy (stcp, sudp, xtcp). A visitor must present
	// the same value. Enable [encryption] so the key is not sent in the clear.
	SecretKey string `toml:"secret_key"`
	// AuthMethod selects how a visitor proves it may use a private proxy:
	// "secret" sends the secret key, "nizk" proves knowledge of it with a
	// Schnorr proof so the key never leaves the visitor.
	AuthMethod string `toml:"auth_method"`
}

// Proxy auth methods accepted in [[proxies]].auth_method.
const (
	AuthMethodSecret = "secret"
	AuthMethodNIZK   = "nizk"
)

// Proxy types accepted in [[proxies]].
const (
	ProxyTypeTCP   = "tcp"
	ProxyTypeUDP   = "udp"
	ProxyTypeHTTP  = "http"
	ProxyTypeHTTPS = "https"
	ProxyTypeSTCP  = "stcp"
	ProxyTypeSUDP  = "sudp"
	ProxyTypeXTCP  = "xtcp"
)

// ProxyTypes lists every type this build implements.
var ProxyTypes = []string{
	ProxyTypeTCP, ProxyTypeUDP, ProxyTypeHTTP, ProxyTypeHTTPS,
	ProxyTypeSTCP, ProxyTypeSUDP, ProxyTypeXTCP,
}

// IsProxyType reports whether name is an implemented proxy type.
func IsProxyType(name string) bool {
	for _, t := range ProxyTypes {
		if t == name {
			return true
		}
	}
	return false
}

// IsPrivateProxyType reports whether the type is reached only by a visitor that
// holds the proxy's secret key.
func IsPrivateProxyType(name string) bool {
	return name == ProxyTypeSTCP || name == ProxyTypeSUDP || name == ProxyTypeXTCP
}

// IsDatagramProxyType reports whether the type carries datagrams rather than a
// byte stream, which changes how a data connection frames its payload.
func IsDatagramProxyType(name string) bool {
	return name == ProxyTypeUDP || name == ProxyTypeSUDP
}

// LocalAddr returns the address the client forwards to.
func (p ProxyConfig) LocalAddr() string {
	host := p.LocalIP
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, p.LocalPort)
}

// VisitorConfig is one [[visitors]] entry. A visitor reaches a private proxy
// (stcp, sudp or xtcp) published by another client: it listens on a local port
// and sends every connection through the server, or directly to the owner when a
// hole punch succeeds.
type VisitorConfig struct {
	Name string `toml:"name"`
	// Type must be stcp, sudp or xtcp.
	Type string `toml:"type"`
	// ServerName is the name of the proxy to visit.
	ServerName string `toml:"server_name"`
	// SecretKey must match the proxy's secret_key. With auth_method = "nizk" it
	// is used to build a proof and never leaves this machine.
	SecretKey string `toml:"secret_key"`
	// AuthMethod must match the proxy's auth_method for the key to stay local:
	// "secret" sends it, "nizk" proves knowledge of it.
	AuthMethod string `toml:"auth_method"`
	BindAddr   string `toml:"bind_addr"`
	BindPort   int    `toml:"bind_port"`
}

// ListenAddr is the local address a visitor listens on.
func (v VisitorConfig) ListenAddr() string {
	host := v.BindAddr
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, v.BindPort)
}

// VisitorTypes lists the proxy types a visitor may ask for.
var VisitorTypes = []string{ProxyTypeSTCP, ProxyTypeSUDP, ProxyTypeXTCP}

// IsVisitorType reports whether name is a type a visitor may request.
func IsVisitorType(name string) bool {
	for _, t := range VisitorTypes {
		if t == name {
			return true
		}
	}
	return false
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
	// PostQuantum replaces the passphrase-derived key with one agreed through
	// X25519 together with ML-KEM-768, and derives a separate key for every data
	// connection. It requires Enabled.
	PostQuantum bool `toml:"post_quantum"`
}

// TransportConfig is the [transport] section: TLS on the control port.
//
// TLS hides the tunnel's framing from anything that can observe the connection,
// and is the only way to get certificate-based server authentication. It is off
// by default so an upgrade does not break an existing deployment.
type TransportConfig struct {
	EnableTLS bool   `toml:"enable_tls"`
	CertFile  string `toml:"cert_file"` // server
	KeyFile   string `toml:"key_file"`  // server
	// CAFile is the client's trust anchor. Empty uses the system roots.
	CAFile string `toml:"ca_file"` // client
	// ServerName overrides the name checked against the certificate. Empty uses
	// the host part of client.server_addr.
	ServerName string `toml:"server_name"` // client
	// InsecureSkipVerify accepts any certificate. It makes the connection
	// private but not authenticated, so it is reported as a warning.
	InsecureSkipVerify bool `toml:"insecure_skip_verify"` // client
}

// IdentityConfig is the [identity] section: an Ed25519 key that authenticates a
// client without revealing the auth token.
type IdentityConfig struct {
	Enabled bool `toml:"enabled"`
	// KeyFile holds the client's private key. It is generated on first use.
	KeyFile string `toml:"key_file"`
	// AllowedKeys lists the client public keys the server accepts, in hex. An
	// empty list with Enabled accepts any key that signs correctly, which proves
	// possession but does not restrict who may connect.
	AllowedKeys []string `toml:"allowed_keys"` // server
	// RequireIdentity refuses any client that does not present a valid identity.
	RequireIdentity bool `toml:"require_identity"` // server
}

// MetricsConfig is the [metrics] section. The endpoint is served by the dashboard
// listener, so it is only reachable when the dashboard is enabled.
type MetricsConfig struct {
	Enabled bool `toml:"enabled"`
	// Token, when set, is required on /metrics in addition to the dashboard token.
	Token string `toml:"token"`
}

// AuditConfig is the [audit] section: an append-only JSONL record of security
// relevant events.
type AuditConfig struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
	// MaxBytes rotates the file once it exceeds this size. Zero disables rotation.
	MaxBytes int64 `toml:"max_bytes"`
}

// ObfuscationConfig is the [obfuscation] section. Padding hides exact frame
// lengths; it does not make the traffic look like TLS.
type ObfuscationConfig struct {
	Enabled     bool   `toml:"enabled"`
	DefaultType string `toml:"default_type"`
	// PadTo rounds every frame payload up to a multiple of this many bytes, so
	// observing the frame length reveals less about the payload. Zero disables
	// padding even when Enabled is true.
	PadTo int `toml:"pad_to"`
	// JitterMillis adds a random delay in [0, JitterMillis) before each write.
	JitterMillis int `toml:"jitter_millis"`
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
	Transport   TransportConfig   `toml:"transport"`
	Identity    IdentityConfig    `toml:"identity"`
	Obfuscation ObfuscationConfig `toml:"obfuscation"`
	Metrics     MetricsConfig     `toml:"metrics"`
	Audit       AuditConfig       `toml:"audit"`
	VPN         VPNConfig         `toml:"vpn"`
	Proxies     []ProxyConfig     `toml:"proxies"`
	Visitors    []VisitorConfig   `toml:"visitors"`

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
	if c.Server.RateLimitBurst == 0 {
		c.Server.RateLimitBurst = 20
	}
	if c.Server.LoadBalance == "" {
		c.Server.LoadBalance = LoadBalanceRoundRobin
	}
	if c.Audit.Enabled && c.Audit.Path == "" {
		c.Audit.Path = "aethertunnel-audit.jsonl"
	}
	if c.Audit.MaxBytes == 0 {
		c.Audit.MaxBytes = 32 << 20
	}
	if c.Obfuscation.Enabled && c.Obfuscation.PadTo == 0 {
		c.Obfuscation.PadTo = 256
	}
	for i := range c.Proxies {
		if c.Proxies[i].Type == "" {
			c.Proxies[i].Type = ProxyTypeTCP
		}
		if c.Proxies[i].AuthMethod == "" {
			c.Proxies[i].AuthMethod = AuthMethodSecret
		}
	}
	for i := range c.Visitors {
		if c.Visitors[i].Type == "" {
			c.Visitors[i].Type = ProxyTypeSTCP
		}
		if c.Visitors[i].AuthMethod == "" {
			c.Visitors[i].AuthMethod = AuthMethodSecret
		}
	}
	if c.Identity.Enabled && c.Identity.KeyFile == "" {
		c.Identity.KeyFile = "aethertunnel-identity.key"
	}
}

// Load-balancing strategies accepted by [server].load_balance.
const (
	LoadBalanceRoundRobin = "round-robin"
	LoadBalanceRandom     = "random"
	LoadBalanceLatency    = "latency"
	LoadBalanceFailover   = "failover"
)

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

// PostQuantum reports whether sessions should agree a key with X25519 and
// ML-KEM-768 instead of deriving one from the passphrase.
func (c *Config) PostQuantum() bool {
	return c.Encryption.Enabled && c.Encryption.PostQuantum
}

// ServerTLSConfig builds the server's TLS configuration, or returns nil when TLS
// is disabled.
func (c *Config) ServerTLSConfig() (*tls.Config, error) {
	if !c.Transport.EnableTLS {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.Transport.CertFile, c.Transport.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds the client's TLS configuration, or returns nil when TLS
// is disabled.
func (c *Config) ClientTLSConfig() (*tls.Config, error) {
	if !c.Transport.EnableTLS {
		return nil, nil
	}

	config := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.Transport.InsecureSkipVerify,
	}
	if c.Transport.ServerName != "" {
		config.ServerName = c.Transport.ServerName
	} else if host, _, err := splitHostPort(c.Client.ServerAddr); err == nil {
		config.ServerName = host
	}

	if c.Transport.CAFile != "" {
		pem, err := os.ReadFile(c.Transport.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read transport.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("transport.ca_file %s contains no certificate", c.Transport.CAFile)
		}
		config.RootCAs = pool
	}
	return config, nil
}

// AllowedIdentities parses [identity].allowed_keys.
func (c *Config) AllowedIdentities() ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(c.Identity.AllowedKeys))
	for _, text := range c.Identity.AllowedKeys {
		key, err := crypto.ParseIdentityKey(text)
		if err != nil {
			return nil, fmt.Errorf("identity.allowed_keys entry %q: %w", text, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// HTTPAddr is the address of the shared virtual-host listener for http proxies.
func (c *Config) HTTPAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.BindAddr, c.Server.HTTPPort)
}

// HTTPSAddr is the address of the shared TLS virtual-host listener.
func (c *Config) HTTPSAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.BindAddr, c.Server.HTTPSPort)
}

// P2PAddr is the UDP rendezvous address used for hole punching.
func (c *Config) P2PAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.BindAddr, c.Server.P2PPort)
}

// DashboardAddr is the address the dashboard binds.
func (c *Config) DashboardAddr() string {
	return fmt.Sprintf("%s:%d", c.Dashboard.BindAddr, c.Dashboard.Port)
}

// Validate checks the configuration for the given role and returns every problem
// it finds, so the user can fix them in one pass.
//
// Validate also normalises the settings that have a default and are validated
// against a fixed set, so a Config built in code (tests, embedders) does not have
// to call applyDefaults itself.
func (c *Config) Validate(role string) error {
	var problems []string

	if c.Server.LoadBalance == "" {
		c.Server.LoadBalance = LoadBalanceRoundRobin
	}

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

	if c.Encryption.PostQuantum && !c.Encryption.Enabled {
		problems = append(problems, "encryption.post_quantum is true but encryption.enabled is false")
	}

	if c.Transport.EnableTLS {
		switch role {
		case RoleServer:
			if c.Transport.CertFile == "" || c.Transport.KeyFile == "" {
				problems = append(problems, "transport.enable_tls is true but cert_file and key_file are not both set")
			}
		case RoleClient:
			if c.Transport.InsecureSkipVerify {
				c.Warnings = append(c.Warnings,
					"transport.insecure_skip_verify is true: the control connection is encrypted but the server is not authenticated")
			}
		}
	} else if c.Transport.CertFile != "" || c.Transport.CAFile != "" {
		c.Warnings = append(c.Warnings, "a certificate is configured but transport.enable_tls is false; the files are unused")
	}

	if c.Identity.Enabled {
		for _, key := range c.Identity.AllowedKeys {
			if _, err := hex.DecodeString(strings.TrimSpace(key)); err != nil {
				problems = append(problems, fmt.Sprintf("identity.allowed_keys entry %q is not hex: %v", key, err))
				continue
			}
		}
		if role == RoleServer && c.Identity.RequireIdentity && len(c.Identity.AllowedKeys) == 0 {
			c.Warnings = append(c.Warnings,
				"identity.require_identity is true but identity.allowed_keys is empty: "+
					"any client that signs correctly is accepted, which proves possession but does not restrict who connects")
		}
		if role == RoleClient && c.Identity.KeyFile == "" {
			problems = append(problems, "identity.enabled is true but identity.key_file is empty")
		}
	}

	if c.Obfuscation.Enabled && c.Obfuscation.PadTo < 0 {
		problems = append(problems, "obfuscation.pad_to cannot be negative")
	}
	if c.Obfuscation.JitterMillis < 0 {
		problems = append(problems, "obfuscation.jitter_millis cannot be negative")
	}
	if c.Server.RateLimitPerSecond < 0 {
		problems = append(problems, "server.rate_limit_per_second cannot be negative")
	}
	switch c.Server.LoadBalance {
	case LoadBalanceRoundRobin, LoadBalanceRandom, LoadBalanceLatency, LoadBalanceFailover:
	default:
		problems = append(problems, fmt.Sprintf(
			"server.load_balance %q is not supported (use %s, %s, %s or %s)",
			c.Server.LoadBalance, LoadBalanceRoundRobin, LoadBalanceRandom,
			LoadBalanceLatency, LoadBalanceFailover))
	}
	if c.Server.LoadBalance != LoadBalanceRoundRobin {
		c.Warnings = append(c.Warnings,
			"server.load_balance is set but load balancing across clients is not implemented yet; "+
				"the value is ignored and a second client publishing the same proxy name is refused")
	}
	for _, cidr := range c.Server.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			problems = append(problems, fmt.Sprintf("server.allow_cidrs entry %q is not a CIDR: %v", cidr, err))
		}
	}
	for _, cidr := range c.Server.DenyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			problems = append(problems, fmt.Sprintf("server.deny_cidrs entry %q is not a CIDR: %v", cidr, err))
		}
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

		if !IsProxyType(p.Type) {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: type %q is not supported (use one of %s)",
				p.Name, p.Type, strings.Join(ProxyTypes, ", ")))
			continue
		}
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			problems = append(problems, fmt.Sprintf("proxy %q: local_port must be 1-65535, got %d", p.Name, p.LocalPort))
		}
		if p.RemotePort < 0 || p.RemotePort > 65535 {
			problems = append(problems, fmt.Sprintf("proxy %q: remote_port must be 0-65535, got %d", p.Name, p.RemotePort))
		}

		switch p.Type {
		case ProxyTypeHTTP, ProxyTypeHTTPS:
			if p.RemotePort != 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: %s tunnels are reached through the server's shared listener, so remote_port must be 0",
					p.Name, p.Type))
			}
			if len(p.Domains) == 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: a %s tunnel needs at least one entry in domains", p.Name, p.Type))
			}
		case ProxyTypeSTCP, ProxyTypeSUDP, ProxyTypeXTCP:
			if p.RemotePort != 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: %s is a private tunnel and must not open a public port, so remote_port must be 0",
					p.Name, p.Type))
			}
			if p.SecretKey == "" {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: %s is a private tunnel and needs a secret_key", p.Name, p.Type))
			}
			switch p.AuthMethod {
			case AuthMethodSecret, AuthMethodNIZK:
			default:
				problems = append(problems, fmt.Sprintf(
					"proxy %q: auth_method %q is not supported (use %q or %q)",
					p.Name, p.AuthMethod, AuthMethodSecret, AuthMethodNIZK))
			}
		}
		for _, domain := range p.Domains {
			if !ValidDomain(domain) {
				problems = append(problems, fmt.Sprintf("proxy %q: domain %q is not a hostname pattern", p.Name, domain))
			}
		}
	}

	seenVisitors := map[string]bool{}
	for _, v := range c.Visitors {
		if v.Name == "" {
			problems = append(problems, "every [[visitors]] entry needs a name")
			continue
		}
		if seenVisitors[v.Name] {
			problems = append(problems, fmt.Sprintf("duplicate visitor name %q", v.Name))
		}
		seenVisitors[v.Name] = true

		if !IsVisitorType(v.Type) {
			problems = append(problems, fmt.Sprintf(
				"visitor %q: type %q is not supported (use one of %s)",
				v.Name, v.Type, strings.Join(VisitorTypes, ", ")))
		}
		if v.ServerName == "" {
			problems = append(problems, fmt.Sprintf("visitor %q: server_name is required", v.Name))
		}
		if v.SecretKey == "" {
			problems = append(problems, fmt.Sprintf("visitor %q: secret_key is required", v.Name))
		}
		switch v.AuthMethod {
		case AuthMethodSecret, AuthMethodNIZK:
		default:
			problems = append(problems, fmt.Sprintf(
				"visitor %q: auth_method %q is not supported (use %q or %q); it must match the proxy's auth_method",
				v.Name, v.AuthMethod, AuthMethodSecret, AuthMethodNIZK))
		}
		if v.BindPort < 1 || v.BindPort > 65535 {
			problems = append(problems, fmt.Sprintf("visitor %q: bind_port must be 1-65535, got %d", v.Name, v.BindPort))
		}
	}

	if c.Server.HTTPPort != 0 || c.Server.HTTPSPort != 0 {
		for name, port := range map[string]int{"http_port": c.Server.HTTPPort, "https_port": c.Server.HTTPSPort} {
			if port < 0 || port > 65535 {
				problems = append(problems, fmt.Sprintf("server.%s must be 0-65535, got %d", name, port))
			}
		}
		if c.Server.HTTPSPort != 0 && (c.Server.HTTPSCertFile == "" || c.Server.HTTPSKeyFile == "") {
			problems = append(problems, "server.https_port is set but https_cert_file and https_key_file are not both set")
		}
	}
	if c.Server.SubdomainHost != "" && !ValidDomain(c.Server.SubdomainHost) {
		problems = append(problems, fmt.Sprintf("server.subdomain_host %q is not a hostname", c.Server.SubdomainHost))
	}
	if c.Server.P2PPort < 0 || c.Server.P2PPort > 65535 {
		problems = append(problems, fmt.Sprintf("server.p2p_port must be 0-65535, got %d", c.Server.P2PPort))
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ValidDomain reports whether pattern is a usable hostname or a wildcard
// hostname such as "*.example.com".
func ValidDomain(pattern string) bool {
	host := pattern
	if strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			switch {
			case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			case ch == '-', ch == '_':
				if i == 0 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
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
