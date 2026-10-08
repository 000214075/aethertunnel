package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// ParseBandwidth parses a decimal bytes-per-second rate: "500" is 500 B/s,
// "1MB" is 1 000 000 B/s, "500KB/s" is the same as "500KB". The units follow
// frp's, so a config written for frp reads the same here.
func ParseBandwidth(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "/s"))
	if s == "" {
		return 0, errors.New("empty bandwidth")
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"GB", 1e9},
		{"MB", 1e6},
		{"KB", 1e3},
		{"B", 1},
	}
	upper := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			return scaledBandwidth(num, u.mult, s)
		}
	}
	return scaledBandwidth(s, 1, s)
}

// pluginLocalPortProblem checks the port a plugin-backed proxy dials. The plugin
// paths build "127.0.0.1:<local_port>" and hand it to a dialer, so a port outside
// the range is a proxy that is accepted and then fails every connection.
func pluginLocalPortProblem(p ProxyConfig, what string) []string {
	if p.LocalPort >= 1 && p.LocalPort <= 65535 {
		return nil
	}
	return []string{fmt.Sprintf(
		"proxy %q: plugin %q %s, which must be 1-65535, got %d",
		p.Name, p.Plugin, what, p.LocalPort)}
}

// scaledBandwidth turns a rate and its unit into bytes per second. The result has
// to be at least one: a rate the parser accepts but that truncates to zero — "0.5"
// is the example — leaves a token bucket that can grant nothing, and the relay
// that wraps a connection with it then spends every read and write looping over a
// count it never reduces. NaN and Inf pass a plain positivity test and convert to
// an out-of-range integer, so they are refused by name.
func scaledBandwidth(number string, mult float64, original string) (int64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
	if err != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("bandwidth %q is not a positive number", original)
	}
	scaled := v * mult
	if scaled < 1 {
		return 0, fmt.Errorf("bandwidth %q is below one byte per second, which cannot be relayed", original)
	}
	if scaled > float64(math.MaxInt64) {
		return 0, fmt.Errorf("bandwidth %q is larger than this program can count", original)
	}
	// float64(math.MaxInt64) rounds up to 2^63, so a scaled value exactly at that
	// boundary passes the test above and converts to a negative int64 — which the
	// relay reads as "no limit at all" and drops the cap the operator wrote.
	bytes := int64(scaled)
	if bytes < 1 {
		return 0, fmt.Errorf("bandwidth %q is larger than this program can count", original)
	}
	return bytes, nil
}

// PortSet is the parsed form of [server].allow_ports: the remote_port ranges
// a client may register. A nil *PortSet allows every port.
type PortSet struct {
	ranges [][2]int
}

// ParsePortRanges parses entries like "6000-6999" or "8000".
func ParsePortRanges(ranges []string) (*PortSet, error) {
	ps := &PortSet{}
	for _, r := range ranges {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		lo, hi, found := strings.Cut(r, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("port range %q: %v", r, err)
		}
		b := a
		if found {
			b, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("port range %q: %v", r, err)
			}
		}
		if a < 1 || b > 65535 || a > b {
			return nil, fmt.Errorf("port range %q is outside 1-65535 or reversed", r)
		}
		ps.ranges = append(ps.ranges, [2]int{a, b})
	}
	if len(ps.ranges) == 0 {
		// An empty list restricts nothing; the nil set means "every port".
		return nil, nil
	}
	return ps, nil
}

// Contains reports whether port is inside one of the ranges. A nil set allows
// every port.
func (ps *PortSet) Contains(port int) bool {
	if ps == nil {
		return true
	}
	for _, r := range ps.ranges {
		if port >= r[0] && port <= r[1] {
			return true
		}
	}
	return false
}

// ExpandRemotePorts turns each proxy's remote_ports range into separate
// proxies — "6000-6002" becomes remote_port 6000, 6001 and 6002 with names
// name-6000, name-6001 and name-6002, so a range never collides with the
// unexpanded names. Everything else in the entry applies to every copy.
func ExpandRemotePorts(proxies []ProxyConfig) ([]ProxyConfig, error) {
	out := make([]ProxyConfig, 0, len(proxies))
	for _, p := range proxies {
		if p.RemotePorts == "" {
			out = append(out, p)
			continue
		}
		a, b, err := parseRemotePortRange(p)
		if err != nil {
			return nil, err
		}
		for port := a; port <= b; port++ {
			expanded := p
			expanded.RemotePorts = ""
			expanded.RemotePort = port
			expanded.Name = ExpandedProxyName(p.Name, port)
			out = append(out, expanded)
		}
	}
	return out, nil
}

