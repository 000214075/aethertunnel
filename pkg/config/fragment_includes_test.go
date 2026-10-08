package config

import (
	"strings"
	"testing"
)

// A fragment is resolved against the directory of the file that names its
// includes, and a fragment has none of its own. LoadString refuses the key for
// the same reason: silently ignoring it left the caller believing the proxies it
// names were published.
func TestAFragmentRefusesIncludes(t *testing.T) {
	_, _, _, err := LoadFragment("includes = [\"other.toml\"]\n", "store.toml", ValidateOptions{Role: RoleClient})
	if err == nil {
		t.Fatal("LoadFragment accepted an includes key")
	}
	if !strings.Contains(err.Error(), "includes") {
		t.Fatalf("error %q does not name the key that is refused", err)
	}
}

// A fragment that carries no includes still loads, which is what the store file
// and the dynamic-entry API depend on.
func TestAFragmentWithoutIncludesStillLoads(t *testing.T) {
	proxies, visitors, _, err := LoadFragment(`
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
remote_port = 7000
`, "store.toml", ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("LoadFragment: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "web" {
		t.Fatalf("LoadFragment returned %d proxies, want the one it was given", len(proxies))
	}
	if len(visitors) != 0 {
		t.Fatalf("LoadFragment returned %d visitors, want none", len(visitors))
	}
}
