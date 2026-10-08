package reliable

import (
	"testing"
	"time"
)

// closeableRecorder is a recordingConn whose Close succeeds: Close tears the
// transport down at the end, and this case has to reach that step.
type closeableRecorder struct {
	*recordingConn
}

func (closeableRecorder) Close() error { return nil }

// A FIN takes no sequence position on the wire, so the acknowledgement for the
// last data byte sets finAcked with the FIN itself never read. Close used to
// treat that as completion and release the transport at once, which left a lost
// FIN sent once and then abandoned — the peer's Read stayed blocked until its
// idle timeout. The close path now retransmits the FIN the full floor while the
// transport is still open.
func TestCloseRetransmitsALostFinBeforeItReleasesTheTransport(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: closeableRecorder{recordingConn: recorder}}
	c.cfg.RetransmitLimit = DefaultRetransmitLimit

	c.mu.Lock()
	// The peer acknowledged the last data byte, which carries the same offset
	// as the FIN: finAcked says nothing about whether the FIN arrived.
	c.finAcked = true
	c.mu.Unlock()

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The retransmissions are sent up front, so an acknowledged close does not
	// wait out the floor's backoff: Close is on the data path.
	elapsed := time.Since(start)
	if elapsed >= closeWait {
		t.Fatalf("Close waited %s after an acknowledged FIN, want less than closeWait (%s)", elapsed, closeWait)
	}
	t.Logf("Close returned %s after an acknowledged FIN; closeWait is %s", elapsed, closeWait)

	// One transmission from sendFinLocked plus the floor of retransmissions.
	if got := recorder.sentCount(); got < 1+finRetransmitFloor {
		t.Fatalf("Close released the transport after %d FIN transmission(s), want at least %d: "+
			"a lost FIN then leaves the peer reading until its idle timeout",
			got, 1+finRetransmitFloor)
	}
	recorder.mu.Lock()
	last := recorder.sent[len(recorder.sent)-1]
	recorder.mu.Unlock()
	seg, ok := parseSegment(c.token, last)
	if !ok || seg.typ != typeFin {
		t.Fatalf("the last transmission was not an authenticated FIN: ok=%v type=%v", ok, seg.typ)
	}
}
