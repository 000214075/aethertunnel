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
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/dht"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// ServerConfig is the [server] section.
type ServerConfig struct {
	BindAddr string `toml:"bind_addr"`
	BindPort int    `toml:"bind_port"`
	// ProxyBindAddr is the address the published listeners bind (http_port,
	// https_port, tcpmux_port, https_passthrough_port and every remote port);
	// empty means bind_addr. It is frp's proxyBindAddr, defaulted the same way:
	// the control port can face one network while the published services face
	// another.
	ProxyBindAddr string `toml:"proxy_bind_addr"`
	// TCPKeepAliveSeconds is the interval between TCP keep-alive probes on the
	// connections a client holds to this server; 0 keeps the Go default (15
	// seconds). frp calls it transport.tcpKeepalive and defaults to 7200.
	TCPKeepAliveSeconds int `toml:"tcp_keepalive_seconds"`
	// TCPMuxPassthrough hands the CONNECT request on the tcpmux port to the
	// tunnel instead of answering it, which is frp's tcpmuxPassthrough: the
	// server still reads the request to route by its authority, but the
	// backend sees it exactly as sent — a service that speaks CONNECT itself,
	// such as an HTTP proxy behind the tunnel, gets to answer it. Off by
	// default, because a backend that expects to be handed a raw stream
	// answers nothing and the visitor hangs instead of getting a status.
	TCPMuxPassthrough bool `toml:"tcpmux_passthrough"`
	// UserConnTimeoutSeconds bounds how long a published connection waits for
	// the client to hand back a data connection once it has been asked for one;
	// 0 falls back to dial_timeout_seconds. It is frp's userConnTimeout, which
	// defaults to 10 — this server keeps the dial timeout unless the operator
	// asks for a separate bound, so the two waits stay one knob by default.
	UserConnTimeoutSeconds int `toml:"user_conn_timeout_seconds"`

	AuthToken string `toml:"auth_token"`
	// AuthTokenFile reads the shared secret from a file instead of the file
	// holding it, which is frp's auth.tokenSource with type "file". Trailing
	// whitespace and a final newline are trimmed, and a token in the
	// configuration file is overridden — with a warning, because a file that
	// carries both is usually a half-finished migration.
	AuthTokenFile string `toml:"auth_token_file"`

	MaxConnections       int `toml:"max_connections"`
	HandshakeTimeoutSecs int `toml:"handshake_timeout_seconds"`
	ReadTimeoutSecs      int `toml:"read_timeout_seconds"`
	HeartbeatSeconds     int `toml:"heartbeat_seconds"`
	DialTimeoutSecs      int `toml:"dial_timeout_seconds"`
	GracefulShutdownSecs int `toml:"graceful_shutdown_seconds"`

	// MaxPoolCount caps how many data connections one client session may hold
	// parked in advance (client.pool_count), which is frp's maxPoolCount. A
	// connection offered beyond the cap is refused by name and the client falls
	// back to dialing per stream, so a low value costs a dial, never a stream.
	// Zero keeps the built-in 64.
	MaxPoolCount int `toml:"max_pool_count"`

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

	// AllowPorts, when set, lists the remote_port ranges a client may register,
	// e.g. ["6000-6999", "8000"]. A registration outside every range is refused
	// at registration time.
	AllowPorts []string `toml:"allow_ports"`

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
	// TCPMuxPort serves every tcpmux proxy on one listener: a visitor sends an
	// HTTP CONNECT whose authority names the hostname, and the server routes by
	// it. 0 disables the multiplexer.
	TCPMuxPort int `toml:"tcpmux_port"`
	// MaxPortsPerClient caps how many public ports one client session may
	// register; a registration past the cap is refused by name. 0 means
	// unlimited.
	MaxPortsPerClient int `toml:"max_ports_per_client"`
	// HTTPSPassthroughPort serves https proxies that opted into TLS
	// passthrough: the server sniffs the ClientHello's server name, routes by
	// it, and relays the visitor's TLS session untouched, so the certificate
	// the visitor sees is the one the client's side presents. 0 disables it.
	HTTPSPassthroughPort int `toml:"https_passthrough_port"`

	// Custom404Page names a file whose contents answer an http request that
	// names no published hostname on the shared listener, in place of the
	// plain built-in refusal. Empty keeps the built-in answer. Read at request
	// time, so an edited page appears without a restart.
	Custom404Page string `toml:"custom_404_page"`

	// VhostHTTPTimeout bounds how long the terminating virtual-host path waits
	// for a local service's response headers, in seconds; 0 leaves that bound at
	// the dial timeout instead of setting one of its own (frp's
	// vhostHTTPTimeout).
	VhostHTTPTimeout int `toml:"vhost_http_timeout"`

	// SubdomainHost, when set, lets an http proxy without explicit domains be
	// reached at <proxy-name>.<subdomain_host>.
	SubdomainHost string `toml:"subdomain_host"`

	// DetailedErrorsToClient sends the refusal's full wording to the client
	// instead of a short summary. It is frp's detailedErrorsToClient, and it
	// keeps that key's default: absent means detailed. Turning it off keeps a
	// refusal from describing the server's own configuration — which ports are
	// taken, which address a subdomain needs, whether a name is in use — to a
	// client that has not yet proven itself. The audit log and the server's log
	// keep the detail either way, so an operator still sees the reason.
	DetailedErrorsToClient *bool `toml:"detailed_errors_to_client"`

	// P2PPort is the UDP rendezvous port used by xtcp hole punching. It is
	// served by the same process as the control port; zero disables xtcp.
	P2PPort int `toml:"p2p_port"`
	// UDPPacketSize caps a relayed datagram's payload in bytes; a datagram
	// over the cap is dropped and counted rather than truncated. Zero selects
	// the largest UDP payload (65535), which is what the server did before the
	// key existed — frp calls the same knob udpPacketSize and defaults to 1500.
	UDPPacketSize int `toml:"udp_packet_size"`

	// SSHTunnelGateway runs an SSH server on a port of its own and turns an
	// `ssh -R` remote forwarding into a tunnel, which is frp's
	// sshTunnelGateway: a machine that will not run a client binary can
	// still publish a service with nothing but ssh. Nil disables it.
	SSHTunnelGateway *SSHTunnelGatewayConfig `toml:"ssh_tunnel_gateway"`

	// FeatureGates is [server] feature_gates: capabilities of this program an
	// operator may want to forbid. See the Feature* constants.
	FeatureGates map[string]bool `toml:"feature_gates"`
}

// SSHTunnelGatewayConfig is [server.ssh_tunnel_gateway]: an SSH server whose
// remote forwardings become tunnels. It is frp's sshTunnelGateway, reached the
// same way:
//
//	ssh -R :18080:127.0.0.1:8080 user@server -p 2200 tcp --proxy_name web --remote_port 18080
//
// The forwarded port is published exactly as a client's [[proxies]] entry would
// publish it, so server.allow_ports, the [[proxies]] policies by name and the
// dashboard all apply to it unchanged.
type SSHTunnelGatewayConfig struct {
	// BindAddr is the address the SSH listener binds; empty means
	// server.bind_addr.
	BindAddr string `toml:"bind_addr"`
	// BindPort runs the SSH listener on its own port. 0 disables the gateway,
	// so the section can stay in a file whose deployment turns it off.
	BindPort int `toml:"bind_port"`
	// KeyFile holds the host private key, in OpenSSH or PEM form. Empty
	// generates a key on first use and writes it to AutoGenKeyFile.
	KeyFile string `toml:"key_file"`
	// AutoGenKeyFile is where a generated host key is written; empty selects
	// "aethertunnel_ssh_gateway_key", which is the shape of frp's
	// autoGenPrivateKeyPath. A key generated here survives a restart, so
	// clients that pinned the host key keep working.
	AutoGenKeyFile string `toml:"auto_gen_key_file"`
	// AuthorizedKeysFile lists the public keys allowed to open a forwarding,
	// one per line, in authorized_keys form. A listed key is the only kind of
	// key admitted. Empty leaves no key listed, and then a key is admitted
	// only when no password is configured either: that combination accepts any
	// key that authenticates, for a gateway behind another access control.
	AuthorizedKeysFile string `toml:"authorized_keys_file"`
	// User and Password are a shared credential, for an ssh client with no key
	// installed. Password authentication turns on only when both are set, and
	// then a key is admitted only if AuthorizedKeysFile lists it: a gateway
	// that asks for a password does not also accept whatever key turns up.
	User     string `toml:"user"`
	Password string `toml:"password"`
	// MaxProxies caps how many tunnels one SSH session may open; 0 means
	// unlimited.
	MaxProxies int `toml:"max_proxies"`
	// IdleTimeoutSeconds closes a gateway session that has been quiet for
	// this long; 0 keeps the connection open until the client ends it.
	IdleTimeoutSeconds int `toml:"idle_timeout_seconds"`
}

// DefaultSSHGatewayKeyFile is where a generated host key is written when the
// section names no file, which is the shape of frp's autoGenPrivateKeyPath
// default.
const DefaultSSHGatewayKeyFile = "aethertunnel_ssh_gateway_key"

// KeyPath is the host key file the gateway uses: the configured key_file, or the
// file a generated key is written to and read back from.
func (g *SSHTunnelGatewayConfig) KeyPath() string {
	if g.KeyFile != "" {
		return g.KeyFile
	}
	if g.AutoGenKeyFile != "" {
		return g.AutoGenKeyFile
	}
	return DefaultSSHGatewayKeyFile
}

// OIDCConfig is the [oidc] section. A server fills the issuer side (issuer,
// audience, jwks); a client fills the token-endpoint side and fetches its
// access token with the client-credentials grant. audience is shared: the
// client asks the provider for it, the server checks the claim.
type OIDCConfig struct {
	// Issuer is the identity provider the server verifies tokens with; its
	// discovery document names the key set, and its value must match the
	// token's iss claim. Server only.
	Issuer string `toml:"issuer"`
	// Audience is the audience the token must carry. Empty skips the audience
	// check, the way frp's auth.oidc does.
	Audience string `toml:"audience"`
	// JWKSURL overrides the discovery document's jwks_uri, for issuers that
	// publish no well-known document. Server only.
	JWKSURL string `toml:"jwks_url"`
	// SkipExpiryCheck and SkipIssuerCheck mirror frp's auth.oidc switches for
	// issuers whose claims do not line up. Server only.
	SkipExpiryCheck bool `toml:"skip_expiry_check"`
	SkipIssuerCheck bool `toml:"skip_issuer_check"`
	// TimeoutSecs bounds the discovery and key-set fetches; 0 selects 10
	// seconds. Server only.
	TimeoutSecs int `toml:"timeout_secs"`
	// TokenEndpointURL is where the client exchanges its client credentials
	// for an access token. Client only.
	TokenEndpointURL string `toml:"token_endpoint_url"`
	// ClientID and ClientSecret authenticate the token request. Client only,
	// both required.
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	// Scope asks the provider for; optional.
	Scope string `toml:"scope"`
}

// HTTPPluginConfig is one [[http_plugins]] webhook: the server POSTs a JSON
// envelope for every operation the plugin declares and refuses the session,
// the registration or the visitor connection when the answer says reject.
type HTTPPluginConfig struct {
	Name string `toml:"name"`
	// Addr is the plugin's base URL, "http://host:port" or "https://host:port";
	// a bare host:port is taken as http.
	Addr string `toml:"addr"`
	// Path is appended to Addr; it must start with a slash.
	Path string `toml:"path"`
	// Ops lists the operations this plugin wants: "login", "newProxy" or
	// "newUserConn".
	Ops []string `toml:"ops"`
	// TimeoutSecs bounds each call; 0 selects 5 seconds. A plugin that cannot
	// be reached rejects the operation.
	TimeoutSecs int `toml:"timeout_secs"`
	// TLSVerify turns certificate verification on for an https plugin address.
	TLSVerify bool `toml:"tls_verify"`
}

// ClientConfig is the [client] section.
type ClientConfig struct {
	ServerAddr string `toml:"server_addr"`
	AuthToken  string `toml:"auth_token"`
	// AuthTokenFile reads the shared secret from a file instead of the file
	// holding it, which is frp's auth.tokenSource with type "file". Trailing
	// whitespace and a final newline are trimmed, and a token in the
	// configuration file is overridden — with a warning, because a file that
	// carries both is usually a half-finished migration.
	AuthTokenFile string `toml:"auth_token_file"`

	ReconnectSeconds    int `toml:"reconnect_seconds"`
	MaxReconnectSeconds int `toml:"max_reconnect_seconds"`
	HeartbeatSeconds    int `toml:"heartbeat_seconds"`
	DialTimeoutSecs     int `toml:"dial_timeout_seconds"`
	IdleTimeoutSecs     int `toml:"idle_timeout_seconds"`

	// DialVia optionally routes the connections to the server through a
	// SOCKS5 or HTTP CONNECT proxy, for example "socks5://user:pass@10.0.0.2:1080"
	// or "http://proxy.lan:3128". Empty dials the server directly.
	DialVia string `toml:"dial_via"`

	// Admin runs the client's management API on a local HTTP listener,
	// mirroring frp's client webServer: /healthz, /api/status and
	// /api/reload. See AdminConfig.
	Admin *AdminConfig `toml:"admin"`
	// Metas carries operator-chosen key-value pairs that ride the auth
	// request, so the server can show them and hand them to its
	// [[http_plugins]] webhooks.
	Metas map[string]string `toml:"metas"`
	// LoginFailExit makes the client give up when its very first login is
	// refused instead of retrying forever (frp's loginFailExit). A session
	// that logged in once still reconnects when it drops. frp defaults this
	// to true; this client keeps retrying unless the operator asks for the
	// exit.
	LoginFailExit bool `toml:"login_fail_exit"`
	// Start names the proxies and visitors to start; the others in the file
	// stay defined and stay stopped, which is frp's `start`. Empty starts
	// everything, and a name that matches nothing is a warning rather than an
	// error, so a file can carry a name a disabled fragment would define.
	Start []string `toml:"start"`
	// TCPKeepAliveSeconds is the interval between TCP keep-alive probes on the
	// connections this client holds to its server; 0 keeps the Go default (15
	// seconds). frp calls it transport.dialServerKeepalive and defaults to 7200.
	TCPKeepAliveSeconds int `toml:"tcp_keepalive_seconds"`
	// PoolCount keeps this many data connections open and parked at the server
	// in advance, so a visitor's first byte does not wait for a dial and a key
	// agreement — frp's transport.poolCount, whose default (0) this key keeps:
	// without it every stream opens its own connection when a visitor arrives.
	// A parked connection carries one stream and is replaced afterwards, so the
	// client holds that many idle connections per session; the server refuses
	// more than MaxPoolCount of them.
	PoolCount int `toml:"pool_count"`
	// DNSServer is the resolver this client sends its lookups to when it
	// resolves the server address; empty uses the system resolver. frp calls it
	// dnsServer. It is a host:port, and the port is required so a typo cannot
	// silently fall back to the system resolver's default port.
	DNSServer string `toml:"dns_server"`
	// User is this client's identity, which is frp's `user`: a private proxy
	// may name it in allow_users, and a private proxy without allow_users is
	// reachable only by visitors that give this same identity.
	User string `toml:"user"`
	// ConnectServerLocalIP binds this client's source address when it opens a
	// connection to its server, which is frp's connectServerLocalIP: a
	// multi-homed client picks which local address the server sees, and a
	// firewall rule can name it. It must be an IP literal on this machine; an
	// address that is not there fails at dial time.
	ConnectServerLocalIP string `toml:"connect_server_local_ip"`
	// UDPPacketSize caps a relayed datagram's payload in bytes on the visitor
	// side of a udp or sudp tunnel; a datagram over the cap is dropped rather
	// than truncated. Zero selects the largest UDP payload (65535); frp calls
	// the same knob udpPacketSize and defaults to 1500.
	UDPPacketSize int `toml:"udp_packet_size"`

	// ClientID is this client's stable name, which frp calls clientID. It
	// rides the auth request, and the server uses it wherever it names the
	// client — its log, its audit records, the dashboard's client and pool rows
	// and the bandwidth ledger's entries — so that a name outlives one session's
	// random identifier. Empty defaults to the host name; a client that is older
	// than this key sends nothing and is named by its session identifier.
	ClientID string `toml:"client_id"`

	// FeatureGates is [client] feature_gates: capabilities of this program an
	// operator may want to forbid. See the Feature* constants.
	FeatureGates map[string]bool `toml:"feature_gates"`
}

