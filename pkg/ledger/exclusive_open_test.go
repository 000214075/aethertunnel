package ledger

import (
	"path/filepath"
	"strings"
	"testing"
)

// The ledger file has one writer. Two instances configured with one ledger path,
// each inheriting the same signing key, would otherwise load the same head and
// append records carrying one Index and PrevHash — the chain breaks on the spot,
// --verify-ledger reports it and the next start refuses to load it. The lock is
// taken on the descriptor before the file is read, so the second instance also
// cannot parse a half-written line and report a corruption instead of the real
// reason it must not start.
func TestASecondLedgerOnTheSameFileRefusesToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.jsonl")
	key, _ := testKey(t)

	first, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Append("client-a", "web", 10, 20); err != nil {
		t.Fatalf("Append: %v", err)
	}

	second, err := New(path, key)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second ledger opened the file the first one is writing")
	}
	if !strings.Contains(err.Error(), "already being written") {
		t.Fatalf("the refusal does not name the other instance: %v", err)
	}

	// The lock lives on the descriptor, so Close has to release it: the file
	// must be usable again afterwards.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third, err := New(path, key)
	if err != nil {
		t.Fatalf("New after the first ledger closed: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
