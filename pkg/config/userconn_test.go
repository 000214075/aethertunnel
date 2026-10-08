package config

import (
	"strconv"
	"strings"
	"testing"
)

const userConnBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`

// The key is a duration in seconds; a negative one would mean a bound that
// expires before the request is sent.
func TestUserConnTimeoutRejectsNegativeValues(t *testing.T) {
	path := writeConfig(t, userConnBase+"user_conn_timeout_seconds = -1\n")
	_, err := LoadServer(path)
	if err == nil {
		t.Fatal("LoadServer accepted a negative server.user_conn_timeout_seconds")
	}
	if !strings.Contains(err.Error(), "server.user_conn_timeout_seconds") {
		t.Errorf("error %q does not name the offending key", err.Error())
	}
}

// Zero keeps the dial timeout in charge, which is the wait the server had
// before this key existed.
func TestUserConnTimeoutAcceptsZeroAndAValue(t *testing.T) {
	for _, value := range []int{0, 10} {
		path := writeConfig(t, userConnBase+"user_conn_timeout_seconds = "+strconv.Itoa(value)+"\n")
		cfg, err := LoadServer(path)
		if err != nil {
			t.Fatalf("LoadServer rejected %d: %v", value, err)
		}
		if cfg.Server.UserConnTimeoutSeconds != value {
			t.Errorf("server.user_conn_timeout_seconds is %d, want %d", cfg.Server.UserConnTimeoutSeconds, value)
		}
	}
}

// The resolver has to be reachable as written: a bare address would look fine
// and then be sent to a port nobody named.
func TestDNSServerMustBeHostPort(t *testing.T) {
	const base = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`
	for _, bad := range []string{"10.0.0.53", "10.0.0.53:", "10.0.0.53:dns", "10.0.0.53:0", "10.0.0.53:70000"} {
		body := base + "dns_server = \"" + bad + "\"\n"
		if _, err := LoadClient(writeConfig(t, body)); err == nil {
			t.Errorf("LoadClient accepted client.dns_server = %q", bad)
		}
	}
	for _, good := range []string{"10.0.0.53:53", "127.0.0.1:5353", "[::1]:53"} {
		body := base + "dns_server = \"" + good + "\"\n"
		cfg, err := LoadClient(writeConfig(t, body))
		if err != nil {
			t.Errorf("LoadClient rejected client.dns_server = %q: %v", good, err)
			continue
		}
		if cfg.Client.DNSServer != good {
			t.Errorf("client.dns_server is %q, want %q", cfg.Client.DNSServer, good)
		}
	}
}
