package config

import (
	"strings"
	"testing"
)

// client.pool_count is offered up to the number of connections the server parks,
// and no further: a larger pool would ask for connections the server refuses, and
// the reason would be a limit nothing warned about.
func TestPoolCountIsBoundedByWhatTheServerParks(t *testing.T) {
	body := enabledBase + "\npool_count = " + itoa(MaxPoolCount+1) + "\n"
	if _, err := LoadClient(writeConfig(t, body)); err == nil {
		t.Fatal("a pool larger than the server parks was accepted")
	} else if !strings.Contains(err.Error(), "client.pool_count cannot exceed") {
		t.Fatalf("the error does not explain the limit: %v", err)
	}

	atCap := enabledBase + "\npool_count = " + itoa(MaxPoolCount) + "\n"
	cfg, err := LoadClient(writeConfig(t, atCap))
	if err != nil {
		t.Fatalf("a pool of exactly the cap was refused: %v", err)
	}
	if cfg.Client.PoolCount != MaxPoolCount {
		t.Fatalf("pool_count read back as %d", cfg.Client.PoolCount)
	}
}

// The pool is the client's half of the arrangement — it opens the connections and
// the server parks them — so a server configuration carrying the key is a mistake
// worth naming rather than a setting that quietly does nothing.
func TestPoolCountInAServerConfigurationIsWarnedAbout(t *testing.T) {
	body := `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"

[client]
pool_count = 4
`
	cfg, err := LoadServer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "client.pool_count has no effect in a server configuration") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning about the key, warnings were %v", cfg.Warnings)
	}
}

// itoa keeps the test bodies readable without pulling strconv in for two calls.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
