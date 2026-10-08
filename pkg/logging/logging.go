// Package logging builds the logger a process writes through from the [log]
// section, so an operator can keep the log in a file with a bounded archive, ask
// for a severity floor, and see a terminal coloured by level instead of
// depending on a supervisor to collect standard error.
//
// It is frp's [log] section: log.to (whose frp default, "console", this project
// leaves at standard error), log.maxDays, log.level and log.disablePrintColor.
// The file keeps its name: log.to is the live file, and a file that holds
// nothing from an earlier day is moved aside to <to>.<YYYY-MM-DD> when the next
// line is about to be written.
//
// Levels ride the writer rather than the type, which is why a logger is still a
// *log.Logger: Open installs a writer that reads a marker out of each line and
// decides whether to keep it, so every existing `logger.Printf` keeps meaning
// "info" and nothing in the project had to change type. A caller that wants to
// write above or below info uses this package's Tracef, Debugf, Warnf and Errorf
// with the same logger:
//
//	logging.Warnf(logger, "source %s is banned for %s", addr, window)
//
// A logger that Open did not build — a test's buffer, an embedder's own
// *log.Logger — has no filter installed, so the helpers write the line plainly
// and nothing is dropped.
//
// This package deliberately does not import pkg/config, so the packages config
// depends on (pkg/dht, pkg/discovery) can log through it too; a caller builds
// Options from its configuration.
package logging

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is a line's severity. The order is the one log.level names: trace is the
// least severe and is written only when the configured floor is trace.
type Level int

// The levels, in the order log.level accepts them.
const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
)

// levelNames maps each level to the name log.level uses.
var levelNames = [...]string{"trace", "debug", "info", "warn", "error"}

func (l Level) String() string {
	if l < LevelTrace || int(l) >= len(levelNames) {
		return "unknown"
	}
	return levelNames[l]
}

// ParseLevel turns a log.level value into a Level; an empty value is info, which
// is frp's default.
func ParseLevel(name string) (Level, error) {
	if name == "" {
		return LevelInfo, nil
	}
	for index, candidate := range levelNames {
		if candidate == name {
			return Level(index), nil
		}
	}
	return LevelInfo, fmt.Errorf("log.level %q is not supported (use %s)",
		name, strings.Join(levelNames[:], ", "))
}

// Options carries the [log] section into this package without importing it.
type Options struct {
	// To is the file the log is appended to; empty keeps the fallback writer,
	// which a process sets to standard error.
	To string
	// MaxDays bounds the archive: 0 keeps one growing file behind To, a positive
	// value rotates yesterday's file aside and deletes archives older than that
	// many days.
	MaxDays int
	// Level is the least severe line that is written.
	Level string
	// DisableTimestamp drops the date and time from every line.
	DisableTimestamp bool
	// DisablePrintColor stops a terminal from being coloured by level.
	DisablePrintColor bool
}

// dateLayout names a rotated file: <log.to>.<YYYY-MM-DD>.
const dateLayout = "2006-01-02"

// levelMarker opens the marker a level-carrying line starts its message with. It
// is a NUL byte — which no log line this project writes contains — followed by
// the digit of the level, followed by another NUL. The writer strips it before
// the line reaches its destination, so it is never visible in the log.
const levelMarker = "\x00"

// Open returns the logger for this process and the function that closes it. The
// closer is never nil, so a caller can always defer it.
//
// With an empty opts.To the fallback writer is used — standard error, which is
// what a supervisor collects — and nothing is opened. With a path, the file is
// appended to and rotated daily when opts.MaxDays is set.
func Open(opts Options, fallback io.Writer) (*log.Logger, func() error, error) {
	floor, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}
	flags := log.LstdFlags
	if opts.DisableTimestamp {
		flags = 0
	}

	var dst io.Writer = fallback
	closer := func() error { return nil }
	if opts.To != "" {
		w, err := newDailyWriter(opts.To, opts.MaxDays, fallback, time.Now)
		if err != nil {
			return nil, nil, err
		}
		dst, closer = w, w.Close
	}
	// A file is never coloured, whatever the configuration says: the only place
	// colour helps is a terminal, and only a fallback writer can be one.
	allowColor := opts.To == "" && !opts.DisablePrintColor
	return log.New(&levelWriter{dst: dst, floor: floor, color: allowColor && isTerminal(dst), prefixLen: stdlibPrefixLen(flags)}, "", flags), closer, nil
}

// New builds a logger over an arbitrary writer, which is what a test or an
// embedder that owns its output uses. The writer is filtered the same way Open
// filters its destination.
func New(w io.Writer, opts Options) (*log.Logger, error) {
	floor, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	flags := log.LstdFlags
	if opts.DisableTimestamp {
		flags = 0
	}
	return log.New(&levelWriter{dst: w, floor: floor, color: !opts.DisablePrintColor && isTerminal(w), prefixLen: stdlibPrefixLen(flags)}, "", flags), nil
}

