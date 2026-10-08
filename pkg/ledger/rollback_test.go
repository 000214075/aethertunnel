package ledger

import (
	"path/filepath"
	"testing"
)

// A rollback that fails leaves the bytes of the failed record in the file while
// the in-memory chain does not advance, so the next Append reuses that record's
// index and PrevHash and writes a duplicate past the torn line — and the next
// start's verification then refuses the file, which keeps the server down. The
// ledger has to know it is broken and refuse to write, rather than reporting
// success on a file only an operator can repair.
//
// The truncate itself cannot be made to fail on demand, so the case drives the
// state machine: the file handle is closed under the rollback, which is the
// closest a test gets without injecting a fault into the filesystem.
func TestALedgerThatCannotRollBackRefusesToWriteAgain(t *testing.T) {
	key, _ := testKey(t)
	ledger, err := New(filepath.Join(t.TempDir(), "chain.jsonl"), key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	if _, err := ledger.Append("client-a", "example.com:443", 1, 2); err != nil {
		t.Fatalf("first append: %v", err)
	}

	if err := ledger.file.Close(); err != nil {
		t.Fatalf("close the file: %v", err)
	}
	if err := ledger.rollbackLocked(0); err == nil {
		t.Fatal("a rollback that could not truncate the file reported success")
	}
	if _, err := ledger.Append("client-a", "example.com:443", 3, 4); err == nil {
		t.Fatal("Append wrote to a ledger whose failed record could not be rolled back")
	}
}
