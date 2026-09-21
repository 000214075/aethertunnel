package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEvent is one line of the audit log.
type AuditEvent struct {
	Time     string `json:"time"`
	Event    string `json:"event"`
	ClientID string `json:"client_id,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Outcome  string `json:"outcome"`
}

// Event names written to the audit log.
const (
	EventControlAccepted = "control_accepted"
	EventControlRejected = "control_rejected"
	EventAuthFailed      = "auth_failed"
	EventClientGone      = "client_disconnected"
	EventProxyRegistered = "proxy_registered"
	EventProxyRejected   = "proxy_rejected"
	EventProxyRemoved    = "proxy_removed"
	EventACLDenied       = "acl_denied"
	EventRateLimited     = "rate_limited"
	EventDashboardAction = "dashboard_action"

	// EventSourceBanned is recorded when a source is banned after repeated
	// authentication failures, and EventBanRefused when a banned source tries
	// again.
	EventSourceBanned = "source_banned"
	EventBanRefused   = "ban_refused"

	// EventProxyVisitorDenied is recorded when a proxy's own allow/deny lists
	// refuse a visitor that the server as a whole had admitted.
	EventProxyVisitorDenied = "proxy_visitor_denied"

	EventVisitorAccepted = "visitor_accepted"
	EventVisitorRejected = "visitor_rejected"
	// EventP2PDirect is recorded when a visitor reports that it reached the owner
	// over a punched path, and EventP2PRelayed when the punch failed and the
	// server carried the traffic instead. EventP2PAbandoned is recorded when a
	// visitor's control connection goes away without either answer: the server
	// then does not know whether the direct path came up.
	EventP2PDirect    = "p2p_direct"
	EventP2PRelayed   = "p2p_relayed"
	EventP2PAbandoned = "p2p_abandoned"

	EventVPNAssigned = "vpn_address_assigned"
	EventVPNRejected = "vpn_address_rejected"
)

// Auditor writes JSON Lines audit records. A disabled auditor discards
// everything, so call sites do not need to check whether auditing is on.
//
// A record that cannot be written must never take the tunnel down, but silence is
// not an option either: an audit log that stopped recording looks exactly like a
// quiet server. Every failed write is therefore counted, the last error is kept,
// and the configured path is reopened before the next record so a handler that was
// closed or replaced underneath the server heals instead of losing the rest of the
// run.
type Auditor struct {
	mu         sync.Mutex
	configured bool
	closed     bool
	file       *os.File
	fileInfo   os.FileInfo
	path       string
	maxBytes   int64
	keep       int
	written    int64
	encoder    *json.Encoder
	logger     *log.Logger

	// failures counts writes that failed at least once, lost counts records that
	// could not be written even after reopening the file, and recovered counts
	// records that only landed on the second attempt. lastError is the most
	// recent failure and is cleared by a write that succeeds.
	failures  int64
	lost      int64
	recovered int64
	lastError string
}

// NewAuditor opens the audit log. A disabled configuration returns an auditor
// that writes nowhere. logger may be nil; the auditor then reports failures only
// through its counters. One rotated generation is kept beside the live log.
func NewAuditor(enabled bool, path string, maxBytes int64, logger *log.Logger) (*Auditor, error) {
	return NewAuditorWithRetention(enabled, path, maxBytes, 1, logger)
}

// NewAuditorWithRetention opens the audit log and keeps up to keep rotated
// generations beside it, so an operator has more than the last one to look at
// after an incident. A keep below one means the default of one.
func NewAuditorWithRetention(enabled bool, path string, maxBytes int64, keep int, logger *log.Logger) (*Auditor, error) {
	if !enabled {
		return &Auditor{}, nil
	}
	if keep < 1 {
		keep = 1
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	return &Auditor{
		configured: true,
		file:       file,
		fileInfo:   info,
		path:       path,
		maxBytes:   maxBytes,
		keep:       keep,
		written:    info.Size(),
		encoder:    json.NewEncoder(file),
		logger:     logger,
	}, nil
}

// Configured reports whether this auditor was asked to record anything. It stays
// true while the file is closed, which is exactly the state the dashboard has to
// be able to show: auditing is on, and right now nothing is being written.
func (a *Auditor) Configured() bool { return a != nil && a.configured }

// Enabled reports whether records are being written right now.
func (a *Auditor) Enabled() bool { return a != nil && a.file != nil }

// Failures reports how many writes failed at least once. A record that landed on
// the second attempt counts here as well: the operator wants to know that the log
// hiccuped, not only that it eventually caught up.
func (a *Auditor) Failures() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failures
}

// Lost reports how many records could not be written at all. This is the number
// that says the audit trail has a hole in it.
func (a *Auditor) Lost() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lost
}

// Recovered reports how many records landed on the second attempt, after the file
// was reopened.
func (a *Auditor) Recovered() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recovered
}

// LastError reports the most recent write error, or an empty string when the last
// write succeeded.
func (a *Auditor) LastError() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastError
}

// Summary is the audit log's state for the dashboard: whether records are being
// written, how many were dropped, and the last error. A log that was configured
// but cannot be written is reported as enabled with a non-empty last_error, not as
// disabled: the two mean very different things to an operator.
func (a *Auditor) Summary() map[string]any {
	if a == nil || !a.configured {
		return map[string]any{"enabled": false}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"enabled":        true,
		"writable":       a.file != nil,
		"path":           a.path,
		"max_bytes":      a.maxBytes,
		"bytes_written":  a.written,
		"write_failures": a.failures,
		"records_lost":   a.lost,
		"recovered":      a.recovered,
		"last_error":     a.lastError,
	}
}

// Record appends one event. A closed auditor ignores it, which is what keeps a
// late record from recreating a log after shutdown.
func (a *Auditor) Record(event AuditEvent) {
	if a == nil || !a.configured {
		return
	}
	if event.Time == "" {
		event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return
	}
	// A file that could not be reopened on an earlier record is retried here, so
	// a log that comes back is picked up again without restarting the server.
	if a.file == nil {
		a.reopen()
		if a.file == nil {
			a.lost++
			return
		}
	}
	if a.maxBytes > 0 && a.written >= a.maxBytes {
		a.rotate()
	}
	a.ensureCurrentFile()

	if a.encoder == nil {
		a.noteFailure(errors.New("the audit log has no open file"))
		a.lost++
		return
	}
	err := a.encoder.Encode(event)
	if err != nil {
		a.noteFailure(err)

		// The write failed, which is what an external log rotator or a handler
		// closed elsewhere looks like. Reopen the configured path and try this
		// record once more, so a hiccup costs nothing.
		a.reopen()
		if a.encoder == nil {
			a.lost++
			return
		}
		if retryErr := a.encoder.Encode(event); retryErr != nil {
			// Already counted as a failure by noteFailure above; this only
			// records that the record never made it.
			a.noteError(retryErr)
			a.lost++
			return
		}
		a.recovered++
	}
	a.clearFailure()

	a.written += int64(len(event.Event) + len(event.Remote) + len(event.Proxy) + len(event.Detail) + 128)
	if info, err := a.file.Stat(); err == nil {
		a.written = info.Size()
		a.fileInfo = info
	}
}

// ensureCurrentFile reopens the log when the configured path no longer names the
// file this auditor holds open. That is what a log rotator that renames the file
// and creates a new one leaves behind: without this, records keep going to the
// renamed file and the path an operator reads stays empty.
//
// A path that is gone counts as replaced. os.Rename with no replacement, or a
// plain rm to free space, leaves the handle writing into an inode that is no
// longer reachable by name, and the log at the configured path never comes back
// until the server restarts.
func (a *Auditor) ensureCurrentFile() {
	if a.file == nil || a.fileInfo == nil {
		return
	}
	live, err := os.Stat(a.path)
	if err == nil && os.SameFile(a.fileInfo, live) {
		return
	}
	if a.logger != nil {
		if err != nil {
			a.logger.Printf("audit: %s is gone; recreating it", a.path)
		} else {
			a.logger.Printf("audit: %s was replaced underneath the server; reopening it", a.path)
		}
	}
	a.reopen()
}

// reopen closes the current file and opens the configured path again. It leaves
// file and encoder nil when the path cannot be opened, which Record reports.
func (a *Auditor) reopen() {
	if a.file != nil {
		_ = a.file.Close()
		a.file = nil
	}
	a.encoder = nil
	a.fileInfo = nil

	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.noteError(err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		a.noteError(err)
		return
	}
	a.file = file
	a.fileInfo = info
	a.encoder = json.NewEncoder(file)
	a.written = info.Size()
}

// noteFailure counts one record whose first write attempt failed and reports the
// error.
func (a *Auditor) noteFailure(err error) {
	a.failures++
	a.noteError(err)
}

// noteError keeps the error and logs the transition into a failing state, so a
// repeating problem does not fill the log with one line per record.
func (a *Auditor) noteError(err error) {
	announce := a.lastError == ""
	a.lastError = err.Error()
	if announce && a.logger != nil {
		a.logger.Printf("audit: cannot write %s: %v", a.path, err)
	}
}

// clearFailure logs the recovery of a log that had been failing.
func (a *Auditor) clearFailure() {
	if a.lastError == "" {
		return
	}
	if a.logger != nil {
		a.logger.Printf("audit: %s is writable again", a.path)
	}
	a.lastError = ""
}

// rotate moves the current file aside and lets the generations shift by one, so
// path.1 is the generation that was just rotated away, path.2 the one before it,
// and the one older than the last kept generation is dropped.
func (a *Auditor) rotate() {
	if a.file == nil {
		return
	}
	keep := a.keep
	if keep < 1 {
		keep = 1
	}
	_ = a.file.Close()
	// Each rename replaces its destination, so the shift leaves no moment where a
	// kept generation is missing and the oldest one falls off the end by being
	// overwritten on the way.
	for generation := keep - 1; generation >= 1; generation-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", a.path, generation), fmt.Sprintf("%s.%d", a.path, generation+1))
	}
	if err := os.Rename(a.path, a.path+".1"); err != nil {
		// If the rename fails (for example the file is open elsewhere on
		// Windows), truncate instead so the log keeps growing from zero.
		if truncated, openErr := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); openErr == nil {
			truncated.Close()
		}
	}
	a.reopen()
}

// Close flushes and closes the log.
func (a *Auditor) Close() error {
	if a == nil || !a.Enabled() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.closed = true
	a.file = nil
	a.fileInfo = nil
	a.encoder = nil
	return err
}

// Discard is the no-op writer used by tests that only need the interface.
var Discard io.Writer = io.Discard
