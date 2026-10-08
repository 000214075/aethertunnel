package config

import (
	"strings"
	"testing"
)

// A client file that turns on several server-only sections is told about each of
// them. These warnings were one switch statement, so only the first matching
// case ran: an operator who removed the section it named ran again and only then
// learned about the next one, one start at a time.
func TestAClientFileEnablingEveryServerOnlySectionIsToldAboutEachOne(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "a-long-enough-test-token"
	cfg.Ledger.Enabled = true
	cfg.Dashboard.Enabled = true
	cfg.Metrics.Enabled = true
	cfg.Audit.Enabled = true

	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a client configuration with server-only sections has to be accepted with warnings, got %v", err)
	}

	for _, section := range []string{"ledger.enabled", "dashboard.enabled", "metrics.enabled", "audit.enabled"} {
		found := false
		for _, warning := range cfg.Warnings {
			if strings.HasPrefix(warning, section+" has no effect in a client configuration") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no warning names %s; the warnings are %q", section, cfg.Warnings)
		}
	}
}

// The server side of the same block is untouched: an enabled ledger without a
// path and without a signing key is still two problems, and it is not turned into
// the client warning by the rewrite.
func TestAnEnabledLedgerOnTheServerStillNeedsItsPathAndKey(t *testing.T) {
	cfg := &Config{}
	cfg.Ledger.Enabled = true

	err := cfg.Validate(RoleServer)
	if err == nil {
		t.Fatal("an enabled ledger with neither path nor signing key was accepted")
	}
	for _, want := range []string{"ledger.path is empty", "ledger.signing_key_file is empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// identity.key_file has a documented default that does not depend on [identity]
// being enabled, because the command that creates the file --identity has to be
// usable on a default configuration. Filling it only when the section was on left
// the key file empty there, and --identity then handed LoadIdentity an empty
// path: no file to read, and the generated key had nowhere to be written.
func TestTheIdentityKeyFileDefaultDoesNotNeedTheIdentitySection(t *testing.T) {
	path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("a client configuration without [identity] was refused: %v", err)
	}
	if cfg.Identity.KeyFile != "aethertunnel-identity.key" {
		t.Fatalf("identity.key_file is %q in a default configuration, want the documented default", cfg.Identity.KeyFile)
	}
}

// The use_encryption warning beside [encryption] has to describe what the code
// does with the per-proxy layer: a session that already has a cipher does not add
// it, so the warning has to say the layer is skipped. The wording it replaced
// claimed the layer "would sit inside" the session's, which is the opposite of
// what client.go does with request.Encrypted.
func TestTheUseEncryptionWarningSaysTheExtraLayerIsNotAdded(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7001"
	cfg.Client.AuthToken = "0123456789abcdef0123456789abcdef"
	cfg.Encryption.Enabled = true
	cfg.Proxies = []ProxyConfig{{
		Name: "web", Type: ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: 8080, UseEncryption: true,
	}}
	cfg.applyDefaults()
	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("validate: %v", err)
	}

	warning := warningAbout(t, cfg.Warnings, `proxy "web" sets use_encryption`)
	if !strings.Contains(warning, "is not added") {
		t.Fatalf("the warning does not say the per-proxy layer is skipped: %q", warning)
	}
	if strings.Contains(warning, "would sit inside") {
		t.Fatalf("the warning still claims the layer is added inside the session's: %q", warning)
	}
}

// The datagram use_encryption warning has two cases and they say opposite
// things. With [encryption] on, the session cipher is all the datagrams have;
// with it off they have no encryption at all, and the old single wording told the
// operator of that deployment the traffic was "protected by [encryption] alone".
func TestTheDatagramUseEncryptionWarningSaysWhatIsMissing(t *testing.T) {
	build := func(encryption bool) []string {
		cfg := &Config{}
		cfg.Client.ServerAddr = "127.0.0.1:7001"
		cfg.Client.AuthToken = "0123456789abcdef0123456789abcdef"
		cfg.Encryption.Enabled = encryption
		cfg.Proxies = []ProxyConfig{{
			Name: "dns", Type: ProxyTypeUDP, LocalIP: "127.0.0.1", LocalPort: 5353, UseEncryption: true,
		}}
		cfg.applyDefaults()
		if err := cfg.Validate(RoleClient); err != nil {
			t.Fatalf("validate (encryption=%v): %v", encryption, err)
		}
		return cfg.Warnings
	}

	withSessionCipher := warningAbout(t, build(true), `proxy "dns": use_encryption wraps the relayed byte stream`)
	if !strings.Contains(withSessionCipher, "protected by [encryption] alone") {
		t.Fatalf("with [encryption] on the warning does not name the session cipher: %q", withSessionCipher)
	}

	withoutCipher := warningAbout(t, build(false), `proxy "dns": use_encryption wraps the relayed byte stream`)
	if !strings.Contains(withoutCipher, "without any encryption") {
		t.Fatalf("with [encryption] off the warning does not say the datagrams are in the clear: %q", withoutCipher)
	}
}

// warningAbout returns the one warning that starts with prefix.
func warningAbout(t *testing.T, warnings []string, prefix string) string {
	t.Helper()
	for _, warning := range warnings {
		if strings.HasPrefix(warning, prefix) {
			return warning
		}
	}
	t.Fatalf("no warning starts with %q; the warnings are %q", prefix, warnings)
	return ""
}
