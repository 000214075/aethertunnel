package config

import (
	"strings"
	"testing"
)

// A negative vhost_http_timeout has no meaning in seconds and is rejected.
func TestVhostHTTPTimeoutRejectsNegativeValues(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
vhost_http_timeout = -1
`
	path := writeConfig(t, body)
	_, err := LoadServer(path)
	if err == nil {
		t.Fatal("LoadServer accepted a negative server.vhost_http_timeout")
	}
	if !strings.Contains(err.Error(), "server.vhost_http_timeout cannot be negative") {
		t.Errorf("error %q does not name the offending key", err.Error())
	}
}

// Zero keeps the previous behaviour: the dial timeout bounds the wait.
func TestVhostHTTPTimeoutAcceptsZero(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
vhost_http_timeout = 0
`
	path := writeConfig(t, body)
	if _, err := LoadServer(path); err != nil {
		t.Errorf("LoadServer rejected server.vhost_http_timeout = 0: %v", err)
	}
}
