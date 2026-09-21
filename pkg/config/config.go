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
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/dht"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
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

	// Automatic ban of sources that keep failing authentication. Zero disables
	// it. BanIgnoreCIDRs names sources that are never banned, which is what a
	// deployment behind a load balancer or on a loopback address needs.
	BanAfterFailures int      `toml:"ban_after_failures"`
	BanSeconds       int      `toml:"ban_seconds"`
	BanMaxSeconds    int      `toml:"ban_max_seconds"`
	BanIgnoreCIDRs   []string `toml:"ban_ignore_cidrs"`

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

	// Group, when set, pools this proxy with every other proxy of the same name
	// and group: the server publishes one endpoint and distributes visitors
	// across the members according to [server].load_balance.
	Group string `toml:"group"`
	// Multipath opens this many parallel data connections for one datagram
	// session and spreads the datagrams across them. It applies to udp and sudp
	// proxies; a byte stream stays on one path.
	Multipath int `toml:"multipath"`

	// AllowCIDRs and DenyCIDRs restrict which visitor source addresses may use
	// this proxy's public endpoint, or may pair with it when it is private. Deny
	// wins; an empty allow list accepts every source that reached the endpoint.
	AllowCIDRs []string `toml:"allow_cidrs"`
	DenyCIDRs  []string `toml:"deny_cidrs"`

	// AllowTargets lists the address ranges a socks5 proxy may be asked to dial.
	// The proxy is only published when a client declares a non-empty list: the
	// list is what keeps a socks5 tunnel from becoming an exit for everything the
	// client can reach.
	AllowTargets []string `toml:"allow_targets"`
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
	ProxyTypeSOCKS = "socks5"
)

