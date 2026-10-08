package config

import (
	"strings"
	"testing"
)

// A configuration pasted from frp fails as unknown keys, which on its own says
// only that something is wrong. The names frp uses are well known across its
// releases, so the message says what to write instead.
func TestAnFRPConfigurationIsToldWhatToWriteInstead(t *testing.T) {
	body := `
[common]
serverAddr = "example.com"
serverPort = 7000
token = "0123456789abcdef0123456789abcdef"
loginFailExit = true
transport.tls.enable = true
log.maxDays = 3

[[proxies]]
name = "ssh"
type = "tcp"
localIP = "127.0.0.1"
localPort = 22
remotePort = 6000
customDomains = ["a.example.com"]
`
	_, err := LoadString(body, "frpc.toml", ValidateOptions{Role: RoleClient, RejectUnknownKeys: true})
	if err == nil {
		t.Fatal("an frp configuration was accepted as it is")
	}
	for _, want := range []string{
		"serverAddr → client.server_addr",
		"token → auth_token",
		"loginFailExit → client.login_fail_exit",
		"transport.tls.enable → transport.enable_tls",
		"log.maxDays → log.max_days",
		"localIP → local_ip",
		"customDomains → domains",
		"common → [client] or [server]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %v", want, err)
		}
	}
}

// A name frp has and this program does not gets an honest answer instead of a
// suggestion that would not work either.
func TestAnFRPFeatureThisProgramDoesNotHaveSaysSo(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
kcpBindPort = 7000
`
	_, err := LoadString(body, "frps.toml", ValidateOptions{Role: RoleServer, RejectUnknownKeys: true})
	if err == nil {
		t.Fatal("kcpBindPort was accepted")
	}
	if !strings.Contains(err.Error(), "kcpBindPort names a feature this program does not have") {
		t.Fatalf("the message does not explain the missing feature: %v", err)
	}
}

// A key nothing maps keeps the plain unknown-key report: the loader does not
// invent a guess it cannot stand behind.
func TestAnUnrelatedUnknownKeyGetsNoFRPGuess(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
made_up_setting = 1
`
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "made_up_setting") {
		t.Fatalf("the unknown key is not named: %v", cfg.Warnings)
	}
	if strings.Contains(joined, "frp names some of these differently") {
		t.Fatalf("an frp guess was invented for a key frp does not have: %v", cfg.Warnings)
	}
}
