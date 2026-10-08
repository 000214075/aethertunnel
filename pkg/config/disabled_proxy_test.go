package config

import (
	"strings"
	"testing"
)

// A proxy with enabled = false publishes nothing, so it does not hold its
// remote port. Counting it refused a legitimate port handover through the
// management API, and the startup trim then deleted the entry that actually
// runs — the visitor side already skips stopped entries for the same reason.
func TestADisabledProxyDoesNotHoldItsRemotePort(t *testing.T) {
	off := false
	cfg := clientBase()
	cfg.Proxies = []ProxyConfig{
		{Name: "live", Type: ProxyTypeTCP, LocalPort: 8000, RemotePort: 4000},
		{Name: "off", Type: ProxyTypeTCP, LocalPort: 8001, RemotePort: 4000, Enabled: &off},
	}
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a disabled proxy was counted as holding port 4000: %v", err)
	}

	// Two proxies that both run still conflict.
	on := true
	cfg.Proxies[1].Enabled = &on
	err := cfg.Validate(RoleClient)
	if err == nil || !strings.Contains(err.Error(), "both ask for tcp port 4000") {
		t.Fatalf("two enabled proxies on one port were accepted: %v", err)
	}
}

// A gateway that lists a key file and a user is complete: the runtime admits the
// keys from that file, and only a password needs a user to check it against. The
// check demanded a password anyway, so --check refused a configuration the same
// binary would have served.
func TestAnSSHGatewayWithAKeyFileAndAUserNeedsNoPassword(t *testing.T) {
	cfg := serverConfigForPortTests()
	cfg.Server.SSHTunnelGateway = &SSHTunnelGatewayConfig{
		BindPort:           2200,
		User:               "ops",
		AuthorizedKeysFile: "authorized_keys",
	}
	if err := cfg.Validate(RoleServer); err != nil {
		t.Fatalf("an ssh gateway with a key file was refused for a missing password: %v", err)
	}
}

// The datagram path does limit by bandwidth: prepareStream parses the cap for a
// datagram proxy, serveDatagrams wraps the local UDP socket in the same limiter,
// and the manual says so. The warning that claimed the opposite told an operator
// to delete a cap that was working, so it is gone.
func TestADatagramProxysBandwidthIsNotWarnedAboutAsIneffective(t *testing.T) {
	cfg := clientBase()
	cfg.Proxies = []ProxyConfig{{
		Name: "dgram", Type: ProxyTypeUDP, LocalPort: 8000, RemotePort: 4000, Bandwidth: "1MB",
	}}
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a datagram proxy with a bandwidth cap was refused: %v", err)
	}
	if warnings := strings.Join(cfg.Warnings, "\n"); strings.Contains(warnings, "bandwidth") {
		t.Fatalf("a datagram proxy's working bandwidth cap is still warned about: %v", cfg.Warnings)
	}

	// use_compression is the neighbouring case and is different: it really does
	// gain nothing on datagrams, so its warning stays.
	cfg.Proxies[0].UseCompression = true
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if warnings := strings.Join(cfg.Warnings, "\n"); !strings.Contains(warnings, "use_compression") {
		t.Fatalf("the use_compression warning went with it: %v", cfg.Warnings)
	}
}

// A single number's range does not depend on the section's enabled flag: that is
// the rule the manual states for every other port key, and the one dashboard.port
// and metrics.port already follow. A client.admin.port that could never be used
// was accepted as long as the management API was switched off.
func TestAClientAdminPortIsRangeCheckedEvenWhenTheAPIIsOff(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		cfg := clientBase()
		cfg.Client.Admin = &AdminConfig{Port: port}
		err := cfg.Validate(RoleClient)
		if err == nil || !strings.Contains(err.Error(), "client.admin.port must be 0-65535") {
			t.Fatalf("client.admin.port %d was accepted while the API is off: %v", port, err)
		}
	}

	// A section that is present but switched off, with no port written, is not an
	// error: Port == 0 means "not set", and only "enabled with no usable port" is
	// the problem.
	cfg := clientBase()
	cfg.Client.Admin = &AdminConfig{}
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a disabled [client.admin] with no port was refused: %v", err)
	}

	// Enabled with no usable port still is one.
	cfg = clientBase()
	cfg.Client.Admin = &AdminConfig{Enabled: true}
	err := cfg.Validate(RoleClient)
	if err == nil || !strings.Contains(err.Error(), "client.admin.enabled is true but client.admin.port 0") {
		t.Fatalf("an enabled admin API with no port was accepted: %v", err)
	}
}
