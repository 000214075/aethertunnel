package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditLogRotatesAtMaxBytes drives audit.max_bytes, which is what keeps a
// long-running server from filling the disk with one unbounded file.
//
// The rotated generation is the file an operator reads to find what happened
// before the current one started, so it has to exist, hold the records that were
// written before the rotation, and the live file has to keep growing again from a
// smaller size than the limit.
func TestAuditLogRotatesAtMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	const limit = 1024
	auditor, err := NewAuditor(true, path, limit, discardLogger())
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	// Enough records to pass the limit several times over: one record is a few
	// hundred bytes, so a single write never crosses it.
	for i := 0; i < 40; i++ {
		auditor.Record(AuditEvent{
			Event:   EventACLDenied,
			Remote:  "127.0.0.2:40000",
			Detail:  "the source is in server.deny_cidrs",
			Outcome: "denied",
		})
	}

	rotated := path + ".1"
	info, err := os.Stat(rotated)
	if err != nil {
		live, _ := os.Stat(path)
		size := int64(-1)
		if live != nil {
			size = live.Size()
		}
		t.Fatalf("40 records left the live file at %d bytes and no %s beside it, so max_bytes was not applied: %v",
			size, filepath.Base(rotated), err)
	}
	if info.Size() == 0 {
		t.Errorf("the rotated generation is empty, so the records written before the rotation were lost")
	}
	// The size is checked when a record is about to be written, so a generation
	// reaches the limit and then holds at most one record more than it.
	if info.Size() < limit {
		t.Errorf("the log rotated at %d bytes, before the %d limit", info.Size(), limit)
	}
	if info.Size() > limit+512 {
		t.Errorf("the rotated generation is %d bytes, more than one record past the %d limit", info.Size(), limit)
	}

	// The rotated generation holds whole records, not a partial line.
	records, err := readAuditEvents(t, rotated)
	if err != nil {
		t.Fatalf("read the rotated generation: %v", err)
	}
	if len(records) == 0 {
		t.Fatalf("the rotated generation holds no record")
	}
	for _, event := range records {
		if event.Event != EventACLDenied {
			t.Errorf("the rotated generation holds a %q record, want only %q", event.Event, EventACLDenied)
		}
	}

	live, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the live file: %v", err)
	}
	if live.Size() >= info.Size() {
		t.Errorf("the live file is %d bytes and the rotated one %d, so the log did not restart from a smaller size",
			live.Size(), info.Size())
	}

	// Records keep being written after a rotation.
	before := live.Size()
	auditor.Record(AuditEvent{Event: EventControlAccepted, Remote: "127.0.0.1:50000", Outcome: "allowed"})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the live file after another record: %v", err)
	}
	if after.Size() <= before {
		t.Errorf("the live file did not grow after the rotation: %d then %d", before, after.Size())
	}
}

// TestAuditLogRotationCanBeDisabled covers the other half of audit.max_bytes: zero
// means one file that is never rotated, which is what a deployment that ships the
// log elsewhere wants.
func TestAuditLogRotationCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	auditor, err := NewAuditor(true, path, 0, discardLogger())
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	const written = 40
	for i := 0; i < written; i++ {
		auditor.Record(AuditEvent{Event: EventACLDenied, Remote: "127.0.0.2:40000", Outcome: "denied"})
	}

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Errorf("with max_bytes at zero the log rotated anyway")
	}
	records, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(records) != written {
		t.Errorf("the log holds %d records, want %d", len(records), written)
	}
}

// readAuditEvents parses a JSON Lines audit log.
func readAuditEvents(t *testing.T, path string) ([]AuditEvent, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []AuditEvent
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// TestAuditLogKeepsRecordingWhenTheFileIsClosedUnderneathIt is the failure this
// auditor used to hide: any write error was discarded, so a log whose file handle had
// gone away stopped recording for the rest of the run while everything else looked
// healthy. The event a check reads was that the server went quiet.
//
// A half-finished shutdown, an external log rotator and a file removed by an operator
// all look the same from inside: the handle is there, the writes fail. The auditor has
// to reopen the configured path and keep going.
func TestAuditLogKeepsRecordingWhenTheFileIsClosedUnderneathIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	auditor.Record(AuditEvent{Event: EventControlAccepted, Remote: "127.0.0.1:1", Outcome: "ok"})

	// Close the descriptor the auditor is holding, without telling it.
	if err := auditor.file.Close(); err != nil {
		t.Fatalf("close the handle: %v", err)
	}

	auditor.Record(AuditEvent{Event: EventAuthFailed, Remote: "127.0.0.1:2", Outcome: "denied"})
	auditor.Record(AuditEvent{Event: EventProxyRegistered, Proxy: "ssh", Outcome: "ok"})

	if got := auditor.Lost(); got != 0 {
		t.Errorf("%d record(s) were reported lost although the path was writable again", got)
	}
	if got := auditor.Failures() + auditor.Recovered(); got == 0 {
		t.Errorf("the failed write was not reported at all: failures=%d recovered=%d",
			auditor.Failures(), auditor.Recovered())
	}
	if err := auditor.LastError(); err != "" {
		t.Errorf("the last error is still %q after a write succeeded", err)
	}

	records, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("the log holds %d records, want 3: %v", len(records), records)
	}
	want := []string{EventControlAccepted, EventAuthFailed, EventProxyRegistered}
	for i, event := range records {
		if event.Event != want[i] {
			t.Errorf("record %d is %q, want %q", i, event.Event, want[i])
		}
	}
}

