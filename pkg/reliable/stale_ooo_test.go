package reliable

import (
	"bytes"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

// discardConn is a packetConn with nowhere to send: the receive path under test
// only needs a transport to hand its acknowledgements to.
type discardConn struct{ packetConn }

func (discardConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }

// newReceiveOnlyStream builds a stream whose next expected byte is base, so
// segments can be handed to deliverLocked one at a time. No Socket is involved:
// these cases are about how the receive buffer accounts for what it holds.
func newReceiveOnlyStream(base uint32) *stream {
	return &stream{
		tx:      &sender{pc: discardConn{}},
		peer:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65000},
		token:   []byte("stale-out-of-order"),
		logger:  log.New(io.Discard, "", 0),
		cfg:     testConfig(),
		session: 1,
		done:    make(chan struct{}),
		wake:    make(chan struct{}),
		ooo:     make(map[uint32][]byte),
		rcvBase: base,
	}
}

// payloadAt builds the stream's bytes for [off, off+n). Two segments whose
// ranges overlap therefore carry the same bytes where they overlap, as they do
// on the wire, so the expected result does not depend on which copy was kept.
func payloadAt(off uint32, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((off + uint32(i)) % 251)
	}
	return b
}

// takeBuffered reads the receive buffer the way the caller's Read would, minus
// the blocking parts.
func (c *stream) takeBuffered() []byte {
	out := make([]byte, c.recvBuf.len())
	c.recvBuf.read(out)
	return out
}

// keysOf lists the sequence numbers the out-of-order map is holding, for a
// failure message that says what is stuck.
func (c *stream) keysOf() []uint32 {
	keys := make([]uint32, 0, len(c.ooo))
	for key := range c.ooo {
		keys = append(keys, key)
	}
	return keys
}

func (c *stream) checkNothingStuck(t *testing.T, want []byte) {
	t.Helper()
	if c.oooBytes != 0 || len(c.ooo) != 0 {
		t.Errorf("the buffer still accounts for %d bytes in %d out-of-order segment(s) "+
			"whose bytes have been delivered; keys=%v", c.oooBytes, len(c.ooo), c.keysOf())
	}
	if got := c.takeBuffered(); !bytes.Equal(got, want) {
		t.Errorf("the reader got %d bytes, want %d (first difference at byte %d)",
			len(got), len(want), firstDiff(got, want))
	}
}

// TestARetransmissionInDifferentPiecesLeavesNothingStuck covers the case where
// the segment that fills a hole does not end where the buffered one did. The
// buffered segment at 1100 covers 1100..1500; the retransmission delivers
// 1000..1300, so the first 200 bytes of it have now reached the reader and the
// remaining 200 still belong to the stream. A receive buffer that keeps the
// whole entry keyed at 1100 can never use it again — the hole is past it — so
// those bytes are held until the stream closes, and the window advertised to the
// peer stays smaller by that much for the rest of the connection.
func TestARetransmissionInDifferentPiecesLeavesNothingStuck(t *testing.T) {
	c := newReceiveOnlyStream(1000)
	now := time.Now()

	held := payloadAt(1100, 400) // 1100..1500, out of order
	c.deliverLocked(segment{typ: typeData, seq: 1100, payload: held}, now)
	if c.oooBytes != len(held) {
		t.Fatalf("the out-of-order segment was not buffered: oooBytes=%d, want %d",
			c.oooBytes, len(held))
	}

	c.deliverLocked(segment{typ: typeData, seq: 1000, payload: payloadAt(1000, 300)}, now)

	c.checkNothingStuck(t, payloadAt(1000, 500))
}

// TestABufferedSegmentThatArrivedAgainIsDropped is the same case with no bytes
// left over: the retransmission covers the buffered segment completely, so the
// entry has to go. It is the smallest form of the leak — one entry, held until
// the stream closes, counted against the window the whole time.
func TestABufferedSegmentThatArrivedAgainIsDropped(t *testing.T) {
	c := newReceiveOnlyStream(1000)
	now := time.Now()

	c.deliverLocked(segment{typ: typeData, seq: 1100, payload: payloadAt(1100, 100)}, now)
	covering := payloadAt(1000, 300) // 1000..1300, contains 1100..1200
	c.deliverLocked(segment{typ: typeData, seq: 1000, payload: covering}, now)

	c.checkNothingStuck(t, covering)
}

// TestTwoBufferedPiecesThatMeetAtTheHoleKeepTheLongerOne covers the case where
// closing a hole makes a trimmed remainder start exactly where another buffered
// segment already starts: 1100..1500 and 1300..1600 are both held, and the hole
// closes at 1300. The two entries describe the same offsets from 1300 on, so one
// of them has to win, and it has to be the one that reaches further: keeping the
// shorter one would silently drop bytes the peer has already sent and, having
// acknowledged them, will not send again.
func TestTwoBufferedPiecesThatMeetAtTheHoleKeepTheLongerOne(t *testing.T) {
	c := newReceiveOnlyStream(1000)
	now := time.Now()

	c.deliverLocked(segment{typ: typeData, seq: 1100, payload: payloadAt(1100, 400)}, now)
	c.deliverLocked(segment{typ: typeData, seq: 1300, payload: payloadAt(1300, 300)}, now)
	if c.oooBytes != 700 {
		t.Fatalf("the two out-of-order segments were not buffered: oooBytes=%d, want 700",
			c.oooBytes)
	}

	c.deliverLocked(segment{typ: typeData, seq: 1000, payload: payloadAt(1000, 300)}, now)

	c.checkNothingStuck(t, payloadAt(1000, 600))
}
