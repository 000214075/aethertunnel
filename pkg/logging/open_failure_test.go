package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// When the new day's file cannot be opened — a full disk, a removed directory
// — the old code retried the whole rotate-and-open on every line and reported
// the failure on the fallback writer once per line, drowning whatever an
// operator was trying to read. The day is marked attempted either way now, so
// the reason is reported once and the retry waits for the next boundary.
func TestAnOpenFailureIsReportedOnceForTheDay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the failure is injected by removing the file and pulling the directory's permissions, which Windows neither allows on an open file nor enforces this way")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	day := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	current := day
	w, err := newDailyWriter(path, 7, os.Stderr, func() time.Time { return current })
	if err != nil {
		t.Fatalf("newDailyWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("first day\n")); err != nil {
		t.Fatalf("write on the first day: %v", err)
	}

	// The next day the file is gone and the directory no longer accepts a
	// new one — a removed directory, or one whose permissions were pulled.
	// The still-open day-one file keeps receiving the lines.
	current = day.Add(24 * time.Hour)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the log file: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var fallback bytes.Buffer
	w.fallback = &fallback

	if _, err := w.Write([]byte("second day\n")); err != nil {
		t.Fatalf("write on the second day: %v", err)
	}
	first := strings.Count(fallback.String(), "log:")
	if first == 0 {
		t.Fatal("the open failure was not reported at all")
	}

	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte("still going\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := strings.Count(fallback.String(), "log:"); got != first {
		t.Fatalf("the failure was reported %d time(s) after the first report, want 0 more; the fallback reads:\n%s", got-first, fallback.String())
	}
}
