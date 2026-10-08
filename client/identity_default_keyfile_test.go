package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --identity works on a configuration that carries no [identity] section at all:
// the key file's documented default is what it writes there. The default used to
// be filled in only when the section was enabled, so this command — the one that
// creates the key — was handed an empty path: nothing to read, and the new key
// had nowhere to be written.
func TestIdentityUsesTheDefaultKeyFileWithoutTheIdentitySection(t *testing.T) {
	// The default path is relative, so the command has to run in a directory the
	// test can look in.
	dir := t.TempDir()
	t.Chdir(dir)

	path := writeConfig(t, minimalConfig)
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--identity", "--config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("--identity on a configuration without [identity] exited %d (stderr: %s)", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("--identity printed no public key")
	}
	if _, err := os.Stat(filepath.Join(dir, "aethertunnel-identity.key")); err != nil {
		t.Fatalf("the default key file was not created: %v", err)
	}
}
