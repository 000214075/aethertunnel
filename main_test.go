package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/ledger"
)

// newTestLedger writes a small ledger file and returns its path and public key.
func newTestLedger(t *testing.T, entries int) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")

	key, err := ledger.NewSigningKey()
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	l, err := ledger.New(path, key)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	for i := 0; i < entries; i++ {
		if _, err := l.Append("client-"+string(rune('a'+i)), "ssh", int64(10*(i+1)), int64(20*(i+1))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path, hex.EncodeToString(key.PublicKey())
}

// ledgerProof runs the proof helper with its output captured, which the helper
// takes as an argument since the refactor: no process-wide redirection, so these
// tests cannot interfere with each other.
func ledgerProof(t *testing.T, path string, index int) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := writeLedgerProof(&stdout, &stderr, path, index)
	return stdout.String(), err
}

// A proof is the chain up to one entry, in the ledger's own format, so a holder of the
// public key can check that entry's inclusion without the rest of the file.
func TestLedgerProofVerifiesOnItsOwn(t *testing.T) {
	path, pubHex := newTestLedger(t, 4)

	out, err := ledgerProof(t, path, 2)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("the proof has %d lines, want entries 0 through 2", len(lines))
	}

	// The proof must carry no signature-free bytes and must verify under the public key
	// alone, which is what an auditor has.
	chain, err := ledger.ReadFile(path)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	proof, err := ledger.ReadFile(writeToTemp(t, out))
	if err != nil {
		t.Fatalf("the proof is not readable as a ledger: %v", err)
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	verified, err := ledger.Verify(proof, pub)
	if err != nil {
		t.Fatalf("the proof does not verify: %v", err)
	}
	if verified != 3 {
		t.Fatalf("verified %d entries, want 3", verified)
	}
	if proof[len(proof)-1].Hash != chain[2].Hash {
		t.Fatalf("the proof ends at %s, want the chain's entry 2 (%s)",
			proof[len(proof)-1].Hash, chain[2].Hash)
	}
	if len(proof) >= len(chain) {
		t.Fatalf("the proof (%d entries) is not shorter than the chain (%d)", len(proof), len(chain))
	}

	// A proof that has been edited must not verify: the entry commits to its own bytes.
	edited := chain[:3]
	edited[0].BytesIn += 1000
	if _, err := ledger.Verify(edited, pub); err == nil {
		t.Fatal("an entry with an inflated byte count verified")
	}
}

func TestLedgerProofRefusesAnImpossibleIndex(t *testing.T) {
	path, _ := newTestLedger(t, 2)

	if _, err := ledgerProof(t, path, -1); err == nil {
		t.Error("a proof without an index was accepted")
	}
	if _, err := ledgerProof(t, path, 2); err == nil {
		t.Error("a proof of an entry past the end was accepted")
	}
}

func TestLedgerProofRefusesAnEmptyLedger(t *testing.T) {
	path, _ := newTestLedger(t, 0)

	if _, err := ledgerProof(t, path, 0); err == nil {
		t.Error("a proof of an empty ledger was accepted")
	}
}

// writeToTemp saves the proof so it can be read back the way an auditor would.
func writeToTemp(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "proof.jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("save the proof: %v", err)
	}
	return path
}

const serverConfigBody = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`

func writeServerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path
}

// The command line is what a script and a container probe rely on, so its
// answers are pinned: --version needs no configuration, --check reports on
// stdout, a refused configuration reports on stderr, and the two failure kinds
// have distinct statuses.
func TestServerCommandLine(t *testing.T) {
	valid := writeServerConfig(t, serverConfigBody)
	broken := writeServerConfig(t, "[server]\nbind_addr = \"127.0.0.1\"\n")
	unknown := writeServerConfig(t, serverConfigBody+"future_key = 1\n")

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"-version"}, 0, "aethertunnel-server ", ""},
		{"check a valid configuration", []string{"-check", "-config", valid}, 0, valid + " is valid", ""},
		{"refuse a broken configuration", []string{"-check", "-config", broken}, 1, "",
			"auth_token"},
		{"warn about an unknown key", []string{"-check", "-config", unknown}, 0, unknown + " is valid", "future_key"},
		{"refuse an unknown key when asked to", []string{"-check", "-reject-unknown-keys", "-config", unknown}, 1, "", "future_key"},
		{"usage", []string{"-h"}, 0, "", "Usage: aethertunnel-server"},
		{"an unparsable command line", []string{"-not-a-flag"}, 2, "", "not-a-flag"},
		{"the configuration can be positional", []string{"-check", valid}, 0, valid + " is valid", ""},
		// -ledger-key and -proof-index only modify an action. Alone they used to be
		// dropped while the server started, so a script saw a success status for a
		// request nothing carried out.
		{"a verification key without its action", []string{"-ledger-key", "3b1f"}, 2, "", "-verify-ledger"},
		{"a proof index without its action", []string{"-proof-index", "5", "-config", valid}, 2, "", "-ledger-proof"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run("aethertunnel-server", tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit status %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// --ledger-proof without an index explains itself, and the proof of a real
// ledger verifies through the same command line an auditor would use.
func TestLedgerFlagsThroughTheCommandLine(t *testing.T) {
	path, pubHex := newTestLedger(t, 3)

	var stdout, stderr bytes.Buffer
	if code := run("aethertunnel-server", []string{"-ledger-proof", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit status %d, want 1 without -proof-index", code)
	}
	if !strings.Contains(stderr.String(), "-proof-index") {
		t.Fatalf("stderr %q does not say what is missing", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run("aethertunnel-server", []string{"-ledger-proof", path, "-proof-index", "1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr %q)", code, stderr.String())
	}
	if lines := strings.Count(strings.TrimRight(stdout.String(), "\n"), "\n") + 1; lines != 2 {
		t.Fatalf("the proof has %d lines, want entries 0 through 1", lines)
	}

	proofPath := writeToTemp(t, stdout.String())
	stdout.Reset()
	stderr.Reset()
	if code := run("aethertunnel-server", []string{"-verify-ledger", proofPath, "-ledger-key", pubHex}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 entries verified") {
		t.Fatalf("the verification said %q", stdout.String())
	}
}

// --dht-key needs a [dht] section with a signing key; without one it says so
// instead of printing something an operator would paste into a configuration.
func TestDHTKeyWithoutAKeyIsRefused(t *testing.T) {
	path := writeServerConfig(t, serverConfigBody)
	var stdout, stderr bytes.Buffer
	code := run("aethertunnel-server", []string{"-dht-key", "-config", path}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("a configuration without a DHT key printed one: %q", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a refusal wrote to stdout: %q", stdout.String())
	}
}
