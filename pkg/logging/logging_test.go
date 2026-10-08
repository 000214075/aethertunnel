package logging

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// Without log.to the logger keeps writing where the caller was writing, which is
// standard error, and opens nothing.
func TestWithoutATargetTheFallbackWriterIsKept(t *testing.T) {
	var buf bytes.Buffer
	logger, closeLog, err := Open(Options{}, &buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Print("hello")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("the fallback writer got %q", buf.String())
	}
}

// disable_timestamp is the whole line and nothing else, so a log that is already
// collected with a timestamp by something else does not carry two.
func TestDisableTimestampWritesBareLines(t *testing.T) {
	var buf bytes.Buffer
	logger, closeLog, err := Open(Options{DisableTimestamp: true}, &buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Print("hello")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := buf.String(); got != "hello\n" {
		t.Fatalf("the line was %q, want %q", got, "hello\n")
	}
}

// A second run appends to the file rather than replacing it: the reason to name a
// file at all is that yesterday's lines are still there.
func TestTheFileIsAppendedToAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	for _, line := range []string{"first", "second"} {
		logger, closeLog, err := Open(Options{To: path}, io.Discard)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		logger.Print(line)
		if err := closeLog(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	body := read(t, path)
	for _, line := range []string{"first", "second"} {
		if !strings.Contains(body, line) {
			t.Fatalf("%q is missing from %q", line, body)
		}
	}
}

// A file that holds nothing from today is moved aside under the date it holds,
// so the name the operator configured keeps today's lines.
func TestAFileFromAnEarlierDayIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	if err := os.WriteFile(path, []byte("yesterday\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatalf("touch: %v", err)
	}

	logger, closeLog, err := Open(Options{To: path, MaxDays: 3}, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Print("today")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := read(t, path); strings.Contains(got, "yesterday") {
		t.Fatalf("the live file still holds the old day: %q", got)
	}
	archive := path + "." + yesterday.Format(dateLayout)
	if got := read(t, archive); !strings.Contains(got, "yesterday") {
		t.Fatalf("the archive holds %q", got)
	}
}

// The retention window bounds the archive: files older than it are deleted, and a
// file next to the log that this package did not write is left alone.
func TestArchivesOlderThanTheWindowAreDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	if err := os.WriteFile(path, []byte("today\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	day := func(ago int) string {
		return time.Now().AddDate(0, 0, -ago).Format(dateLayout)
	}
	for _, ago := range []int{1, 5, 10} {
		if err := os.WriteFile(path+"."+day(ago), []byte("old\n"), 0o644); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}
	// Not an archive: a different suffix, and the operator's file.
	other := filepath.Join(dir, "server.log.1")
	if err := os.WriteFile(other, []byte("rotated by something else\n"), 0o644); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	logger, closeLog, err := Open(Options{To: path, MaxDays: 3}, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Print("a line")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := os.Stat(path + "." + day(1)); err != nil {
		t.Fatalf("the archive inside the window is gone: %v", err)
	}
	for _, ago := range []int{5, 10} {
		if _, err := os.Stat(path + "." + day(ago)); !os.IsNotExist(err) {
			t.Fatalf("the archive from %d days ago is still there (err %v)", ago, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a file this package did not write was removed: %v", err)
	}
}

// The rotation happens on the first line of a new day, without a restart: a server
// that runs for weeks must not write into a file named after the day it started.
// The clock is injected here, so the test does not wait for midnight.
func TestANewDayRotatesOnTheFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")

	base := time.Date(2026, 3, 1, 23, 0, 0, 0, time.Local)
	now := base
	w, err := newDailyWriter(path, 7, io.Discard, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newDailyWriter: %v", err)
	}
	if _, err := w.Write([]byte("before midnight\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The archive is named after the day the file actually holds, which the
	// filesystem records as the modification time.
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatalf("touch: %v", err)
	}

	next := base.Add(2 * time.Hour) // 01:00 the following day
	now = next
	if _, err := w.Write([]byte("after midnight\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := read(t, path); strings.Contains(got, "before midnight") {
		t.Fatalf("the new day kept writing into the old file: %q", got)
	} else if !strings.Contains(got, "after midnight") {
		t.Fatalf("the new day's line went nowhere: %q", got)
	}
	archive := path + "." + base.Format(dateLayout)
	if got := read(t, archive); !strings.Contains(got, "before midnight") {
		t.Fatalf("the archive holds %q", got)
	}
}

// A target that cannot be opened is an error the caller reports, not a log that
// silently goes nowhere.
func TestATargetThatCannotBeOpenedIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "server.log")
	_, _, err := Open(Options{To: path}, io.Discard)
	if err == nil {
		t.Fatal("Open accepted a path in a directory that does not exist")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error does not name the path: %v", err)
	}
}

// The default floor is info, so every line this project already wrote keeps
// appearing: a level key that quietly dropped the existing log would be worse
// than no key at all. trace and debug are below info and have to be asked for,
// which is what frp's default does too.
func TestTheDefaultLevelWritesInfoAndAbove(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := Open(Options{DisableTimestamp: true}, &buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Printf("a plain line")
	Tracef(logger, "a trace line")
	Debugf(logger, "a debug line")
	Warnf(logger, "a warn line")
	Errorf(logger, "an error line")

	got := buf.String()
	for _, want := range []string{"a plain line", "a warn line", "an error line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q is missing from %q", want, got)
		}
	}
	for _, unwanted := range []string{"a trace line", "a debug line"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("%q was written at the default level: %q", unwanted, got)
		}
	}
	// The marker that carries the level must never reach the destination.
	if strings.ContainsRune(got, 0) {
		t.Fatalf("the level marker leaked into the log: %q", got)
	}
}

// warn keeps the lines that report trouble and drops the chatter above them.
func TestWarnDropsInfoAndBelow(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := Open(Options{DisableTimestamp: true, Level: "warn"}, &buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Printf("ordinary detail")
	Debugf(logger, "debug detail")
	Warnf(logger, "something to look at")
	Errorf(logger, "something broke")

	got := buf.String()
	if strings.Contains(got, "ordinary detail") || strings.Contains(got, "debug detail") {
		t.Fatalf("a line below the floor was written: %q", got)
	}
	if !strings.Contains(got, "something to look at") || !strings.Contains(got, "something broke") {
		t.Fatalf("a line at or above the floor is missing: %q", got)
	}
}

// error is the strictest floor: only failures survive it.
func TestErrorKeepsOnlyFailures(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := Open(Options{DisableTimestamp: true, Level: "error"}, &buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Printf("ordinary detail")
	Warnf(logger, "a warning")
	Errorf(logger, "a failure")

	if got := buf.String(); got != "a failure\n" {
		t.Fatalf("the log was %q, want only the failure", got)
	}
}

// debug is below the default floor, so it has to be asked for.
func TestDebugAppearsOnlyWhenAskedFor(t *testing.T) {
	var quiet bytes.Buffer
	logger, _, err := Open(Options{DisableTimestamp: true}, &quiet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	Debugf(logger, "frame detail")
	if quiet.Len() != 0 {
		t.Fatalf("debug was written at the default level: %q", quiet.String())
	}

	var loud bytes.Buffer
	verbose, _, err := Open(Options{DisableTimestamp: true, Level: "debug"}, &loud)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	Debugf(verbose, "frame detail")
	if !strings.Contains(loud.String(), "frame detail") {
		t.Fatalf("debug was dropped at log.level = debug: %q", loud.String())
	}
}

// An unknown level is refused rather than silently read as the default.
func TestAnUnknownLevelIsRefused(t *testing.T) {
	if _, _, err := Open(Options{Level: "verbose"}, io.Discard); err == nil {
		t.Fatal("Open accepted a level that is not one of the five")
	}
	if _, err := ParseLevel(""); err != nil {
		t.Fatalf("the empty level should mean info: %v", err)
	}
	if got, err := ParseLevel("warn"); err != nil || got != LevelWarn {
		t.Fatalf("ParseLevel(\"warn\") = %v, %v", got, err)
	}
}

// A logger this package did not build has no filter, so the helpers write the
// line as it is: the marker must not appear, and nothing is dropped.
func TestAPlainLoggerGetsUnmarkedLines(t *testing.T) {
	var buf bytes.Buffer
	plain := log.New(&buf, "", 0)
	Warnf(plain, "warning %d", 7)
	Debugf(plain, "debug line")

	if got := buf.String(); got != "warning 7\ndebug line\n" {
		t.Fatalf("a plain logger got %q", got)
	}
}

// New filters the writer it is given, which is what an embedder that owns its
// output uses.
func TestNewFiltersTheWriterItIsGiven(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, Options{DisableTimestamp: true, Level: "debug"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	Debugf(logger, "kept")
	Tracef(logger, "dropped")

	if got := buf.String(); got != "kept\n" {
		t.Fatalf("New wrote %q", got)
	}
}

// Colour is only ever added for a terminal, and a buffer is not one; a log that
// is collected by a supervisor must not carry escape sequences. The terminal
// case is checked by hand, because a test cannot make a *bytes.Buffer a
// character device.
func TestABufferIsNeverColoured(t *testing.T) {
	for _, opts := range []Options{
		{DisableTimestamp: true},
		{DisableTimestamp: true, Level: "trace"},
	} {
		var buf bytes.Buffer
		logger, _, err := Open(opts, &buf)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		Errorf(logger, "red?")
		if got := buf.String(); strings.Contains(got, "\x1b[") {
			t.Fatalf("a buffer was coloured: %q", got)
		}
	}
}

// The marker sits at the start of the message, right after the timestamp the
// standard library writes — and only there. The scan used to trust the first
// NUL digit NUL sequence anywhere in the line, so peer-controlled bytes printed
// through a plain Printf could carry a marker of their own and re-level or
// suppress the line; the marker position now decides, and mid-line bytes stay
// literal data. (Rewritten when splitMarker gained the prefix argument.)
func TestTheMarkerIsOnlyReadAtTheStartOfTheMessage(t *testing.T) {
	withTS := stdlibPrefixLen(log.LstdFlags)
	level, line := splitMarker([]byte("2026/01/02 15:04:05 \x000\x00hello\n"), withTS)
	if level != LevelTrace {
		t.Fatalf("the level was read as %v", level)
	}
	if string(line) != "2026/01/02 15:04:05 hello\n" {
		t.Fatalf("the line came out as %q", line)
	}

	level, line = splitMarker([]byte("2026/01/02 15:04:05 a\x000\x00b\n"), withTS)
	if level != LevelInfo || string(line) != "2026/01/02 15:04:05 a\x000\x00b\n" {
		t.Fatalf("mid-line marker bytes were taken for a marker: %v, %q", level, line)
	}

	level, line = splitMarker([]byte("no marker here\n"), 0)
	if level != LevelInfo || string(line) != "no marker here\n" {
		t.Fatalf("a line with no marker was changed: %v, %q", level, line)
	}
}

// A day change that cannot open the new file must not lose the line. open() used to
// close the current file before trying, so Write's fallback — "the line still goes
// to the file that is open" — had nothing to write to, and the day's first line
// disappeared whenever the path stopped resolving.
func TestAFailedReopenKeepsTheOpenFileForTheNextLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the failure is injected by renaming the directory out from under the open file, which Windows refuses while the file is open")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")

	base := time.Date(2026, 3, 1, 23, 0, 0, 0, time.Local)
	now := base
	w, err := newDailyWriter(path, 7, io.Discard, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newDailyWriter: %v", err)
	}
	if _, err := w.Write([]byte("before\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The path stops resolving: the directory is moved out from under it, so the
	// day's file cannot be opened again while the open descriptor still works.
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("move the directory: %v", err)
	}
	now = base.Add(24 * time.Hour)
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("the line was refused instead of going to the file that is open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := read(t, filepath.Join(moved, "server.log"))
	if !strings.Contains(got, "after") {
		t.Fatalf("the line was dropped when the reopen failed: %q", got)
	}
}
