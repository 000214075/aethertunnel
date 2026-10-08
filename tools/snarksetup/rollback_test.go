package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A file that was not there before the run has no backup to put back, so the old
// rollback skipped it and left the new generation's file in place: r1cs.bin from
// this setup beside keys from the previous one. Both decode cleanly and proofs
// made for one never verify against the other, which is exactly the mixed
// generation the staging exists to prevent.
func TestARollbackRemovesTheFileThisRunCreated(t *testing.T) {
	dir := t.TempDir()
	old := []string{"old-r1cs", "old-pk", "old-vk"}
	// The third staging file is missing, so the third rename fails with ENOENT
	// after the first two succeeded.
	new := []string{"new-r1cs", "new-pk", ""}
	files := stagedTestFiles(t, dir, old, new)

	// r1cs.bin is what an interrupted or first run leaves absent.
	if err := os.Remove(files[0].name); err != nil {
		t.Fatalf("remove r1cs.bin: %v", err)
	}

	if err := commit(files); err == nil {
		t.Fatal("commit succeeded although a rename could not")
	}

	if _, err := os.Lstat(files[0].name); !os.IsNotExist(err) {
		t.Fatalf("the file this run created (r1cs.bin) survived the rollback: %v", err)
	}
	for i, name := range []string{"r1cs.bin", "proving.key", "verifying.key"} {
		if i == 0 {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read the previous %s: %v", name, err)
		}
		if string(content) != old[i] {
			t.Fatalf("%s holds %q after the failed commit, want the previous %q", name, content, old[i])
		}
	}
}
