package ledger

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A chain whose last record lost its final newline — a crash, or a hand edit —
// loads fine, and the next append used to be concatenated onto that line, so the
// following start refused the whole file. The record that is there has to survive.
func TestALedgerWithoutAFinalNewlineStaysReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	key, _ := testKey(t)

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fill(t, l, 3)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(raw, []byte("\n")), 0o600); err != nil {
		t.Fatalf("drop the final newline: %v", err)
	}

	again, err := New(path, key)
	if err != nil {
		t.Fatalf("a chain without a final newline was refused: %v", err)
	}
	if _, err := again.Append("client-a", "example.com:443", 7, 8); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The real assertion: the file the append left behind is a readable chain.
	third, err := New(path, key)
	if err != nil {
		t.Fatalf("the chain the append left is unreadable: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