// ExpandedProxyName is the name one port of a remote_ports range is published
// under. It is a function so the expansion and anything that has to follow a name
// through it — client.start, for one — cannot drift apart.
func ExpandedProxyName(name string, port int) string {
	return fmt.Sprintf("%s-%d", name, port)
}

// parseRemotePortRange reads a remote_ports value: "6000", "6000-6002".
func parseRemotePortRange(p ProxyConfig) (int, int, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(p.RemotePorts), "-")
	a, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, fmt.Errorf("proxy %q: remote_ports %q: %v", p.Name, p.RemotePorts, err)
	}
	b := a
	if found {
		b, err = strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return 0, 0, fmt.Errorf("proxy %q: remote_ports %q: %v", p.Name, p.RemotePorts, err)
		}
	}
	if a < 1 || b > 65535 || a > b {
		return 0, 0, fmt.Errorf("proxy %q: remote_ports %q is outside 1-65535 or reversed", p.Name, p.RemotePorts)
	}
	if b-a > 255 {
		return 0, 0, fmt.Errorf("proxy %q: remote_ports %q spans more than 256 ports", p.Name, p.RemotePorts)
	}
	return a, b, nil
}

// ExpandStartNames maps client.start entries through the remote_ports expansion: a
// range renames its entry into one proxy per port, and client.start selects by name,
// so an entry naming the original selected nothing at all while its warning pointed
// at a name the operator had written correctly.
func ExpandStartNames(start []string, proxies []ProxyConfig) []string {
	if len(start) == 0 {
		return start
	}
	out := make([]string, 0, len(start))
	for _, name := range start {
		expanded := false
		for _, p := range proxies {
			if p.Name != name || p.RemotePorts == "" {
				continue
			}
			lo, hi, err := parseRemotePortRange(p)
			if err != nil {
				// Something else reports the malformed range; keep the name so this
				// never becomes the place the error surfaces.
				break
			}
			for port := lo; port <= hi; port++ {
				out = append(out, ExpandedProxyName(name, port))
			}
			expanded = true
			break
		}
		if !expanded {
			out = append(out, name)
		}
	}
	return out
}

// Plugins the client can run in place of a local service.
const (
	PluginStaticFile = "static_file"
	PluginUnixSocket = "unix_domain_socket"
	// PluginHTTPS2HTTP terminates the visitor's TLS at this client with the
	// configured certificate and forwards the plaintext to the plain HTTP
	// service named by local_port, so a server without a certificate of its
	// own can still publish HTTPS on a dedicated tcp port.
	PluginHTTPS2HTTP = "https2http"
	// PluginHTTPProxy runs an HTTP forward proxy on this client: the visitor
	// speaks the HTTP proxy protocol (absolute-form requests and CONNECT) to
	// the public port, and this client dials the named targets. allow_targets
	// bounds what it may dial.
	PluginHTTPProxy = "http_proxy"
	// PluginHTTP2HTTPS answers plain HTTP on the public port and forwards to
	// the local HTTPS service named by local_port, wrapping the local leg in
	// TLS with the request's Host as SNI.
	PluginHTTP2HTTPS = "http2https"
	// PluginSocks5 runs a SOCKS5 server on this client: the visitor speaks
	// SOCKS5 to the public port and this client dials the named targets from
	// its own network. allow_targets bounds what it may dial; plugin_user and
	// plugin_password, when both set, require SOCKS5 username/password
	// authentication.
	PluginSocks5 = "socks5"
	// PluginHTTPS2HTTPS terminates the visitor's TLS at this client with the
	// configured certificate and forwards to the local HTTPS service named by
	// local_port, wrapping the local leg in TLS again — both legs encrypted.
	PluginHTTPS2HTTPS = "https2https"
	// PluginTLS2Raw terminates the visitor's TLS at this client with the
	// configured certificate and relays the plaintext bytes to the local TCP
	// service named by local_port: the https2http bridge without the HTTP
	// expectations, for a backend that speaks its own protocol.
	PluginTLS2Raw = "tls2raw"
)

// ProxyPlugins lists the accepted [[proxies]].plugin values.
var ProxyPlugins = []string{PluginStaticFile, PluginUnixSocket, PluginHTTPS2HTTP, PluginHTTPProxy, PluginHTTP2HTTPS, PluginSocks5, PluginHTTPS2HTTPS, PluginTLS2Raw}

