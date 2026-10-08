package config

import (
	"strings"
	"testing"
)

const visitorFallbackBase = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`

func visitorFallbackBody(extra string) string {
	return visitorFallbackBase + `
[[visitors]]
name = "ssh"
type = "stcp"
server_name = "ssh"
secret_key = "0123456789abcdef0123456789abcdef"
bind_port = 8079
` + extra
}

// The fallback is dialled as written, so its address has to be one.
func TestVisitorFallbackToMustBeHostPort(t *testing.T) {
	for _, bad := range []string{"8079", "127.0.0.1", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:70000"} {
		body := visitorFallbackBody("fallback_to = \"" + bad + "\"\n")
		_, err := LoadClient(writeConfig(t, body))
		if err == nil {
			t.Errorf("LoadClient accepted fallback_to = %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "fallback_to") {
			t.Errorf("the error for %q does not name the key: %v", bad, err)
		}
	}

	body := visitorFallbackBody("fallback_to = \"127.0.0.1:8079\"\n")
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient rejected a usable fallback: %v", err)
	}
	if cfg.Visitors[0].FallbackTo != "127.0.0.1:8079" {
		t.Errorf("fallback_to is %q, want the configured address", cfg.Visitors[0].FallbackTo)
	}
}

// A negative wait would expire before the tunnel attempt starts.
func TestVisitorFallbackTimeoutRejectsNegativeValues(t *testing.T) {
	body := visitorFallbackBody("fallback_to = \"127.0.0.1:8079\"\nfallback_timeout_ms = -1\n")
	_, err := LoadClient(writeConfig(t, body))
	if err == nil {
		t.Fatal("LoadClient accepted a negative fallback_timeout_ms")
	}
	if !strings.Contains(err.Error(), "fallback_timeout_ms") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// The wait only matters when there is somewhere to fall back to, so saying so
// is a warning rather than an error.
func TestVisitorFallbackTimeoutWithoutAFallbackIsWarnedAbout(t *testing.T) {
	body := visitorFallbackBody("fallback_timeout_ms = 2000\n")
	cfg, err := LoadClient(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "fallback_timeout_ms does nothing without fallback_to") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning explains the orphan wait, got: %v", cfg.Warnings)
	}
}
