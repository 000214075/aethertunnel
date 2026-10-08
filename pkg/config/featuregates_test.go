package config

import (
	"strings"
	"testing"
)

// feature_gates turns capabilities off. A name this program does not know is a
// mistake; frp's one gate is understood but changes nothing, and a section
// written for the other role is a warning rather than a silent no-op.
func TestFeatureGatesValidation(t *testing.T) {
	t.Run("unknown name is an error", func(t *testing.T) {
		body := metricsBase + `
[server.feature_gates]
SomethingElse = true
`
		_, err := LoadServer(writeConfig(t, body))
		if err == nil {
			t.Fatal("an unknown gate was accepted")
		}
		for _, want := range []string{"SomethingElse", "Snark", "WebRTC", "VirtualNet"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
	})

	t.Run("frp's VirtualNet is known and changes nothing", func(t *testing.T) {
		body := metricsBase + `
[server.feature_gates]
VirtualNet = true
`
		cfg, err := LoadServer(writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadServer: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "not reproduced") {
			t.Fatalf("no warning about the gate that changes nothing: %v", cfg.Warnings)
		}
	})

	t.Run("a section for the other role is a warning", func(t *testing.T) {
		body := metricsBase + `
[client.feature_gates]
WebRTC = false
`
		cfg, err := LoadServer(writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadServer: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "client.feature_gates in a server configuration") {
			t.Fatalf("no warning about the misplaced section: %v", cfg.Warnings)
		}
	})
}

// The client's own gates are checked at configuration time: an entry that needs a
// turned-off capability is refused, and the same entry is fine without the gate.
func TestClientGatesRefuseEntriesThatNeedThem(t *testing.T) {
	base := `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "private"
type = "stcp"
local_port = 22
secret_key = "0123456789abcdef0123456789abcdef"
auth_method = "snark"

[[visitors]]
name = "v"
type = "stcp"
server_name = "private"
secret_key = "0123456789abcdef0123456789abcdef"
bind_port = 8079
transport = "webrtc"
`
	body := base + `
[client.feature_gates]
Snark = false
WebRTC = false
`
	_, err := LoadClient(writeConfig(t, body))
	if err == nil {
		t.Fatal("a gated client configuration was accepted")
	}
	if !strings.Contains(err.Error(), "feature_gates.Snark") {
		t.Errorf("the error does not name the snark gate: %v", err)
	}
	if !strings.Contains(err.Error(), "feature_gates.WebRTC") {
		t.Errorf("the error does not name the webrtc gate: %v", err)
	}

	if _, err := LoadClient(writeConfig(t, base)); err != nil {
		t.Fatalf("the same entries were refused without the gates: %v", err)
	}
}
