package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fragment is [[proxies]] and [[visitors]] only. Any other section it
// carries decodes cleanly and is then dropped by the merge, so the load has
// to say so: settings the operator believes they made must not disappear
// without a word.
func TestAFragmentCarryingOtherSectionsWarnsAboutThem(t *testing.T) {
	dir := t.TempDir()
	fragment := filepath.Join(dir, "frag.toml")
	body := `
[client]
server_addr = "10.0.0.1:7000"

[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
`
	if err := os.WriteFile(fragment, []byte(body), 0o600); err != nil {
		t.Fatalf("write the fragment: %v", err)
	}
	main := filepath.Join(dir, "main.toml")
	mainBody := `
includes = ["frag.toml"]

[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(main, []byte(mainBody), 0o600); err != nil {
		t.Fatalf("write the main file: %v", err)
	}

	cfg, err := Load(main, ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("Load rejected the configuration: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "frag.toml") && strings.Contains(warning, "only [[proxies]] and [[visitors]]") {
			return
		}
	}
	t.Fatalf("a fragment carrying [client] produced no warning about the ignored sections; warnings: %v", cfg.Warnings)
}

// A fragment that carries only the two consumed sections stays silent: the
// warning names sections that are lost, and a well-formed fragment loses
// nothing.
func TestAWellFormedFragmentStaysSilent(t *testing.T) {
	dir := t.TempDir()
	fragment := filepath.Join(dir, "frag.toml")
	body := `
[[proxies]]
name = "web"
type = "tcp"
local_port = 8080
`
	if err := os.WriteFile(fragment, []byte(body), 0o600); err != nil {
		t.Fatalf("write the fragment: %v", err)
	}
	main := filepath.Join(dir, "main.toml")
	mainBody := `
includes = ["frag.toml"]

[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(main, []byte(mainBody), 0o600); err != nil {
		t.Fatalf("write the main file: %v", err)
	}

	cfg, err := Load(main, ValidateOptions{Role: RoleClient})
	if err != nil {
		t.Fatalf("Load rejected the configuration: %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "only [[proxies]] and [[visitors]]") {
			t.Fatalf("a well-formed fragment was warned about: %v", cfg.Warnings)
		}
	}
	if len(cfg.Proxies) != 1 || cfg.Proxies[0].Name != "web" {
		t.Fatalf("the fragment's proxy did not survive the merge: %v", cfg.Proxies)
	}
}