// TestAuditLogReportsRecordsItCannotWrite covers the other half: when the path cannot
// be reopened, the record is counted as lost and the error is kept, which is what the
// dashboard and the metric report. The server keeps running either way.
func TestAuditLogReportsRecordsItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	// Replacing the log with a directory makes every open of that path fail, which
	// is the state an operator has to be able to see from the dashboard.
	if err := auditor.file.Close(); err != nil {
		t.Fatalf("close the handle: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the log: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("put a directory where the log was: %v", err)
	}

	auditor.Record(AuditEvent{Event: EventControlAccepted, Outcome: "ok"})
	auditor.Record(AuditEvent{Event: EventAuthFailed, Outcome: "denied"})

	if got := auditor.Lost(); got != 2 {
		t.Errorf("%d record(s) reported lost, want 2", got)
	}
	if auditor.LastError() == "" {
		t.Errorf("no error is reported although every write failed")
	}
	summary := auditor.Summary()
	if summary["enabled"] != true {
		t.Errorf("the audit log reports itself disabled although it was configured: %v", summary)
	}
	if summary["writable"] != false {
		t.Errorf("the audit log reports itself writable although it cannot open its path: %v", summary)
	}
	if summary["records_lost"] != int64(2) {
		t.Errorf("the summary reports %v lost records, want 2", summary["records_lost"])
	}

	// The server keeps working: this is not a fatal condition.
	auditor.Record(AuditEvent{Event: EventControlAccepted, Outcome: "ok"})
	if got := auditor.Lost(); got != 3 {
		t.Errorf("after another record %d are reported lost, want 3", got)
	}
}

// TestAuditLogReopensAFileThatWasRotatedAway covers the deployment where something
// other than this server rotates the log: the path is renamed and recreated, so the
// auditor's open handle points at the old file and the path an operator reads stays
// empty. It has to notice and reopen.
//
// The scenario needs a rename of a file that is still open, which Windows refuses, so
// it is exercised on the platforms where it arises. On Windows the equivalent rotation
// is a truncation in place, which leaves the handle valid and needs no reopen.
func TestAuditLogReopensAFileThatWasRotatedAway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming a file that is still open is refused on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	auditor.Record(AuditEvent{Event: EventControlAccepted, Remote: "127.0.0.1:1", Outcome: "ok"})

	// What logrotate does: move the file aside and start a fresh one.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate the log: %v", err)
	}
	fresh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the new log: %v", err)
	}
	fresh.Close()

	auditor.Record(AuditEvent{Event: EventProxyRegistered, Proxy: "ssh", Outcome: "ok"})

	records, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("read the log the operator looks at: %v", err)
	}
	if len(records) != 1 || records[0].Event != EventProxyRegistered {
		t.Fatalf("the path an operator reads holds %v, want the record written after the rotation", records)
	}
	if got := auditor.Failures(); got != 0 {
		t.Errorf("%d write failures reported although nothing failed", got)
	}
}

// TestAuditLogRecreatesAFileThatWasRemoved covers the other half of the same
// failure: the path is renamed away or deleted with nothing put in its place, so
// the handle writes into an inode no name reaches any more. The path has to come
// back without a restart.
//
// Removing a file that is still open is allowed on Unix and refused on Windows,
// so this runs where the situation arises.
func TestAuditLogRecreatesAFileThatWasRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("removing a file that is still open is refused on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	auditor.Record(AuditEvent{Event: EventControlAccepted, Remote: "127.0.0.1:1", Outcome: "ok"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log before removing it: %v", err)
	}

	// An operator archiving the log, or freeing space, with no replacement.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the log: %v", err)
	}

	auditor.Record(AuditEvent{Event: EventProxyRegistered, Proxy: "ssh", Outcome: "ok"})

	records, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("the configured path was not recreated: %v", err)
	}
	if len(records) != 1 || records[0].Event != EventProxyRegistered {
		t.Fatalf("the recreated log holds %v, want only the record written after the removal", records)
	}
	if len(before) == 0 {
		t.Fatal("the first record was never written, so the removal proves nothing")
	}
	if got := auditor.Lost(); got != 0 {
		t.Errorf("%d records reported lost although the record was written to the recreated file", got)
	}
	if got := auditor.Failures(); got != 0 {
		t.Errorf("%d write failures reported although nothing failed", got)
	}
}

// TestAuditLogIgnoresRecordsAfterClose keeps a late record from recreating a log file
// during shutdown.
func TestAuditLogIgnoresRecordsAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	auditor.Record(AuditEvent{Event: EventControlAccepted, Outcome: "ok"})
	if err := auditor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	auditor.Record(AuditEvent{Event: EventAuthFailed, Outcome: "denied"})

	records, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("the closed log holds %d records, want the 1 written before Close", len(records))
	}
}
