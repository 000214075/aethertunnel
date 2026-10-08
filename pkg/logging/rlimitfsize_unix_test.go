//go:build unix

package logging

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// When the archive already holds a file for the day being rotated, the live
// file is appended to it. If that append fails halfway — an archive volume
// running out of space, say — the bytes that landed stayed there, and the
// next rotation copied the live file from the start again: every line that
// made it in ended up in the archive twice, one more copy per retry. The
// target is put back to what it was, so the retry starts clean.
func TestARotateAppendFailureLeavesTheTargetAsItWas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	live := strings.Repeat("live line\n", 30000) // 300 KB
	if err := os.WriteFile(path, []byte(live), 0o644); err != nil {
		t.Fatalf("write the live file: %v", err)
	}
	// rotate judges the day by the live file's mtime; stamp it with
	// yesterday so the rotation actually runs.
	yesterday := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	target := path + ".2026-10-05"
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("write the target: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat the target: %v", err)
	}

	// Cap every file this process may create at the target's size plus one
	// copy chunk: the append fails partway through, after some chunks made
	// it in. Go ignores SIGXFSZ, so the write returns EFBIG instead of
	// killing the test.
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Skipf("cannot read RLIMIT_FSIZE: %v", err)
	}
	limit.Cur = uint64(before.Size()) + 32*1024
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Skipf("cannot cap RLIMIT_FSIZE: %v", err)
	}
	defer func() {
		limit.Cur = limit.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit)
	}()

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if err := rotate(path, 7, now); err == nil {
		t.Fatal("the append past the file size limit succeeded")
	}

	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat the target after the failure: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the target grew from %d to %d bytes before the failure was reported", before.Size(), after.Size())
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if string(content) != "old\n" {
		t.Fatalf("the target holds %q, want the untouched \"old\\n\"", content)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the live file did not survive the failed rotation")
	}
}
