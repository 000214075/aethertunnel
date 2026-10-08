package config

import (
	"strings"
	"testing"
)

const enabledBase = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`

// An entry is on unless the file says otherwise, and a file that says so can be
// read back: enabled = false is a value, not an absence.
func TestEnabledDefaultsToOnAndReadsOff(t *testing.T) {
	body := enabledBase + `
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080

[[proxies]]
name = "ssh"
type = "tcp"
local_port = 22
remote_port = 10022
enabled = false
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !cfg.Proxies[0].IsEnabled() {
		t.Error("a proxy without the key is not enabled")
	}
	if cfg.Proxies[1].IsEnabled() {
		t.Error("a proxy with enabled = false is enabled")
	}
}

// A visitor carries the same switch, and it reads the same way.
func TestVisitorEnabledIsRead(t *testing.T) {
	body := enabledBase + `
[[visitors]]
name = "ssh"
type = "stcp"
server_name = "ssh"
secret_key = "0123456789abcdef0123456789abcdef"
bind_port = 8079

[[visitors]]
name = "off"
type = "stcp"
server_name = "ssh"
secret_key = "0123456789abcdef0123456789abcdef"
bind_port = 8080
enabled = false
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if !cfg.Visitors[0].IsEnabled() || cfg.Visitors[1].IsEnabled() {
		t.Errorf("visitors read as enabled=%v and enabled=%v, want true and false",
			cfg.Visitors[0].IsEnabled(), cfg.Visitors[1].IsEnabled())
	}
}

// Naming a disabled entry in client.start asks for something that cannot start,
// which is the same mistake as naming nothing at all.
func TestStartNamingADisabledEntryIsWarnedAbout(t *testing.T) {
	body := enabledBase + `start = ["ssh"]

[[proxies]]
name = "web"
type = "tcp"
local_port = 8080

[[proxies]]
name = "ssh"
type = "tcp"
local_port = 22
remote_port = 10022
enabled = false
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, `client.start names "ssh", which carries enabled = false`) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning explains the disabled start entry, got: %v", cfg.Warnings)
	}
	// The name is defined, so the "nothing defines it" warning must not fire.
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "which no proxy or visitor defines") {
			t.Errorf("a defined but disabled entry was reported as undefined: %s", warning)
		}
	}
}

// Labels ride along with the proxy and are read back as written.
func TestAnnotationsAreRead(t *testing.T) {
	body := enabledBase + `
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
remote_port = 10080

[proxies.annotations]
env = "prod"
ticket = "OPS-4711"
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	got := cfg.Proxies[0].Annotations
	if got["env"] != "prod" || got["ticket"] != "OPS-4711" || len(got) != 2 {
		t.Fatalf("annotations read as %v, want the two labels", got)
	}
}

// A client configuration that carries a server-only section says so rather than
// carrying it silently: the panel, the metrics endpoint and the audit log are
// all written by a server, so an operator who wrote one here has either mixed
// up two files or is reading the wrong one.
func TestServerOnlySectionsInAClientConfigurationAreWarnedAbout(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"dashboard", "dashboard.enabled = true", "dashboard.enabled has no effect in a client configuration"},
		{"metrics", "metrics.enabled = true", "metrics.enabled has no effect in a client configuration"},
		{"audit", "audit.enabled = true", "audit.enabled has no effect in a client configuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := enabledBase + "\n[" + tc.name + "]\nenabled = true\n"
			cfg, err := LoadClient(writeConfig(t, body))
			if err != nil {
				t.Fatalf("LoadClient: %v", err)
			}
			var warned bool
			for _, warning := range cfg.Warnings {
				if strings.Contains(warning, tc.want) {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning explains the unused section, got: %v", cfg.Warnings)
			}
		})
	}
}
