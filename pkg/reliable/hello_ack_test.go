package reliable

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// recordingConn is a packetConn that keeps what the stream sends, so a test can
// see whether an acknowledgement went out without a socket or a peer.
type recordingConn struct {
	packetConn
	mu   sync.Mutex
	sent [][]byte
}

func (c *recordingConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, append([]byte(nil), b...))
	return len(b), nil
}

func (c *recordingConn) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// One lost acknowledgement used to leave the peer repeating its hello-ack until
// confirmBy failed a stream that was already carrying data: the side that had
// confirmed answered a hello-ack with nothing. It answers (rate-limited) now, so
// the peer confirms instead of failing.
func TestAConfirmedPeerAnswersAHelloAck(t *testing.T) {
	recorder := &recordingConn{}
	c := newReceiveOnlyStream(0)
	c.tx = &sender{pc: recorder}

	local := bytes.Repeat([]byte{0x11}, nonceSize)
	peer := bytes.Repeat([]byte{0x22}, nonceSize)
	ack := buildHelloAck(c.token, local, peer, c.session)
	c.helloAck = ack
	c.confirmed = true

	c.handleDatagram(ack)
	if got := recorder.sentCount(); got != 1 {
		t.Fatalf("a confirmed peer answered a hello-ack %d times, want once", got)
	}
	recorder.mu.Lock()
	answer := recorder.sent[0]
	recorder.mu.Unlock()
	if !bytes.Equal(answer, ack) {
		t.Fatal("the answer is not this side's hello-ack")
	}

	// The answer is rate-limited: the peer repeats at its own cadence, and one
	// reply per interval is enough to confirm it without echoing every datagram.
	c.handleDatagram(ack)
	if got := recorder.sentCount(); got != 1 {
		t.Fatalf("a second hello-ack drew a reply inside the interval (%d in total)", got)
	}
}

// The byte bound does not bound how many out-of-order entries the map holds: the
// peer chooses the segment sizes, so it can fill the window with one-byte
// segments, and appendInOrderLocked walks the whole map for every in-order
// segment it appends. That was work the peer chose, on a connection one punch
// exists to carry.
func TestTheOutOfOrderBufferIsBoundedInEntriesNotJustBytes(t *testing.T) {
	c := newReceiveOnlyStream(1000)
	now := time.Now()
	for i := 0; i < maxOutOfOrderSegments*2; i++ {
		// Every segment starts past the hole at 1000, and the byte bound of the
		// window is never reached, so only the entry cap can stop this.
		c.deliverLocked(segment{
			typ:     typeData,
			session: c.session,
			seq:     uint32(1002 + i),
			payload: []byte{byte(i)},
		}, now)
	}
	if len(c.ooo) > maxOutOfOrderSegments {
		t.Fatalf("%d out-of-order segments are buffered, the cap is %d", len(c.ooo), maxOutOfOrderSegments)
	}
	if c.oooBytes > recvWindow {
		t.Fatalf("%d buffered bytes, more than the window %d", c.oooBytes, recvWindow)
	}
}
