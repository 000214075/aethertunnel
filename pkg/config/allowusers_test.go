package config

import (
	"strings"
	"testing"
)

const allowUsersBase = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
user = "ada"
`

// The identity and the list travel with the proxy: the client sends both to the
// server at registration.
func TestUserAndAllowUsersAreRead(t *testing.T) {
	body := allowUsersBase + `
[[proxies]]
name = "ssh"
type = "stcp"
local_port = 22
secret_key = "0123456789abcdef0123456789abcdef"
allow_users = ["grace", "linus"]
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Client.User != "ada" {
		t.Errorf("client.user is %q, want ada", cfg.Client.User)
	}
	if got := cfg.Proxies[0].AllowUsers; len(got) != 2 || got[0] != "grace" || got[1] != "linus" {
		t.Errorf("allow_users is %v, want [grace linus]", got)
	}
}

// A list on a proxy anyone can reach does nothing, so it is worth saying.
func TestAllowUsersOnAPublicProxyIsWarnedAbout(t *testing.T) {
	body := allowUsersBase + `
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
remote_port = 10080
allow_users = ["grace"]
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "allow_users names the identities that may visit a private proxy") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning explains the useless list, got: %v", cfg.Warnings)
	}
}

// A duplicate is harmless but usually a copy-paste leftover.
func TestAllowUsersDuplicatesAreWarnedAbout(t *testing.T) {
	body := allowUsersBase + `
[[proxies]]
name = "ssh"
type = "stcp"
local_port = 22
secret_key = "0123456789abcdef0123456789abcdef"
allow_users = ["grace", "grace"]
`
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, `allow_users names "grace" twice`) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning names the duplicate, got: %v", cfg.Warnings)
	}
}
