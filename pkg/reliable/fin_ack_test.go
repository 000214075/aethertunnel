package reliable

import (
	"bytes"
	"testing"
	"time"
)

// A FIN carries the acknowledgement for everything the sender has received, like
// a data segment does. Dropping that field left the last segment of a simultaneous
// close unacknowledged: with the peer's own ack lost, the FIN was the only answer
// that named it, so the sender retransmitted until the limit failed a stream the
// peer had already received.
func TestAFinAcknowledgesTheSegmentsItNames(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: recorder}
	c.peerWindow = recvWindow

	payload := []byte("the last segment of the close")
	c.unacked = []*outSeg{{seq: 0, data: payload, sent: time.Now()}}
	c.sndNxt = uint32(len(payload))
	c.sndUna = 0

	c.handleDatagram(marshalSegment(nil, c.token, segment{
		typ:     typeFin,
		session: c.session,
		seq:     uint32(len(payload)),
		ack:     uint32(len(payload)),
		window:  uint16(recvWindow / windowScale),
	}))

	if len(c.unacked) != 0 {
		t.Fatalf("%d segment(s) are still unacknowledged after the FIN that names them", len(c.unacked))
	}
	if c.sndUna != uint32(len(payload)) {
		t.Fatalf("the acknowledged offset is %d, want %d", c.sndUna, len(payload))
	}
}

// A hello is answered once per punch interval, like a repeated hello-ack: every
// hello used to draw an ack of its own, so a peer that flooded hellos — or one
// whose acks kept being lost — made this side answer every datagram it received.
func TestHelloRepeatsAreAnsweredAtMostOncePerInterval(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: recorder}

	local := bytes.Repeat([]byte{0x33}, nonceSize)
	peer := bytes.Repeat([]byte{0x44}, nonceSize)
	c.helloAck = buildHelloAck(c.token, local, peer, c.session)
	hello := buildHello(c.token, peer)

	c.handleDatagram(hello)
	if got := recorder.sentCount(); got != 1 {
		t.Fatalf("the first hello drew %d answers, want one", got)
	}
	// A flood inside the interval is not answered one for one.
	for i := 0; i < 10; i++ {
		c.handleDatagram(hello)
	}
	if got := recorder.sentCount(); got != 1 {
		t.Fatalf("ten hellos inside the interval drew %d answers in total, want one", got)
	}
}