// stdlibPrefixLen is the length of the prefix the standard library writes for
// the flags this package builds its loggers with: log.LstdFlags renders
// "2006/01/02 15:04:05 ", and no flags render nothing. If the flags ever
// change, this has to change with them — the marker position is what keeps
// printed data from masquerading as a level.
func stdlibPrefixLen(flags int) int {
	if flags == log.LstdFlags {
		return len("2006/01/02 15:04:05 ")
	}
	return 0
}

// isTerminal reports whether a writer is a character device, which is the
// cheapest way to tell a console from a file or a pipe without a dependency.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ANSI colours per level. Info is left alone: it is the default and the common
// case, and colouring an ordinary line would only make the log harder to read.
var levelColors = [...]string{
	LevelTrace: "\x1b[90m", // bright black
	LevelDebug: "\x1b[36m", // cyan
	LevelInfo:  "",
	LevelWarn:  "\x1b[33m", // yellow
	LevelError: "\x1b[31m", // red
}

const colorReset = "\x1b[0m"

// levelWriter drops the lines below the floor, strips the level marker and
// colours a terminal.
type levelWriter struct {
	dst   io.Writer
	floor Level
	color bool
	// prefixLen is the length of the prefix the standard library writes
	// ahead of every message on this logger: the date-and-time prefix
	// (log.LstdFlags) or nothing. The level marker sits at the start of the
	// message — right after this prefix — and only there: a plain
	// logger.Printf can print peer-controlled bytes, and bytes that happen
	// to spell a marker in the middle of the line are data, not a level.
	prefixLen int
}

func (w *levelWriter) Write(p []byte) (int, error) {
	level, line := splitMarker(p, w.prefixLen)
	if level < w.floor {
		// The line is reported as written: a logger that starts failing because
		// its output was filtered would be worse than a quiet log.
		return len(p), nil
	}
	if w.color {
		if code := levelColors[level]; code != "" {
			// The logger hands the line over terminated by \n; wrapping has
			// to move that newline to the end of the colored line, not leave
			// the reset stranded on the next one with a blank line behind it.
			line = append([]byte(code), bytes.TrimSuffix(line, []byte("\n"))...)
			line = append(line, colorReset...)
			line = append(line, '\n')
		}
	}
	if _, err := w.dst.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}

// splitMarker reads the level out of a line and returns the line without it. A
// line with no marker is info, which is what a plain logger.Printf means. The
// marker is trusted only at the start of the message, right after the logger's
// own prefix (prefixLen): the bytes after it belong to the message, and
// peer-controlled data printed through a plain Printf can spell a marker of its
// own — trusting the first NUL sequence anywhere in the line let an
// authenticated ssh gateway client suppress or re-level the log lines that
// named it. The package builds both sides of its loggers, so the offset the
// marker sits at is known here.
func splitMarker(p []byte, prefixLen int) (Level, []byte) {
	start := prefixLen
	if start+2 >= len(p) || p[start] != 0 || p[start+2] != 0 {
		return LevelInfo, p
	}
	digit, err := strconv.Atoi(string(p[start+1 : start+2]))
	if err != nil || digit < int(LevelTrace) || digit > int(LevelError) {
		return LevelInfo, p
	}
	line := make([]byte, 0, len(p)-3)
	line = append(line, p[:start]...)
	line = append(line, p[start+3:]...)
	return Level(digit), line
}

// at writes one message through a logger at a level. A logger this package did
// not build has no filter, so the marker is left out: the line appears as the
// caller wrote it.
func at(logger *log.Logger, level Level, message string) {
	if logger == nil {
		return
	}
	if _, filtered := logger.Writer().(*levelWriter); filtered {
		message = levelMarker + strconv.Itoa(int(level)) + levelMarker + message
	}
	// The call depth is not reported (no Lshortfile), so the exact value only
	// matters for the standard library's own bookkeeping.
	_ = logger.Output(3, message)
}

// Tracef writes at trace level through logger.
func Tracef(logger *log.Logger, format string, args ...any) {
	at(logger, LevelTrace, sprintf(format, args...))
}

// Debugf writes at debug level through logger.
func Debugf(logger *log.Logger, format string, args ...any) {
	at(logger, LevelDebug, sprintf(format, args...))
}

// Warnf writes at warn level through logger.
func Warnf(logger *log.Logger, format string, args ...any) {
	at(logger, LevelWarn, sprintf(format, args...))
}

// Errorf writes at error level through logger.
func Errorf(logger *log.Logger, format string, args ...any) {
	at(logger, LevelError, sprintf(format, args...))
}

func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// dailyWriter appends to one file and moves it aside when the day it holds ends.
//
// The rotation happens on the first write of a new day rather than on a timer: a
// process that is quiet overnight must not wake up to rotate, and one that is
// busy writes its first line of the day into the new file.
type dailyWriter struct {
	path     string
	maxDays  int
	fallback io.Writer
	now      func() time.Time

	mu   sync.Mutex
	file *os.File
	day  string
}