// validateProxyExtras holds the checks every client-side proxy pays for,
// beyond the type and port rules the caller already applies. It returns the
// problems and appends the warnings it finds.
func (c *Config) validateProxyExtras(p ProxyConfig) []string {
	var problems []string
	if p.Bandwidth != "" {
		if _, err := ParseBandwidth(p.Bandwidth); err != nil {
			problems = append(problems, fmt.Sprintf("proxy %q: %v", p.Name, err))
		}
	}
	if p.ProxyProtocol != "" && p.ProxyProtocol != "v1" && p.ProxyProtocol != "v2" {
		problems = append(problems, fmt.Sprintf(
			"proxy %q: proxy_protocol must be \"v1\", \"v2\" or empty, got %q", p.Name, p.ProxyProtocol))
	}
	if p.Subdomain != "" {
		if err := ValidateDNSLabel(p.Subdomain); err != nil {
			problems = append(problems, fmt.Sprintf("proxy %q: subdomain %v", p.Name, err))
		}
	}
	if (p.HTTPUser != "" || p.HTTPPassword != "") &&
		p.Type != ProxyTypeHTTP && p.Type != ProxyTypeHTTPS {
		problems = append(problems, fmt.Sprintf(
			"proxy %q: http_user and http_password guard http and https hostnames, not %s",
			p.Name, p.Type))
	}
	if p.TLSPassthrough && p.Type != ProxyTypeHTTPS {
		problems = append(problems, fmt.Sprintf(
			"proxy %q: tls_passthrough routes an https tunnel by its ClientHello, not a %s tunnel",
			p.Name, p.Type))
	}
	if p.Plugin != "" && IsDatagramProxyType(p.Type) {
		// The datagram path relays to local_addr and never builds a plugin, and
		// local_port is waived when a plugin is set, so the tunnel would carry
		// nothing to port 0.
		problems = append(problems, fmt.Sprintf(
			"proxy %q: plugin %q serves a byte stream; a %s tunnel moves datagrams and never runs it",
			p.Name, p.Plugin, p.Type))
	}
	if p.TLSPassthrough && p.Plugin != "" &&
		p.Plugin != PluginHTTPS2HTTP && p.Plugin != PluginHTTPS2HTTPS && p.Plugin != PluginTLS2Raw {
		problems = append(problems, fmt.Sprintf(
			"proxy %q: plugin %q does not terminate the visitor's TLS; with tls_passthrough the raw session reaches this client",
			p.Name, p.Plugin))
	}
	if p.UseCompression && IsDatagramProxyType(p.Type) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"proxy %q: use_compression compresses byte streams; a %s tunnel moves datagrams and gains nothing from it",
			p.Name, p.Type))
	}
	if p.UseEncryption && IsDatagramProxyType(p.Type) {
		// Which of the two layers the datagrams really have depends on
		// [encryption], and the two cases say opposite things: with it on the
		// session cipher is all there is, with it off the datagrams have no
		// encryption at all. One wording covered both and told an operator with
		// the section off that a stream which is in the clear is "protected".
		if c.Encryption.Enabled {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: use_encryption wraps the relayed byte stream; a %s tunnel moves datagram frames, so this proxy's stream is protected by [encryption] alone",
				p.Name, p.Type))
		} else {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: use_encryption wraps the relayed byte stream; a %s tunnel moves datagram frames and this configuration has [encryption].enabled false, so this proxy carries its datagrams without any encryption",
				p.Name, p.Type))
		}
	}
	if (len(p.RequestHeaders) > 0 || len(p.ResponseHeaders) > 0) &&
		p.Type != ProxyTypeHTTP && p.Type != ProxyTypeHTTPS {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"proxy %q: request_headers and response_headers apply to the terminating http/https path; a %s tunnel never parses HTTP and ignores them",
			p.Name, p.Type))
	}
	for _, location := range p.Locations {
		if !strings.HasPrefix(location, "/") {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: location %q must start with a slash: locations are path prefixes",
				p.Name, location))
		}
	}
	if len(p.Locations) > 0 && p.Type != ProxyTypeHTTP && p.Type != ProxyTypeHTTPS {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"proxy %q: locations split a hostname's paths on the terminating http/https listeners; a %s tunnel never parses HTTP and ignores them",
			p.Name, p.Type))
	}
	if p.RouteByHTTPUser != "" && p.Type != ProxyTypeHTTP && p.Type != ProxyTypeHTTPS {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"proxy %q: route_by_http_user splits a hostname's http/https traffic by the username the visitor presents; a %s tunnel never parses HTTP and ignores it",
			p.Name, p.Type))
	}
	if len(p.Locations) > 0 && p.Type == ProxyTypeHTTPS && p.TLSPassthrough {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"proxy %q: locations are ignored with tls_passthrough: the server relays the TLS session without reading the HTTP path",
			p.Name))
	}
	switch p.Plugin {
	case "":
	case PluginStaticFile, PluginUnixSocket:
		if p.PluginLocalPath == "" {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs plugin_local_path", p.Name, p.Plugin))
		}
		if p.LocalPort != 0 || p.LocalIP != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q replaces the local service, so local_ip and local_port are ignored",
				p.Name, p.Plugin))
		}
	case PluginHTTPS2HTTP:
		problems = append(problems, pluginLocalPortProblem(p, "forwards to the plain HTTP service named by local_port")...)
		if p.PluginCertFile == "" || p.PluginKeyFile == "" {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs plugin_cert_file and plugin_key_file",
				p.Name, p.Plugin))
		}
		if p.PluginLocalPath != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q has no use for plugin_local_path", p.Name, p.Plugin))
		}
	case PluginHTTPProxy:
		// The plugin dials whatever the visitor asks for from this client's
		// network, so an empty target list would make it an exit for
		// everything the client can reach — the same rule a socks5 tunnel
		// answers to.
		if len(p.AllowTargets) == 0 {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs allow_targets: without a list it is an exit for everything the client can reach",
				p.Name, p.Plugin))
		}
		for _, cidr := range p.AllowTargets {
			if _, err := socks.NewTargetPolicy([]string{cidr}); err != nil {
				problems = append(problems, fmt.Sprintf("proxy %q: allow_targets entry %q: %v", p.Name, cidr, err))
			}
		}
		if p.LocalPort != 0 || p.LocalIP != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q dials the targets the visitor names, so local_ip and local_port are ignored",
				p.Name, p.Plugin))
		}
		if p.PluginLocalPath != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q has no use for plugin_local_path", p.Name, p.Plugin))
		}
	case PluginHTTP2HTTPS:
		problems = append(problems, pluginLocalPortProblem(p, "forwards to the local HTTPS service named by local_port")...)
		if p.PluginLocalPath != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q has no use for plugin_local_path", p.Name, p.Plugin))
		}
	case PluginSocks5:
		// The plugin dials whatever the visitor asks for from this client's
		// network — the same rule the socks5 proxy type and the http_proxy
		// plugin answer to.
		if len(p.AllowTargets) == 0 {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs allow_targets: without a list it is an exit for everything the client can reach",
				p.Name, p.Plugin))
		}
		for _, cidr := range p.AllowTargets {
			if _, err := socks.NewTargetPolicy([]string{cidr}); err != nil {
				problems = append(problems, fmt.Sprintf("proxy %q: allow_targets entry %q: %v", p.Name, cidr, err))
			}
		}
		if (p.PluginUser == "") != (p.PluginPassword == "") {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs plugin_user and plugin_password together: SOCKS5 authentication is both or none",
				p.Name, p.Plugin))
		}
		if p.LocalPort != 0 || p.LocalIP != "" || p.PluginLocalPath != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q dials the targets the visitor names, so local_ip, local_port and plugin_local_path are ignored",
				p.Name, p.Plugin))
		}
	case PluginHTTPS2HTTPS, PluginTLS2Raw:
		problems = append(problems, pluginLocalPortProblem(p, "forwards to the local service named by local_port")...)
		if p.PluginCertFile == "" || p.PluginKeyFile == "" {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: plugin %q needs plugin_cert_file and plugin_key_file",
				p.Name, p.Plugin))
		}
		if p.PluginLocalPath != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: plugin %q has no use for plugin_local_path", p.Name, p.Plugin))
		}
	default:
		problems = append(problems, fmt.Sprintf(
			"proxy %q: plugin %q is not supported (use one of %s)",
			p.Name, p.Plugin, strings.Join(ProxyPlugins, ", ")))
	}
	if h := p.HealthCheck; h != nil {
		// A probe dials the local service at local_ip:local_port. Two kinds of proxy
		// have no such address — a socks5 tunnel, whose target comes from the
		// visitor, and the plugins that run their own server — so the probe would
		// fail on every attempt and the client would withdraw a tunnel that works,
		// for good, while its log repeated the same line.
		switch {
		case p.Type == ProxyTypeSOCKS:
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check probes the local service at local_ip:local_port, and a socks5 tunnel has none (its target comes from the visitor), so the probe would fail every time and the tunnel would be withdrawn for good",
				p.Name))
		case isInProcessPlugin(p.Plugin):
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check probes the local service at local_ip:local_port, and the %s plugin runs its own server instead, so the probe would fail every time and the tunnel would be withdrawn for good",
				p.Name, p.Plugin))
		case IsDatagramProxyType(p.Type):
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check connects to local_ip:local_port over TCP, and a %s proxy's local service is a UDP port, so the probe would fail every time and the tunnel would be withdrawn for good",
				p.Name, p.Type))
		}
		// The probe schedule a configuration left out, filled in here as well as by
		// applyDefaults: Validate promises a Config built in code the same treatment,
		// and an entry that names only an interval used to be refused for a type the
		// runtime reads as tcp anyway.
		h.healthCheckDefaults()
		// A negative interval panics NewTicker inside the probe goroutine and
		// takes the whole client down; a negative timeout fails every probe
		// instantly, withdrawing a healthy service; a negative max_failed
		// withdraws on the first hiccup. Zero keeps its documented meaning.
		if h.IntervalS < 0 || h.TimeoutS < 0 || h.MaxFailed < 0 {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check.interval_s, timeout_s and max_failed cannot be negative, got %d/%d/%d",
				p.Name, h.IntervalS, h.TimeoutS, h.MaxFailed))
		}
		// A large-but-parseable interval overflows the conversion to a
		// time.Duration, which goes negative and panics NewTicker inside the probe
		// goroutine — taking the whole client process down. A day is already far
		// past any useful probe schedule.
		const maxHealthSeconds = 86400
		if h.IntervalS > maxHealthSeconds || h.TimeoutS > maxHealthSeconds {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check.interval_s and timeout_s must be at most %d seconds, got %d/%d",
				p.Name, maxHealthSeconds, h.IntervalS, h.TimeoutS))
		}
		if h.Type != "tcp" && h.Type != "http" {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check.type must be \"tcp\" or \"http\", got %q", p.Name, h.Type))
		}
		if h.Type == "http" && h.Path != "" && !strings.HasPrefix(h.Path, "/") {
			problems = append(problems, fmt.Sprintf(
				"proxy %q: health_check.path must start with /, got %q", p.Name, h.Path))
		}
		if h.Type == "tcp" && h.Path != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: health_check.path has no effect on a tcp health check", p.Name))
		}
		if h.Type == "tcp" && len(h.HTTPHeaders) > 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"proxy %q: health_check.http_headers has no effect on a tcp health check", p.Name))
		}
	}
	return problems
}

