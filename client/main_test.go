package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalConfig is a client configuration that loads: the command line's own
// behaviour is what these tests are about, so the tunnel it would open is kept
// as small as the validation allows.
const minimalConfig = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path
}

// --version answers without touching a configuration file, which is what makes
// it usable from a health check or a container's startup probe.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "aethertunnel-client ") {
		t.Fatalf("the version line is %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("a version request wrote to stderr: %q", stderr.String())
	}
}

// --check says so plainly, on stdout, and an operator can rely on the exit
// status in a script.
func TestCheckAcceptsAValidConfiguration(t *testing.T) {
	path := writeConfig(t, minimalConfig)
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--check", "--config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), path+" is valid") {
		t.Fatalf("the check said %q", stdout.String())
	}
}

// A rejected configuration fails the command and says why on stderr: the
// message belongs with the warnings and errors an operator is already reading.
func TestCheckRefusesABrokenConfiguration(t *testing.T) {
	path := writeConfig(t, "[client]\nserver_addr = \"127.0.0.1:7001\"\n")
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--check", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit status %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused configuration was reported on stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "auth_token") {
		t.Errorf("stderr does not name what is missing: %q", stderr.String())
	}
}

// An unknown key is a warning by default and a refusal with
// --reject-unknown-keys, and the difference is visible in the exit status.
func TestUnknownKeysAreWarnedAboutUnlessRefused(t *testing.T) {
	path := writeConfig(t, minimalConfig+"future_key = 1\n")

	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--check", "--config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 with a warning (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "future_key") {
		t.Errorf("the warning does not name the key: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run("aethertunnel-client", []string{"--check", "--reject-unknown-keys", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit status %d, want 1 when unknown keys are rejected", code)
	}
}

// --identity prints the client's public key, which is the value an operator
// hands to [identity].allowed_keys on the server; the private key is created
// where the configuration says if it is not there yet.
func TestIdentityPrintsThePublicKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "client.key")
	// The path goes into a TOML basic string, so a Windows path's backslashes have
	// to be escaped: "\U" in C:\Users is otherwise read as a unicode escape.
	body := minimalConfig + "\n[identity]\nkey_file = \"" + strings.ReplaceAll(keyFile, `\`, `\\`) + "\"\n"
	path := writeConfig(t, body)

	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--identity", "--config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr: %s)", code, stderr.String())
	}
	printed := strings.TrimSpace(stdout.String())
	if printed == "" {
		t.Fatal("--identity printed nothing")
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Errorf("the key file was not created where the configuration names it: %v", err)
	}

	// A second run prints the same identity: the key is loaded, not remade.
	stdout.Reset()
	stderr.Reset()
	if code := run("aethertunnel-client", []string{"--identity", "--config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d on the second run (stderr: %s)", code, stderr.String())
	}
	if again := strings.TrimSpace(stdout.String()); again != printed {
		t.Fatalf("the identity changed between runs: %q then %q", printed, again)
	}
}

// --discover without a DHT section explains itself instead of starting a node
// nobody asked for.
func TestDiscoverWithoutDHTExplainsItself(t *testing.T) {
	path := writeConfig(t, minimalConfig)
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--discover", "web", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit status %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "needs a [dht] section with enabled = true") {
		t.Fatalf("the refusal is %q, which does not say what is missing", stderr.String())
	}
}

// A command line that does not parse is a usage error: the status is distinct
// from a configuration that was refused, so a script can tell them apart.
func TestAnUnparsableFlagIsAUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--not-a-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit status %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not-a-flag") {
		t.Errorf("stderr does not mention the flag: %q", stderr.String())
	}
}

// The configuration file is the positional argument when --config is not given,
// which is the form the documentation shows.
func TestTheConfigurationCanBePositional(t *testing.T) {
	path := writeConfig(t, minimalConfig)
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--check", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), path+" is valid") {
		t.Fatalf("the check said %q", stdout.String())
	}
}

// Asking for the usage is not a mistake: it is written where the flag set puts
// it and the status is success, which is how the standard flag handling behaves.
func TestHelpIsNotAnError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-client", []string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage: aethertunnel-client") {
		t.Errorf("the usage was not written: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "-discover") {
		t.Errorf("the usage does not list the flags: %q", stderr.String())
	}
}
