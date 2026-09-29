package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// sendFirstFrame dials the control port, sends one frame as the first frame of the
// connection, and returns the answer the server wrote before closing it.
func sendFirstFrame(t *testing.T, serverAddr string, msg protocol.Message) []byte {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	if err := framer.WriteFrame(&msg); err != nil {
		t.Fatalf("write the first frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	answer, err := framer.ReadFrame()
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return answer.Payload
}

// TestAMalformedVisitorOrDataFrameIsCountedAsUnusable covers the two first-frame
// payloads that were invisible on the series that names this reason.
//
// Three frame types may start a connection — an auth request, a visitor-connect and a
// data-open — and all three are dispatched before anything is authenticated, so a
// payload that does not parse is a garbage frame aimed at the port whichever type it
// claims to be. Only the auth request was counted here, which left a scan that used
// the other two types showing up as nothing on the series an operator reads to tell
// garbage apart from a policy decision.
func TestAMalformedVisitorOrDataFrameIsCountedAsUnusable(t *testing.T) {
	dir := t.TempDir()
	auditPath := dir + "/audit.jsonl"

	cfg := testConfig(t, false)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = auditPath
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	metrics := rs.server.metrics
	unusableBefore := metrics.unusableFrames.Load()
	rejectedBefore := metrics.controlRejected.Load()

	// A visitor-connect whose payload is not JSON.
	answer := sendFirstFrame(t, rs.addr, protocol.Message{
		Type: protocol.TypeVisitorConnect, Payload: []byte("this is not json"),
	})
	if len(answer) == 0 {
		t.Error("the malformed visitor-connect was closed without an answer")
	}
	if got := metrics.unusableFrames.Load(); got != unusableBefore+1 {
		t.Errorf("the unusable-first-frame counter is %d after a malformed visitor-connect, want %d",
			got, unusableBefore+1)
	}
	// It is answered as a refusal of a connection on the control port, so the
	// aggregate counter sees it too.
	if got := metrics.controlRejected.Load(); got != rejectedBefore+1 {
		t.Errorf("the rejection counter is %d after a malformed visitor-connect, want %d",
			got, rejectedBefore+1)
	}
	// And it leaves the same audit record every other refusal of a visitor does.
	audited := false
	for _, event := range waitForAuditEvents(t, auditPath, 1) {
		if event.Event == EventVisitorRejected && strings.Contains(event.Detail, "malformed visitor-connect") {
			audited = true
		}
	}
	if !audited {
		t.Error("the malformed visitor-connect is not in the audit log")
	}

	// A data-open whose payload is not JSON.
	answer = sendFirstFrame(t, rs.addr, protocol.Message{
		Type: protocol.TypeDataOpen, Payload: []byte("this is not json"),
	})
	if len(answer) == 0 {
		t.Error("the malformed data-open was closed without an answer")
	}
	if got := metrics.unusableFrames.Load(); got != unusableBefore+2 {
		t.Errorf("the unusable-first-frame counter is %d after a malformed data-open too, want %d",
			got, unusableBefore+2)
	}
	// A data connection is not a control connection: it never became a session, so it
	// stays out of the aggregate the control path's refusals move.
	if got := metrics.controlRejected.Load(); got != rejectedBefore+1 {
		t.Errorf("the rejection counter is %d after a malformed data-open, want it to stay at %d",
			got, rejectedBefore+1)
	}
	if got := metrics.controlAccepted.Load(); got != 0 {
		t.Errorf("the accepted counter is %d, want 0", got)
	}
	if got := metrics.dataConnections.Load(); got != 0 {
		t.Errorf("the data-connection counter is %d after a frame that never opened a stream, want 0", got)
	}
}