// healthCheckDefaults fills the probe schedule a configuration left out.
func (h *HealthCheckConfig) healthCheckDefaults() {
	if h.Type == "" {
		// The probe treats anything but "http" as a TCP connect, so an entry that
		// names only an interval has to read as tcp: demanding a type would refuse a
		// key whose absence the runtime already handles, and frp defaults it the same
		// way.
		h.Type = "tcp"
	}
	if h.IntervalS == 0 {
		h.IntervalS = 10
	}
	if h.TimeoutS == 0 {
		h.TimeoutS = 3
	}
	if h.MaxFailed == 0 {
		h.MaxFailed = 3
	}
	if h.Type == "http" && h.Path == "" {
		h.Path = "/"
	}
}

// ValidateDNSLabel checks one DNS label: lowercase letters, digits and hyphens,
// 1-63 characters, no leading or trailing hyphen.
func ValidateDNSLabel(label string) error {
	if label == "" || len(label) > 63 {
		return errors.New("a DNS label is 1-63 characters")
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return errors.New("a DNS label cannot start or end with a hyphen")
	}
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("%q is not a lowercase letter, digit or hyphen", string(r))
		}
	}
	return nil
}

// ValidateDialVia accepts the [client].dial_via URLs: a SOCKS5 or HTTP CONNECT
// proxy sitting between the client and the server.
func ValidateDialVia(via string) error {
	u, err := url.Parse(via)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return fmt.Errorf("scheme %q is not supported (use socks5, socks5h, http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("a proxy host is required, got %q", via)
	}
	// The CONNECT paths dial u.Host verbatim, and the SOCKS5 client defaults a
	// missing port to 1080 on its own. Without this a proxy named without a port
	// passed --check and then failed every connection with "missing port in
	// address", which names the wrong setting.
	switch u.Scheme {
	case "http", "https":
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return fmt.Errorf("the %s proxy needs host:port, got %q: the CONNECT request has no default port", u.Scheme, via)
		}
	}
	return nil
}