// FeatureEnabled reports whether the named gate is on. A gate the operator did
// not mention is on, because the gated features ship enabled and the section is
// how they are turned off. Both sections are consulted, so a gate takes effect
// wherever it was written.
func (c *Config) FeatureEnabled(name string) bool {
	for _, gates := range []map[string]bool{c.Server.FeatureGates, c.Client.FeatureGates} {
		if enabled, ok := gates[name]; ok {
			return enabled
		}
	}
	return true
}

// StartSet returns the names client.start selects, or nil when the list is
// empty and every proxy and visitor starts.
func (c ClientConfig) StartSet() map[string]struct{} {
	if len(c.Start) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(c.Start))
	for _, name := range c.Start {
		set[name] = struct{}{}
	}
	return set
}

// startNamesDisabled returns, in configuration order and once each, the
// client.start names that match an entry carrying enabled = false.
func (c *Config) startNamesDisabled() []string {
	if len(c.Client.Start) == 0 {
		return nil
	}
	off := map[string]bool{}
	for _, p := range c.Proxies {
		if !p.IsEnabled() {
			off[p.Name] = true
		}
	}
	for _, v := range c.Visitors {
		if !v.IsEnabled() {
			off[v.Name] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range c.Client.Start {
		if off[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// missingStartNames returns, in configuration order and once each, the
// client.start names that no proxy or visitor defines.
func (c *Config) missingStartNames() []string {
	if len(c.Client.Start) == 0 {
		return nil
	}
	defined := make(map[string]struct{}, len(c.Proxies)+len(c.Visitors))
	for _, p := range c.Proxies {
		defined[p.Name] = struct{}{}
	}
	for _, v := range c.Visitors {
		defined[v.Name] = struct{}{}
	}
	var missing []string
	seen := make(map[string]struct{}, len(c.Client.Start))
	for _, name := range c.Client.Start {
		if _, ok := defined[name]; ok {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		missing = append(missing, name)
	}
	return missing
}

// AdminConfig is the [client.admin] section. It serves the client's
// management API: GET /healthz answers without credentials, GET /api/status
// reports the session and every proxy, POST /api/reload re-reads the config
// file. When user and password are both set the API routes beyond /healthz
// require HTTP basic auth. The listener binds 127.0.0.1 unless bind_addr says
// otherwise.
type AdminConfig struct {
	Enabled  bool   `toml:"enabled"`
	BindAddr string `toml:"bind_addr"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// ProxyConfig is one [[proxies]] entry.
type ProxyConfig struct {
	Name string `toml:"name"`
	Type string `toml:"type"`
	// Enabled turns this proxy off without deleting it, which is frp's
	// `enabled`: the entry stays in the file, keeps its place in the review
	// history, and starts nothing. Unset means enabled.
	Enabled    *bool  `toml:"enabled"`
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

	// Subdomain publishes an http/https proxy under this label plus the
	// server's [server].subdomain_host, next to any custom domains. One DNS
	// label; the server composes and validates the full name.
	Subdomain string `toml:"subdomain"`
	// HTTPUser and HTTPPassword guard an http/https hostname with HTTP basic
	// auth; the server checks them before any byte reaches the tunnel.
	HTTPUser     string `toml:"http_user"`
	HTTPPassword string `toml:"http_password"`
	// Multiplexer names the shared entry a tcpmux proxy rides; only
	// "httpconnect" exists.
	Multiplexer string `toml:"multiplexer"`
	// UseCompression carries the proxy's byte streams snappy-compressed
	// between this client and the server. It has no effect on a datagram
	// tunnel and on an xtcp path that punched past the server.
	UseCompression bool `toml:"use_compression"`
	// PluginCertFile and PluginKeyFile hold the certificate the https2http
	// plugin terminates the visitor's TLS with.
	PluginCertFile string `toml:"plugin_cert_file"`
	PluginKeyFile  string `toml:"plugin_key_file"`
	// RouteByHTTPUser splits a hostname by the basic-auth username the visitor
	// presents: on one hostname, a proxy that names a user is matched only by
	// requests presenting that username, and a proxy that names none is matched
	// by everyone the named routes miss. Only for http/https.
	RouteByHTTPUser string `toml:"route_by_http_user"`
	// HostHeaderRewrite replaces the Host header the local service sees: the
	// http2https plugin sends it to its local service, and the server's
	// terminating virtual-host path forwards it on http/https proxies. Empty
	// keeps the visitor's Host.
	HostHeaderRewrite string `toml:"host_header_rewrite"`
	// TLSPassthrough moves an https tunnel to the server's passthrough
	// listener: the visitor's TLS session is relayed untouched and the
	// certificate the visitor sees is this client's own — the local service
	// must therefore speak TLS itself (or the https2http plugin terminates
	// for it). The server's shared listener keeps terminating TLS for https
	// proxies without this flag.
	TLSPassthrough bool `toml:"tls_passthrough"`
	// Annotations are free-form labels this proxy carries, which is frp's
	// annotations: the dashboard shows them and nothing else reads them, so an
	// operator can attach "owner", "env", a ticket number and the like to a
	// tunnel without inventing a naming convention for its name.
	Annotations map[string]string `toml:"annotations"`
	// AllowUsers lists the client identities that may visit this private proxy
	// (stcp, sudp or xtcp), which is frp's allow_users. An empty list allows
	// the publisher's own [client].user, so a private proxy is reachable by the
	// publishing operator's other clients and by nobody else.
	AllowUsers []string `toml:"allow_users"`
	// Bandwidth caps this proxy's data rate on the client, both directions
	// combined, in decimal bytes per second: "1MB", "500KB". Empty means no
	// limit.
	Bandwidth string `toml:"bandwidth"`
	// RequestHeaders and ResponseHeaders are set on the HTTP requests the
	// server's terminating virtual-host path relays and on the answers it sends
	// back. They apply to http proxies and to https proxies that terminate on
	// the shared listener; a tls_passthrough relay never parses HTTP.
	RequestHeaders  map[string]string `toml:"request_headers"`
	ResponseHeaders map[string]string `toml:"response_headers"`
	// Locations are the path prefixes this http/https proxy serves on its
	// hostnames, so several proxies can share one hostname and split it by
	// path: the server routes each request to the proxy whose prefix is the
	// longest match. A proxy without locations catches whatever is left on the
	// hostname.
	Locations []string `toml:"locations"`
	// ProxyProtocol asks the server to prepend a PROXY protocol header with the
	// visitor's addresses to the stream the local service receives, so the
	// service can log and filter on the real visitor. "v1" (text) or "v2"
	// (binary); empty sends none.
	ProxyProtocol string `toml:"proxy_protocol"`
	// RemotePorts expands one entry into several proxies: "6000-6002" becomes
	// remote_port 6000, 6001 and 6002 with names name-6000, name-6001 and
	// name-6002. Everything else in the entry applies to every copy.
	RemotePorts string `toml:"remote_ports"`
	// Plugin replaces the local service with a component the client runs
	// itself: "static_file" serves the directory at plugin_local_path over
	// HTTP, "unix_domain_socket" dials the socket at plugin_local_path.
	Plugin             string `toml:"plugin"`
	PluginLocalPath    string `toml:"plugin_local_path"`
	PluginHTTPUser     string `toml:"plugin_http_user"`
	PluginHTTPPassword string `toml:"plugin_http_password"`
	// PluginUser and PluginPassword are the optional SOCKS5 credentials of the
	// socks5 plugin; both set turns on username/password authentication, both
	// empty answers any visitor.
	PluginUser     string `toml:"plugin_user"`
	PluginPassword string `toml:"plugin_password"`
	// HealthCheck probes the local service and refuses dials while it fails.
	HealthCheck *HealthCheckConfig `toml:"health_check"`
	// UseEncryption wraps this proxy's relayed byte stream in the same AEAD the
	// control connection uses even when [encryption] is off for the process,
	// which is frp's per-proxy transport.useEncryption. The key is derived from
	// the shared auth_token and the proxy's name, so both ends reach it without
	// another exchange; a session that already agreed a cipher keeps using it,
	// and this flag then changes nothing. The server echoes the flag on every
	// DataRequest, so a peer that predates it leaves the stream in the clear
	// instead of making it unreadable.
	UseEncryption bool `toml:"use_encryption"`
}

// HealthCheckConfig probes the local service behind a proxy. Type "tcp" opens
// a connection; "http" performs a GET on path and takes a 2xx answer as healthy.
// After max_failed consecutive failures the client withdraws the proxy from the
// server — the public endpoint closes — and publishes it again when a probe
// succeeds. A server from before the withdrawal message stays silent, and the
// client falls back to refusing dials while the proxy stays registered.
type HealthCheckConfig struct {
	Type      string `toml:"type"`
	IntervalS int    `toml:"interval_s"`
	TimeoutS  int    `toml:"timeout_s"`
	MaxFailed int    `toml:"max_failed"`
	Path      string `toml:"path"`
	// HTTPHeaders are sent with the http probe, so a health endpoint that wants
	// a credential — or a particular Host — can be probed at all. It is frp's
	// healthCheck.httpHeaders. A map rather than frp's list of name/value pairs:
	// two headers with the same name in one probe is not a case worth carrying.
	HTTPHeaders map[string]string `toml:"http_headers"`
}

// Proxy auth methods accepted in [[proxies]].auth_method.
const (
	AuthMethodSecret = "secret"
	AuthMethodNIZK   = "nizk"
	// AuthMethodSNARK proves knowledge of the secret with a Groth16 proof instead
	// of a signature: the proof says nothing about the secret and is bound to the
	// challenge. See pkg/snarkauth for the circuit and its trusted-setup note.
	AuthMethodSNARK = "snark"
)

// Visitor transports accepted in [[visitors]].transport.
const (
	// maxUDPPayloadSize is the largest payload a UDP datagram can carry, and so
	// the largest value server.udp_packet_size and client.udp_packet_size can
	// usefully ask for.
	maxUDPPayloadSize = 65535

	// TransportDefault relays the visitor's data over the control connection.
	TransportDefault = ""
	// TransportWebRTC moves the visitor's data path onto a WebRTC DataChannel.
	TransportWebRTC = "webrtc"
)

// Feature-gate names. A gate names a capability an operator may want to forbid:
// FeatureWebRTC is the visitor's WebRTC data path and FeatureSnark is the
// Groth16 visitor proof. FeatureVirtualNet is frp's only gate; it is accepted so
// a configuration pasted from frp is understood, but the virtual network itself
// is not reproduced here and the gate changes nothing.
//
// This is the opposite direction from frp, whose gates opt *into* staged
// features: here every gated feature ships enabled, so the section is how an
// operator turns one off, and a gate the operator did not mention is on.
const (
	FeatureWebRTC     = "WebRTC"
	FeatureSnark      = "Snark"
	FeatureVirtualNet = "VirtualNet"
)

// KnownFeatureGates are the names feature_gates accepts.
var KnownFeatureGates = []string{FeatureSnark, FeatureVirtualNet, FeatureWebRTC}

// Proxy types accepted in [[proxies]].
const (
	ProxyTypeTCP    = "tcp"
	ProxyTypeUDP    = "udp"
	ProxyTypeHTTP   = "http"
	ProxyTypeHTTPS  = "https"
	ProxyTypeSTCP   = "stcp"
	ProxyTypeSUDP   = "sudp"
	ProxyTypeXTCP   = "xtcp"
	ProxyTypeSOCKS  = "socks5"
	ProxyTypeTCPMUX = "tcpmux"
)

// ProxyTypes lists every type this build implements.
var ProxyTypes = []string{
	ProxyTypeTCP, ProxyTypeUDP, ProxyTypeHTTP, ProxyTypeHTTPS,
	ProxyTypeSTCP, ProxyTypeSUDP, ProxyTypeXTCP, ProxyTypeSOCKS,
	ProxyTypeTCPMUX,
}

// MultiplexerHTTPConnect is the only multiplexer a tcpmux proxy can ride.
const MultiplexerHTTPConnect = "httpconnect"

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
	return net.JoinHostPort(host, strconv.Itoa(p.LocalPort))
}

// isInProcessPlugin reports whether a plugin serves the visitor from inside this
// process rather than at local_ip:local_port. The other plugins — https2http,
// https2https, http2https and tls2raw — terminate the visitor's connection
// themselves and then dial local_port, so that address is real for them.
func isInProcessPlugin(name string) bool {
	switch name {
	case PluginStaticFile, PluginUnixSocket, PluginSocks5, PluginHTTPProxy:
		return true
	}
	return false
}

// VisitorConfig is one [[visitors]] entry. A visitor reaches a private proxy
// (stcp, sudp or xtcp) published by another client: it listens on a local port
// and sends every connection through the server, or directly to the owner when a
// hole punch succeeds.
type VisitorConfig struct {
	Name string `toml:"name"`
	// Enabled turns this visitor off without deleting it, which is frp's
	// `enabled` on a visitor: the entry stays in the file and listens on
	// nothing. Unset means enabled.
	Enabled *bool `toml:"enabled"`
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
	// Transport selects the data path for this visitor: the default relays over
	// the control connection, "webrtc" moves it onto a WebRTC DataChannel.
	Transport string `toml:"transport"`
	BindAddr  string `toml:"bind_addr"`
	BindPort  int    `toml:"bind_port"`
	// FallbackTo is a local address a caller is connected to when the tunnel
	// cannot be opened, which is frp's fallbackTo: the visitor of a private
	// proxy keeps working while the server or the tunnel is down, at the cost
	// of going straight to that address. Empty refuses the caller as before.
	FallbackTo string `toml:"fallback_to"`
	// FallbackTimeoutMs bounds the tunnel attempt before the fallback is used,
	// which is frp's fallbackTimeoutMs; 0 leaves the tunnel's own timeout in
	// charge. It does nothing without fallback_to.
	FallbackTimeoutMs int `toml:"fallback_timeout_ms"`
}

// IsEnabled reports whether this proxy starts: it does unless enabled = false
// says otherwise.
func (p ProxyConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// IsEnabled reports whether this visitor listens: it does unless
// enabled = false says otherwise.
func (v VisitorConfig) IsEnabled() bool { return v.Enabled == nil || *v.Enabled }

// ListenAddr is the local address a visitor listens on.
func (v VisitorConfig) ListenAddr() string {
	host := v.BindAddr
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(v.BindPort))
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
	// Protocol selects how the connection to the server is carried: empty is a
	// plain stream, "websocket" wraps every connection to the server — control,
	// data and visitor — in RFC 6455 binary frames, which lets the tunnel pass
	// intermediaries that only forward well-formed websocket sessions. TLS
	// (enable_tls) stays on the outside of the websocket, the wss shape. The
	// server needs no setting: it recognises the upgrade on the shared port.
	Protocol string `toml:"protocol"` // client
	// WireProtocol names the wire format the two ends speak, which is frp's
	// transport.wireProtocol. This program has exactly one wire format and
	// negotiates its revision inside the handshake, so the accepted value is
	// "v2" — the name frp gives the format this program implements — and the
	// value is checked rather than ignored: "v1" is refused with a message that
	// says so, because silently accepting it would promise a format that is not
	// there.
	WireProtocol string `toml:"wire_protocol"`
	// TCPMux keeps frp's transport.tcpMux key. frp multiplexes every logical
	// connection of a client over one yamux session, and this program does not:
	// each data connection carries one stream and they are never multiplexed,
	// which is the shape tcp_mux = false asks for. The absent key means true,
	// as it does in frp, so the field is a pointer: only a value the operator
	// actually wrote is reported.
	TCPMux *bool `toml:"tcp_mux"`
	// AdditionalScopes is frp's transport.additionalScopes: names of messages
	// that may omit the credentials frp repeats on them ("HeartBeats" and
	// "NewWorkConns"). The frames this program sends carry no credentials to
	// omit — a heartbeat is empty and a data open carries a session identifier
	// and a per-stream key — so the two frp names are accepted as the request
	// they make and change nothing.
	AdditionalScopes []string `toml:"additional_scopes"`
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

// LogConfig is the [log] section: where this process writes its own log and how
// much of it. Both roles read it, because both write one.
//
// It reproduces frp's [log] section: the file, the daily rotation with a
// retention window, the bare-line form, the level and the console colours. frp's
// log.to defaults to "console", its stdout; this program leaves the default
// writer at standard error, which is the stream a supervisor collects.
type LogConfig struct {
	// To is the file the log is appended to. Empty keeps the process's
	// standard error, which is what a supervisor collects; a path is what an
	// operator without a supervisor wants.
	To string `toml:"to"`
	// MaxDays keeps this many days of rotated files. A file that holds nothing
	// from an earlier day is not rotated, so this only bounds how long the
	// archive is kept: 0 leaves one growing file behind log.to, and a positive
	// value moves yesterday's file to <to>.<YYYY-MM-DD> and deletes archives
	// older than that many days.
	MaxDays int `toml:"max_days"`
	// Level is the least severe line that is written: "trace", "debug",
	// "info", "warn" or "error". It is frp's log.level, with frp's default
	// ("info"); empty means info. The levels above info are currently thin:
	// debug has a single call site (the xtcp hole-punching note) and trace is
	// kept for future use, so neither writes per-stream or per-frame lines
	// today. "warn" and "error" drop everything below them, which is what a
	// deployment that only wants to be told about trouble sets.
	Level string `toml:"level"`
	// DisableTimestamp drops the date and time from every line.
	DisableTimestamp bool `toml:"disable_timestamp"`
	// DisablePrintColor stops the console output from colouring each line by
	// its level, which is frp's disablePrintColor. Colour is only ever added
	// when the writer is a terminal: a file, a pipe and a supervisor's capture
	// are never coloured, whatever this key says.
	DisablePrintColor bool `toml:"disable_print_color"`
}

// LogLevels lists the accepted log.level values, least severe first.
var LogLevels = []string{"trace", "debug", "info", "warn", "error"}

const (
	LogLevelTrace = "trace"
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// LogLevelRank maps a level name to its position in LogLevels; an unknown name
// is reported as not found so a caller can refuse it.
func LogLevelRank(level string) (int, bool) {
	for i, name := range LogLevels {
		if name == level {
			return i, true
		}
	}
	return 0, false
}

// StoreConfig is the [store] section (client role), which is frp's Store: the
// proxies and visitors created at runtime through the client's admin API instead
// of being written in the configuration file.
type StoreConfig struct {
	// Path is the JSON file the runtime entries are persisted to and read back
	// from at startup. Empty disables the store: the endpoints that manage it
	// answer 404 and nothing is written, so a deployment that has not asked for
	// runtime changes cannot have them by accident.
	Path string `toml:"path"`
}

// MetricsConfig is the [metrics] section. The endpoint is served by the dashboard
// listener when the dashboard is enabled, and on its own address when Port is set;
// the second form is what a deployment that keeps the dashboard off uses, and it
// corresponds to frp's metrics.addr.
type MetricsConfig struct {
	Enabled bool `toml:"enabled"`
	// Token, when set, is required on /metrics in addition to the dashboard token.
	Token string `toml:"token"`
	// BindAddr is the address the metrics listener binds when Port is set. It
	// defaults to the loopback address, like the dashboard's.
	BindAddr string `toml:"bind_addr"`
	// Port serves GET /metrics on its own listener. 0 keeps the endpoint on the
	// dashboard listener alone. It needs Enabled: the key that decides whether the
	// endpoint exists at all is the one that says so.
	Port int `toml:"port"`
}

// AuditConfig is the [audit] section: an append-only JSONL record of security
// relevant events.
type AuditConfig struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
	// MaxBytes rotates the file once it exceeds this size. Zero selects the
	// 32 MiB default, like the other keys with a non-zero default: rotation is
	// what keeps the audit log bounded, and the auditor's own "no limit" reading
	// of zero is only reachable through the library, not through this file.
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
	// AnnounceTTLSeconds is how long an announcement stays valid on a reader, as
	// written into the record by its publisher; a reader's own value does not
	// shorten what it reads. Zero
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
	// observing the frame length reveals less about the payload. A nil pointer is
	// the key being absent, which selects 256 while the section is on; an explicit
	// zero disables padding, which an int could not express: it made the
	// documented "0 means no padding" unreachable.
	PadTo *int `toml:"pad_to"`
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

// DefaultObfuscationPadTo is the padding an enabled [obfuscation] section applies
// when the configuration does not name one.
const DefaultObfuscationPadTo = 256

// PadToBytes is the padding to apply. An absent key reads as no padding; the 256
// default is filled in by applyDefaults, so a loaded configuration that enables the
// section always has one, and only a configuration built in code can be left
// without.
//
// It is zero while the section is off, which is what `enabled` means: the file may
// keep a pad_to beside `enabled = false` — both shipped examples do — and the
// documented "打开后下列各项才会生效" has to hold for the value actually applied.
func (o ObfuscationConfig) PadToBytes() int {
	if !o.Enabled || o.PadTo == nil {
		return 0
	}
	return *o.PadTo
}

// Jitter is the random delay to apply before each write, zero while the section
// is off, for the same reason as PadToBytes.
func (o ObfuscationConfig) Jitter() time.Duration {
	if !o.Enabled || o.JitterMillis <= 0 {
		return 0
	}
	// The multiplication wraps for a millisecond count near the int64 ceiling, and a
	// wrapped-positive duration parks every write for years. Clamped here as well as
	// in the framer, because a configuration built in code need not have been
	// validated.
	delay := time.Duration(o.JitterMillis) * time.Millisecond
	if delay <= 0 || delay > protocol.MaxJitter {
		return protocol.MaxJitter
	}
	return delay
}

// ObfuscationDisguise is the disguise in force, with the empty value read as
// "none". A disguise named while the section is off is not applied: it is the
// outermost wrapper on every connection, and the validator's warning about the
// combination has to describe what the tunnel actually does.
func (c *Config) ObfuscationDisguise() string {
	if !c.Obfuscation.Enabled || c.Obfuscation.Disguise == "" {
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
	// OIDC, when present, lets a server accept an OIDC access token in place
	// of the static auth token, and lets a client fetch that token from its
	// identity provider with the client-credentials grant. Server only on the
	// issuer side, client only on the token-endpoint side.
	OIDC *OIDCConfig `toml:"oidc"`
	// HTTPPlugins names webhook endpoints a server calls on session login,
	// proxy registration and every visitor connection, so an operator can gate
	// and observe the server with its own HTTP service. Server only.
	HTTPPlugins []HTTPPluginConfig `toml:"http_plugins"`
	Server      ServerConfig       `toml:"server"`
	Client      ClientConfig       `toml:"client"`
	Dashboard   DashboardConfig    `toml:"dashboard"`
	Encryption  EncryptionConfig   `toml:"encryption"`
	Transport   TransportConfig    `toml:"transport"`
	Identity    IdentityConfig     `toml:"identity"`
	Obfuscation ObfuscationConfig  `toml:"obfuscation"`
	Metrics     MetricsConfig      `toml:"metrics"`
	Audit       AuditConfig        `toml:"audit"`
	Log         LogConfig          `toml:"log"`
	Ledger      LedgerConfig       `toml:"ledger"`
	DHT         DHTConfig          `toml:"dht"`
	VPN         VPNConfig          `toml:"vpn"`
	// Store keeps the proxies and visitors created at runtime through the
	// client's admin API, which is frp's Store. Client only.
	Store    StoreConfig     `toml:"store"`
	Proxies  []ProxyConfig   `toml:"proxies"`
	Visitors []VisitorConfig `toml:"visitors"`

	// SourceFile is the path the configuration was read from, set by Load. A
	// client whose configuration names a file re-reads it on SIGHUP; a
	// configuration built from a string has none to reload from.
	SourceFile string `toml:"-"`

	// Includes lists glob patterns of extra TOML fragments whose [[proxies]]
	// and [[visitors]] are appended to this configuration, relative to the
	// main file's directory. Resolved by Load; a configuration built from a
	// string has nothing to resolve them against.
	Includes []string `toml:"includes"`

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

// HTTP plugin operations: what a [[http_plugins]] webhook can be asked about.
const (
	HTTPPluginOpLogin       = "login"
	HTTPPluginOpNewProxy    = "newProxy"
	HTTPPluginOpNewUserConn = "newUserConn"
)

// HTTPPluginOps lists the accepted [[http_plugins]] ops values.
var HTTPPluginOps = []string{HTTPPluginOpLogin, HTTPPluginOpNewProxy, HTTPPluginOpNewUserConn}

// defaults fills in the values that make a minimal config usable.
func (c *Config) applyDefaults() {
	for _, p := range c.Proxies {
		if p.HealthCheck != nil {
			p.HealthCheck.healthCheckDefaults()
		}
	}
	if c.Server.MaxConnections == 0 {
		c.Server.MaxConnections = 512
	}
	// The published listeners follow the control port unless the operator moves
	// them, which is what frp's proxyBindAddr does.
	if c.Server.ProxyBindAddr == "" {
		c.Server.ProxyBindAddr = c.Server.BindAddr
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
	if c.Server.MaxPoolCount == 0 {
		c.Server.MaxPoolCount = MaxPoolCount
	}
	if c.Server.DialTimeoutSecs == 0 {
		c.Server.DialTimeoutSecs = 10
	}
	// The default applies when the key is absent or zero, like the other
	// durations here; a negative value is rejected by Validate.
	if c.Server.GracefulShutdownSecs == 0 {
		c.Server.GracefulShutdownSecs = 5
	}
	c.ApplyClientDefaults()
	if c.Log.Level == "" {
		c.Log.Level = LogLevelInfo
	}
	if c.Server.SSHTunnelGateway != nil && c.Server.SSHTunnelGateway.BindAddr == "" {
		c.Server.SSHTunnelGateway.BindAddr = c.Server.BindAddr
	}
	if c.Client.Admin != nil && c.Client.Admin.Enabled && c.Client.Admin.BindAddr == "" {
		// frp's webServer defaults to the loopback interface too: the
		// management API is for the operator on the same machine.
		c.Client.Admin.BindAddr = "127.0.0.1"
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = 7500
	}
	if c.Dashboard.BindAddr == "" {
		c.Dashboard.BindAddr = "127.0.0.1"
	}
	if c.Metrics.BindAddr == "" {
		c.Metrics.BindAddr = "127.0.0.1"
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
		// Only fill in a ceiling the operator did not choose, and make the
		// default at least ban_seconds: an explicit ceiling stays as given,
		// so validation can refuse one that is below ban_seconds.
		if c.Server.BanMaxSeconds == 0 {
			c.Server.BanMaxSeconds = max(c.Server.BanSeconds, 3600)
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
			c.DHT.RepublishSeconds = DefaultRepublishSeconds(c.DHT.AnnounceTTLSeconds)
		}
		if c.DHT.TTLSeconds == 0 {
			// The DHT package would pick the same hour, but the check below
			// compares this value with announce_ttl_seconds, and a check that
			// ignores the default lets announce_ttl_seconds be longer than the TTL
			// the node really uses: the record then expires between two
			// republishes — and readers stop honouring it even earlier — so the
			// name stops resolving while the server is still running.
			c.DHT.TTLSeconds = int(dht.DefaultTTL / time.Second)
		}
		if c.DHT.LookupTimeoutSeconds == 0 {
			c.DHT.LookupTimeoutSeconds = 5
		}
	}
	if c.Obfuscation.Enabled && c.Obfuscation.PadTo == nil {
		padTo := DefaultObfuscationPadTo
		c.Obfuscation.PadTo = &padTo
	}
	// The [[proxies]] list is left alone here: in a client configuration every entry
	// describes a service and gets its type filled in, while in a server
	// configuration the same list states a policy per name and an absent type means
	// "any type". Which of the two it is is decided in Validate, which knows the
	// role.
	for i := range c.Visitors {
		if c.Visitors[i].Type == "" {
			c.Visitors[i].Type = ProxyTypeSTCP
		}
		if c.Visitors[i].AuthMethod == "" {
			c.Visitors[i].AuthMethod = AuthMethodSecret
		}
	}
	// Filled in whether or not [identity] is enabled: the default names the file
	// an operator creates on the first --identity run, which is also what the
	// documented default says, and the run path only reads it when the section
	// is on. Gating it on Enabled left the key file empty in a default
	// configuration, where --identity then tried to write a key to "".
	if c.Identity.KeyFile == "" {
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
	// LoadBalanceBandit picks the member a UCB1 multi-armed bandit would pick: every
	// member is tried once, then the one with the highest estimated reward plus an
	// exploration term wins, where the reward of a stream is its speed. The estimates
	// are learned online from the streams the pool actually serves — there is no
	// offline training and no model file.
	LoadBalanceBandit = "bandit"
)

// MaxMultipath bounds [[proxies]].multipath.
const MaxMultipath = 8

// MaxPoolCount bounds [client].pool_count and the number of parked connections
// the server keeps per session. The two are one number so a client cannot ask
// for more than the server holds: a client whose pool is refused would fall back
// to opening a connection per stream, and the reason would be a limit that
// nothing warned about.
const MaxPoolCount = 64

// ApplyClientDefaults fills the [client] section's defaults into a configuration
// that did not come from Load. Run and RunWithShell accept a configuration built in
// code, and these settings are the ones whose zero is not "unset" at every use
// site: a zero dial timeout expires the deadline the moment it is set (the client
// could never log in), a zero reconnect interval turns the backoff into a retry
// loop, a zero idle timeout means no idle timeout, and the heartbeat falls back
// to nothing. It is the same defaulting Load does for this section, and the
// server's own defaults are left alone.
func (c *Config) ApplyClientDefaults() {
	if c.Client.ReconnectSeconds == 0 {
		c.Client.ReconnectSeconds = 3
	}
	if c.Client.MaxReconnectSeconds == 0 {
		c.Client.MaxReconnectSeconds = max(c.Client.ReconnectSeconds, 60)
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
	// A client with no client_id of its own is named by its host, which is frp's
	// default and is stable enough to appear in a server's log and audit records.
	if c.Client.ClientID == "" {
		if host, err := os.Hostname(); err == nil {
			c.Client.ClientID = host
		}
	}
}

// MaxDurationSeconds is the largest value a setting in seconds may have. Every
// one of them is multiplied by time.Second into an int64 duration, and a few are
// multiplied again by a small factor (the heartbeat window is three intervals),
// so this leaves room for the largest factor any use site applies, eight. Past
// it the product wraps: the wrapped value is usually negative, and every use
// site reads a non-positive limit as "no limit", so an absurd timeout would
// silently remove the bound it names instead of setting it. The 1e9 below is the
// nanoseconds in a second, written out so the constant stays untyped.
const MaxDurationSeconds = math.MaxInt64 / 1_000_000_000 / 8

// MaxDurationMillis is the same ceiling for a setting in milliseconds. Exactly one
// exists — visitor.fallback_timeout_ms — and it is multiplied by time.Millisecond,
// so the wrap happens a thousand times later than a seconds setting's. It is typed
// int64 because 9223372036854 does not fit an int on a 32-bit target, and this
// package is still built for the Android bindings' 32-bit architectures.
const MaxDurationMillis int64 = math.MaxInt64 / 1_000_000

// node can be bootstrapped by address without first being told which port it
// chose.
const (
	DefaultDHTAddr      = "0.0.0.0:7001"
	DefaultDHTNamespace = "aethertunnel"
)

// DefaultRepublishSeconds is the interval [dht].republish_seconds takes when the
// configuration does not name one: a third of the announce TTL, so a live
// announcement is rewritten twice before a reader stops honouring it.
//
// The floor of one second is what keeps that true for a short TTL. A third of
// one second is zero, and the discovery node reads a zero interval as "the
// caller chose nothing" and falls back to its own 30-second default, which is
// longer than such a TTL: the announcement would lapse before it was rewritten
// and the name would stop resolving while the server was still running.
func DefaultRepublishSeconds(announceTTLSeconds int) int {
	if interval := announceTTLSeconds / 3; interval > 0 {
		return interval
	}
	return 1
}

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

// applyTokenFiles replaces an inline auth token with the contents of the file
// the configuration names, before the environment gets its turn: an unreadable
// or empty file is a hard error, because a token that silently became empty
// would surface much later as an authentication failure that names nothing.
func (c *Config) applyTokenFiles() error {
	for _, side := range []struct {
		name  string
		path  string
		token *string
	}{
		{"server", c.Server.AuthTokenFile, &c.Server.AuthToken},
		{"client", c.Client.AuthTokenFile, &c.Client.AuthToken},
	} {
		if side.path == "" {
			continue
		}
		data, err := os.ReadFile(side.path)
		if err != nil {
			return fmt.Errorf("%s.auth_token_file: %w", side.name, err)
		}
		// A token file written by an editor ends in a newline; the secret is
		// the line, not the file's last byte.
		value := strings.TrimSpace(string(data))
		if value == "" {
			return fmt.Errorf("%s.auth_token_file %s is empty", side.name, side.path)
		}
		if *side.token != "" && *side.token != value {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"%s.auth_token_file overrides the inline %s.auth_token", side.name, side.name))
		}
		*side.token = value
	}
	return nil
}

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
	return net.JoinHostPort(c.Server.BindAddr, strconv.Itoa(c.Server.BindPort))
}

// ProxyHost is the address the published proxy listeners bind: the configured
// server.proxy_bind_addr, or server.bind_addr when it is unset — a
// configuration built without applyDefaults still lands on the control
// address rather than on every interface.
func (c *Config) ProxyHost() string {
	if c.Server.ProxyBindAddr != "" {
		return c.Server.ProxyBindAddr
	}
	return c.Server.BindAddr
}

// DetailedErrors reports whether a refusal that goes to a client carries the
// server's full wording. An absent server.detailed_errors_to_client means
// detailed, the same default frp's detailedErrorsToClient has; only an
// explicit false shortens the text.
func (c *Config) DetailedErrors() bool {
	return c.Server.DetailedErrorsToClient == nil || *c.Server.DetailedErrorsToClient
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
	} else if host, _, err := net.SplitHostPort(c.Client.ServerAddr); err == nil {
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

// AllowedIdentities parses [identity].allowed_keys. A section that is off has no
// allow-list, exactly as the client ignores its key file when the section is off:
// parsing one anyway made a disabled section's placeholder text stop the server.
func (c *Config) AllowedIdentities() ([]ed25519.PublicKey, error) {
	if !c.Identity.Enabled {
		return nil, nil
	}
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
	return net.JoinHostPort(c.ProxyHost(), strconv.Itoa(c.Server.HTTPPort))
}

// HTTPSAddr is the address of the shared TLS virtual-host listener.
func (c *Config) HTTPSAddr() string {
	return net.JoinHostPort(c.ProxyHost(), strconv.Itoa(c.Server.HTTPSPort))
}

// P2PAddr is the UDP rendezvous address used for hole punching.
func (c *Config) P2PAddr() string {
	return net.JoinHostPort(c.Server.BindAddr, strconv.Itoa(c.Server.P2PPort))
}

// DashboardAddr is the address the dashboard binds. An empty bind address is the
// loopback default, filled here as well as by applyDefaults: server.New also takes
// a configuration built in code, where an empty host would bind every interface on
// a panel that carries the server's control surface.
func (c *Config) DashboardAddr() string {
	bind := c.Dashboard.BindAddr
	if bind == "" {
		bind = "127.0.0.1"
	}
	return net.JoinHostPort(bind, strconv.Itoa(c.Dashboard.Port))
}

// MetricsAddr is the address the metrics listener binds. It is only used when
// [metrics].port is set.
func (c *Config) MetricsAddr() string {
	bind := c.Metrics.BindAddr
	if bind == "" {
		// The default applyDefaults fills in, applied here as well because server.New
		// also takes a configuration built in code, where an empty address would bind
		// every interface on a listener that has no authentication of its own.
		bind = "127.0.0.1"
	}
	return net.JoinHostPort(bind, strconv.Itoa(c.Metrics.Port))
}

// boundListener is one address this process would bind, with the key that names it.
type boundListener struct {
	key  string
	kind string // "tcp" or "udp"
	host string
	port int
}

// listenerConflict returns a problem when two of these listeners would bind the same
// address, and false when none would.
//
// One process cannot listen on a port twice under the same address, and the failure
// arrives late and beside the point: the server binds what it can, prints that it is
// listening, and then exits with "bind: address already in use" naming an address
// rather than the two keys that disagree. Both are in the file, so this is decided
// before anything is bound.
func listenerConflict(listeners []boundListener) (string, bool) {
	for i := 0; i < len(listeners); i++ {
		for j := i + 1; j < len(listeners); j++ {
			first, second := listeners[i], listeners[j]
			if first.kind != second.kind || first.port != second.port {
				continue
			}
			if first.host != second.host && !isWildcardHost(first.host) && !isWildcardHost(second.host) {
				continue
			}
			return fmt.Sprintf(
				"%s and %s would both listen on %s port %d (%s and %s): one process cannot bind the same address twice",
				first.key, second.key, first.kind, first.port,
				net.JoinHostPort(first.host, strconv.Itoa(first.port)),
				net.JoinHostPort(second.host, strconv.Itoa(second.port))), true
		}
	}
	return "", false
}

// isWildcardHost reports whether a bind address means "every local address", which
// overlaps whatever another listener binds on the same port.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsUnspecified()
}

// listeners lists every address this configuration would bind for the given role.
func (c *Config) listeners(role string) []boundListener {
	var listeners []boundListener
	if role == RoleServer {
		listeners = append(listeners, boundListener{
			key: "server.bind_port", kind: "tcp", host: c.Server.BindAddr, port: c.Server.BindPort,
		})
		if c.Dashboard.Enabled {
			// The empty bind means loopback here exactly as it does at the real
			// bind site (DashboardAddr), so an absent host must not read as the
			// wildcard it never binds as.
			host := c.Dashboard.BindAddr
			if host == "" {
				host = "127.0.0.1"
			}
			listeners = append(listeners, boundListener{
				key: "dashboard.port", kind: "tcp",
				host: host, port: c.Dashboard.Port,
			})
		}
		if c.Metrics.Enabled && c.Metrics.Port > 0 {
			host := c.Metrics.BindAddr
			if host == "" {
				host = "127.0.0.1"
			}
			listeners = append(listeners, boundListener{
				key: "metrics.port", kind: "tcp",
				host: host, port: c.Metrics.Port,
			})
		}
		if c.Server.HTTPPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.http_port", kind: "tcp", host: c.ProxyHost(), port: c.Server.HTTPPort,
			})
		}
		if c.Server.HTTPSPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.https_port", kind: "tcp", host: c.ProxyHost(), port: c.Server.HTTPSPort,
			})
		}
		if c.Server.TCPMuxPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.tcpmux_port", kind: "tcp", host: c.ProxyHost(), port: c.Server.TCPMuxPort,
			})
		}
		if c.Server.HTTPSPassthroughPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.https_passthrough_port", kind: "tcp", host: c.ProxyHost(), port: c.Server.HTTPSPassthroughPort,
			})
		}
		if c.DHT.Enabled {
			if host, port, err := net.SplitHostPort(c.DHT.ListenAddr); err == nil {
				if number, err := strconv.Atoi(port); err == nil {
					listeners = append(listeners, boundListener{
						key: "dht.listen_addr", kind: "udp", host: host, port: number,
					})
				}
			}
		}
		if c.Server.P2PPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.p2p_port", kind: "udp", host: c.Server.BindAddr, port: c.Server.P2PPort,
			})
		}
		if g := c.Server.SSHTunnelGateway; g != nil && g.BindPort > 0 {
			listeners = append(listeners, boundListener{
				key: "server.ssh_tunnel_gateway.bind_port", kind: "tcp", host: g.BindAddr, port: g.BindPort,
			})
		}
	}
	if role == RoleClient {
		for _, visitor := range c.Visitors {
			if !visitor.IsEnabled() {
				// A stopped visitor binds nothing, so counting it would report a
				// conflict with a port nothing is listening on.
				continue
			}
			host := visitor.BindAddr
			if host == "" {
				host = "127.0.0.1"
			}
			kind := "tcp"
			if IsDatagramProxyType(visitor.Type) {
				// A udp/sudp visitor listens on its own UDP socket: the same port
				// as a tcp visitor is two sockets, not a conflict.
				kind = "udp"
			}
			listeners = append(listeners, boundListener{
				key:  fmt.Sprintf("visitor %q bind_port", visitor.Name),
				kind: kind, host: host, port: visitor.BindPort,
			})
		}
		// The client's own admin API and the DHT node it runs to resolve a
		// discovered address bind ports too; leaving them out let a visitor be
		// written onto the same address and fail late, at the second net.Listen.
		if c.Client.Admin != nil && c.Client.Admin.Enabled && c.Client.Admin.Port > 0 {
			host := c.Client.Admin.BindAddr
			if host == "" {
				host = "127.0.0.1"
			}
			listeners = append(listeners, boundListener{
				key: "client.admin.port", kind: "tcp", host: host, port: c.Client.Admin.Port,
			})
		}
		if c.usesClientDHT() {
			if host, port, err := net.SplitHostPort(c.DHT.ListenAddr); err == nil {
				if number, err := strconv.Atoi(port); err == nil {
					listeners = append(listeners, boundListener{
						key: "dht.listen_addr", kind: "udp", host: host, port: number,
					})
				}
			}
		}
	}
	return listeners
}

// usesClientDHT reports whether a client configuration runs a DHT node of its
// own, which is what makes dht.listen_addr a bound socket. It is the loader's
// copy of the client's own usesDiscoveredAddress: a client that was given a
// server address never starts the node.
func (c *Config) usesClientDHT() bool {
	return c.DHT.Enabled && c.DHT.Discover != "" && c.Client.ServerAddr == ""
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
	// The rest of what applyDefaults supplies for values this function then
	// checks against a fixed set. The doc above promises a Config built in code
	// gets the same treatment as a loaded file, and without this a visitor with
	// no type or an enabled [encryption] with no algorithm was refused for a
	// default the file path would have filled in.
	if c.Encryption.Algorithm == "" {
		c.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
	}
	if c.Encryption.Salt == "" {
		c.Encryption.Salt = "aethertunnel"
	}
	for i := range c.Visitors {
		if c.Visitors[i].Type == "" {
			c.Visitors[i].Type = ProxyTypeSTCP
		}
		if c.Visitors[i].AuthMethod == "" {
			c.Visitors[i].AuthMethod = AuthMethodSecret
		}
	}

	switch role {
	case RoleServer:
		if c.Server.BindAddr == "" {
			problems = append(problems, "server.bind_addr is required")
		}
		if c.Server.ProxyBindAddr != "" && net.ParseIP(c.Server.ProxyBindAddr) == nil {
			if c.Server.ProxyBindAddr == c.Server.BindAddr {
				// applyDefaults copies server.bind_addr here, so the offending
				// value is the one the operator wrote under the other key;
				// naming proxy_bind_addr sends them looking for a line that is
				// not in the file.
				problems = append(problems, fmt.Sprintf(
					"server.bind_addr %q is not an IP address, and it is the address the published listeners "+
						"bind (server.proxy_bind_addr defaults to it): set server.proxy_bind_addr to the IP "+
						"those listeners should bind", c.Server.BindAddr))
			} else {
				problems = append(problems, fmt.Sprintf(
					"server.proxy_bind_addr %q is not an IP address: the published listeners bind it directly", c.Server.ProxyBindAddr))
			}
		}
		if c.Server.UserConnTimeoutSeconds < 0 {
			problems = append(problems, fmt.Sprintf(
				"server.user_conn_timeout_seconds cannot be negative, got %d", c.Server.UserConnTimeoutSeconds))
		}
		if c.Server.BindPort < 1 || c.Server.BindPort > 65535 {
			problems = append(problems, fmt.Sprintf("server.bind_port must be 1-65535, got %d", c.Server.BindPort))
		}
		if c.Server.MaxPortsPerClient < 0 {
			problems = append(problems, fmt.Sprintf("server.max_ports_per_client cannot be negative, got %d", c.Server.MaxPortsPerClient))
		}
		if c.Server.MaxPoolCount < 0 {
			problems = append(problems, fmt.Sprintf("server.max_pool_count cannot be negative, got %d", c.Server.MaxPoolCount))
		}
		if c.Server.HTTPSPassthroughPort < 0 {
			problems = append(problems, fmt.Sprintf("server.https_passthrough_port cannot be negative, got %d", c.Server.HTTPSPassthroughPort))
		}
		if _, err := ParsePortRanges(c.Server.AllowPorts); err != nil {
			problems = append(problems, fmt.Sprintf("server.allow_ports: %v", err))
		}
		if c.Server.VhostHTTPTimeout < 0 {
			problems = append(problems, "server.vhost_http_timeout cannot be negative")
		}
		if c.Server.UDPPacketSize < 0 {
			problems = append(problems, "server.udp_packet_size cannot be negative")
		}
		if c.Server.UDPPacketSize > maxUDPPayloadSize {
			problems = append(problems, fmt.Sprintf("server.udp_packet_size cannot exceed %d, got %d", maxUDPPayloadSize, c.Server.UDPPacketSize))
		}
		if c.Server.HTTPSPassthroughPort > 0 && c.Server.HTTPSPassthroughPort == c.Server.HTTPSPort {
			problems = append(problems, "server.https_passthrough_port and server.https_port must differ: one relays TLS and the other terminates it")
		}
		if o := c.OIDC; o != nil {
			if o.Issuer == "" && o.JWKSURL == "" {
				problems = append(problems, "oidc.issuer is required when the [oidc] section is present: the server needs an identity provider to verify tokens with")
			}
			if o.TimeoutSecs < 0 {
				problems = append(problems, "oidc.timeout_secs cannot be negative")
			}
			if o.TokenEndpointURL != "" || o.ClientID != "" || o.ClientSecret != "" || o.Scope != "" {
				c.Warnings = append(c.Warnings, "oidc.token_endpoint_url, oidc.client_id/client_secret and oidc.scope have no effect in a server configuration: the token request runs in a client")
			}
		}
		for i := range c.HTTPPlugins {
			plugin := &c.HTTPPlugins[i]
			if plugin.Name == "" {
				problems = append(problems, fmt.Sprintf("http_plugins[%d]: name is required", i))
			}
			if plugin.Addr == "" {
				problems = append(problems, fmt.Sprintf("http_plugins[%d] %q: addr is required", i, plugin.Name))
			}
			if plugin.Path != "" && !strings.HasPrefix(plugin.Path, "/") {
				problems = append(problems, fmt.Sprintf("http_plugins[%d] %q: path must start with a slash", i, plugin.Name))
			}
			if plugin.TimeoutSecs < 0 {
				problems = append(problems, fmt.Sprintf("http_plugins[%d] %q: timeout_secs cannot be negative", i, plugin.Name))
			}
			if len(plugin.Ops) == 0 {
				problems = append(problems, fmt.Sprintf("http_plugins[%d] %q: ops is empty; name at least one of %s",
					i, plugin.Name, strings.Join(HTTPPluginOps, ", ")))
			}
			for _, op := range plugin.Ops {
				known := false
				for _, knownOp := range HTTPPluginOps {
					if op == knownOp {
						known = true
						break
					}
				}
				if !known {
					problems = append(problems, fmt.Sprintf("http_plugins[%d] %q: op %q is not one of %s",
						i, plugin.Name, op, strings.Join(HTTPPluginOps, ", ")))
				}
			}
		}
		if c.Server.AuthToken == "" {
			problems = append(problems, "server.auth_token is required")
		} else if isWeakToken(c.Server.AuthToken) {
			c.Warnings = append(c.Warnings, "server.auth_token is short or a well-known placeholder; use at least 16 random characters")
		}
		if c.Client.Admin != nil {
			c.Warnings = append(c.Warnings, "client.admin has no effect in a server configuration: the management API runs in a client")
		}
		if c.Store.Path != "" {
			c.Warnings = append(c.Warnings, "store has no effect in a server configuration: the runtime entries belong to a client's admin API")
		}
		if len(c.Client.Start) > 0 {
			c.Warnings = append(c.Warnings, "client.start has no effect in a server configuration: it selects what a client runs")
		}
		if len(c.Visitors) > 0 {
			// A visitor is the local end that dials into somebody else's private proxy,
			// so a server never reads the section. It is still validated, which is why
			// this has to say so: otherwise an operator fixes the visitor entries a
			// server was never going to run.
			c.Warnings = append(c.Warnings, "visitors has no effect in a server configuration: a visitor runs in a client and dials into a private proxy")
		}
		if g := c.Server.SSHTunnelGateway; g != nil {
			// 0 means "the section stays in the file but the gateway is off", which is
			// what the field's own documentation and the listener table both treat it
			// as: refusing it here made a configuration the manual describes impossible
			// to load.
			if g.BindPort < 0 || g.BindPort > 65535 {
				problems = append(problems, fmt.Sprintf(
					"server.ssh_tunnel_gateway.bind_port must be 0-65535 (0 disables the gateway), got %d", g.BindPort))
			}
			if g.Password != "" && g.User == "" {
				problems = append(problems, "server.ssh_tunnel_gateway.password needs a user: an SSH password is checked against a name")
			}
			// The other half of the pair. A user with no password registers
			// neither a password rule nor — when no key file is listed — a key
			// rule, so the gateway turns away every client while its startup line
			// and the open-gateway warning both claim it still admits something.
			if g.User != "" && g.Password == "" && g.AuthorizedKeysFile == "" {
				problems = append(problems, "server.ssh_tunnel_gateway.user needs a password: the user names the account the shared password is checked against")
			}
			if g.MaxProxies < 0 {
				problems = append(problems, "server.ssh_tunnel_gateway.max_proxies cannot be negative")
			}
			if g.IdleTimeoutSeconds < 0 {
				problems = append(problems, "server.ssh_tunnel_gateway.idle_timeout_seconds cannot be negative")
			}
			if g.BindPort > 0 && g.KeyFile == "" {
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"server.ssh_tunnel_gateway has no key_file: a host key is generated on first use and written to %s",
					g.KeyPath()))
			}
			if g.BindPort > 0 && g.AuthorizedKeysFile == "" && g.Password == "" {
				c.Warnings = append(c.Warnings,
					"server.ssh_tunnel_gateway has neither authorized_keys_file nor password: any SSH client that "+
						"reaches the port may publish a tunnel through it")
			}
		}
	case RoleClient:
		if c.Client.ServerAddr == "" {
			if c.DHT.Discover == "" {
				problems = append(problems, "client.server_addr is required, unless dht.enabled and dht.discover resolve the server address from the DHT")
			} else if !c.DHT.Enabled {
				// With no address and the DHT off, the name is never looked
				// up, so the client has no target: refuse it at --check
				// instead of failing at the first connection attempt.
				problems = append(problems, fmt.Sprintf(
					"client.server_addr is empty and dht.discover is set to %q, but dht.enabled is false: the name is never resolved",
					c.DHT.Discover))
			}
		} else if _, _, err := net.SplitHostPort(c.Client.ServerAddr); err != nil {
			problems = append(problems, fmt.Sprintf("client.server_addr %q is not host:port", c.Client.ServerAddr))
		}
		if c.Client.AuthToken == "" {
			problems = append(problems, "client.auth_token is required")
		}
		if c.Store.Path != "" {
			// The store exists so the admin API can create and delete entries at
			// runtime; without the API nothing could reach it, so the file would
			// stay empty and the key would do nothing.
			if c.Client.Admin == nil || !c.Client.Admin.Enabled {
				problems = append(problems, "store.path is set but client.admin.enabled is false: "+
					"the store is managed through the client's admin API, so nothing could create an entry")
			}
		}
		if c.Encryption.Enabled {
			for _, p := range c.Proxies {
				if p.UseEncryption {
					c.Warnings = append(c.Warnings, fmt.Sprintf(
						"proxy %q sets use_encryption while [encryption].enabled is true: the whole session is "+
							"already encrypted, so this proxy's own key layer is not added", p.Name))
				}
			}
		}
		// use_encryption needs no warning beside [oidc]: both ends derive the
		// per-proxy key from the credential the session authenticated with, which
		// the client records when it sends its auth request. That used to require
		// client.auth_token to equal the server's, which is why a warning lived
		// here.
		if c.Client.DialVia != "" {
			if err := ValidateDialVia(c.Client.DialVia); err != nil {
				problems = append(problems, fmt.Sprintf("client.dial_via: %v", err))
			}
		}
		if c.Client.DNSServer != "" {
			host, port, err := net.SplitHostPort(c.Client.DNSServer)
			if err != nil || host == "" {
				problems = append(problems, fmt.Sprintf(
					"client.dns_server %q is not host:port, for example \"10.0.0.53:53\"", c.Client.DNSServer))
			} else if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
				problems = append(problems, fmt.Sprintf("client.dns_server port %q is not a usable port", port))
			}
		}
		if c.Client.ConnectServerLocalIP != "" && net.ParseIP(c.Client.ConnectServerLocalIP) == nil {
			problems = append(problems, fmt.Sprintf(
				"client.connect_server_local_ip %q is not an IP address", c.Client.ConnectServerLocalIP))
		}
		// A single number's range does not depend on the section's enabled flag:
		// that is the rule docs/CONFIGURATION.md states for every other port key,
		// and the one dashboard.port and metrics.port already follow. Only
		// "enabled with no usable port" belongs inside the gate.
		if a := c.Client.Admin; a != nil && (a.Port < 0 || a.Port > 65535) {
			problems = append(problems, fmt.Sprintf("client.admin.port must be 0-65535, got %d", a.Port))
		}
		if a := c.Client.Admin; a != nil && a.Enabled {
			if a.Port <= 0 {
				problems = append(problems, fmt.Sprintf("client.admin.enabled is true but client.admin.port %d is not a usable port", a.Port))
			}
			if (a.User == "") != (a.Password == "") {
				problems = append(problems, "client.admin.user and client.admin.password must be set together: the API authenticates with both or neither")
			}
			// An empty address is the loopback default the client fills in, so it
			// is not warned about; isLoopback is used rather than net.ParseIP
			// because a bind address may be a hostname (client.admin really
			// listens on whatever JoinHostPort makes of it), and a hostname parsed
			// to a nil IP and drew no warning while the same address written as an
			// IP did.
			if a.User == "" && a.BindAddr != "" && !isLoopback(a.BindAddr) {
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"client.admin binds %s without user and password: everyone who reaches the port can read the proxy list and reload the client; bind 127.0.0.1 or set credentials",
					a.BindAddr))
			}
		} else if a := c.Client.Admin; a != nil && (a.Port != 0 || a.BindAddr != "" || a.User != "" || a.Password != "") {
			c.Warnings = append(c.Warnings, "client.admin is configured but client.admin.enabled is false: the management API stays off")
		}
		if len(c.HTTPPlugins) > 0 {
			c.Warnings = append(c.Warnings, "http_plugins has no effect in a client configuration: the webhooks run on a server")
		}
		// client.start selects what runs, so a name nothing defines is worth
		// saying out loud: the operator meant a proxy that is not there, and
		// the rest of the file started without it.
		// A name that matches an entry the file itself turned off is worth the
		// same warning as a name that matches nothing: either way the operator
		// asked for something that does not start.
		for _, off := range c.startNamesDisabled() {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"client.start names %q, which carries enabled = false: it starts nothing", off))
		}
		for _, missing := range c.missingStartNames() {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"client.start names %q, which no proxy or visitor defines: it starts nothing", missing))
		}
		if o := c.OIDC; o != nil {
			// audience is consumed by the token request and by nothing else, and the
			// token request only happens when token_endpoint_url is set: a file that
			// writes audience alone is a client block that would silently do nothing,
			// the way scope alone already is a hard error.
			clientSide := o.TokenEndpointURL != "" || o.ClientID != "" || o.ClientSecret != "" || o.Scope != "" || o.Audience != ""
			if clientSide {
				if o.TokenEndpointURL == "" {
					problems = append(problems, "oidc.token_endpoint_url is required for an oidc client")
				}
				if (o.ClientID == "") != (o.ClientSecret == "") {
					problems = append(problems, "oidc.client_id and oidc.client_secret must be set together: the token request authenticates with both")
				}
				if o.ClientID == "" {
					problems = append(problems, "oidc.client_id is required for an oidc client")
				}
			}
			// Server-side keys are misplaced in a client configuration whether or not
			// the client-side ones are also there — an [oidc] client that copied the
			// server's section in is the likeliest way to end up with them. Every
			// key is named, not just the two strings: a client that sets
			// skip_expiry_check believes it turned a check off, and silence there is
			// worse than an ineffective key.
			if o.Issuer != "" || o.JWKSURL != "" || o.SkipExpiryCheck || o.SkipIssuerCheck || o.TimeoutSecs != 0 {
				c.Warnings = append(c.Warnings, "oidc.issuer, oidc.jwks_url, oidc.timeout_secs, oidc.skip_expiry_check and oidc.skip_issuer_check have no effect in a client configuration: the verification runs on a server")
			}
		}
	}

	// A single-value range check is a statement about the file itself, so it
	// runs whether or not the section is enabled — the same rule the top of
	// docs/CONFIGURATION.md states and metrics.port below follows. A port
	// that is out of range behind enabled = false would only surface when the
	// operator turned the panel on.
	if c.Dashboard.Port < 0 || c.Dashboard.Port > 65535 {
		problems = append(problems, fmt.Sprintf("dashboard.port must be 0-65535, got %d", c.Dashboard.Port))
	}
	if c.Dashboard.Enabled {
		// An empty address is the loopback default, as in the metrics and
		// client.admin checks below and in docs/CONFIGURATION.md; isLoopback("")
		// is false, so without the guard every dashboard with a token and the
		// default address drew a warning about a non-loopback bind.
		if c.Dashboard.Token == "" && c.Dashboard.BindAddr != "" && !isLoopback(c.Dashboard.BindAddr) {
			c.Warnings = append(c.Warnings,
				"dashboard.bind_addr is not a loopback address but dashboard.token is empty: "+
					"anyone who can reach that port can read status and traffic metadata")
		}
	}

	if c.Metrics.Port < 0 || c.Metrics.Port > 65535 {
		problems = append(problems, fmt.Sprintf("metrics.port must be 0-65535, got %d", c.Metrics.Port))
	}
	if c.Metrics.Port > 0 && !c.Metrics.Enabled {
		// The listener would serve one endpoint that is switched off, which is a
		// configuration that says two things at once.
		problems = append(problems, "metrics.port needs metrics.enabled: the endpoint has to exist before it can be served on its own address")
	}
	if c.Metrics.Port > 0 && c.Metrics.Token == "" && c.Metrics.BindAddr != "" && !isLoopback(c.Metrics.BindAddr) {
		c.Warnings = append(c.Warnings,
			"metrics.bind_addr is not a loopback address but metrics.token is empty: "+
				"anyone who can reach that port can read the server's counters")
	}
	if c.Metrics.Enabled && c.Metrics.Port == 0 && !c.Dashboard.Enabled {
		// /metrics lives either on an address of its own or on the dashboard's
		// listener. With neither, the switch is on and no listener answers, which
		// is the same contradiction metrics.port without metrics.enabled is.
		c.Warnings = append(c.Warnings,
			"metrics.enabled is set but nothing serves the endpoint: metrics.port is 0 and the dashboard is "+
				"disabled, so no listener answers /metrics")
	}
	if !c.Dashboard.Enabled && c.Dashboard.Token != "" && !(c.Metrics.Enabled && c.Metrics.Port > 0) {
		c.Warnings = append(c.Warnings,
			"dashboard.token is set but dashboard.enabled is false: nothing serves the dashboard, and no "+
				"metrics listener of its own uses the token either")
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

	switch c.Transport.Protocol {
	case "", "websocket":
	default:
		problems = append(problems, fmt.Sprintf(
			"transport.protocol must be \"websocket\" or empty, got %q", c.Transport.Protocol))
	}
	if c.Transport.Protocol != "" && role == RoleServer {
		c.Warnings = append(c.Warnings, "transport.protocol has no effect in a server configuration: the server recognises the websocket upgrade on the shared port")
	}

	switch c.Transport.WireProtocol {
	case "", "v2":
	default:
		problems = append(problems, fmt.Sprintf(
			"transport.wire_protocol must be \"v2\" or empty, got %q: this program speaks one wire "+
				"format, the one frp calls v2, and its revision is negotiated in the handshake", c.Transport.WireProtocol))
	}
	if c.Transport.TCPMux != nil && *c.Transport.TCPMux {
		c.Warnings = append(c.Warnings,
			"transport.tcp_mux = true asks for frp's multiplexing of a client's data connections; this program "+
				"opens one connection per stream and never multiplexes them, which is the shape tcp_mux = false "+
				"names, so the data path is the same either way")
	}
	for _, scope := range c.Transport.AdditionalScopes {
		switch scope {
		case "HeartBeats", "NewWorkConns":
		default:
			problems = append(problems, fmt.Sprintf(
				"transport.additional_scopes entry %q is not recognised (use \"HeartBeats\" or \"NewWorkConns\")", scope))
		}
	}
	if len(c.Transport.AdditionalScopes) > 0 {
		// frp repeats credentials on those messages so it can omit them when
		// asked; this program's heartbeat is empty and its data open carries a
		// session identifier and a per-stream key, so the request already holds
		// and there is nothing for the key to change.
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"transport.additional_scopes names %s: frp uses those scopes to omit credentials from that message, "+
				"while this program's heartbeat carries no credential and its data open carries a session "+
				"identifier and a per-stream key, so the message is already as bare as the scope asks",
			strings.Join(c.Transport.AdditionalScopes, ", ")))
	}

	if c.Transport.EnableTLS {
		switch role {
		case RoleServer:
			if c.Transport.CertFile == "" || c.Transport.KeyFile == "" {
				problems = append(problems, "transport.enable_tls is true but cert_file and key_file are not both set")
			}
			// The other half of the pair the client branch warns about: these three
			// belong to the client, and a server that sets them is pinning or
			// naming a peer certificate nothing in the server ever looks at.
			if c.Transport.CAFile != "" {
				c.Warnings = append(c.Warnings,
					"transport.ca_file has no effect in a server configuration: the server presents a certificate, it does not verify one")
			}
			if c.Transport.ServerName != "" {
				c.Warnings = append(c.Warnings,
					"transport.server_name has no effect in a server configuration: it is the name the client checks its server against")
			}
			if c.Transport.InsecureSkipVerify {
				c.Warnings = append(c.Warnings,
					"transport.insecure_skip_verify has no effect in a server configuration: the server presents a certificate, it does not verify one")
			}
		case RoleClient:
			if c.Transport.KeyFile != "" {
				c.Warnings = append(c.Warnings,
					"transport.key_file has no effect in a client configuration: only the server presents a certificate")
			}
			if c.Transport.CertFile != "" {
				c.Warnings = append(c.Warnings,
					"transport.cert_file has no effect in a client configuration: only the server presents a certificate")
			}
			if c.Transport.InsecureSkipVerify {
				c.Warnings = append(c.Warnings,
					"transport.insecure_skip_verify is true: the control connection is encrypted but the server is not authenticated")
			}
		}
	} else if c.Transport.CertFile != "" || c.Transport.KeyFile != "" || c.Transport.CAFile != "" {
		c.Warnings = append(c.Warnings, "a certificate is configured but transport.enable_tls is false; the files are unused")
	}

	// This one is checked before the block below, which only runs when identity
	// support is on: with require_identity alone, an operator believes identities
	// are mandatory while the server checks none at all.
	if role == RoleServer && c.Identity.RequireIdentity && !c.Identity.Enabled {
		c.Warnings = append(c.Warnings,
			"identity.require_identity is true but identity.enabled is false: no identity assertion is checked, so every client holding the auth_token is accepted")
	}
	if c.Identity.Enabled {
		for _, key := range c.Identity.AllowedKeys {
			// The whole check the loader makes, not just the hex decode: a key of
			// the wrong length decodes fine and then stops the server from starting.
			if _, err := crypto.ParseIdentityKey(key); err != nil {
				problems = append(problems, fmt.Sprintf("identity.allowed_keys entry %q: %v", key, err))
			}
		}
		if role == RoleServer && c.Identity.RequireIdentity && len(c.Identity.AllowedKeys) == 0 {
			c.Warnings = append(c.Warnings,
				"identity.require_identity is true but identity.allowed_keys is empty: "+
					"any client that signs correctly is accepted, which proves possession but does not restrict who connects")
		}
		if role == RoleServer && len(c.Identity.AllowedKeys) > 0 && !c.Identity.RequireIdentity {
			c.Warnings = append(c.Warnings,
				"identity.allowed_keys is set but identity.require_identity is false: a client that presents no "+
					"identity is still accepted, so the list only restricts the clients that present one")
		}
		if role == RoleClient && c.Identity.KeyFile == "" {
			problems = append(problems, "identity.enabled is true but identity.key_file is empty")
		}
	}

	if c.Obfuscation.PadTo != nil && *c.Obfuscation.PadTo < 0 {
		problems = append(problems, "obfuscation.pad_to cannot be negative")
	}
	// Padding happens before the frame is written, and a frame larger than the limit is
	// refused by its own sender: a pad_to above the limit therefore refuses every
	// frame, so a client configured with one never gets as far as authenticating.
	// The two ends do not have to agree on pad_to (the receiver unpads from the frame
	// flag), but both are bound by the same frame limit.
	if c.Obfuscation.PadTo != nil && *c.Obfuscation.PadTo > protocol.DefaultMaxPayload {
		problems = append(problems, fmt.Sprintf(
			"obfuscation.pad_to must not exceed %d (the frame limit), got %d: every frame would be refused by its sender",
			protocol.DefaultMaxPayload, *c.Obfuscation.PadTo))
	}
	if c.Obfuscation.JitterMillis < 0 {
		problems = append(problems, "obfuscation.jitter_millis cannot be negative")
	} else if c.Obfuscation.JitterMillis > int(protocol.MaxJitter/time.Millisecond) {
		// The delay is taken before every write, so a value past the bound is a stall
		// rather than a disguise, and the conversion to time.Duration wraps.
		problems = append(problems, fmt.Sprintf(
			"obfuscation.jitter_millis must be at most %d, got %d: the delay is taken before every write",
			int(protocol.MaxJitter/time.Millisecond), c.Obfuscation.JitterMillis))
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
	// TOML 1.0 admits nan and inf, and both pass a plain "< 0" check: inf
	// silently disables the token bucket, nan poisons every refill so each
	// per-source bucket refuses forever after its burst.
	if math.IsNaN(c.Server.RateLimitPerSecond) || math.IsInf(c.Server.RateLimitPerSecond, 0) {
		problems = append(problems, "server.rate_limit_per_second must be a finite number")
	}
	// A cap near the int64 ceiling wraps time.Duration(x)*time.Second into
	// a negative, which the backoff then treats as "always over the cap"
	// and collapses reconnects to about a second.
	if c.Client.MaxReconnectSeconds > 86400 {
		problems = append(problems, fmt.Sprintf("client.max_reconnect_seconds cannot exceed 86400 (a day), got %d", c.Client.MaxReconnectSeconds))
	}
	// The server refuses a longer client_id at authentication, so a
	// configuration that carries one would connect nowhere: name it here where
	// --check can. The server's reason is permanent storage — the ledger keys
	// its per-client totals by the name — so the bound is on the wire, not
	// just in this file. (MaxClientIDLen lives in pkg/server, which imports
	// this package; the number is pinned by that package's test.)
	if len(c.Client.ClientID) > 128 {
		problems = append(problems, fmt.Sprintf("client.client_id cannot exceed 128 bytes (the server refuses a longer one), got %d", len(c.Client.ClientID)))
	}
	if c.Server.GracefulShutdownSecs < 0 {
		problems = append(problems, "server.graceful_shutdown_seconds cannot be negative")
	}
	if problem, conflict := listenerConflict(c.listeners(role)); conflict {
		problems = append(problems, problem)
	}
	// A negative duration is not a smaller setting, and the program does not treat it
	// as one. -1 second of dial timeout is an immediate timeout, so a client
	// configured with it never reaches its server; a negative reconnect interval
	// makes every wait fall back to one second, which turns the backoff into a retry
	// loop; the limits on the server side (heartbeat window, stream idle time,
	// handshake deadline, connection cap) each stop being enforced, silently and
	// invisibly, because every use site guards with "if the value is positive".
	// Zero keeps its documented meaning everywhere: it selects the default.
	for _, key := range []struct {
		name  string
		value int
	}{
		{"server.max_connections", c.Server.MaxConnections},
		{"server.handshake_timeout_seconds", c.Server.HandshakeTimeoutSecs},
		{"server.read_timeout_seconds", c.Server.ReadTimeoutSecs},
		{"server.heartbeat_seconds", c.Server.HeartbeatSeconds},
		{"server.dial_timeout_seconds", c.Server.DialTimeoutSecs},
		{"server.rate_limit_burst", c.Server.RateLimitBurst},
		{"client.reconnect_seconds", c.Client.ReconnectSeconds},
		{"client.max_reconnect_seconds", c.Client.MaxReconnectSeconds},
		{"client.heartbeat_seconds", c.Client.HeartbeatSeconds},
		{"client.dial_timeout_seconds", c.Client.DialTimeoutSecs},
		{"client.idle_timeout_seconds", c.Client.IdleTimeoutSecs},
		{"client.udp_packet_size", c.Client.UDPPacketSize},
		{"client.tcp_keepalive_seconds", c.Client.TCPKeepAliveSeconds},
		{"client.pool_count", c.Client.PoolCount},
		{"server.tcp_keepalive_seconds", c.Server.TCPKeepAliveSeconds},
		{"server.user_conn_timeout_seconds", c.Server.UserConnTimeoutSeconds},
		{"dht.lookup_timeout_seconds", c.DHT.LookupTimeoutSeconds},
	} {
		if key.value < 0 {
			problems = append(problems, fmt.Sprintf("%s cannot be negative, got %d", key.name, key.value))
		}
	}
	// The other end of the same problem: a value multiplied by time.Second wraps
	// inside the int64 a duration is, and the wrapped value is read as "no limit"
	// at every use site — the handshake deadline and the stream idle timeout among
	// them, so an absurd timeout silently removes the bound it names. Only the
	// values that would actually wrap are refused, so every timeout an operator
	// could mean still loads. The DHT intervals are bounded further below.
	for _, bounded := range []struct {
		name  string
		value int
	}{
		{"server.handshake_timeout_seconds", c.Server.HandshakeTimeoutSecs},
		{"server.read_timeout_seconds", c.Server.ReadTimeoutSecs},
		{"server.heartbeat_seconds", c.Server.HeartbeatSeconds},
		{"server.dial_timeout_seconds", c.Server.DialTimeoutSecs},
		{"server.graceful_shutdown_seconds", c.Server.GracefulShutdownSecs},
		{"server.tcp_keepalive_seconds", c.Server.TCPKeepAliveSeconds},
		{"server.user_conn_timeout_seconds", c.Server.UserConnTimeoutSeconds},
		{"server.ban_seconds", c.Server.BanSeconds},
		{"server.ban_max_seconds", c.Server.BanMaxSeconds},
		{"server.vhost_http_timeout", c.Server.VhostHTTPTimeout},
		{"client.reconnect_seconds", c.Client.ReconnectSeconds},
		{"client.max_reconnect_seconds", c.Client.MaxReconnectSeconds},
		{"client.heartbeat_seconds", c.Client.HeartbeatSeconds},
		{"client.dial_timeout_seconds", c.Client.DialTimeoutSecs},
		{"client.idle_timeout_seconds", c.Client.IdleTimeoutSecs},
		{"client.tcp_keepalive_seconds", c.Client.TCPKeepAliveSeconds},
	} {
		if bounded.value > MaxDurationSeconds {
			problems = append(problems, fmt.Sprintf(
				"%s cannot exceed %d, got %d: the value is multiplied by a second, and a larger one wraps to a negative duration that every use site reads as no limit",
				bounded.name, MaxDurationSeconds, bounded.value))
		}
	}
	if c.OIDC != nil && c.OIDC.TimeoutSecs > MaxDurationSeconds {
		problems = append(problems, fmt.Sprintf(
			"oidc.timeout_secs cannot exceed %d, got %d: the value is multiplied by a second, and a larger one wraps to a negative duration that every use site reads as no limit",
			MaxDurationSeconds, c.OIDC.TimeoutSecs))
	}
	if g := c.Server.SSHTunnelGateway; g != nil && g.IdleTimeoutSeconds > MaxDurationSeconds {
		problems = append(problems, fmt.Sprintf(
			"server.ssh_tunnel_gateway.idle_timeout_seconds cannot exceed %d, got %d: the value is multiplied by a second, and a larger one wraps to a negative duration that every use site reads as no limit",
			MaxDurationSeconds, g.IdleTimeoutSeconds))
	}
	for i := range c.HTTPPlugins {
		if c.HTTPPlugins[i].TimeoutSecs > MaxDurationSeconds {
			problems = append(problems, fmt.Sprintf(
				"http_plugins[%d] %q: timeout_secs cannot exceed %d, got %d: the value is multiplied by a second, and a larger one wraps to a negative duration that every use site reads as no limit",
				i, c.HTTPPlugins[i].Name, MaxDurationSeconds, c.HTTPPlugins[i].TimeoutSecs))
		}
	}
	if c.Client.UDPPacketSize > maxUDPPayloadSize {
		problems = append(problems, fmt.Sprintf("client.udp_packet_size cannot exceed %d, got %d", maxUDPPayloadSize, c.Client.UDPPacketSize))
	}
	if c.Client.PoolCount > MaxPoolCount {
		// The server keeps at most MaxPoolCount parked connections per session, so a
		// larger pool would leave connections the server refuses; the two limits are
		// one number for that reason.
		problems = append(problems, fmt.Sprintf(
			"client.pool_count cannot exceed %d (the server parks at most that many), got %d",
			MaxPoolCount, c.Client.PoolCount))
	}
	// A ceiling below the starting value would be exceeded by the first wait, so the
	// backoff would begin above the limit that is supposed to bound it.
	if c.Client.ReconnectSeconds > 0 && c.Client.MaxReconnectSeconds > 0 &&
		c.Client.MaxReconnectSeconds < c.Client.ReconnectSeconds {
		problems = append(problems, fmt.Sprintf(
			"client.max_reconnect_seconds (%d) is smaller than client.reconnect_seconds (%d), "+
				"so the first wait would already exceed the ceiling",
			c.Client.MaxReconnectSeconds, c.Client.ReconnectSeconds))
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

	if c.Log.MaxDays < 0 || c.Log.MaxDays > 3650 {
		problems = append(problems, fmt.Sprintf(
			"log.max_days must be 0-3650 (0 keeps one file that only grows), got %d", c.Log.MaxDays))
	}
	if c.Log.MaxDays > 0 && c.Log.To == "" {
		// A retention window with nowhere to rotate is a key that cannot do
		// anything, which is exactly what an operator would not notice.
		problems = append(problems, "log.max_days needs log.to: without a file there is nothing to rotate")
	}
	if c.Log.Level != "" {
		// An empty level is the default (info), so only a value the operator
		// wrote is judged: a configuration built in code rather than decoded
		// never went through applyDefaults.
		if _, ok := LogLevelRank(c.Log.Level); !ok {
			problems = append(problems, fmt.Sprintf(
				"log.level %q is not supported (use %s)", c.Log.Level, strings.Join(LogLevels, ", ")))
		}
	}
	switch c.Server.LoadBalance {
	case LoadBalanceRoundRobin, LoadBalanceRandom, LoadBalanceLatency, LoadBalanceFailover,
		LoadBalanceAdaptive, LoadBalanceBandit:
	default:
		problems = append(problems, fmt.Sprintf(
			"server.load_balance %q is not supported (use %s, %s, %s, %s, %s or %s)",
			c.Server.LoadBalance, LoadBalanceRoundRobin, LoadBalanceRandom,
			LoadBalanceLatency, LoadBalanceFailover, LoadBalanceAdaptive, LoadBalanceBandit))
	}
	if c.Server.LoadBalance != LoadBalanceRoundRobin {
		c.Warnings = append(c.Warnings,
			"server.load_balance is set; it applies to proxies that declare a group, "+
				"and has no effect on a proxy published by a single client")
	}
	for _, cidr := range c.Server.AllowCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			problems = append(problems, fmt.Sprintf("server.allow_cidrs entry %q is not a CIDR: %v", cidr, err))
		}
	}
	for _, cidr := range c.Server.DenyCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
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
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			problems = append(problems, fmt.Sprintf("server.ban_ignore_cidrs entry %q is not a CIDR: %v", cidr, err))
		}
	}
	// A range check on a single value is made whether or not its section is enabled: a
	// negative or absurd number is a mistake in the file, and the point of --check is to
	// name it before the section is switched on rather than the moment it is. What stays
	// behind the gates below is what only means something while the section is in use:
	// addresses and keys that have to parse, and relations between two values whose
	// defaults are only filled in once the section is on (dht.republish_seconds = 0
	// means "derive it", so it cannot be compared with announce_ttl_seconds).
	if c.VPN.MTU != 0 && (c.VPN.MTU < vpn.MinMTU || c.VPN.MTU > vpn.MaxMTU) {
		problems = append(problems, fmt.Sprintf("vpn.mtu must be %d-%d, got %d", vpn.MinMTU, vpn.MaxMTU, c.VPN.MTU))
	}
	// Checked outside the gate below for the same reason as
	// identity.require_identity: require alone reads as "every session must use the
	// tunnel", but with the tunnel off the server has none to hand out and refuses
	// every session, both the ones that ask for a tunnel and the ones that do not.
	if role == RoleServer && c.VPN.Require && !c.VPN.Enabled {
		problems = append(problems, "vpn.require is true but vpn.enabled is false: the server has no layer-3 tunnel to require, so it refuses every session")
	}
	// The client's warning is about the file, not about the switch: vpn.address has
	// no effect on a client whether or not the section is enabled, and a client that
	// wrote it under enabled = false has the same line to delete. It sits beside the
	// range checks above for the same reason. The server's address check stays under
	// the gate: with the tunnel off there is no subnet to validate or allocate from.
	if role == RoleClient && c.VPN.Address != "" {
		c.Warnings = append(c.Warnings, "vpn.address has no effect in a client configuration: the server decides the subnet and tells the client which address it got")
	}
	if c.VPN.Enabled && role == RoleServer {
		if c.VPN.Address == "" {
			problems = append(problems, "vpn.enabled is true but vpn.address is empty: the server needs a subnet to allocate client addresses from")
		} else if _, err := vpn.NewPool(c.VPN.Address); err != nil {
			problems = append(problems, fmt.Sprintf("vpn.address %q is not usable: %v", c.VPN.Address, err))
		}
	}

	// Every server-only section a client file turns on gets its own warning:
	// these were one switch statement, so a file that enabled two or more of
	// them named the first and stayed silent about the rest — the operator
	// removed the one it named, ran again, and only then learned about the
	// next. The warnings are independent, so they are ifs.
	if role == RoleClient {
		if c.Ledger.Enabled {
			c.Warnings = append(c.Warnings, "ledger.enabled has no effect in a client configuration: the bandwidth ledger is kept by the server")
		}
		if c.Dashboard.Enabled {
			c.Warnings = append(c.Warnings, "dashboard.enabled has no effect in a client configuration: the panel is served by a server")
		}
		if c.Metrics.Enabled {
			c.Warnings = append(c.Warnings, "metrics.enabled has no effect in a client configuration: a client exposes its state through [client.admin]")
		}
		if c.Audit.Enabled {
			c.Warnings = append(c.Warnings, "audit.enabled has no effect in a client configuration: the audit log is written by a server")
		}
	} else if c.Ledger.Enabled {
		if c.Ledger.Path == "" {
			problems = append(problems, "ledger.enabled is true but ledger.path is empty")
		}
		if c.Ledger.SigningKey == "" {
			problems = append(problems, "ledger.enabled is true but ledger.signing_key_file is empty")
		}
	}

	// These are multiplied by a second, so a value near the int64 ceiling wraps
	// into a negative duration — which withDefaults then reads as "unset" and
	// silently replaces, and a wrapped-positive one makes the republish ticker
	// fire continuously. max_reconnect_seconds is bounded for the same reason.
	for _, bounded := range []struct {
		name  string
		value int
	}{
		{"ttl_seconds", c.DHT.TTLSeconds},
		{"announce_ttl_seconds", c.DHT.AnnounceTTLSeconds},
		{"republish_seconds", c.DHT.RepublishSeconds},
		{"lookup_timeout_seconds", c.DHT.LookupTimeoutSeconds},
	} {
		if bounded.value > 86400 {
			problems = append(problems, fmt.Sprintf("dht.%s cannot exceed 86400 (a day), got %d", bounded.name, bounded.value))
		}
	}
	if c.DHT.AnnounceTTLSeconds < 0 {
		problems = append(problems, "dht.announce_ttl_seconds cannot be negative")
	}
	if c.DHT.AnnounceTTLSeconds > 0 && c.DHT.AnnounceTTLSeconds < 2 {
		problems = append(problems, fmt.Sprintf(
			"dht.announce_ttl_seconds (%d) is too short: an announcement has to be rewritten before it lapses, and the interval is a whole number of seconds",
			c.DHT.AnnounceTTLSeconds))
	}
	if c.DHT.RepublishSeconds < 0 {
		problems = append(problems, "dht.republish_seconds cannot be negative")
	}
	if c.DHT.TTLSeconds < 0 {
		problems = append(problems, "dht.ttl_seconds cannot be negative")
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
		if c.DHT.RepublishSeconds > 0 && c.DHT.RepublishSeconds >= c.DHT.AnnounceTTLSeconds {
			problems = append(problems, fmt.Sprintf(
				"dht.republish_seconds (%d) must be shorter than dht.announce_ttl_seconds (%d), otherwise an announcement lapses before it is rewritten",
				c.DHT.RepublishSeconds, c.DHT.AnnounceTTLSeconds))
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
			// A bare IPv6 address contains colons, so "contains a colon" would
			// refuse it for the wrong reason; parse as host:port instead, which
			// only succeeds when a port really is present.
			if _, _, err := net.SplitHostPort(c.DHT.AdvertiseHost); err == nil {
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

	// The signing key belongs to a server: a client that sets one gets nothing from
	// it, whether or not the section is enabled.
	if role == RoleClient && c.DHT.SigningKeyFile != "" {
		c.Warnings = append(c.Warnings, "dht.signing_key_file has no effect in a client configuration: it signs the announcements a server makes")
	}

	if role == RoleServer {
		for _, p := range c.Proxies {
			problems = append(problems, c.validateProxyPolicy(p)...)
		}
		if c.Client.PoolCount != 0 {
			// The pool is the client's half of the arrangement: it opens the
			// connections, and the server parks what it is given. There is no
			// server-side setting behind this key, so a server configuration
			// carrying it is a mistake worth naming.
			c.Warnings = append(c.Warnings,
				"client.pool_count has no effect in a server configuration: the client keeps the connections ready, the server only parks what it is offered")
		}
	} else if role == RoleClient {
		for i := range c.Proxies {
			if c.Proxies[i].Type == "" {
				c.Proxies[i].Type = ProxyTypeTCP
			}
			if c.Proxies[i].AuthMethod == "" {
				c.Proxies[i].AuthMethod = AuthMethodSecret
			}
		}
	}

	seen := map[string]bool{}
	// publishedPorts maps the protocol and port a proxy asks for to the proxy that asked
	// for it first. The server binds that port for the first registration, so a second
	// proxy of the same protocol asking for it is refused with "address already in use"
	// — a message that names the port but not the proxy holding it. tcp and udp on one
	// port number are two different sockets, so they only share the number.
	publishedPorts := map[string]string{}
	for _, p := range c.Proxies {
		if p.Name == "" {
			problems = append(problems, "every [[proxies]] entry needs a name")
			continue
		}
		if seen[p.Name] {
			problems = append(problems, fmt.Sprintf("duplicate proxy name %q", p.Name))
		}
		seen[p.Name] = true

		if role != RoleClient {
			// A server entry is a policy, not a service: its own checks ran above.
			// A role-less load — --dht-lookup, which only needs [dht] — has no
			// client service here either, and validating one refused a server
			// configuration that the same binary accepts for --check.
			continue
		}

		if !IsProxyType(p.Type) {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: type %q is not supported (use one of %s)",
				p.Name, p.Type, strings.Join(ProxyTypes, ", ")))
			continue
		}
		// A socks5 tunnel has no local service to name: the visitor chooses the
		// target, so local_ip and local_port are not part of its configuration.
		// A plugin-backed proxy runs its own service and needs neither.
		if p.Type != ProxyTypeSOCKS && p.Plugin == "" {
			if p.LocalPort < 1 || p.LocalPort > 65535 {
				problems = append(problems, fmt.Sprintf("proxy %q: local_port must be 1-65535, got %d", p.Name, p.LocalPort))
			}
		} else if p.Type == ProxyTypeSOCKS && (p.LocalPort != 0 || p.LocalIP != "") {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: local_ip and local_port have no effect on a socks5 tunnel, whose target comes from the request",
				p.Name))
		}
		if p.RemotePort < 0 || p.RemotePort > 65535 {
			problems = append(problems, fmt.Sprintf("proxy %q: remote_port must be 0-65535, got %d", p.Name, p.RemotePort))
		}
		problems = append(problems, c.validateProxyExtras(p)...)

		// A proxy that is turned off publishes nothing, so it cannot collide
		// with one that is running: counting it refused a legitimate port
		// handover through the management API, and the startup trim then
		// deleted the entry that actually runs. The visitor side skips
		// stopped entries for the same reason.
		if p.RemotePort > 0 && p.IsEnabled() {
			protocol := "tcp"
			if IsDatagramProxyType(p.Type) {
				protocol = "udp"
			}
			key := fmt.Sprintf("%s/%d", protocol, p.RemotePort)
			if holder, taken := publishedPorts[key]; taken {
				problems = append(problems, fmt.Sprintf(
					"proxies %q and %q both ask for %s port %d, and only the first to register is published",
					holder, p.Name, protocol, p.RemotePort))
			} else {
				publishedPorts[key] = p.Name
			}
		}
		for _, cidr := range append(append([]string{}, p.AllowCIDRs...), p.DenyCIDRs...) {
			// The runtime parsers trim, so validation has to trim the same way
			// or a padded entry is accepted at login time and refused at load
			// time depending on which role reads the file.
			if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
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
		case ProxyTypeHTTP, ProxyTypeHTTPS, ProxyTypeTCPMUX:
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
			case AuthMethodSecret, AuthMethodNIZK, AuthMethodSNARK:
				if p.AuthMethod == AuthMethodSNARK && !c.FeatureEnabled(FeatureSnark) {
					problems = append(problems, fmt.Sprintf(
						"proxy %q: auth_method = %q needs feature_gates.Snark, which this configuration turns off",
						p.Name, AuthMethodSNARK))
				}
			default:
				problems = append(problems, fmt.Sprintf(
					"proxy %q: auth_method %q is not supported (use %q, %q or %q)",
					p.Name, p.AuthMethod, AuthMethodSecret, AuthMethodNIZK, AuthMethodSNARK))
			}
		}
		if p.Type == ProxyTypeTCPMUX && p.Multiplexer != MultiplexerHTTPConnect {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: multiplexer %q is not supported (the only multiplexer is %q)",
				p.Name, p.Multiplexer, MultiplexerHTTPConnect))
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
			p.Type != ProxyTypeTCPMUX &&
			!IsPrivateProxyType(p.Type) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: it has no remote_port and no group, so nothing can reach it", p.Name))
		}
		if len(p.AllowUsers) > 0 && !IsPrivateProxyType(p.Type) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: allow_users names the identities that may visit a private proxy, and a %s proxy is open to anyone who can reach it",
				p.Name, p.Type))
		}
		seenUsers := map[string]bool{}
		for _, user := range p.AllowUsers {
			if seenUsers[user] {
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"proxy %q: allow_users names %q twice", p.Name, user))
			}
			seenUsers[user] = true
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
		if v.FallbackTo != "" {
			host, port, err := net.SplitHostPort(v.FallbackTo)
			if err != nil || host == "" {
				problems = append(problems, fmt.Sprintf(
					"visitor %q: fallback_to %q is not host:port", v.Name, v.FallbackTo))
			} else if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
				problems = append(problems, fmt.Sprintf(
					"visitor %q: fallback_to port %q is not a usable port", v.Name, port))
			}
		}
		if v.FallbackTimeoutMs < 0 {
			problems = append(problems, fmt.Sprintf(
				"visitor %q: fallback_timeout_ms cannot be negative, got %d", v.Name, v.FallbackTimeoutMs))
		}
		if int64(v.FallbackTimeoutMs) > MaxDurationMillis {
			// The same wrap as the seconds settings, one step earlier: this one is
			// multiplied by time.Millisecond, and a negative result makes the
			// context WithTimeout builds already expired, so the visitor is sent to
			// the fallback at once instead of after the window it asked for.
			problems = append(problems, fmt.Sprintf(
				"visitor %q: fallback_timeout_ms cannot exceed %d, got %d: the value is multiplied by a millisecond, and a larger one wraps to a negative duration",
				v.Name, MaxDurationMillis, v.FallbackTimeoutMs))
		}
		if v.FallbackTimeoutMs > 0 && v.FallbackTo == "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"visitor %q: fallback_timeout_ms does nothing without fallback_to", v.Name))
		}
		switch v.AuthMethod {
		case AuthMethodSecret, AuthMethodNIZK, AuthMethodSNARK:
			if v.AuthMethod == AuthMethodSNARK && !c.FeatureEnabled(FeatureSnark) {
				problems = append(problems, fmt.Sprintf(
					"visitor %q: auth_method = %q needs feature_gates.Snark, which this configuration turns off",
					v.Name, AuthMethodSNARK))
			}
		default:
			problems = append(problems, fmt.Sprintf(
				"visitor %q: auth_method %q is not supported (use %q, %q or %q); it must match the proxy's auth_method",
				v.Name, v.AuthMethod, AuthMethodSecret, AuthMethodNIZK, AuthMethodSNARK))
		}
		switch v.Transport {
		case "", TransportWebRTC:
			if v.Transport == TransportWebRTC && IsDatagramProxyType(v.Type) {
				// The server dispatches a datagram visitor to the datagram relay and
				// only ever reads an offer for the byte-stream path, so a udp/sudp
				// visitor would send an offer the server drops and then wait out the
				// client's answer deadline before failing.
				problems = append(problems, fmt.Sprintf(
					"visitor %q: transport = %q carries a byte stream, so a %s visitor cannot use it",
					v.Name, TransportWebRTC, v.Type))
			}
			if v.Transport == TransportWebRTC && !c.FeatureEnabled(FeatureWebRTC) {
				problems = append(problems, fmt.Sprintf(
					"visitor %q: transport = %q needs feature_gates.WebRTC, which this configuration turns off",
					v.Name, TransportWebRTC))
			}
		default:
			problems = append(problems, fmt.Sprintf(
				"visitor %q: transport %q is not supported (use the default or webrtc)",
				v.Name, v.Transport))
		}
		if v.BindPort < 1 || v.BindPort > 65535 {
			problems = append(problems, fmt.Sprintf("visitor %q: bind_port must be 1-65535, got %d", v.Name, v.BindPort))
		}
		// A visitor's listener authenticates nobody: whoever can open that port can use
		// the tunnel. The server-side [[proxies]] allow_cidrs/deny_cidrs do not help
		// either — they judge the address the visitor connects from, which is this client
		// rather than the user behind it. So binding anything but a loopback address
		// hands the private proxy to whoever can reach the listener.
		if v.BindAddr != "" && !isLoopback(v.BindAddr) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"visitor %q listens on %s, which is not a loopback address: the listener authenticates nobody, "+
					"and the server-side allow_cidrs/deny_cidrs judge this client rather than the user behind it, "+
					"so anyone who can reach that port can use the tunnel", v.Name, v.BindAddr))
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
	// The same range as every other listener: a negative port is not "off" in
	// any documented sense, and a port above the range only fails later, at bind
	// time, with a low-level error instead of a line naming the key.
	for name, port := range map[string]int{
		"tcpmux_port":            c.Server.TCPMuxPort,
		"https_passthrough_port": c.Server.HTTPSPassthroughPort,
	} {
		if port < 0 || port > 65535 {
			problems = append(problems, fmt.Sprintf("server.%s must be 0-65535, got %d", name, port))
		}
	}

	// Feature gates are judged the same way in both roles: a name this program
	// does not know is a mistake, and frp's one gate is understood but changes
	// nothing because the virtual network is not reproduced. A section written for
	// the other role is a warning rather than a silent no-op, like store and
	// transport.protocol above.
	for _, section := range []struct {
		name  string
		gates map[string]bool
	}{
		{"server", c.Server.FeatureGates},
		{"client", c.Client.FeatureGates},
	} {
		names := make([]string, 0, len(section.gates))
		for name := range section.gates {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch name {
			case FeatureWebRTC, FeatureSnark:
			case FeatureVirtualNet:
				c.Warnings = append(c.Warnings, fmt.Sprintf(
					"%s.feature_gates.%s: frp's virtual network is not reproduced by this program, so the gate changes nothing",
					section.name, name))
			default:
				problems = append(problems, fmt.Sprintf(
					"%s.feature_gates names %q, which is not a feature of this program (known: %s)",
					section.name, name, strings.Join(KnownFeatureGates, ", ")))
			}
		}
	}
	switch role {
	case RoleServer:
		if len(c.Client.FeatureGates) > 0 {
			c.Warnings = append(c.Warnings, "client.feature_gates in a server configuration: a server reads both sections, so the gate takes effect, but the setting belongs in [server]")
		}
	case RoleClient:
		if len(c.Server.FeatureGates) > 0 {
			c.Warnings = append(c.Warnings, "server.feature_gates in a client configuration: the client reads both sections, so the gate takes effect, but the setting belongs in [client]")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// validateProxyPolicy checks one [[proxies]] entry of a server configuration.
//
// A server does not publish a service of its own: an entry in this list states the
// policy the server enforces for that name, whoever registers it. The keys that
// describe the client's own service are still accepted, so one file can be used for
// both roles, but they are reported because the server does not act on them.
func (c *Config) validateProxyPolicy(p ProxyConfig) []string {
	var problems []string

	if p.Type != "" && !IsProxyType(p.Type) {
		problems = append(problems, fmt.Sprintf(
			"proxy policy %q: type %q is not supported (use one of %s, or leave it out to accept any type)",
			p.Name, p.Type, strings.Join(ProxyTypes, ", ")))
	}
	if p.RemotePort < 0 || p.RemotePort > 65535 {
		problems = append(problems, fmt.Sprintf(
			"proxy policy %q: remote_port must be 0-65535, got %d", p.Name, p.RemotePort))
	}
	if p.RemotePort != 0 && IsPrivateProxyType(p.Type) {
		problems = append(problems, fmt.Sprintf(
			"proxy policy %q: %s is a private tunnel and has no public port, so remote_port must be 0",
			p.Name, p.Type))
	}
	for _, cidr := range append(append([]string{}, p.AllowCIDRs...), p.DenyCIDRs...) {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			problems = append(problems, fmt.Sprintf("proxy policy %q: %q is not a CIDR: %v", p.Name, cidr, err))
		}
	}
	if p.Multipath < 0 || p.Multipath > MaxMultipath {
		problems = append(problems, fmt.Sprintf(
			"proxy policy %q: multipath must be 0-%d, got %d", p.Name, MaxMultipath, p.Multipath))
	}

	for _, key := range []struct {
		name  string
		given bool
	}{
		{"local_ip", p.LocalIP != ""},
		{"local_port", p.LocalPort != 0},
		{"group", p.Group != ""},
		{"multipath", p.Multipath != 0},
		{"secret_key", p.SecretKey != ""},
		{"auth_method", p.AuthMethod != ""},
		{"allow_targets", len(p.AllowTargets) > 0},
		{"domains", len(p.Domains) > 0},
		{"subdomain", p.Subdomain != ""},
		{"http_user", p.HTTPUser != ""},
		{"http_password", p.HTTPPassword != ""},
		{"host_header_rewrite", p.HostHeaderRewrite != ""},
		{"multiplexer", p.Multiplexer != ""},
		{"use_compression", p.UseCompression},
		{"use_encryption", p.UseEncryption},
		{"tls_passthrough", p.TLSPassthrough},
		{"request_headers", len(p.RequestHeaders) > 0},
		{"response_headers", len(p.ResponseHeaders) > 0},
		{"bandwidth", p.Bandwidth != ""},
		{"proxy_protocol", p.ProxyProtocol != ""},
		{"remote_ports", p.RemotePorts != ""},
		{"plugin", p.Plugin != ""},
		{"health_check", p.HealthCheck != nil},
	} {
		if key.given {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy policy %q: %s has no effect in a server configuration; it describes the service the client publishes",
				p.Name, key.name))
		}
	}

	return problems
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
			case ch == '-':
				// A hyphen may not sit at either end of a label; a trailing one is as
				// unresolvable as a leading one, and ValidateDNSLabel already refuses
				// both for subdomain.
				if i == 0 || i == len(label)-1 {
					return false
				}
			case ch == '_':
				// Underscores appear in real service names (DKIM, _acme-challenge), so
				// they are allowed inside a label, only not at its start.
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

// isLoopback reports whether a bind address stays on this machine.
//
// A hostname that is not "localhost" is not loopback: it can resolve to any interface, and
// both callers of this decide whether to warn that a port is reachable from elsewhere, where
// the cost of being wrong is a port nobody was warned about.
func isLoopback(addr string) bool {
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	return ip != nil && ip.IsLoopback()
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

// Load reads and validates a configuration file.
//
// On a validation failure it returns both the configuration it built and the error,
// unlike the other failure paths, so the caller can still report what was collected
// on the way: an unknown key and a missing required value are usually part of the
// same mistake, and they belong in the same report. The configuration must not be
// used in that case — it only carries the warnings.
func Load(filename string, opts ValidateOptions) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", filename, err)
	}
	cfg, err := decodeConfig(string(data), filename, opts)
	if cfg != nil {
		cfg.SourceFile = filename
		if err == nil {
			if mergeErr := cfg.mergeIncludes(filepath.Dir(filename), opts); mergeErr != nil {
				return cfg, mergeErr
			}
			// After the merge, not inside decodeConfig: an included proxy with
			// remote_ports is what a start name has to be expanded against.
			if expandErr := cfg.expandClientEntries(opts); expandErr != nil {
				return cfg, expandErr
			}
		}
	}
	if err != nil {
		return cfg, err
	}
	if err := cfg.Validate(opts.Role); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// mergeIncludes appends the [[proxies]] and [[visitors]] of every fragment the
// includes patterns match, relative to the main file's directory. One level
// only: a fragment that names its own includes is refused rather than
// silently ignored, and names must stay unique across the whole set.
func (c *Config) mergeIncludes(base string, opts ValidateOptions) error {
	if len(c.Includes) == 0 {
		return nil
	}
	// A merged fragment contributes only [[proxies]] and [[visitors]], and only
	// a client runs those: in a server configuration [[proxies]] is the
	// operator's policy (validateProxyPolicy), so merging a fragment there
	// silently turns it into one. The key has no effect outside a client
	// configuration, so say so and leave the file unmerged — a warning the merge
	// then ignored would contradict the behavior it describes. An empty role is
	// the internal --dht-lookup path, which keeps merging as before.
	if opts.Role != "" && opts.Role != RoleClient {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"includes has no effect in a %s configuration: only a client configuration merges [[proxies]] and [[visitors]] fragments", opts.Role))
		return nil
	}
	seen := map[string]bool{}
	for _, proxy := range c.Proxies {
		seen["proxy:"+proxy.Name] = true
	}
	for _, visitor := range c.Visitors {
		seen["visitor:"+visitor.Name] = true
	}
	for _, pattern := range c.Includes {
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(base, pattern)
		}
		files, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("includes pattern %q: %w", pattern, err)
		}
		if len(files) == 0 {
			return fmt.Errorf("includes pattern %q matched no files", pattern)
		}
		for _, file := range files {
			data, readErr := os.ReadFile(file)
			if readErr != nil {
				return fmt.Errorf("read include %s: %w", file, readErr)
			}
			fragment, fragErr := decodeConfig(string(data), file, opts)
			if fragErr != nil {
				return fmt.Errorf("include %s: %w", file, fragErr)
			}
			// The fragment was decoded with the caller's options, so its own
			// unknown-key and token-file/env warnings must reach the main
			// config's report: a typo inside a fragment would otherwise be
			// silently ignored while the same typo in the main file is
			// reported.
			c.Warnings = append(c.Warnings, fragment.Warnings...)
			// This merge consumes only the proxy and visitor lists, so any
			// other section a fragment carries decodes cleanly and is then
			// dropped — settings the operator believes they made. The raw
			// top-level keys tell the sections the operator wrote apart from
			// the defaults the decoder filled in behind them.
			var raw map[string]any
			if _, rawErr := toml.Decode(string(data), &raw); rawErr == nil {
				var extra []string
				for key := range raw {
					switch key {
					case "proxies", "visitors", "includes":
					default:
						extra = append(extra, key)
					}
				}
				if len(extra) > 0 {
					sort.Strings(extra)
					c.Warnings = append(c.Warnings, fmt.Sprintf(
						"%s: only [[proxies]] and [[visitors]] take effect in a fragment; its other settings are ignored: %s",
						file, strings.Join(extra, ", ")))
				}
			}
			if len(fragment.Includes) > 0 {
				return fmt.Errorf("include %s: nested includes are not supported", file)
			}
			for _, proxy := range fragment.Proxies {
				if seen["proxy:"+proxy.Name] {
					return fmt.Errorf("include %s: proxy %q is named twice", file, proxy.Name)
				}
				seen["proxy:"+proxy.Name] = true
				c.Proxies = append(c.Proxies, proxy)
			}
			for _, visitor := range fragment.Visitors {
				if seen["visitor:"+visitor.Name] {
					return fmt.Errorf("include %s: visitor %q is named twice", file, visitor.Name)
				}
				seen["visitor:"+visitor.Name] = true
				c.Visitors = append(c.Visitors, visitor)
			}
		}
	}
	return nil
}

// LoadString parses and validates a configuration held in a string. It behaves
// like Load on the contents of a configuration file; name stands in for the file
// name in the messages it returns and attaches as warnings.
func LoadString(data, name string, opts ValidateOptions) (*Config, error) {
	cfg, err := decodeConfig(data, name, opts)
	if err != nil {
		return cfg, err
	}
	if len(cfg.Includes) > 0 {
		// Load is what merges includes, against the directory of the file that
		// names them; a string has no such directory, so a config loaded this way
		// would silently publish none of the fragments it asks for.
		return cfg, fmt.Errorf("%s: includes needs a configuration file, because the fragments are resolved relative to the file that names them", name)
	}
	// No includes to wait for, so the expansion runs here; Load runs it after its
	// merge instead.
	if err := cfg.expandClientEntries(opts); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(opts.Role); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// frpKeyEquivalents maps a key frp uses to the key this program gives the same
// setting, so a configuration lifted from frp says what to write instead of only
// being listed as unknown. frp's names moved across its releases — camelCase keys
// such as bindPort and localIP, and a shared [common] section that later split
// into [client] and [server] — so both spellings of the common ones are here. A
// name whose feature this program does not have maps to "", which the message
// reports as such rather than suggesting a key that would not work either.
var frpKeyEquivalents = map[string]string{
	// frps: the settings frp keeps at the top level, in its own camelCase.
	"bindAddr":              "server.bind_addr",
	"bind_addr":             "server.bind_addr",
	"bindPort":              "server.bind_port",
	"bind_port":             "server.bind_port",
	"proxyBindAddr":         "server.proxy_bind_addr",
	"vhostHTTPPort":         "server.http_port",
	"vhost_http_port":       "server.http_port",
	"vhostHTTPSPort":        "server.https_port",
	"vhost_https_port":      "server.https_port",
	"subDomainHost":         "server.subdomain_host",
	"subdomainHost":         "server.subdomain_host",
	"subdomain_host":        "server.subdomain_host",
	"allowPorts":            "server.allow_ports",
	"maxPortsPerClient":     "server.max_ports_per_client",
	"userConnTimeout":       "server.user_conn_timeout_seconds",
	"tcpmuxHTTPConnectPort": "server.tcpmux_port",
	"kcpBindPort":           "",
	"kcp_bind_port":         "",
	"quicBindPort":          "",
	"quic_bind_port":        "",
	"maxPoolCount":          "server.max_pool_count",
	// frpc: [common] in older releases, [client] settings now.
	"common":             "[client] or [server], whichever holds the setting",
	"serverAddr":         "client.server_addr",
	"server_addr":        "client.server_addr",
	"serverPort":         "client.server_addr (host:port)",
	"clientID":           "client.client_id",
	"featureGates":       "feature_gates",
	"loginFailExit":      "client.login_fail_exit",
	"login_fail_exit":    "client.login_fail_exit",
	"poolCount":          "client.pool_count",
	"pool_count":         "client.pool_count",
	"heartbeatInterval":  "client.heartbeat_seconds",
	"heartbeat_interval": "client.heartbeat_seconds",
	"heartbeatTimeout":   "",
	"dnsServer":          "client.dns_server",
	"dns_server":         "client.dns_server",
	"admin_addr":         "client.admin.bind_addr",
	"admin_port":         "client.admin.port",
	"admin_user":         "client.admin.user",
	"admin_pwd":          "client.admin.password",
	"protocol":           "transport.protocol",
	"start":              "client.start",
	"metas":              "client.metas",
	"tls_enable":         "transport.enable_tls",
	"tls.enable":         "transport.enable_tls",
	"tcp_mux":            "transport.tcp_mux",
	// frp's credential, under whichever section it appears.
	"auth.token":           "auth_token",
	"token":                "auth_token",
	"authentication_token": "auth_token",
	"authenticationToken":  "auth_token",
	// frp's dashboard, [dashboard] here.
	"webServer.addr":     "$ROLE_DASH.bind_addr",
	"webServer.port":     "$ROLE_DASH.port",
	"webServer.password": "$ROLE_DASH.token",
	"dashboardAddr":      "dashboard.bind_addr",
	"dashboard_addr":     "dashboard.bind_addr",
	"dashboardPort":      "dashboard.port",
	"dashboard_port":     "dashboard.port",
	"dashboardPwd":       "dashboard.token",
	"dashboard_pwd":      "dashboard.token",
	// frp's log section, the same keys here with one rename.
	"log.to":       "log.to",
	"log_file":     "log.to",
	"log.level":    "log.level",
	"log_level":    "log.level",
	"log.maxDays":  "log.max_days",
	"log_max_days": "log.max_days",
	// frp's TLS block, [transport] here.
	"transport.tls.certFile":      "transport.cert_file",
	"transport.tls.keyFile":       "transport.key_file",
	"transport.tls.trustedCaFile": "transport.ca_file",
	"tls_cert_file":               "transport.cert_file",
	"tls_key_file":                "transport.key_file",
	"tls_trusted_ca_file":         "transport.ca_file",
	"transport.poolCount":         "client.pool_count",
	"transport.heartbeatInterval": "client.heartbeat_seconds",
	"transport.heartbeatTimeout":  "",
	"transport.tcpMux":            "transport.tcp_mux",
	// A [[proxies]] entry, field by field.
	"localIP":                  "local_ip",
	"localPort":                "local_port",
	"remotePort":               "remote_port",
	"customDomains":            "domains",
	"secretKey":                "secret_key",
	"transport.useEncryption":  "use_encryption",
	"transport.useCompression": "use_compression",
	"virtualNet":               "",
	"virtual_net":              "",
}

// frpKeySuggestion renders what to write instead of one unknown key that frp
// uses, or reports that neither the key nor any suffix of it is an frp name. The
// suffixes matter because frp's path to a key changed with its sections: what is
// common.transport.tls.enable in an old frpc.toml is transport.tls.enable in a
// newer one, and both end at the tls.enable this program calls
// transport.enable_tls.
func frpKeySuggestion(key string, role string) (string, bool) {
	parts := strings.Split(key, ".")
	for start := 0; start < len(parts); start++ {
		candidate := strings.Join(parts[start:], ".")
		if replacement, ok := frpKeyEquivalents[candidate]; ok {
			if replacement == "" {
				return key + " names a feature this program does not have", true
			}
			replacement = strings.ReplaceAll(replacement, "$ROLE_DASH", dashboardSection(role))
			return key + " → " + replacement, true
		}
	}
	return "", false
}

// dashboardSection names the section that carries frp's webServer settings in
// the role being validated: the server's dashboard, or the client's admin API.
func dashboardSection(role string) string {
	if role == RoleClient {
		return "client.admin"
	}
	return "dashboard"
}

// frpKeySuggestions renders the frp names among a set of unknown keys. It keeps
// one suggestion per key: the list of unknown keys it follows is just as long, so
// trimming it here would drop the answer for keys the operator is looking at.
func frpKeySuggestions(keys []string, role string) []string {
	suggestions := make([]string, 0, len(keys))
	for _, key := range keys {
		if suggestion, ok := frpKeySuggestion(key, role); ok {
			suggestions = append(suggestions, suggestion)
		}
	}
	return suggestions
}

// expandClientEntries applies the remote_ports expansion to the client's
// [[proxies]] and follows it through client.start. It runs after every source of
// proxies has been merged — includes included — because the expansion renames a
// range entry into one proxy per port, and a start name expanded before an
// included range was merged would name a proxy that does not exist, so the
// selection would silently start nothing.
//
// A server's [[proxies]] are policy, matched by the name a client registers, and
// remote_ports means nothing there: expanding renamed each policy — "web" became
// "web-6000", "web-6001", … — so the allow_cidrs and deny_cidrs an operator
// wrote for "web" matched no registration at all.
func (c *Config) expandClientEntries(opts ValidateOptions) error {
	if opts.Role != RoleClient {
		// A server's entries are policy and a role-less load (--dht-lookup, which
		// only needs [dht]) has no client entries to speak of: expanding either
		// renamed "web" to "web-6000", … and left the allow_cidrs written for
		// "web" matching no registration.
		return nil
	}
	// client.start names what to publish, and the expansion below renames each
	// range entry into one proxy per port, so those names have to follow the same
	// expansion or the selection silently picks nothing.
	c.Client.Start = ExpandStartNames(c.Client.Start, c.Proxies)
	// The expansion replaces remote_port on every copy it makes, so a remote_port
	// written beside remote_ports never reaches a proxy. Say so before the original
	// is gone: the warning has to be made here, because validation runs after this
	// and no longer sees a remote_ports.
	for i := range c.Proxies {
		p := &c.Proxies[i]
		if p.RemotePort > 0 && p.RemotePorts != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: remote_port %d is ignored because remote_ports is set: the range expands into one proxy per port",
				p.Name, p.RemotePort))
		}
	}
	expanded, err := ExpandRemotePorts(c.Proxies)
	if err != nil {
		return err
	}
	c.Proxies = expanded
	return nil
}

// decodeConfig parses and completes one configuration file without validating
// it: the includes mechanism needs every fragment parsed and merged before
// the whole set can be judged as one, and the remote_ports expansion has to run
// after that merge (see expandClientEntries).
func decodeConfig(data, name string, opts ValidateOptions) (*Config, error) {
	var cfg Config
	md, err := toml.Decode(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", name, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		message := fmt.Sprintf("config %s contains %d key(s) this version does not understand: %s",
			name, len(keys), strings.Join(keys, ", "))
		if suggestions := frpKeySuggestions(keys, opts.Role); len(suggestions) > 0 {
			// A file pasted from frp fails as unknown keys; naming the key that
			// replaces each one is the difference between "this is wrong" and
			// "this is what to write".
			message += "; frp names some of these differently: " + strings.Join(suggestions, ", ")
		}
		if opts.RejectUnknownKeys {
			return nil, fmt.Errorf("%s", message)
		}
		cfg.Warnings = append(cfg.Warnings, message)
	}

	cfg.applyDefaults()

	// A token read from a file takes the place of the inline one before the
	// environment is consulted, so precedence reads: environment, file, the
	// configuration itself.
	if err := cfg.applyTokenFiles(); err != nil {
		return &cfg, err
	}

	// Credentials from the environment are applied before validation, so a
	// configuration file that deliberately holds no secret passes the checks that
	// require one. The names used are reported as warnings so the operator can see
	// that the file was not the whole story.
	if applied := cfg.ApplyEnv(); len(applied) > 0 {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("%s overrode the configuration file", strings.Join(applied, ", ")))
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

// ClientHeartbeatInterval returns the configured client heartbeat period. It is the
// fallback for a server that does not state an interval of its own: a server that does
// state one governs, because it is what drops a client that goes quiet.
func (c *Config) ClientHeartbeatInterval() time.Duration {
	return time.Duration(c.Client.HeartbeatSeconds) * time.Second
}
