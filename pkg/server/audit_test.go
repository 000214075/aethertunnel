package server

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	auditor, err := NewAuditor(true, path, limit)
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

	auditor, err := NewAuditor(true, path, 0)
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
