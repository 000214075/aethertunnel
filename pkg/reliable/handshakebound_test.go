package reliable

import (
	"io"
	"net"
	"testing"
	"time"
)

// boundConn is a transport that accepts everything and closes cleanly. The stream
// under test sends acknowledgements (which this discards) and ends by closing it,
// which the bare discardConn used elsewhere cannot do: its embedded packetConn is
// nil.
type boundConn struct{}

func (boundConn) ReadFrom([]byte) (int, net.Addr, error)    { return 0, nil, io.EOF }
func (boundConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }
func (boundConn) SetReadDeadline(time.Time) error           { return nil }
func (boundConn) Close() error                              { return nil }

func newBoundStream(t *testing.T) *stream {
	t.Helper()
	s := newReceiveOnlyStream(0)
	s.tx = &sender{pc: boundConn{}}
	return s
}

// The repeat of the handshake acknowledgement is what keeps an unconfirmed stream
// alive: sendLocked refreshes the activity the idle timeout watches, so without a
// bound a peer that never confirmed (its acknowledgement was lost, or it has
// nothing to answer a hello-ack with) left both ends retransmitting ten datagrams
// a second forever, with the stream, its socket and its goroutines leaking.
func TestAnUnconfirmedStreamGivesUpItsHandshake(t *testing.T) {
	stream := newBoundStream(t)
	stream.helloAck = []byte("hello-ack")
	stream.lastHelloAck = time.Now().Add(-2 * punchInterval)
	stream.lastActivity = time.Now()

	// The bound is still ahead: the acknowledgement is repeated, as the peer may
	// be waiting for it.
	stream.confirmBy = time.Now().Add(time.Second)
	stream.onTimer(time.Now())
	if stream.stoppedNow() {
		t.Fatal("a stream inside its handshake window gave up")
	}

	// The bound has passed: the stream ends instead of retransmitting forever.
	stream.confirmBy = time.Now().Add(-time.Millisecond)
	stream.onTimer(time.Now())
	if !stream.stoppedNow() {
		t.Fatal("a stream whose peer never confirmed the handshake kept running")
	}
}

// The idle timeout cannot bound the repeat, because the repeat refreshes the
// activity it watches. Where the operator chose an idle timeout shorter than the
// handshake window, that is the bound.
func TestTheConfirmationWindowTakesTheShorterTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.HandshakeTimeout = 30 * time.Second
	cfg.IdleTimeout = 2 * time.Second
	before := time.Now()
	deadline := confirmDeadline(cfg)
	if deadline.IsZero() {
		t.Fatal("a stream with both timeouts set has no confirmation deadline")
	}
	if got := deadline.Sub(before); got > 3*time.Second {
		t.Fatalf("the confirmation deadline is %s away, want the two-second idle timeout", got)
	}

	cfg = testConfig()
	cfg.HandshakeTimeout = 0
	cfg.IdleTimeout = 0
	if !confirmDeadline(cfg).IsZero() {
		t.Fatal("a stream with no timeouts at all has a confirmation deadline")
	}
}

// The per-segment window check is not enough on its own: a segment is stored under
// its own starting offset, so a peer that withholds the first missing byte and sends
// an overlapping segment at every offset below the window retains
// (window / spacing) × payload bytes — hundreds of megabytes against a 256 KiB
// window. The aggregate bound is what keeps the buffer at what was advertised.
func TestTheOutOfOrderBufferStaysWithinTheWindow(t *testing.T) {
	stream := newBoundStream(t)
	const segmentSize = 1152

	for i := 1; i <= 400; i++ {
		stream.mu.Lock()
		stream.deliverLocked(segment{
			typ:     typeData,
			seq:     uint32(i),
			payload: payloadAt(uint32(i), segmentSize),
		}, time.Now())
		held := stream.oooBytes
		stream.mu.Unlock()

		if held > recvWindow {
			t.Fatalf("the out-of-order buffer holds %d bytes after %d segments, more than the %d-byte window",
				held, i, recvWindow)
		}
	}

	stream.mu.Lock()
	held, segments := stream.oooBytes, len(stream.ooo)
	stream.mu.Unlock()
	if segments == 0 {
		t.Fatal("no out-of-order segment was retained at all, so the test proves nothing")
	}
	if held > recvWindow {
		t.Fatalf("the buffer holds %d bytes across %d segments, want at most the %d-byte window",
			held, segments, recvWindow)
	}
}

// A peer that delivers one out-of-order segment and then stops leaves the gap probe
// running every 20 ms. Those probes are acknowledgements this side sends, and when
// sending refreshed the activity the idle timeout watches, the probe kept the stream
// — its socket, its read loop and its timer loop — alive for the life of the process.
// Only real traffic may keep a stream alive.
func TestAGapProbeDoesNotKeepADeadPeerAlive(t *testing.T) {
	stream := newBoundStream(t)
	stream.cfg.IdleTimeout = 100 * time.Millisecond

	// One segment the receive buffer cannot place, so the gap probe has work.
	stream.mu.Lock()
	stream.deliverLocked(segment{
		typ:     typeData,
		seq:     4096,
		payload: payloadAt(4096, 64),
	}, time.Now())
	start := time.Now()
	stream.lastActivity = start
	stream.mu.Unlock()

	if len(stream.ooo) == 0 {
		t.Fatal("the segment was placed in order, so no gap is being probed")
	}

	// The peer is gone: nothing arrives, and the timer keeps firing on the wall
	// clock, which is what lets a probe that refreshes the activity mask the idle
	// timeout for good.
	deadline := time.Now().Add(10 * stream.cfg.IdleTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(gapProbeInterval)
		stream.onTimer(time.Now())
		if stream.stoppedNow() {
			return
		}
	}
	t.Fatal("the gap probe kept a stream alive whose peer had stopped sending")
}

// The receive buffer holds what the application has not read, and the window
// advertised to the peer is derived from it. The per-segment check rejects one
// segment larger than the window but lets a peer that simply ignores the window
// keep appending at the contiguous end, so the buffer grew without bound.
func TestTheInOrderReceiveBufferStaysWithinTheWindow(t *testing.T) {
	stream := newBoundStream(t)
	const segmentSize = 1152

	seq := stream.rcvBase
	for i := 0; i < 2000; i++ {
		stream.mu.Lock()
		stream.deliverLocked(segment{
			typ:     typeData,
			seq:     seq,
			payload: payloadAt(seq, segmentSize),
		}, time.Now())
		held := stream.recvBuf.len()
		stream.mu.Unlock()
		seq += segmentSize

		if held > recvWindow {
			t.Fatalf("the in-order receive buffer holds %d bytes after %d segments, more than the %d-byte window",
				held, i+1, recvWindow)
		}
	}

	stream.mu.Lock()
	held := stream.recvBuf.len()
	stream.mu.Unlock()
	if held == 0 {
		t.Fatal("nothing was retained at all, so the test proves nothing")
	}
	if held > recvWindow {
		t.Fatalf("the buffer holds %d bytes, want at most the %d-byte window", held, recvWindow)
	}
}
