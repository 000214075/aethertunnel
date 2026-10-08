package config

import (
	"strings"
	"testing"
)

const metricsBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`

// The endpoint has to exist before it can be served on an address of its own:
// metrics.port without metrics.enabled would bind a listener that answers nothing.
func TestMetricsPortNeedsTheEndpointEnabled(t *testing.T) {
	body := metricsBase + `
[metrics]
port = 9100
`
	_, err := LoadServer(writeConfig(t, body))
	if err == nil {
		t.Fatal("metrics.port without metrics.enabled was accepted")
	}
	if !strings.Contains(err.Error(), "metrics.port needs metrics.enabled") {
		t.Fatalf("the error does not explain the pair: %v", err)
	}
}

// The metrics listener takes part in the address-collision check, so two
// operators' keys cannot be pointed at one port.
func TestMetricsPortCollidesWithTheDashboardPort(t *testing.T) {
	body := metricsBase + `
[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = 9100

[metrics]
enabled = true
bind_addr = "127.0.0.1"
port = 9100
`
	_, err := LoadServer(writeConfig(t, body))
	if err == nil {
		t.Fatal("two listeners on one address were accepted")
	}
	if !strings.Contains(err.Error(), "metrics.port") || !strings.Contains(err.Error(), "dashboard.port") {
		t.Fatalf("the error does not name both keys: %v", err)
	}
}

// A metrics listener on anything but loopback is readable by anyone who can reach
// the port, so it says so when no token is configured — the dashboard's rule.
func TestMetricsOnANonLoopbackAddressWithoutATokenIsWarnedAbout(t *testing.T) {
	body := metricsBase + `
[metrics]
enabled = true
bind_addr = "0.0.0.0"
port = 9100
`
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "metrics.bind_addr is not a loopback address") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning about the open metrics listener, warnings were %v", cfg.Warnings)
	}
	if cfg.MetricsAddr() != "0.0.0.0:9100" {
		t.Fatalf("MetricsAddr is %q", cfg.MetricsAddr())
	}
}

// The range is checked like every other port in the file, and 0 keeps the endpoint
// where it was: on the dashboard listener.
func TestMetricsPortRange(t *testing.T) {
	body := metricsBase + `
[metrics]
enabled = true
port = 70000
`
	if _, err := LoadServer(writeConfig(t, body)); err == nil {
		t.Fatal("an out-of-range metrics.port was accepted")
	}
}

// The endpoint is served on the dashboard's listener or on an address of its own.
// With neither, metrics.enabled switches on something no listener answers, which
// is the same contradiction as metrics.port without metrics.enabled.
func TestMetricsEnabledWithoutAListenerIsWarnedAbout(t *testing.T) {
	body := metricsBase + `
[metrics]
enabled = true
`
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "metrics.enabled is set but nothing serves the endpoint") {
		t.Fatalf("expected a warning about the listener-less endpoint, got %v", cfg.Warnings)
	}
}

// Enabling the dashboard gives the endpoint somewhere to live, so the warning is
// gone.
func TestMetricsEnabledOnTheDashboardListenerIsNotWarnedAbout(t *testing.T) {
	body := metricsBase + `
[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = 9100

[metrics]
enabled = true
`
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if strings.Contains(strings.Join(cfg.Warnings, "\n"), "metrics.enabled is set but nothing serves") {
		t.Fatalf("a served endpoint was warned about: %v", cfg.Warnings)
	}
}