// ProxyTypes lists every type this build implements.
var ProxyTypes = []string{
	ProxyTypeTCP, ProxyTypeUDP, ProxyTypeHTTP, ProxyTypeHTTPS,
	ProxyTypeSTCP, ProxyTypeSUDP, ProxyTypeXTCP, ProxyTypeSOCKS,
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
//
// A socks5 tunnel has none: the visitor names the address, so an empty string is
// what the client reports for it, and what the dashboard shows in place of a local
// address that would mean nothing.
func (p ProxyConfig) LocalAddr() string {
	if p.Type == ProxyTypeSOCKS {
		return ""
	}
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
	// Keep is how many rotated generations are kept beside the live log: path.1 is
	// the one that was just rotated away, path.2 the one before it. Zero means the
	// default of one.
	Keep int `toml:"keep"`
}

// LedgerConfig is the [ledger] section: a signed, hash-chained record of the
// bandwidth every client used.
//
// Each entry commits to the one before it and is signed with an Ed25519 key, so an
// operator can publish the chain and anyone holding only the public key can check
// that it was not altered or truncated. The key file holds the private seed and is
// generated on first use.
type LedgerConfig struct {
	Enabled    bool   `toml:"enabled"`
	Path       string `toml:"path"`
	SigningKey string `toml:"signing_key_file"`
}

// DHTConfig is the [dht] section: a Kademlia distributed hash table over UDP that
// carries the directory of published proxies.
//
// A server publishes one record per published proxy, naming the address a client
// should dial for it. A client resolves that name instead of being configured with
// an address, so a proxy can move between servers without every visitor's
// configuration changing. The same node type serves both roles.
type DHTConfig struct {
	Enabled bool `toml:"enabled"`
	// ListenAddr is the UDP address the node binds. Empty selects DefaultDHTAddr.
	ListenAddr string `toml:"listen_addr"`
	// Bootstrap lists DHT nodes to contact on start. An empty list makes the node
	// a one-node table, which still resolves the records it publishes itself.
	Bootstrap []string `toml:"bootstrap"`
	// NodeID is a 40-character hex identifier. Empty selects a random one, which
	// changes on every restart.
	NodeID string `toml:"node_id"`
	// Namespace prefixes every key, so two deployments can share one DHT.
	Namespace string `toml:"namespace"`
	// TTLSeconds is how long the DHT keeps a record. Zero selects one hour.
	TTLSeconds int `toml:"ttl_seconds"`
	// AnnounceTTLSeconds is how long a reader honours an announcement. Zero
	// selects 90 seconds.
	AnnounceTTLSeconds int `toml:"announce_ttl_seconds"`
	// RepublishSeconds is how often live announcements are rewritten. Zero selects
	// a third of AnnounceTTLSeconds.
	RepublishSeconds int `toml:"republish_seconds"`
	// LookupTimeoutSeconds bounds one resolution. Zero selects five seconds.
	LookupTimeoutSeconds int `toml:"lookup_timeout_seconds"`
	// AdvertiseHost is the host a client is told to dial for a proxy this server
	// publishes. It carries no port: the port is the one that serves each proxy,
	// which is the control port, a shared http or https listener, or the proxy's
	// own remote_port. Empty uses server.bind_addr, which is wrong behind NAT or
	// a load balancer. It applies to the server role.
	AdvertiseHost string `toml:"advertise_host"`
	// Discover is the proxy name a client resolves when client.server_addr is
	// empty. It applies to the client role.
	Discover string `toml:"discover"`
	// SigningKeyFile holds the Ed25519 seed a server signs its announcements
	// with, so a reader can tell which server published an address. It is
	// generated on first use, like the ledger key. Empty publishes unsigned
	// records, which any node on the DHT can forge. It applies to the server role.
	SigningKeyFile string `toml:"signing_key_file"`
	// RequireSigned refuses an announcement that carries no signature. A reader
	// sets it to reject records written by an unknown node. It applies to the
	// client and query roles.
	RequireSigned bool `toml:"require_signed"`
	// TrustedKeys lists the announcement signing keys a reader accepts, in hex.
	// A non-empty list also refuses unsigned records. It applies to the client and
	// query roles.
	TrustedKeys []string `toml:"trusted_keys"`
}

// ObfuscationConfig is the [obfuscation] section. Padding hides exact frame
// lengths and the disguise hides the framing itself; neither encrypts anything, so
// they complement [encryption] rather than replacing it.
type ObfuscationConfig struct {
	Enabled bool `toml:"enabled"`
	// PadTo rounds every frame payload up to a multiple of this many bytes, so
	// observing the frame length reveals less about the payload. Zero disables
	// padding even when Enabled is true.
	PadTo int `toml:"pad_to"`
	// JitterMillis adds a random delay in [0, JitterMillis) before each write.
	JitterMillis int `toml:"jitter_millis"`
	// Disguise wraps every TCP connection so what an observer sees is not this
	// program's frame header. "none" writes the frames as they are; "tls-record"
	// puts each write inside TLS 1.2 application-data records.
	//
	// It is not a TLS handshake: there is no ClientHello and no key exchange, so a
	// detector that models a TLS session can still tell the connection apart.
	Disguise string `toml:"disguise"`
}

// ObfuscationDisguise is the disguise in force, with the empty value read as
// "none".
func (c *Config) ObfuscationDisguise() string {
	if c.Obfuscation.Disguise == "" {
		return obfs.DisguiseNone
	}
	return c.Obfuscation.Disguise
}

// VPNConfig is the [vpn] section: a layer-3 tunnel that gives each client an
// address on a subnet.
//
// The server owns the subnet and hands out one address per session; the client is
// told which address it got when its session is accepted, so only the server has an
// address to configure.
type VPNConfig struct {
	Enabled bool `toml:"enabled"`
	// Device is the interface name. On the server it is the interface the tunnel
	// reads and writes; on the client it is the interface to create. An empty name
	// asks the kernel for the next free one, which only Linux supports.
	Device string `toml:"device"`
	// Address is the subnet the server allocates client addresses from, in CIDR
	// form. The server keeps the first usable address for itself. Server only.
	Address string `toml:"address"`
	// MTU overrides the interface MTU. Zero uses the interface's own value.
	MTU int `toml:"mtu"`
	// Require asks the server to refuse a session that does not want a tunnel
	// address. Server only.
	Require bool `toml:"require"`
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
	Ledger      LedgerConfig      `toml:"ledger"`
	DHT         DHTConfig         `toml:"dht"`
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
	// The default applies when the key is absent or zero, like the other
	// durations here; a negative value is rejected by Validate.
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
	if c.Server.BanAfterFailures > 0 {
		if c.Server.BanSeconds == 0 {
			c.Server.BanSeconds = 300
		}
		if c.Server.BanMaxSeconds < c.Server.BanSeconds {
			c.Server.BanMaxSeconds = 3600
		}
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
	// Like the other keys with a non-zero default, an absent or zero value means the
	// default of one generation.
	if c.Audit.Keep == 0 {
		c.Audit.Keep = 1
	}
	if c.Ledger.Enabled {
		if c.Ledger.Path == "" {
			c.Ledger.Path = "aethertunnel-ledger.jsonl"
		}
		if c.Ledger.SigningKey == "" {
			c.Ledger.SigningKey = "aethertunnel-ledger.key"
		}
	}
	if c.DHT.Enabled {
		if c.DHT.ListenAddr == "" {
			c.DHT.ListenAddr = DefaultDHTAddr
		}
		if c.DHT.Namespace == "" {
			c.DHT.Namespace = DefaultDHTNamespace
		}
		if c.DHT.AnnounceTTLSeconds == 0 {
			c.DHT.AnnounceTTLSeconds = 90
		}
		if c.DHT.RepublishSeconds == 0 {
			c.DHT.RepublishSeconds = c.DHT.AnnounceTTLSeconds / 3
		}
		if c.DHT.LookupTimeoutSeconds == 0 {
			c.DHT.LookupTimeoutSeconds = 5
		}
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
	// LoadBalanceAdaptive picks the member with the lowest cost, where the cost is
	// the member's moving-average response time multiplied by a penalty for its
	// recent failures. It is a moving average, not a learned model: nothing is
	// trained and no history beyond the average is kept.
	LoadBalanceAdaptive = "adaptive"
)

// MaxMultipath bounds [[proxies]].multipath.
const MaxMultipath = 8

// Defaults for the [dht] section. The listen address is a fixed port so that a
// node can be bootstrapped by address without first being told which port it
// chose.
const (
	DefaultDHTAddr      = "0.0.0.0:7001"
	DefaultDHTNamespace = "aethertunnel"
)

// DHTAddr is the address the DHT node binds.
func (c *Config) DHTAddr() string { return c.DHT.ListenAddr }

// DHTNodeID parses [dht].node_id.
func (c *Config) DHTNodeID() (dht.ID, error) {
	if c.DHT.NodeID == "" {
		return dht.ID{}, nil
	}
	id, err := dht.IDFromString(c.DHT.NodeID)
	if err != nil {
		return dht.ID{}, fmt.Errorf("dht.node_id: %w", err)
	}
	return id, nil
}

// DHTTrustedKeys parses [dht].trusted_keys.
func (c *Config) DHTTrustedKeys() ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(c.DHT.TrustedKeys))
	for _, text := range c.DHT.TrustedKeys {
		key, err := crypto.ParseIdentityKey(text)
		if err != nil {
			return nil, fmt.Errorf("dht.trusted_keys entry %q: %w", text, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// DHTSettings maps the section onto the discovery package's configuration.
//
// The signing key is not part of the mapping: it is a file the role that publishes
// has to load, and the loader lives next to the code that publishes. An entry in
// trusted_keys that does not parse is dropped here, because Load rejects it before
// a caller can reach this point.
func (c *Config) DHTSettings(logger *log.Logger) discovery.Config {
	trusted, err := c.DHTTrustedKeys()
	if err != nil {
		logger.Printf("warning: %v", err)
		trusted = nil
	}
	return discovery.Config{
		ListenAddr:        c.DHT.ListenAddr,
		Bootstrap:         c.DHT.Bootstrap,
		NodeID:            c.DHT.NodeID,
		Namespace:         c.DHT.Namespace,
		TTL:               time.Duration(c.DHT.TTLSeconds) * time.Second,
		AnnounceTTL:       time.Duration(c.DHT.AnnounceTTLSeconds) * time.Second,
		RepublishInterval: time.Duration(c.DHT.RepublishSeconds) * time.Second,
		LookupTimeout:     time.Duration(c.DHT.LookupTimeoutSeconds) * time.Second,
		RequireSigned:     c.DHT.RequireSigned,
		TrustedKeys:       trusted,
		Logger:            logger,
	}
}

// Environment variables that override the values in the file.
//
// They exist for deployments where the configuration file is a ConfigMap, which is
// world-readable inside the cluster, while the credentials are a Secret. The file
// then never contains a credential at all.
const (
	EnvAuthToken            = "AETHERTUNNEL_AUTH_TOKEN"
	EnvDashboardToken       = "AETHERTUNNEL_DASHBOARD_TOKEN"
	EnvEncryptionPassphrase = "AETHERTUNNEL_ENCRYPTION_PASSPHRASE"
)

// ApplyEnv overrides the credentials with the values of the environment variables
// above, and reports which ones it used. An unset or empty variable leaves the file's
// value in place, so this does nothing outside a container.
//
// The auth token feeds the encryption passphrase as well when no passphrase is set,
// which is why the override happens before anything derives a key from it.
func (c *Config) ApplyEnv() []string {
	var applied []string

	if token := os.Getenv(EnvAuthToken); token != "" {
		c.Server.AuthToken = token
		c.Client.AuthToken = token
		applied = append(applied, EnvAuthToken)
	}
	if token := os.Getenv(EnvDashboardToken); token != "" {
		c.Dashboard.Token = token
		applied = append(applied, EnvDashboardToken)
	}
	if passphrase := os.Getenv(EnvEncryptionPassphrase); passphrase != "" {
		c.Encryption.Passphrase = passphrase
		applied = append(applied, EnvEncryptionPassphrase)
	}
	return applied
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
		if c.Client.ServerAddr == "" && c.DHT.Discover == "" {
			problems = append(problems, "client.server_addr is required, unless dht.enabled and dht.discover resolve the server address from the DHT")
		} else if c.Client.ServerAddr != "" {
			if _, _, err := splitHostPort(c.Client.ServerAddr); err != nil {
				problems = append(problems, fmt.Sprintf("client.server_addr %q is not host:port", c.Client.ServerAddr))
			}
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
	if c.Obfuscation.Disguise != "" && !obfs.IsDisguise(c.Obfuscation.Disguise) {
		problems = append(problems, fmt.Sprintf("obfuscation.disguise %q is not supported (use one of %s)",
			c.Obfuscation.Disguise, strings.Join(obfs.Disguises, ", ")))
	}
	if c.Obfuscation.Disguise != "" && !c.Obfuscation.Enabled {
		c.Warnings = append(c.Warnings,
			"obfuscation.disguise is set but obfuscation.enabled is false: the disguise is a wrapper on the connection, "+
				"so it is applied only when the section is enabled")
	}
	if c.Server.RateLimitPerSecond < 0 {
		problems = append(problems, "server.rate_limit_per_second cannot be negative")
	}
	if c.Server.GracefulShutdownSecs < 0 {
		problems = append(problems, "server.graceful_shutdown_seconds cannot be negative")
	}
	// An upper bound keeps a typo from making the server rename files in a loop and
	// from filling a directory with generations nobody asked for. Zero is the
	// default of one, like the other keys here.
	if c.Audit.Keep < 0 || c.Audit.Keep > 100 {
		problems = append(problems, fmt.Sprintf(
			"audit.keep must be 0-100 (0 means the default of one generation), got %d", c.Audit.Keep))
	}
	if c.Audit.MaxBytes < 0 {
		problems = append(problems, "audit.max_bytes cannot be negative")
	}
	switch c.Server.LoadBalance {
	case LoadBalanceRoundRobin, LoadBalanceRandom, LoadBalanceLatency, LoadBalanceFailover, LoadBalanceAdaptive:
	default:
		problems = append(problems, fmt.Sprintf(
			"server.load_balance %q is not supported (use %s, %s, %s, %s or %s)",
			c.Server.LoadBalance, LoadBalanceRoundRobin, LoadBalanceRandom,
			LoadBalanceLatency, LoadBalanceFailover, LoadBalanceAdaptive))
	}
	if c.Server.LoadBalance != LoadBalanceRoundRobin {
		c.Warnings = append(c.Warnings,
			"server.load_balance is set; it applies to proxies that declare a group, "+
				"and has no effect on a proxy published by a single client")
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
	if c.Server.BanAfterFailures < 0 {
		problems = append(problems, fmt.Sprintf("server.ban_after_failures must not be negative, got %d", c.Server.BanAfterFailures))
	}
	if c.Server.BanSeconds < 0 {
		problems = append(problems, fmt.Sprintf("server.ban_seconds must not be negative, got %d", c.Server.BanSeconds))
	}
	if c.Server.BanMaxSeconds < 0 {
		problems = append(problems, fmt.Sprintf("server.ban_max_seconds must not be negative, got %d", c.Server.BanMaxSeconds))
	}
	if c.Server.BanAfterFailures > 0 && c.Server.BanSeconds > 0 &&
		c.Server.BanMaxSeconds > 0 && c.Server.BanMaxSeconds < c.Server.BanSeconds {
		problems = append(problems, fmt.Sprintf(
			"server.ban_max_seconds (%d) is smaller than server.ban_seconds (%d)",
			c.Server.BanMaxSeconds, c.Server.BanSeconds))
	}
	for _, cidr := range c.Server.BanIgnoreCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			problems = append(problems, fmt.Sprintf("server.ban_ignore_cidrs entry %q is not a CIDR: %v", cidr, err))
		}
	}
	if c.VPN.Enabled {
		if c.VPN.MTU != 0 && (c.VPN.MTU < vpn.MinMTU || c.VPN.MTU > vpn.MaxMTU) {
			problems = append(problems, fmt.Sprintf("vpn.mtu must be %d-%d, got %d", vpn.MinMTU, vpn.MaxMTU, c.VPN.MTU))
		}
		switch role {
		case RoleServer:
			if c.VPN.Address == "" {
				problems = append(problems, "vpn.enabled is true but vpn.address is empty: the server needs a subnet to allocate client addresses from")
			} else if _, err := vpn.NewPool(c.VPN.Address); err != nil {
				problems = append(problems, fmt.Sprintf("vpn.address %q is not usable: %v", c.VPN.Address, err))
			}
		case RoleClient:
			if c.VPN.Address != "" {
				c.Warnings = append(c.Warnings, "vpn.address has no effect in a client configuration: the server decides the subnet and tells the client which address it got")
			}
		}
	}

	switch {
	case c.Ledger.Enabled && role == RoleClient:
		c.Warnings = append(c.Warnings, "ledger.enabled has no effect in a client configuration: the bandwidth ledger is kept by the server")
	case c.Ledger.Enabled:
		if c.Ledger.Path == "" {
			problems = append(problems, "ledger.enabled is true but ledger.path is empty")
		}
		if c.Ledger.SigningKey == "" {
			problems = append(problems, "ledger.enabled is true but ledger.signing_key_file is empty")
		}
	}

	if c.DHT.Enabled {
		if _, _, err := net.SplitHostPort(c.DHT.ListenAddr); err != nil {
			problems = append(problems, fmt.Sprintf("dht.listen_addr %q is not host:port", c.DHT.ListenAddr))
		}
		if c.DHT.NodeID != "" {
			if _, err := dht.IDFromString(c.DHT.NodeID); err != nil {
				problems = append(problems, fmt.Sprintf("dht.node_id %q is not a 40-character hex identifier", c.DHT.NodeID))
			}
		}
		for _, addr := range c.DHT.Bootstrap {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				problems = append(problems, fmt.Sprintf("dht.bootstrap entry %q is not host:port", addr))
			}
		}
		if c.DHT.AnnounceTTLSeconds < 0 {
			problems = append(problems, "dht.announce_ttl_seconds cannot be negative")
		}
		if c.DHT.RepublishSeconds < 0 {
			problems = append(problems, "dht.republish_seconds cannot be negative")
		}
		if c.DHT.RepublishSeconds > 0 && c.DHT.RepublishSeconds >= c.DHT.AnnounceTTLSeconds {
			problems = append(problems, fmt.Sprintf(
				"dht.republish_seconds (%d) must be shorter than dht.announce_ttl_seconds (%d), otherwise an announcement lapses before it is rewritten",
				c.DHT.RepublishSeconds, c.DHT.AnnounceTTLSeconds))
		}
		if c.DHT.TTLSeconds < 0 {
			problems = append(problems, "dht.ttl_seconds cannot be negative")
		}
		if c.DHT.TTLSeconds > 0 && c.DHT.TTLSeconds < c.DHT.AnnounceTTLSeconds {
			problems = append(problems, fmt.Sprintf(
				"dht.ttl_seconds (%d) must not be shorter than dht.announce_ttl_seconds (%d), otherwise records expire in the DHT before readers stop honouring them",
				c.DHT.TTLSeconds, c.DHT.AnnounceTTLSeconds))
		}
		for _, key := range c.DHT.TrustedKeys {
			if _, err := crypto.ParseIdentityKey(key); err != nil {
				problems = append(problems, fmt.Sprintf("dht.trusted_keys entry %q: %v", key, err))
			}
		}

		switch role {
		case RoleServer:
			if c.DHT.SigningKeyFile == "" {
				c.Warnings = append(c.Warnings,
					"dht.signing_key_file is empty, so announcements are unsigned: "+
						"any node on the DHT can replace this server's records with an address of its own, "+
						"and a client that sets dht.require_signed or dht.trusted_keys will refuse them")
			}
			if c.DHT.RequireSigned {
				c.Warnings = append(c.Warnings, "dht.require_signed has no effect in a server configuration: it governs what this node accepts when it resolves a name")
			}
			if len(c.DHT.TrustedKeys) > 0 {
				c.Warnings = append(c.Warnings, "dht.trusted_keys has no effect in a server configuration: it lists the publishers this node accepts when it resolves a name")
			}
			if strings.Contains(c.DHT.AdvertiseHost, ":") {
				problems = append(problems, fmt.Sprintf(
					"dht.advertise_host %q contains a port: give the host only, because the port is the one that serves each proxy",
					c.DHT.AdvertiseHost))
			} else if c.DHT.AdvertiseHost == "" && isUnspecifiedAddr(c.Server.BindAddr) {
				c.Warnings = append(c.Warnings,
					"dht.advertise_host is empty and server.bind_addr is a wildcard address "+
						"(0.0.0.0 or ::): the addresses published to the DHT will not be dialable from "+
						"another host, so set dht.advertise_host to the address clients reach this server on")
			}
			if c.DHT.Discover != "" {
				c.Warnings = append(c.Warnings, "dht.discover has no effect in a server configuration: it names the proxy a client resolves")
			}
		case RoleClient:
			if c.DHT.Discover == "" {
				c.Warnings = append(c.Warnings, "dht.enabled is true but dht.discover is empty: no proxy name is resolved, so the section has no effect")
			} else if c.Client.ServerAddr != "" {
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"client.server_addr is set to %s and dht.discover to %q: the configured address is used and the DHT is not consulted",
					c.Client.ServerAddr, c.DHT.Discover))
			}
			if c.DHT.AdvertiseHost != "" {
				c.Warnings = append(c.Warnings, "dht.advertise_host has no effect in a client configuration: it describes the addresses a server publishes")
			}
		}

		// The reader's policy applies to a client and to a query that only
		// resolves a name, so it is checked for both.
		if role != RoleServer && !c.DHT.RequireSigned && len(c.DHT.TrustedKeys) == 0 {
			c.Warnings = append(c.Warnings,
				"dht.require_signed is false and dht.trusted_keys is empty, so any record found under the key is "+
					"accepted: set dht.trusted_keys to the announcement keys you trust")
		}
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
		// A socks5 tunnel has no local service to name: the visitor chooses the
		// target, so local_ip and local_port are not part of its configuration.
		if p.Type != ProxyTypeSOCKS {
			if p.LocalPort < 1 || p.LocalPort > 65535 {
				problems = append(problems, fmt.Sprintf("proxy %q: local_port must be 1-65535, got %d", p.Name, p.LocalPort))
			}
		} else if p.LocalPort != 0 || p.LocalIP != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: local_ip and local_port have no effect on a socks5 tunnel, whose target comes from the request",
				p.Name))
		}
		if p.RemotePort < 0 || p.RemotePort > 65535 {
			problems = append(problems, fmt.Sprintf("proxy %q: remote_port must be 0-65535, got %d", p.Name, p.RemotePort))
		}
		for _, cidr := range append(append([]string{}, p.AllowCIDRs...), p.DenyCIDRs...) {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				problems = append(problems, fmt.Sprintf("proxy %q: %q is not a CIDR: %v", p.Name, cidr, err))
			}
		}

		switch p.Type {
		case ProxyTypeSOCKS:
			if p.RemotePort == 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: a socks5 tunnel needs a remote_port, because visitors reach it on a public port", p.Name))
			}
			if len(p.AllowTargets) == 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: a socks5 tunnel needs allow_targets: without a list the client would be an exit for everything it can reach",
					p.Name))
			}
			for _, cidr := range p.AllowTargets {
				if _, err := socks.NewTargetPolicy([]string{cidr}); err != nil {
					problems = append(problems, fmt.Sprintf("proxy %q: allow_targets entry %q: %v", p.Name, cidr, err))
				}
			}
		case ProxyTypeHTTP, ProxyTypeHTTPS:
			if p.RemotePort != 0 {
				problems = append(problems, fmt.Sprintf(
					"proxy %q: %s tunnels are reached through the server's shared listener, so remote_port must be 0",
					p.Name, p.Type))
			}
			if len(p.Domains) == 0 {
				// Only the server knows its subdomain_host, so this side cannot
				// decide whether the proxy is reachable: a name-less proxy is
				// published as <proxy-name>.<server.subdomain_host>, and the server
				// refuses it when that setting is empty. Refusing the
				// configuration here would make the setting unusable.
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"proxy %q: a %s tunnel without domains is published as <proxy-name>.<server.subdomain_host>; "+
						"the server refuses it when server.subdomain_host is not set",
					p.Name, p.Type))
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
		if p.Multipath < 0 || p.Multipath > MaxMultipath {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: multipath must be 0-%d, got %d", p.Name, MaxMultipath, p.Multipath))
		}
		if p.Multipath > 1 && !IsDatagramProxyType(p.Type) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: multipath only spreads datagrams, so it has no effect on a %s proxy; "+
					"a byte stream stays on the path it started on", p.Name, p.Type))
		}
		if p.RemotePort == 0 && p.Group == "" && p.Type != ProxyTypeHTTP && p.Type != ProxyTypeHTTPS &&
			!IsPrivateProxyType(p.Type) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: it has no remote_port and no group, so nothing can reach it", p.Name))
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

// isUnspecifiedAddr reports whether a bind address accepts every interface, in
// which case it cannot be handed to a peer as a destination.
func isUnspecifiedAddr(addr string) bool {
	switch addr {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
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

	// Credentials from the environment are applied before validation, so a
	// configuration file that deliberately holds no secret passes the checks that
	// require one. The names used are reported as warnings so the operator can see
	// that the file was not the whole story.
	if applied := cfg.ApplyEnv(); len(applied) > 0 {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("%s overrode the configuration file", strings.Join(applied, ", ")))
	}

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