func newDailyWriter(path string, maxDays int, fallback io.Writer, now func() time.Time) (*dailyWriter, error) {
	w := &dailyWriter{path: path, maxDays: maxDays, fallback: fallback, now: now}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open closes whatever is open and prepares today's file, rotating and pruning
// first when a retention window is set. The caller holds the lock.
func (w *dailyWriter) open() error {
	today := w.now()

	if putFileDownBeforeRotate && w.file != nil {
		// Windows refuses to rename a file another handle holds open, and rotate
		// moves the live name aside. With the handle still open the rename
		// fails, the fallback reports it, and the day's lines keep landing in
		// the file named after the day that ended — the day boundary would
		// never take. The handle goes down first on this platform; the cost is
		// platform-local: the reopen failure below has no old handle to fall
		// back to here, while unix keeps one (see rotatefile_other.go).
		_ = w.file.Close()
		w.file = nil
	}

	if err := rotate(w.path, w.maxDays, today); err != nil {
		// A log that cannot be rotated is still a log: the failure is reported
		// on the fallback writer, which is where the line would have gone
		// without [log].to, and writing continues into the file that is there.
		w.report(err)
	}
	if err := prune(w.path, w.maxDays, today); err != nil {
		w.report(err)
	}

	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// The file that is already open stays open. Closing it first, as this used
		// to, left nothing to write to, and Write's own fallback — "the line still
		// goes to the file that is open rather than being dropped" — could not work:
		// the day's first line was lost whenever the new file could not be opened.
		return fmt.Errorf("log.to %s: %w", w.path, err)
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = file
	w.day = today.Format(dateLayout)
	return nil
}

// report writes a rotation failure where a reader will see it. Nothing else can
// be done about it here: the alternative is a line that is silently lost.
func (w *dailyWriter) report(err error) {
	if w.fallback == nil {
		return
	}
	fmt.Fprintf(w.fallback, "%s log: %v\n", w.now().Format("2006/01/02 15:04:05"), err)
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if day := w.now().Format(dateLayout); day != w.day {
		err := w.open()
		// The day is marked attempted either way. A failure that will not go
		// away this side of midnight — a full disk, a removed directory —
		// would otherwise re-run rotate, prune and the open, and report, on
		// every line, drowning the fallback writer. The file that is open
		// keeps receiving the lines, and the retry waits for the next day
		// boundary, which is when the next rotation belongs anyway.
		w.day = day
		if err != nil {
			// The new day's file could not be opened: the line still goes to
			// the file that is open rather than being dropped, and the reason
			// is on the fallback writer.
			w.report(err)
		}
	}
	if w.file == nil {
		return 0, fmt.Errorf("log.to %s: no file is open", w.path)
	}
	return w.file.Write(p)
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate moves a file that holds nothing from today aside to path.<its date>, so
// the live name is free for today's lines. A file that was already written to
// today stays where it is, which is what makes a restart mid-day keep appending.
func rotate(path string, maxDays int, now time.Time) error {
	if maxDays <= 0 {
		// Without a retention window the file is the archive: one file that
		// grows is exactly what was asked for.
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot look at %s: %w", path, err)
	}
	if info.Size() == 0 {
		return nil
	}
	stamp := info.ModTime().Format(dateLayout)
	if stamp == now.Format(dateLayout) {
		return nil
	}

	target := path + "." + stamp
	if info, err := os.Stat(target); err == nil {
		// A file for that day is already there: the lines are joined rather than
		// one of the two being thrown away, which is what a copied or restored
		// log produces.
		if err := appendFile(target, path); err != nil {
			// Undo the part that landed. Without this the next rotation
			// appends the live file from the start again, and every line
			// that made it in before the failure ends up in the archive
			// twice — each retry adding another copy.
			_ = os.Truncate(target, info.Size())
			return err
		}
		return os.Remove(path)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("cannot rotate %s to %s: %w", path, target, err)
	}
	return nil
}

// appendFile adds the contents of from to to.
func appendFile(to, from string) error {
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", from, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("cannot append %s to %s: %w", from, to, err)
	}
	return dst.Close()
}

// prune deletes the archives older than the retention window. Files next to the
// log that are not named <path>.<date> are left alone: the directory belongs to
// the operator, not to this package. Candidates are matched by literal prefix
// rather than by glob, because a log.to that contains glob metacharacters of its
// own — app[1].log is a legal directory entry and a legal pattern — would make
// the pattern match names the log never wrote, and the retention window would
// silently never delete anything.
func prune(path string, maxDays int, now time.Time) error {
	if maxDays <= 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		// A directory this package cannot list is not a directory it can prune
		// in; the file itself was opened by the caller's request.
		return nil
	}
	prefix := filepath.Base(path) + "."
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff := today.AddDate(0, 0, -(maxDays - 1))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		stamp := strings.TrimPrefix(entry.Name(), prefix)
		when, err := time.ParseInLocation(dateLayout, stamp, now.Location())
		if err != nil {
			continue
		}
		if when.Before(cutoff) {
			match := filepath.Join(filepath.Dir(path), entry.Name())
			if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("cannot remove %s: %w", match, err)
			}
		}
	}
	return nil
}
