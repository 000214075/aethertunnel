package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestAFirstFrameThatCannotBeReadIsCountedAndAudited covers the one refusal that
// used to leave no record anywhere but the log.
//
// Every other way a connection is turned away before its handshake finishes is
// booked: admit() for a ban, the allow/deny lists and the rate limit,
// refuseFirstFrame for a first frame that parses but cannot start a connection, and
// the authentication counters for credentials that do not check out. A first frame
// that cannot be read at all — which is what a disguise, encryption or TLS mismatch
// looks like from here, and also what a peer that connects and goes away looks like —
// was written to the log and nowhere else. A fleet whose passphrase had been changed
// on one end therefore appeared in GET /metrics and in the audit trail as nothing at
// all.
func TestAFirstFrameThatCannotBeReadIsCountedAndAudited(t *testing.T) {
	dir := t.TempDir()
	auditPath := dir + "/audit.jsonl"

	cfg := disguisedConfig(t, obfs.DisguiseTLSRecord)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = auditPath
	// A peer that sends nothing is only counted once the server stops waiting for
	// it, so keep that wait short here and well inside the deadline the reads below
	// use; otherwise the two would expire together and the assertions would race.
	cfg.Server.HandshakeTimeoutSecs = 1
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	metrics := rs.server.metrics
	failedBefore := metrics.handshakeFailures.Load()
	rejectedBefore := metrics.controlRejected.Load()

	// A client that did not wrap its frames writes this program's own frame header,
	// so the first byte is the message type rather than the 0x17 that starts an
	// application-data record.
	dialAndClose := func(write func(*protocol.Framer) error) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if write != nil {
			if err := write(protocol.NewFramer(conn, nil, 0)); err != nil {
				t.Fatalf("write the first frame: %v", err)
			}
		}
		// The connection is closed without an answer, so waiting for the read to
		// fail is what makes the counters safe to read afterwards.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatalf("the server wrote %d byte(s) to a connection whose first frame it could not read", n)
		}
	}

	// waitForCounter polls, because the handler that books the failure runs on the
	// connection's own goroutine.
	waitForCounter := func(read func() int64, want int64, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if got := read(); got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%s is %d, want %d", what, read(), want)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	dialAndClose(func(framer *protocol.Framer) error {
		return framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
			Token: testToken, ClientVersion: "without-the-disguise",
			Protocol: protocol.ProtocolVersion,
		})
	})
	waitForCounter(func() int64 { return metrics.handshakeFailures.Load() }, failedBefore+1,
		"the handshake-failure counter after an undecodable first frame")
	// A connection refused this way is closed without an answer, but it is still a
	// connection the control port turned away, which is what the aggregate counts
	// (admit()'s refusals are closed without an answer too).
	waitForCounter(func() int64 { return metrics.controlRejected.Load() }, rejectedBefore+1,
		"the rejection counter after an undecodable first frame")

	// A peer that connects and sends nothing looks the same from here.
	dialAndClose(nil)
	waitForCounter(func() int64 { return metrics.handshakeFailures.Load() }, failedBefore+2,
		"the handshake-failure counter after a peer that sent nothing")
	if got := metrics.controlAccepted.Load(); got != 0 {
		t.Errorf("the accepted counter is %d, want 0", got)
	}

	events := waitForAuditEvents(t, auditPath, 2)
	recorded := 0
	namedDisguise := false
	for _, event := range events {
		if event.Event != EventHandshakeFailed {
			continue
		}
		recorded++
		if event.Outcome != "denied" {
			t.Errorf("the audit record for a failed handshake says outcome %q, want denied",
				event.Outcome)
		}
		if strings.Contains(event.Detail, "record-framed") {
			namedDisguise = true
		}
	}
	if recorded != 2 {
		t.Errorf("the audit log holds %d handshake_failed record(s), want 2", recorded)
	}
	// The detail is what makes the reason attributable, which is the whole point of
	// recording a connection that was never answered.
	if !namedDisguise {
		t.Error("no audit record names the disguise mismatch as the reason")
	}
}
