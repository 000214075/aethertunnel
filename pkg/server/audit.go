package server

import (
	"encoding/json"
	"io"
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
type Auditor struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	maxBytes int64
	written  int64
	encoder  *json.Encoder
}

// NewAuditor opens the audit log. A disabled configuration returns an auditor
// that writes nowhere.
func NewAuditor(enabled bool, path string, maxBytes int64) (*Auditor, error) {
	if !enabled {
		return &Auditor{}, nil
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
		file:     file,
		path:     path,
		maxBytes: maxBytes,
		written:  info.Size(),
		encoder:  json.NewEncoder(file),
	}, nil
}

// Enabled reports whether records are written anywhere.
func (a *Auditor) Enabled() bool { return a != nil && a.file != nil }

// Record appends one event.
func (a *Auditor) Record(event AuditEvent) {
	if !a.Enabled() {
		return
	}
	if event.Time == "" {
		event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.maxBytes > 0 && a.written >= a.maxBytes {
		a.rotate()
	}
	if a.encoder == nil {
		return
	}
	if err := a.encoder.Encode(event); err != nil {
		// An unwritable audit log must not take the tunnel down; the error is
		// surfaced through the dashboard's error banner on the next scrape of
		// the file size instead of panicking here.
		return
	}
	a.written += int64(len(event.Event) + len(event.Remote) + len(event.Proxy) + len(event.Detail) + 128)
	if info, err := a.file.Stat(); err == nil {
		a.written = info.Size()
	}
}

// rotate moves the current file aside, keeping one previous generation.
func (a *Auditor) rotate() {
	if a.file == nil {
		return
	}
	_ = a.file.Close()
	if err := os.Rename(a.path, a.path+".1"); err != nil {
		// If the rename fails (for example the file is open elsewhere on
		// Windows), truncate instead so the log keeps growing from zero.
		if truncated, openErr := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); openErr == nil {
			truncated.Close()
		}
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.file = nil
		a.encoder = nil
		return
	}
	a.file = file
	a.encoder = json.NewEncoder(file)
	a.written = 0
}

// Close flushes and closes the log.
func (a *Auditor) Close() error {
	if !a.Enabled() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.Close()
}

// Discard is the no-op writer used by tests that only need the interface.
var Discard io.Writer = io.Discard
