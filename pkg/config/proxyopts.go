package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
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
			v, err := strconv.ParseFloat(num, 64)
			if err != nil || v <= 0 {
				return 0, fmt.Errorf("bandwidth %q is not a positive number", s)
			}
			return int64(v * u.mult), nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("bandwidth %q is not a positive rate (try \"1MB\")", s)
	}
	return int64(v), nil
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
		lo, hi, found := strings.Cut(strings.TrimSpace(p.RemotePorts), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("proxy %q: remote_ports %q: %v", p.Name, p.RemotePorts, err)
		}
		b := a
		if found {
			b, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("proxy %q: remote_ports %q: %v", p.Name, p.RemotePorts, err)
			}
		}
		if a < 1 || b > 65535 || a > b {
			return nil, fmt.Errorf("proxy %q: remote_ports %q is outside 1-65535 or reversed", p.Name, p.RemotePorts)
		}
		if b-a > 255 {
			return nil, fmt.Errorf("proxy %q: remote_ports %q spans more than 256 ports", p.Name, p.RemotePorts)
		}
		for port := a; port <= b; port++ {
			expanded := p
			expanded.RemotePorts = ""
			expanded.RemotePort = port
			expanded.Name = fmt.Sprintf("%s-%d", p.Name, port)
			out = append(out, expanded)
		}
	}
	return out, nil
}

// Plugins the client can run in place of a local service.
const (
	PluginStaticFile = "static_file"
	PluginUnixSocket = "unix_domain_socket"
)

// ProxyPlugins lists the accepted [[proxies]].plugin values.
var ProxyPlugins = []string{PluginStaticFile, PluginUnixSocket}

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
	if p.ProxyProtocol != "" && p.ProxyProtocol != "v1" {
		problems = append(problems, fmt.Sprintf(
			"proxy %q: proxy_protocol must be \"v1\" or empty, got %q", p.Name, p.ProxyProtocol))
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
	default:
		problems = append(problems, fmt.Sprintf(
			"proxy %q: plugin %q is not supported (use one of %s)",
			p.Name, p.Plugin, strings.Join(ProxyPlugins, ", ")))
	}
	if h := p.HealthCheck; h != nil {
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
	}
	return problems
}

// healthCheckDefaults fills the probe schedule a configuration left out.
func (h *HealthCheckConfig) healthCheckDefaults() {
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
