package config

import (
	"strings"
	"testing"
)

const startClientBase = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`

// client.start keeps the proxies and visitors it names and leaves the others
// defined but stopped, which is frp's `start`.
func TestStartSelectsWhatRuns(t *testing.T) {
	body := startClientBase + `start = ["web", "dns-visitor"]

[[proxies]]
name = "web"
type = "tcp"
local_port = 8080

[[proxies]]
name = "ssh"
type = "tcp"
local_port = 22
remote_port = 10022

[[visitors]]
name = "dns-visitor"
type = "sudp"
server_name = "dns"
secret_key = "0123456789abcdef0123456789abcdef"
bind_port = 5353
`
	path := writeConfig(t, body)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	set := cfg.Client.StartSet()
	if len(set) != 2 {
		t.Fatalf("client.start selects %d names, want 2", len(set))
	}
	for _, name := range []string{"web", "dns-visitor"} {
		if _, ok := set[name]; !ok {
			t.Errorf("client.start does not select %q", name)
		}
	}
	if missing := cfg.missingStartNames(); len(missing) != 0 {
		t.Errorf("every start name is defined, but %v was reported missing", missing)
	}
}

// A start list that names nothing gets a warning rather than an error: the rest
// of the file still runs, and the operator is told what the typo cost.
func TestStartNamesNothingAreWarnedAbout(t *testing.T) {
	body := startClientBase + `start = ["web", "ghost", "ghost"]

[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
`
	path := writeConfig(t, body)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, `client.start names "ghost"`) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names the unmatched start entry, got: %v", cfg.Warnings)
	}
	if missing := cfg.missingStartNames(); len(missing) != 1 || missing[0] != "ghost" {
		t.Errorf("missingStartNames() = %v, want [ghost] once", missing)
	}
}

// An empty start list means everything starts, so StartSet is nil and no
// filtering happens at all.
func TestAnEmptyStartListSelectsEverything(t *testing.T) {
	path := writeConfig(t, startClientBase+`
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if set := cfg.Client.StartSet(); set != nil {
		t.Fatalf("an empty client.start produced a selection of %v, want nil", set)
	}
	if missing := cfg.missingStartNames(); len(missing) != 0 {
		t.Errorf("an empty client.start reported %v missing", missing)
	}
}
