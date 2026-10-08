package config

import (
	"strings"
	"testing"
)

// includes are resolved against the directory of the file that names them, and a
// configuration loaded from a string has none: silently publishing none of the
// fragments it asks for is the one thing LoadString must not do.
func TestLoadStringRefusesIncludes(t *testing.T) {
	_, err := LoadString(
		"includes = [\"fragments/*.toml\"]\n\n[client]\nserver_addr = \"127.0.0.1:1\"\nauth_token = \"0123456789abcdef\"\n",
		"embedded", ValidateOptions{Role: RoleClient})
	if err == nil {
		t.Fatal("a string configuration naming includes was accepted")
	}
	if !strings.Contains(err.Error(), "includes") {
		t.Fatalf("the error does not name includes: %v", err)
	}
}

// A large-but-parseable interval overflows the conversion to a time.Duration,
// which goes negative and panics NewTicker inside the probe goroutine.
func TestAHealthCheckIntervalThatWouldOverflowIsRefused(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:1"
	cfg.Client.AuthToken = "0123456789abcdef"
	cfg.Proxies = []ProxyConfig{{
		Name: "web", Type: ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: 8080,
		HealthCheck: &HealthCheckConfig{Type: "tcp", IntervalS: 10000000000},
	}}
	if err := cfg.Validate(RoleClient); err == nil {
		t.Fatal("a health check interval that overflows a time.Duration was accepted")
	}
}

// With identity support off, require_identity checks nothing at all; an operator
// who sets only that key believes identities are mandatory.
func TestRequireIdentityWithoutIdentitySupportIsWarnedAbout(t *testing.T) {
	cfg := &Config{}
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.BindPort = 7000
	cfg.Server.AuthToken = "0123456789abcdef"
	cfg.Identity.RequireIdentity = true
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "identity.require_identity is true but identity.enabled is false") {
			return
		}
	}
	t.Fatalf("no warning about require_identity with identity.enabled false; warnings: %v", cfg.Warnings)
}

// A rate the parser accepts but that truncates to zero leaves a token bucket with
// a burst of zero: every read and write on that proxy then loops over a count the
// limiter can never grant, spinning a core forever.
func TestABandwidthThatRoundsToZeroIsRefused(t *testing.T) {
	for _, rate := range []string{"0.5", "0.5B", "0.0009KB", "1e-9"} {
		if _, err := ParseBandwidth(rate); err == nil {
			t.Errorf("ParseBandwidth(%q) was accepted", rate)
		}
	}
	// NaN and Inf pass a plain "greater than zero" test and convert to an
	// out-of-range integer, which is the same broken limiter with a negative burst.
	for _, rate := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf"} {
		if _, err := ParseBandwidth(rate); err == nil {
			t.Errorf("ParseBandwidth(%q) was accepted", rate)
		}
	}
	// The rates that mean something still parse.
	for rate, want := range map[string]int64{"500": 500, "1MB": 1000000, "0.5GB": 500000000} {
		got, err := ParseBandwidth(rate)
		if err != nil {
			t.Errorf("ParseBandwidth(%q): %v", rate, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBandwidth(%q) = %d, want %d", rate, got, want)
		}
	}
}

// The manual and the field's own comment both say bind_port = 0 keeps the section
// in the file with the gateway off, and the listener table and the server both
// read it that way; validation refused the configuration the manual describes.
func TestTheSSHGatewayCanBeDisabledByPortZero(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Server.SSHTunnelGateway = &SSHTunnelGatewayConfig{BindPort: 0, User: "v0", Password: "secret"}
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("a section with bind_port = 0 was refused: %v", err)
	}

	cfg.Server.SSHTunnelGateway.BindPort = -1
	if err := cfg.Validate(RoleServer); err == nil {
		t.Fatal("a negative ssh_tunnel_gateway.bind_port was accepted")
	}
	cfg.Server.SSHTunnelGateway.BindPort = 70000
	if err := cfg.Validate(RoleServer); err == nil {
		t.Fatal("an ssh_tunnel_gateway.bind_port above 65535 was accepted")
	}
}

// Every other listener port is range-checked; these two only had a lower bound or
// none at all, so a negative value was silently read as "disabled" and a value
// above the range only failed later, at bind time.
func TestTheSharedListenerPortsAreRangeChecked(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		cfg := serverConfigForPortTests()
		cfg.Server.TCPMuxPort = port
		if err := cfg.Validate(RoleServer); err == nil {
			t.Errorf("server.tcpmux_port = %d was accepted", port)
		}

		cfg = serverConfigForPortTests()
		cfg.Server.HTTPSPassthroughPort = port
		if err := cfg.Validate(RoleServer); err == nil {
			t.Errorf("server.https_passthrough_port = %d was accepted", port)
		}
	}
}

// A plugin-backed proxy builds "127.0.0.1:<local_port>" and hands it to a dialer,
// so the range check the plain proxies get has to cover it too: a port outside the
// range was accepted and then failed every connection.
func TestAPluginProxysLocalPortIsRangeChecked(t *testing.T) {
	for _, plugin := range []string{PluginHTTPS2HTTP, PluginHTTP2HTTPS, PluginHTTPS2HTTPS, PluginTLS2Raw} {
		for _, port := range []int{-1, 70000} {
			cfg := &Config{}
			cfg.Client.ServerAddr = "127.0.0.1:1"
			cfg.Client.AuthToken = "0123456789abcdef"
			cfg.Proxies = []ProxyConfig{{
				Name: "web", Type: ProxyTypeTCP, Plugin: plugin,
				LocalIP: "127.0.0.1", LocalPort: port,
				PluginCertFile: "cert.pem", PluginKeyFile: "key.pem",
			}}
			if err := cfg.Validate(RoleClient); err == nil {
				t.Errorf("plugin %q with local_port = %d was accepted", plugin, port)
			}
		}
	}
}

