package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// prune matches candidates by literal prefix. A log.to that carries glob
// metacharacters of its own — app[1].log is a legal directory entry and a legal
// pattern — used to make filepath.Glob match names the log never wrote, so the
// retention window silently never deleted anything and the archives grew
// without bound.
func TestPruneMatchesArchivesByLiteralPrefix(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	today := now.Format(dateLayout)
	old := now.AddDate(0, 0, -30).Format(dateLayout)

	logPath := filepath.Join(dir, "app[1].log")
	files := map[string]string{
		"app[1].log":            "current",
		"app[1].log." + old:     "old archive",
		"app[1].log." + today:   "today's archive",
		"app1.log." + old:       "a neighbour the glob pattern would have matched",
		"app[1].log.not-a-date": "not an archive",
		"unrelated.txt":         "the operator's file",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := prune(logPath, 7, now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for name, want := range map[string]bool{
		"app[1].log":            true,
		"app[1].log." + today:   true,
		"app1.log." + old:       true,
		"app[1].log.not-a-date": true,
		"unrelated.txt":         true,
		"app[1].log." + old:     false,
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil != want {
			t.Fatalf("%s exists=%v, want %v", name, err == nil, want)
		}
	}

	// The plain case keeps working the way it always has.
	plainDir := t.TempDir()
	plain := filepath.Join(plainDir, "app.log")
	if err := os.WriteFile(filepath.Join(plainDir, "app.log."+old), []byte("old"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plainDir, "app.log."+today), []byte("today"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := prune(plain, 7, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plainDir, "app.log."+old)); !os.IsNotExist(err) {
		t.Fatal("the plain case stopped pruning the old archive")
	}
	if _, err := os.Stat(filepath.Join(plainDir, "app.log."+today)); err != nil {
		t.Fatal("the plain case pruned the fresh archive")
	}
}
