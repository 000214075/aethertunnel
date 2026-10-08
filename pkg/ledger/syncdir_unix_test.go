//go:build !windows

package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// New creates the ledger file when it is missing. The file's own Sync makes its
// contents durable, but not the directory entry that points at the inode — a
// crash right after creation could lose the whole file and the chain in it. So
// the creation flushes the parent directory, and when it cannot be flushed the
// ledger refuses to start rather than claiming a durability it does not have.
//
// The case belongs to the platforms whose syncParentDir can fail: Windows has no
// equivalent directory flush and returns nil, so 0o300 there is not even a
// readable-only mode — the file would be created and New would report success.
func TestNewFlushesTheDirectoryOfAFileItCreates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission this case relies on")
	}
	dir := filepath.Join(t.TempDir(), "chain")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write and search but no read: the file can be created inside, while the
	// directory itself cannot be opened for the flush.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	key, _ := testKey(t)
	_, err := New(filepath.Join(dir, "chain.jsonl"), key)
	if err == nil {
		t.Fatal("New created a ledger whose directory entry it could not flush and reported success")
	}
	if !strings.Contains(err.Error(), "flush the directory") {
		t.Fatalf("the error does not name the directory flush: %v", err)
	}
}
