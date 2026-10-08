package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedTestFiles(t *testing.T, dir string, old, new []string) []staged {
	t.Helper()
	names := []string{"r1cs.bin", "proving.key", "verifying.key"}
	files := make([]staged, len(names))
	for i, name := range names {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte(old[i]), 0o644); err != nil {
			t.Fatalf("write the previous %s: %v", name, err)
		}
		temp := filepath.Join(dir, ".snarksetup-"+name)
		if new[i] != "" {
			if err := os.WriteFile(temp, []byte(new[i]), 0o644); err != nil {
				t.Fatalf("stage the new %s: %v", name, err)
			}
		}
		files[i] = staged{name: full, temp: temp}
	}
	return files
}

// Renames are individually atomic but the set is not. If the second rename
// fails after the first succeeded, the tree would hold r1cs.bin from one
// generation with keys from another — the mixed generation the staging exists
// to prevent — and the old code left exactly that, with no way back. The
// previous set is moved aside first and put back whole when a rename fails.
func TestCommitLeavesThePreviousSetWholeWhenARenameFails(t *testing.T) {
	dir := t.TempDir()
	old := []string{"old-r1cs", "old-pk", "old-vk"}
	// The second staging file is missing, so the second rename fails with
	// ENOENT after the first rename succeeded.
	new := []string{"new-r1cs", "", "new-vk"}
	files := stagedTestFiles(t, dir, old, new)

	err := commit(files)
	if err == nil {
		t.Fatal("commit succeeded although a rename could not")
	}

	for i, name := range []string{"r1cs.bin", "proving.key", "verifying.key"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read the previous %s: %v", name, err)
		}
		if string(content) != old[i] {
			t.Fatalf("%s holds %q after the failed commit, want the previous %q", name, content, old[i])
		}
	}
	// The staging files that are left are removed along the way.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".snarksetup-") {
			t.Fatalf("a staging file survived the failed commit: %s", entry.Name())
		}
	}
}

// On success every file is swapped in and the backups are gone, so a second
// run finds a clean tree.
func TestCommitSwapsEveryFileInAndRemovesTheBackups(t *testing.T) {
	dir := t.TempDir()
	files := stagedTestFiles(t, dir,
		[]string{"old-r1cs", "old-pk", "old-vk"},
		[]string{"new-r1cs", "new-pk", "new-vk"})

	if err := commit(files); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for i, name := range []string{"r1cs.bin", "proving.key", "verifying.key"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(content) != []string{"new-r1cs", "new-pk", "new-vk"}[i] {
			t.Fatalf("%s holds %q, want the new generation", name, content)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".prev") || strings.HasPrefix(entry.Name(), ".snarksetup-") {
			t.Fatalf("a leftover survived the commit: %s", entry.Name())
		}
	}
}
