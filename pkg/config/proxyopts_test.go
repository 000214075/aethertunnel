package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBandwidth(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1MB", 1_000_000, false},
		{"1MB/s", 1_000_000, false},
		{"500KB", 500_000, false},
		{"2GB", 2_000_000_000, false},
		{"10B", 10, false},
		{"100000", 100_000, false},
		{" 1MB ", 1_000_000, false},
		{"", 0, true},
		{"-1MB", 0, true},
		{"abc", 0, true},
		{"MB", 0, true},
	}
	for _, c := range cases {
		got, err := ParseBandwidth(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseBandwidth(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBandwidth(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBandwidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePortRanges(t *testing.T) {
	ps, err := ParsePortRanges([]string{"6000-6999", "8000", ""})
	if err != nil {
		t.Fatalf("ParsePortRanges: %v", err)
	}
	for _, port := range []int{6000, 6500, 6999, 8000} {
		if !ps.Contains(port) {
			t.Errorf("Contains(%d) is false, want true", port)
		}
	}
	for _, port := range []int{5999, 7000, 7999, 8001, 0} {
		if ps.Contains(port) {
			t.Errorf("Contains(%d) is true, want false", port)
		}
	}

	if ps, err := ParsePortRanges(nil); err != nil || ps != nil {
		t.Fatalf("ParsePortRanges(nil) = %v, %v; want nil, nil", ps, err)
	}
	if _, err := ParsePortRanges([]string{"7000-6000"}); err == nil {
		t.Error("a reversed range was accepted")
	}
	if _, err := ParsePortRanges([]string{"0-100"}); err == nil {
		t.Error("a range starting at 0 was accepted")
	}
}

func TestExpandRemotePorts(t *testing.T) {
	proxies := []ProxyConfig{
		{Name: "web", Type: "tcp", LocalPort: 80, RemotePort: 8080},
		{Name: "batch", Type: "tcp", LocalPort: 9000, RemotePorts: "6000-6002"},
	}
	out, err := ExpandRemotePorts(proxies)
	if err != nil {
		t.Fatalf("ExpandRemotePorts: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("expanded to %d proxies, want 4", len(out))
	}
	wantNames := []string{"web", "batch-6000", "batch-6001", "batch-6002"}
	wantPorts := []int{8080, 6000, 6001, 6002}
	for i, p := range out {
		if p.Name != wantNames[i] || p.RemotePort != wantPorts[i] {
			t.Errorf("proxy %d is %s/%d, want %s/%d", i, p.Name, p.RemotePort, wantNames[i], wantPorts[i])
		}
		if p.RemotePorts != "" {
			t.Errorf("proxy %s still carries remote_ports", p.Name)
		}
		if p.LocalPort != 80 && p.LocalPort != 9000 {
			t.Errorf("proxy %s lost its local_port", p.Name)
		}
	}

	if _, err := ExpandRemotePorts([]ProxyConfig{{Name: "x", RemotePorts: "7000-7000-7000"}}); err == nil {
		t.Error("a malformed range was accepted")
	}
	if _, err := ExpandRemotePorts([]ProxyConfig{{Name: "x", RemotePorts: "1-999999"}}); err == nil {
		t.Error("an oversized range was accepted")
	}
}

func TestValidateProxyExtras(t *testing.T) {
	c := &Config{}
	problems := c.validateProxyExtras(ProxyConfig{
		Name: "ok", Bandwidth: "1MB", ProxyProtocol: "v1", Plugin: PluginStaticFile,
		PluginLocalPath: "/srv", HealthCheck: &HealthCheckConfig{Type: "http"},
	})
	if len(problems) != 0 {
		t.Fatalf("a valid entry produced problems: %v", problems)
	}

	problems = c.validateProxyExtras(ProxyConfig{
		Name: "bad", Bandwidth: "fast", ProxyProtocol: "v3", Plugin: "rsync",
		HealthCheck: &HealthCheckConfig{Type: "icmp"},
	})
	if len(problems) != 4 {
		t.Fatalf("got %d problems, want 4: %v", len(problems), problems)
	}
	if !strings.Contains(strings.Join(problems, "\n"), "bandwidth") {
		t.Error("the bandwidth problem does not name bandwidth")
	}
	// v2 is a valid proxy_protocol since the audit round; it must pass cleanly.
	if problems := c.validateProxyExtras(ProxyConfig{
		Name: "v2", ProxyProtocol: "v2",
	}); len(problems) != 0 {
		t.Fatalf("a v2 proxy_protocol produced problems: %v", problems)
	}
}

func TestValidateDNSLabel(t *testing.T) {
	for _, ok := range []string{"shop", "a", "a-b", "0auth", "a-b-c", strings.Repeat("a", 63)} {
		if err := ValidateDNSLabel(ok); err != nil {
			t.Errorf("ValidateDNSLabel(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "trail-", "a_b", "UPPER", "a.b", strings.Repeat("a", 64)} {
		if err := ValidateDNSLabel(bad); err == nil {
			t.Errorf("ValidateDNSLabel(%q) is nil, want an error", bad)
		}
	}
}

func TestValidateDialVia(t *testing.T) {
	for _, ok := range []string{
		"socks5://10.0.0.2:1080",
		"socks5://user:pass@10.0.0.2:1080",
		"socks5h://proxy.lan",
		"http://proxy.lan:3128",
		"https://user:pass@proxy.lan",
	} {
		if err := ValidateDialVia(ok); err != nil {
			t.Errorf("ValidateDialVia(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"ftp://proxy.lan", "socks5://", "http://", ":"} {
		if err := ValidateDialVia(bad); err == nil {
			t.Errorf("ValidateDialVia(%q) is nil, want an error", bad)
		}
	}
}

func TestSubdomainValidationInProxyExtras(t *testing.T) {
	c := &Config{}
	problems := c.validateProxyExtras(ProxyConfig{Name: "web", Subdomain: "bad_label!"})
	if len(problems) == 0 {
		t.Fatal("an invalid subdomain produced no problem")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "subdomain") {
		t.Fatalf("the problem does not name subdomain: %v", problems)
	}
	if problems := c.validateProxyExtras(ProxyConfig{Name: "web", Subdomain: "shop"}); len(problems) != 0 {
		t.Fatalf("a valid subdomain produced problems: %v", problems)
	}
}

func TestClientDialViaValidation(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "token"
	cfg.Client.DialVia = "ftp://proxy.lan"
	err := cfg.Validate(RoleClient)
	if err == nil || !strings.Contains(err.Error(), "dial_via") {
		t.Fatalf("a dial_via the client cannot parse passed validation: %v", err)
	}
	cfg.Client.DialVia = "socks5://user:pass@10.0.0.2:1080"
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a valid dial_via failed validation: %v", err)
	}
}

func TestHTTPAuthValidation(t *testing.T) {
	c := &Config{}
	if problems := c.validateProxyExtras(ProxyConfig{
		Name: "web", Type: ProxyTypeTCP, HTTPUser: "ops", HTTPPassword: "s3cret",
	}); len(problems) == 0 {
		t.Fatal("basic auth on a tcp proxy produced no problem")
	} else if !strings.Contains(strings.Join(problems, "\n"), "http_user") {
		t.Fatalf("the problem does not name http_user: %v", problems)
	}
	if problems := c.validateProxyExtras(ProxyConfig{
		Name: "web", Type: ProxyTypeHTTPS, HTTPUser: "ops", HTTPPassword: "s3cret",
	}); len(problems) != 0 {
		t.Fatalf("basic auth on an https proxy produced problems: %v", problems)
	}
}

func TestTCPMuxValidation(t *testing.T) {
	base := "[client]\nserver_addr = \"127.0.0.1:1\"\nauth_token = \"token\"\n\n"
	mux := "mux.toml"
	_, err := LoadString(base+"[[proxies]]\nname = \"mux\"\ntype = \"tcpmux\"\nmultiplexer = \"socksify\"\ndomains = [\"tunnel1\"]\nlocal_port = 8000\n", mux, ValidateOptions{Role: RoleClient})
	if err == nil || !strings.Contains(err.Error(), "multiplexer") {
		t.Fatalf("an unknown multiplexer passed validation: %v", err)
	}
	_, err = LoadString(base+"[[proxies]]\nname = \"mux\"\ntype = \"tcpmux\"\nmultiplexer = \"httpconnect\"\ndomains = [\"tunnel1\"]\nlocal_port = 8000\nremote_port = 7000\n", mux, ValidateOptions{Role: RoleClient})
	if err == nil || !strings.Contains(err.Error(), "remote_port") {
		t.Fatalf("a tcpmux proxy with a public port passed validation: %v", err)
	}
	if _, err := LoadString(base+"[[proxies]]\nname = \"mux\"\ntype = \"tcpmux\"\nmultiplexer = \"httpconnect\"\ndomains = [\"tunnel1\"]\nlocal_port = 8000\n", mux, ValidateOptions{Role: RoleClient}); err != nil {
		t.Fatalf("a valid tcpmux proxy failed validation: %v", err)
	}
}

func TestLoadRecordsTheSourceFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.toml")
	body := "[client]\nserver_addr = \"127.0.0.1:1\"\nauth_token = \"token\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path, ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SourceFile != path {
		t.Fatalf("SourceFile is %q, want %q", cfg.SourceFile, path)
	}
}

func TestHTTPS2HTTPPluginValidation(t *testing.T) {
	c := &Config{}
	base := ProxyConfig{Name: "site", Type: ProxyTypeTCP, Plugin: PluginHTTPS2HTTP, LocalPort: 8080}
	if problems := c.validateProxyExtras(base); len(problems) == 0 {
		t.Fatal("a missing certificate produced no problem")
	} else if !strings.Contains(strings.Join(problems, "\n"), "plugin_cert_file") {
		t.Fatalf("the problem does not name plugin_cert_file: %v", problems)
	}
	withCert := base
	withCert.PluginCertFile = "/tmp/site.crt"
	withCert.PluginKeyFile = "/tmp/site.key"
	if problems := c.validateProxyExtras(withCert); len(problems) != 0 {
		t.Fatalf("a complete https2http plugin produced problems: %v", problems)
	}
	noPort := base
	noPort.PluginCertFile = "/tmp/site.crt"
	noPort.PluginKeyFile = "/tmp/site.key"
	noPort.LocalPort = 0
	if problems := c.validateProxyExtras(noPort); len(problems) == 0 {
		t.Fatal("a missing local_port produced no problem")
	}
	if problems := c.validateProxyExtras(ProxyConfig{
		Name: "site", Type: ProxyTypeTCP, Plugin: "gopherhole",
	}); len(problems) == 0 {
		t.Fatal("an unknown plugin produced no problem")
	}
}