// serverConfigForPortTests is a minimal server configuration that validates on its
// own, so a test can add the one key it is about.
func serverConfigForPortTests() *Config {
	cfg := &Config{}
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.BindPort = 7000
	cfg.Server.AuthToken = "0123456789abcdef"
	return cfg
}

// pad_to = 0 has to mean "no padding" as the manual says. An int could not tell it
// apart from the key being absent, so the 256-byte default applied to both and the
// documented behaviour was unreachable.
func TestPadToZeroDisablesPadding(t *testing.T) {
	base := `[client]
server_addr = "127.0.0.1:1"
auth_token = "0123456789abcdef"

[obfuscation]
enabled = true
`
	absent, err := LoadString(base, "absent", ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("a configuration without pad_to was refused: %v", err)
	}
	if got := absent.Obfuscation.PadToBytes(); got != DefaultObfuscationPadTo {
		t.Errorf("an absent pad_to applies %d bytes of padding, want the %d default", got, DefaultObfuscationPadTo)
	}

	zero, err := LoadString(base+"pad_to = 0\n", "zero", ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("pad_to = 0 was refused: %v", err)
	}
	if got := zero.Obfuscation.PadToBytes(); got != 0 {
		t.Errorf("pad_to = 0 applies %d bytes of padding, want none", got)
	}

	// The section being off means no padding whatever the key says.
	off, err := LoadString(`[client]
server_addr = "127.0.0.1:1"
auth_token = "0123456789abcdef"
`, "off", ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("a configuration without the section was refused: %v", err)
	}
	if got := off.Obfuscation.PadToBytes(); got != 0 {
		t.Errorf("a disabled [obfuscation] section applies %d bytes of padding", got)
	}
}

// vpn.require with vpn.enabled false used to pass --check and then refuse every
// session: the server has no tunnel to require, so the client that asks for one
// is turned away as well as the client that does not.
func TestRequireVPNWithoutTheTunnelIsRefused(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.VPN.Require = true
	err := cfg.Validate(RoleServer)
	if err == nil {
		t.Fatal("vpn.require with vpn.enabled false was accepted")
	}
	if !strings.Contains(err.Error(), "vpn.require is true but vpn.enabled is false") {
		t.Fatalf("the error does not name the combination: %v", err)
	}

	// The pair the operator meant: a tunnel to require.
	cfg.VPN.Enabled = true
	cfg.VPN.Address = "10.10.0.0/24"
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("a complete vpn section was refused: %v", err)
	}
}

// An ssh gateway user without a password registers neither a password rule nor,
// with no key file, a key rule: it admits nobody while the startup line claimed
// the port was open.
func TestAnSSHGatewayUserNeedsAPassword(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Server.SSHTunnelGateway = &SSHTunnelGatewayConfig{BindPort: 2200, User: "ops"}
	err := cfg.Validate(RoleServer)
	if err == nil {
		t.Fatal("an ssh gateway user without a password was accepted")
	}
	if !strings.Contains(err.Error(), "server.ssh_tunnel_gateway.user needs a password") {
		t.Fatalf("the error does not name the missing password: %v", err)
	}

	cfg.Server.SSHTunnelGateway.Password = "s3cret"
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("a complete gateway credential was refused: %v", err)
	}
}

// Four settings that used to do nothing without saying so: each one is accepted
// by --check, so the only way the operator can learn is a warning.
func TestASettingWithNoEffectIsWarnedAbout(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		toml   string
		expect string
	}{
		{
			name: "transport.key_file in a client",
			role: RoleClient,
			toml: `
[client]
server_addr = "127.0.0.1:1"
auth_token = "0123456789abcdef"

[transport]
enable_tls = true
key_file = "server.key"
`,
			expect: "transport.key_file has no effect in a client configuration",
		},
		{
			name: "remote_port beside remote_ports",
			role: RoleClient,
			toml: `
[client]
server_addr = "127.0.0.1:1"
auth_token = "0123456789abcdef"

[[proxies]]
name = "web"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 8080
remote_port = 6000
remote_ports = "7000-7002"
`,
			expect: "remote_port 6000 is ignored because remote_ports is set",
		},
		{
			name: "oidc.scope in a server",
			role: RoleServer,
			toml: `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef"

[oidc]
issuer = "https://issuer.example"
scope = "openid"
`,
			expect: "oidc.scope have no effect in a server configuration",
		},
		{
			name: "identity.allowed_keys without require_identity",
			role: RoleServer,
			toml: `
[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef"

[identity]
enabled = true
allowed_keys = ["` + dhtTestKey + `"]
`,
			expect: "identity.allowed_keys is set but identity.require_identity is false",
		},
	}
	for _, tc := range cases {
		cfg, err := LoadString(tc.toml, "embedded", ValidateOptions{Role: tc.role})
		if err != nil {
			t.Errorf("%s: load: %v", tc.name, err)
			continue
		}
		warnings := strings.Join(cfg.Warnings, "\n")
		if !strings.Contains(warnings, tc.expect) {
			t.Errorf("%s: no warning containing %q; warnings:\n%s", tc.name, tc.expect, warnings)
		}
	}
}
