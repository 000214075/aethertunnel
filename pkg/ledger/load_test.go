package ledger

import (
	"path/filepath"
	"testing"
)

// An existing file needs no directory flush: its entry is already durable, and a
// path that was created by an earlier run must keep loading.
func TestNewLoadsAnExistingLedgerWithoutTouchingTheDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.jsonl")
	key, _ := testKey(t)

	first, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Append("client-a", "web", 10, 20); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := New(path, key)
	if err != nil {
		t.Fatalf("reopening an existing ledger failed: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
