//go:build unix

package server

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// --- a failed retry of an audit record rolls its partial write back ----------

// A record whose first write fails is retried on a freshly reopened file, and
// that retry can fail the same way. It used to leave the bytes it had already
// written in the log, where the next record's line merges with the fragment and
// the file parses as neither; the retry now rolls its own partial write back the
// way the first attempt does.
func TestAFailedAuditRetryRollsItsPartialWriteBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(true, path, 0, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer func() { _ = auditor.Close() }()

	auditor.Record(AuditEvent{Event: EventControlAccepted, Remote: "127.0.0.1:1", Outcome: "ok"})
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the log: %v", err)
	}

	// One byte past what the log already holds: every write from here on lands
	// one byte and then fails with EFBIG, which is a write that failed partway
	// without root or a full filesystem. Go ignores SIGXFSZ, so the write
	// returns the error instead of killing the test. RLIMIT_FSIZE is
	// process-wide, so the cap is lifted as soon as the record was attempted.
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Skipf("cannot read RLIMIT_FSIZE: %v", err)
	}
	limit.Cur = uint64(before.Size()) + 1
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Skipf("cannot cap RLIMIT_FSIZE: %v", err)
	}

	auditor.Record(AuditEvent{
		Event: EventAuthFailed, Remote: "127.0.0.1:2", Outcome: "denied",
		Detail: "a record long enough that a partial write leaves bytes behind",
	})

	limit.Cur = limit.Max
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the log after the failure: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the log grew from %d to %d bytes with a record that never made it",
			before.Size(), after.Size())
	}
	events, err := readAuditEvents(t, path)
	if err != nil {
		t.Fatalf("the log is no longer parseable: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the log holds %d record(s), want the one that was written", len(events))
	}
	if got := auditor.Lost(); got != 1 {
		t.Fatalf("%d record(s) are reported lost, want the one that could not be written", got)
	}
}
