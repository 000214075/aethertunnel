package config

import (
	"strings"
	"testing"
)

// The runtime parses these lists with strings.TrimSpace before net.ParseCIDR
// (pkg/server/acl.go, ban.go and group.go), so validation has to trim the same
// way: otherwise the same spelling is accepted at login time and refused at load
// time, and which of the two happens depends on the role that read the file.
func TestACIDRPaddedWithWhitespaceIsAcceptedForBothRoles(t *testing.T) {
	data := `[server]
bind_addr = "127.0.0.1"
bind_port = 7000
auth_token = "0123456789abcdef"
allow_cidrs = [" 10.0.0.0/8"]
deny_cidrs = ["10.0.0.0/8 "]
ban_ignore_cidrs = ["	10.0.0.0/8"]

[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef"

[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
remote_port = 18080
allow_cidrs = [" 10.0.0.0/8"]
deny_cidrs = ["10.0.0.0/8 "]
`
	for _, role := range []string{RoleClient, RoleServer} {
		_, err := LoadString(data, "cidr.toml", ValidateOptions{Role: role})
		if err != nil && strings.Contains(err.Error(), "is not a CIDR") {
			t.Fatalf("a CIDR padded with whitespace was refused for the %s role: %v", role, err)
		}
	}
}
