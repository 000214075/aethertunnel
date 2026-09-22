package main

import (
	"bytes"
	"encoding/hex"
	"io"
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

// capture runs fn with stdout redirected and returns what it wrote.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = write
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		done <- buf.String()
	}()

	runErr := fn()
	_ = write.Close()
	os.Stdout = saved
	return <-done, runErr
}

// A proof is the chain up to one entry, in the ledger's own format, so a holder of the
// public key can check that entry's inclusion without the rest of the file.
func TestLedgerProofVerifiesOnItsOwn(t *testing.T) {
	path, pubHex := newTestLedger(t, 4)

	out, err := capture(t, func() error { return writeLedgerProof(path, 2) })
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

	if _, err := capture(t, func() error { return writeLedgerProof(path, -1) }); err == nil {
		t.Error("a proof without an index was accepted")
	}
	if _, err := capture(t, func() error { return writeLedgerProof(path, 2) }); err == nil {
		t.Error("a proof of an entry past the end was accepted")
	}
}

func TestLedgerProofRefusesAnEmptyLedger(t *testing.T) {
	path, _ := newTestLedger(t, 0)

	if _, err := capture(t, func() error { return writeLedgerProof(path, 0) }); err == nil {
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
