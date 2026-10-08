package reliable

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A data acknowledgement and a FIN acknowledgement carry the same offset: a FIN
// takes no sequence position on the wire, so the peer confirms the last data byte
// and the FIN with one number. finAcked is therefore set without the peer ever
// seeing the FIN, and the retransmission branch that would recover a lost FIN used
// to be suppressed by it — leaving the peer's Read blocked until its idle timeout.
// The close now retransmits the FIN a bounded number of times regardless.
func TestALostFinIsRetransmittedAfterADataAcknowledgementSatisfiesIt(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: recorder}
	c.cfg.RetransmitLimit = DefaultRetransmitLimit

	now := time.Now()
	c.mu.Lock()
	c.sentFin = true
	c.finSeq = 0
	c.finSent = now.Add(-time.Second) // due long ago
	c.finRTO = baseRTO
	c.finAcked = true // a data acknowledgement just appeared to cover the FIN
	c.lastActivity = now
	c.mu.Unlock()

	c.onTimer(now)

	if recorder.sentCount() == 0 {
		t.Fatal("a FIN that only a data acknowledgement appeared to cover was never retransmitted; " +
			"a lost FIN would leave the peer reading until its idle timeout")
	}
	recorder.mu.Lock()
	sent := recorder.sent[0]
	recorder.mu.Unlock()
	seg, ok := parseSegment(c.token, sent)
	if !ok || seg.typ != typeFin {
		t.Fatalf("the retransmission was not an authenticated FIN: ok=%v type=%v", ok, seg.typ)
	}
}

// The retransmissions are bounded: once the floor is past and the FIN is
// acknowledged, a closed stream stops emitting, so a half-close cannot add traffic
// for the rest of a long-lived session.
func TestAnAcknowledgedFinStopsBeingRetransmittedPastTheFloor(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: recorder}
	c.cfg.RetransmitLimit = DefaultRetransmitLimit

	now := time.Now()
	c.mu.Lock()
	c.sentFin = true
	c.finSeq = 0
	c.finSent = now.Add(-time.Second)
	c.finRTO = baseRTO
	c.finAcked = true
	c.finTries = finRetransmitFloor
	c.lastActivity = now
	c.mu.Unlock()

	c.onTimer(now)

	if got := recorder.sentCount(); got != 0 {
		t.Fatalf("an acknowledged FIN past the floor was retransmitted %d time(s)", got)
	}
}

// Exchange and establish disagree about a socket that has been closed: establish
// reports net.ErrClosed, and a caller that checks with errors.Is has to get the
// same answer from Exchange rather than "already carrying a connection".
func TestExchangeOnAClosedSocketReportsClosed(t *testing.T) {
	s := listenSocket(t, testConfig(), nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, _, err := s.Exchange(context.Background(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9},
		[]byte("payload"), func([]byte) bool { return true })
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Exchange on a closed socket returned %v, want net.ErrClosed", err)
	}
}
