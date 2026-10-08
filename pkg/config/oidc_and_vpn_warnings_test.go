package config

import (
	"strings"
	"testing"
)

// A client [oidc] block is the keys the token request consumes. audience is one of
// them and it is only ever read by that request, which happens only when
// token_endpoint_url is set — so a file that writes audience alone is a block that
// does nothing, and it used to be entirely silent while the same mistake with scope
// is refused. The refusal names the key that has to be added.
func TestAClientOIDCBlockWithOnlyAnAudienceIsRefused(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "a-long-enough-test-token"
	cfg.OIDC = &OIDCConfig{Audience: "aethertunnel"}

	err := cfg.Validate(RoleClient)
	if err == nil {
		t.Fatal("a client oidc block with only an audience was accepted")
	}
	if !strings.Contains(err.Error(), "oidc.token_endpoint_url is required for an oidc client") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// The shared shape is untouched: a block that names both ends' keys keeps the
// server-side warnings it already had, rather than turning into a hard error.
func TestAClientOIDCBlockWithBothSidesNamesStillOnlyWarns(t *testing.T) {
	cfg := &Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "a-long-enough-test-token"
	cfg.OIDC = &OIDCConfig{
		TokenEndpointURL: "https://idp.example/token",
		ClientID:         "client",
		ClientSecret:     "s3cret",
		Audience:         "aethertunnel",
		Issuer:           "https://idp.example",
	}

	if err := cfg.Validate(RoleClient); err != nil {
		t.Fatalf("a client oidc block that names both sides has to be accepted with warnings, got %v", err)
	}
	warned := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "oidc.issuer") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the server-side oidc keys are no longer warned about: %q", cfg.Warnings)
	}
}

// vpn.address does nothing in a client configuration whether or not the section is
// switched on, so the warning belongs to the file rather than to the gate: a client
// that wrote it under enabled = false had the same line to delete and was told
// nothing. The server side keeps its own checks behind the gate, because with the
// tunnel off there is no subnet to validate.
func TestAClientVPNAddressIsWarnedAboutHoweverTheSectionIsSwitched(t *testing.T) {
	warnsAddress := func(t *testing.T, cfg *Config) bool {
		t.Helper()
		cfg.Client.ServerAddr = "127.0.0.1:7000"
		cfg.Client.AuthToken = "a-long-enough-test-token"
		if err := cfg.Validate(RoleClient); err != nil {
			t.Fatalf("client config: %v", err)
		}
		for _, warning := range cfg.Warnings {
			if strings.HasPrefix(warning, "vpn.address has no effect in a client configuration") {
				return true
			}
		}
		return false
	}

	disabled := &Config{}
	disabled.VPN.Address = "10.10.0.0/24"
	if !warnsAddress(t, disabled) {
		t.Fatal("a client with vpn.address and vpn.enabled = false was not warned")
	}

	enabled := &Config{}
	enabled.VPN.Enabled = true
	enabled.VPN.Address = "10.10.0.0/24"
	if !warnsAddress(t, enabled) {
		t.Fatal("a client with vpn.address and vpn.enabled = true was not warned")
	}

	unset := &Config{}
	if warnsAddress(t, unset) {
		t.Fatal("a client without vpn.address was warned about it")
	}
}
