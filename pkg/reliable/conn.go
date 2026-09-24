package reliable

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// Internal protocol timing and sizing.
const (
	// baseRTO is the first retransmission timeout for a segment; it doubles per
	// retransmission up to maxRTO.
	baseRTO = 200 * time.Millisecond
	maxRTO  = 2 * time.Second
	// ackDelay batches acknowledgements for a single received segment.
	ackDelay = 10 * time.Millisecond
	// closeWait bounds how long Close waits for the peer to acknowledge the FIN.
	closeWait = 750 * time.Millisecond
	// timerInterval is how often the timer goroutine looks for due work.
	timerInterval = 20 * time.Millisecond
	// recvWindow is the receive buffer size. Only maxWindow*windowScale bytes of
	// it can be advertised at once, which is well above the default send window.
	recvWindow = 256 * 1024
	// windowStep is how much free space must appear before reading triggers a
	// window update on its own.
	windowStep = 32 * 1024
	// compactMin is the smallest consumed prefix worth moving to the front of the
	// receive buffer.
	compactMin = 4096
	// dupAckLimit is how many duplicate acknowledgements stand in for an RTO
	// when a segment in the middle of the window is missing.
	dupAckLimit = 2
	// fastRetransmitDelay is the shortest interval between two retransmissions
	// of the same segment triggered by acknowledgements.
	fastRetransmitDelay = 10 * time.Millisecond
	// fastRetransmitTries bounds how often one segment is resent in response to
	// acknowledgements before only the timeout path may resend it.
	fastRetransmitTries = 4
	// gapProbeInterval is how often an acknowledgement is repeated while the
	// receive buffer holds data behind a gap.
	gapProbeInterval = 20 * time.Millisecond
	// readFailLimit is how many transport read errors in a row are tolerated
	// before the connection is treated as broken.
	readFailLimit = 64
)

// errDeadline is reported by Read and Write once a deadline has passed. It wraps
// os.ErrDeadlineExceeded, so it is a net.Error whose Timeout reports true.
var errDeadline = fmt.Errorf("reliable: i/o deadline exceeded: %w", os.ErrDeadlineExceeded)

// outSeg is a segment that has been sent and is waiting to be acknowledged.
// rto is this segment's own retransmission timeout, which doubles per attempt so
// one stalled segment cannot make a fresh one wait longer than baseRTO.
type outSeg struct {
	seq       uint32
	data      []byte
	sent      time.Time
	rto       time.Duration
	retries   int
	fastTries int
}

// stream is the net.Conn handed to callers by Punch and Accept. It owns the
// transport for its lifetime.
type stream struct {
	tx     *sender
	peer   net.Addr
	local  net.Addr
	token  []byte
	logger *log.Logger
	cfg    Config

	segSize  int
	session  uint32
	helloAck []byte

	done chan struct{}
	wake chan struct{}

	// wmu serialises whole Write calls, so concurrent writers cannot interleave
	// in the middle of the byte stream. mu guards the fields below; sendMu, held
	// by tx, is always taken last.
	wmu sync.Mutex
	mu  sync.Mutex

	localClosed bool
	stopped     bool
	cerr        error

	recvBuf     recvQueue
	rcvBase     uint32
	ooo         map[uint32][]byte
	oooBytes    int
	oooHigh     uint32
	ackedWindow uint16
	haveFin     bool
	finSeq      uint32

	sndUna    uint32
	sndNxt    uint32
	unacked   []*outSeg
	sentFin   bool
	finAcked  bool
	finSent   time.Time
	finRTO    time.Duration
	finTries  int
	lastAckAt uint32
	dupAcks   int

	peerWindow int

	ackPending   bool
	ackDue       time.Time
	pendingSegs  int
	confirmed    bool
	lastHelloAck time.Time
	lastAckSent  time.Time
	lastActivity time.Time

	readDeadline  time.Time
	writeDeadline time.Time
}

var _ net.Conn = (*stream)(nil)

// recvQueue is the sliding receive buffer. Read consumes from the front, and
// in-order segments are appended at the back.
type recvQueue struct {
	buf []byte
	off int
}

func (q *recvQueue) len() int { return len(q.buf) - q.off }

func (q *recvQueue) read(p []byte) int {
	n := copy(p, q.buf[q.off:])
	q.off += n
	switch {
	case q.off == len(q.buf):
		q.buf = q.buf[:0]
		q.off = 0
	case q.off >= compactMin && q.off*2 >= len(q.buf):
		q.compact()
	}
	return n
}

func (q *recvQueue) append(p []byte) {
	if len(p) == 0 {
		return
	}
	q.buf = append(q.buf, p...)
}

// compact moves the unconsumed tail to the front so appends can reuse the
// capacity the reads freed.
func (q *recvQueue) compact() {
	n := copy(q.buf, q.buf[q.off:])
	q.buf = q.buf[:n]
	q.off = 0
}

// Read implements net.Conn. It returns io.EOF once the peer's FIN and every byte
// before it have been delivered, and net.ErrClosed once the stream is closed
// locally.
func (c *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	for {
		if c.localClosed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if n := c.recvBuf.read(p); n > 0 {
			c.rcvBase += uint32(n)
			c.maybeWindowUpdateLocked()
			c.mu.Unlock()
			return n, nil
		}
		if c.haveFin && seqDiff(c.rcvBase, c.finSeq) >= 0 {
			c.mu.Unlock()
			return 0, io.EOF
		}
		if c.cerr != nil {
			err := c.cerr
			c.mu.Unlock()
			return 0, err
		}
		if c.stopped {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if err := c.park(c.wake, c.readDeadline); err != nil {
			c.mu.Unlock()
			return 0, err
		}
	}
}

// Write implements net.Conn. Each call is copied into the send window, so the
// buffer stays the caller's; concurrent calls are delivered in the order they
// take the write lock, each as an uninterrupted run of bytes.
func (c *stream) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > c.segSize {
			n = c.segSize
		}
		c.mu.Lock()
		if err := c.readyLocked(c.writeDeadline); err != nil {
			c.mu.Unlock()
			return written, err
		}
		for {
			inflight := int(seqDiff(c.sndNxt, c.sndUna))
			// One segment is always allowed through, so a peer that advertises a
			// window smaller than a segment cannot deadlock the pair.
			if len(c.unacked) == 0 ||
				(len(c.unacked) < c.cfg.SendWindow && inflight+n <= int(c.peerWindow)) {
				break
			}
			if err := c.park(c.wake, c.writeDeadline); err != nil {
				c.mu.Unlock()
				return written, err
			}
			if err := c.readyLocked(c.writeDeadline); err != nil {
				c.mu.Unlock()
				return written, err
			}
		}
		seg := &outSeg{seq: c.sndNxt, data: append([]byte(nil), p[:n]...), sent: time.Now(), rto: baseRTO}
		c.unacked = append(c.unacked, seg)
		c.sndNxt += uint32(n)
		c.lastActivity = time.Now()
		buf := c.envelopeLocked(typeData, seg.seq, seg.data)
		c.mu.Unlock()

		c.tx.writeTo(buf, c.peer)

		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite implements the half-close that net.Conn does not have: it sends the FIN that
// tells the peer the byte stream is complete, and leaves the connection open so the peer's
// reply still arrives. A relay ends each direction with CloseWrite when the connection has
// one and closes the whole stream otherwise, so without this method a direct (punched) path
// drops the reply that a service only sends after it has seen the end of the request — the
// same truncation the relayed paths had.
//
// It is idempotent, and a Close afterwards still tears the connection down.
func (c *stream) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil
	}
	c.sendFinLocked()
	return nil
}

// sendFinLocked queues the FIN once. mu must be held.
func (c *stream) sendFinLocked() {
	if c.sentFin {
		return
	}
	c.sentFin = true
	c.finSeq = c.sndNxt
	c.finSent = time.Now()
	c.finRTO = baseRTO
	c.sendLocked(c.envelopeLocked(typeFin, c.finSeq, nil))
	c.signal()
}

// Close implements net.Conn. It sends FIN, waits briefly for the peer to
// acknowledge it, then releases the transport. Further calls return nil.
func (c *stream) Close() error {
	c.mu.Lock()
	if !c.localClosed {
		c.localClosed = true
		c.sendFinLocked()
	}
	c.mu.Unlock()

	// Keeping the transport open for a moment lets the timer goroutine
	// retransmit the FIN and any segment still in flight, so the last bytes of a
	// short write are not dropped by an immediate teardown.
	deadline := time.Now().Add(closeWait)
	for {
		c.mu.Lock()
		done := c.finAcked || c.haveFin || c.cerr != nil || c.stopped
		if done {
			c.mu.Unlock()
			break
		}
		w := c.wake
		c.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-w:
			timer.Stop()
		case <-timer.C:
		}
	}

	c.shutdown(nil)
	return nil
}

func (c *stream) LocalAddr() net.Addr  { return c.local }
func (c *stream) RemoteAddr() net.Addr { return c.peer }

func (c *stream) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.signal()
	c.mu.Unlock()
	return nil
}

func (c *stream) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.signal()
	c.mu.Unlock()
	return nil
}

func (c *stream) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.signal()
	c.mu.Unlock()
	return nil
}

// park releases mu while it waits for a state change and reacquires it before
// returning. It reports a timeout once deadline passes, and nil when signalled.
func (c *stream) park(w chan struct{}, deadline time.Time) error {
	var (
		timer   *time.Timer
		timeout <-chan time.Time
	)
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errDeadline
		}
		timer = time.NewTimer(remaining)
		timeout = timer.C
	}
	c.mu.Unlock()
	var err error
	select {
	case <-w:
	case <-timeout:
		err = errDeadline
	}
	c.mu.Lock()
	if timer != nil {
		timer.Stop()
	}
	return err
}

// signal wakes every blocked call. mu must be held.
func (c *stream) signal() {
	close(c.wake)
	c.wake = make(chan struct{})
}

// readyLocked reports a finished connection or an expired deadline. mu must be
// held.
func (c *stream) readyLocked(deadline time.Time) error {
	switch {
	case c.cerr != nil:
		return c.cerr
	case c.localClosed:
		return net.ErrClosed
	case !deadline.IsZero() && !time.Now().Before(deadline):
		return errDeadline
	}
	return nil
}

// stopLocked marks the connection finished and releases the transport, which
// unblocks the read loop. mu must be held.
func (c *stream) stopLocked() {
	if c.stopped {
		return
	}
	c.stopped = true
	close(c.done)
	c.signal()
	_ = c.tx.pc.Close()
}

func (c *stream) shutdown(err error) {
	c.mu.Lock()
	if err != nil && c.cerr == nil {
		c.cerr = err
	}
	c.stopLocked()
	c.mu.Unlock()
}

// failLocked records the terminal error and tears the connection down.
func (c *stream) failLocked(err error) {
	if c.cerr == nil {
		c.cerr = err
	}
	if c.logger != nil {
		c.logger.Printf("reliable: %s -> %s: %v", c.local, c.peer, c.cerr)
	}
	c.stopLocked()
}

func (c *stream) stoppedNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// readLoop consumes datagrams until the connection ends. It is the only reader
// of the transport.
func (c *stream) readLoop() {
	_ = c.tx.pc.SetReadDeadline(time.Time{})
	buf := make([]byte, c.cfg.MTU)
	failures := 0
	for {
		n, _, err := c.tx.pc.ReadFrom(buf)
		if err != nil {
			if c.stoppedNow() {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Some platforms report a datagram larger than the read buffer as an
			// error rather than truncating it. Tolerating a bounded run of read
			// errors keeps an unrelated host from taking the connection down,
			// while a socket that has genuinely failed still ends it.
			if failures++; failures <= readFailLimit {
				continue
			}
			c.shutdown(fmt.Errorf("reliable: socket read: %w", err))
			return
		}
		failures = 0
		c.handleDatagram(buf[:n])
	}
}

// timerLoop drives everything the connection does without application
// activity: delayed acknowledgements, retransmission, the handshake
// acknowledgement, and the idle timeout.
func (c *stream) timerLoop() {
	ticker := time.NewTicker(timerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-ticker.C:
			c.onTimer(now)
		}
	}
}

func (c *stream) onTimer(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}

	if c.ackPending && !now.Before(c.ackDue) {
		c.sendAckLocked(now)
	}

	// Until the peer sends anything, keep repeating the acknowledgement that
	// confirms our hello: the peer may still be waiting for it.
	if !c.confirmed && len(c.helloAck) > 0 && now.Sub(c.lastHelloAck) >= punchInterval {
		c.lastHelloAck = now
		c.sendLocked(c.helloAck)
	}

	if c.cfg.IdleTimeout > 0 && now.Sub(c.lastActivity) > c.cfg.IdleTimeout {
		c.failLocked(errors.New("reliable: idle timeout"))
		return
	}

	// A gap with data behind it will not clear by itself: nothing else is
	// arriving to carry the acknowledgement that would say so. Repeating it is
	// what keeps the peer's recovery driven by acknowledgements instead of by
	// its timeout.
	if len(c.ooo) > 0 && now.Sub(c.lastAckSent) >= gapProbeInterval {
		c.sendAckLocked(now)
	}

	if len(c.unacked) > 0 {
		head := c.unacked[0]
		if now.Sub(head.sent) >= head.rto {
			head.rto = min(head.rto*2, maxRTO)
			c.retransmitHeadLocked(now, false)
			if c.stopped {
				return
			}
		}
	}

	if c.sentFin && !c.finAcked && now.Sub(c.finSent) >= c.finRTO {
		if c.finTries >= c.cfg.RetransmitLimit {
			c.failLocked(errors.New("reliable: peer stopped acknowledging the close"))
			return
		}
		c.finTries++
		c.finSent = now
		c.finRTO = min(c.finRTO*2, maxRTO)
		c.sendLocked(c.envelopeLocked(typeFin, c.finSeq, nil))
	}
}

func (c *stream) handleDatagram(b []byte) {
	seg, ok := parseSegment(c.token, b)
	if !ok {
		return
	}
	// Hello and hello-ack predate the session; everything else must carry it.
	if seg.typ != typeHello && seg.typ != typeHelloAck && seg.session != c.session {
		return
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.lastActivity = now

	switch seg.typ {
	case typeHello:
		// The peer is still punching: repeat the acknowledgement that shows we
		// received its hello.
		if len(seg.payload) == nonceSize && len(c.helloAck) > 0 {
			c.lastHelloAck = now
			c.sendLocked(c.helloAck)
		}
	case typeHelloAck:
		c.confirmed = true
	case typeData:
		c.confirmed = true
		c.peerWindow = windowBytes(seg.window)
		c.ackReceivedLocked(seg, now)
		c.deliverLocked(seg, now)
	case typeAck:
		c.confirmed = true
		c.peerWindow = windowBytes(seg.window)
		c.ackReceivedLocked(seg, now)
	case typeFin:
		c.confirmed = true
		c.peerWindow = windowBytes(seg.window)
		c.finReceivedLocked(seg, now)
	}
}

// deliverLocked places an in-order segment in the receive buffer, or parks an
// out-of-order one until the gap before it is filled.
func (c *stream) deliverLocked(seg segment, now time.Time) {
	defer c.signal()

	end := c.rcvBase + uint32(c.recvBuf.len())
	payload := seg.payload
	off := int64(seqDiff(seg.seq, end))
	if off < 0 {
		// Already delivered in part or in full. Retransmissions are expected, so
		// this has to be idempotent.
		skip := -off
		if skip >= int64(len(payload)) {
			// A pure duplicate. It is acknowledged again rather than ignored:
			// the acknowledgement it repeats may be the one that was lost, and
			// without a fresh one the peer would retransmit until it gave up.
			c.sendAckLocked(now)
			return
		}
		payload = payload[skip:]
		off = 0
	}
	if off+int64(len(payload)) > recvWindow {
		// Beyond the receive window: drop it without acknowledging, so the peer
		// keeps its send window where it is until more of the buffer is read out.
		return
	}
	if off == 0 {
		drained := c.appendInOrderLocked(payload)
		// Filling a gap frees a run of bytes at once, so the peer is told
		// immediately instead of waiting for the delayed acknowledgement: it can
		// then send into the window that just opened.
		c.scheduleAckLocked(now, drained)
		return
	}
	if _, dup := c.ooo[seg.seq]; !dup {
		cp := append([]byte(nil), payload...)
		c.ooo[seg.seq] = cp
		c.oooBytes += len(cp)
		if high := seg.seq + uint32(len(cp)); seqDiff(high, c.oooHigh) > 0 {
			c.oooHigh = high
		}
	}
	// An out-of-order segment is acknowledged straight away. The duplicate
	// acknowledgements that produces are what tell the sender which segment to
	// resend without waiting for its timer, so batching them would stall the
	// connection at the first loss in a window.
	c.sendAckLocked(now)
}

// appendInOrderLocked appends data at the contiguous end and then drains
// whatever out-of-order segments the append joined up with, reporting whether
// any were drained.
func (c *stream) appendInOrderLocked(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	c.recvBuf.append(p)
	drained := false
	for {
		key := c.rcvBase + uint32(c.recvBuf.len())
		next, ok := c.ooo[key]
		if !ok {
			break
		}
		delete(c.ooo, key)
		c.oooBytes -= len(next)
		c.recvBuf.append(next)
		drained = true
	}
	if len(c.ooo) == 0 {
		// Nothing is held back, so there is no gap to report to the peer.
		c.oooHigh = 0
	}
	return drained
}

func (c *stream) scheduleAckLocked(now time.Time, immediate bool) {
	c.ackPending = true
	c.pendingSegs++
	if immediate || c.pendingSegs >= 2 {
		c.ackDue = now
		return
	}
	c.ackDue = now.Add(ackDelay)
}

func (c *stream) ackReceivedLocked(seg segment, now time.Time) {
	ack := seg.ack
	if seqDiff(ack, c.sndNxt) > 0 {
		// Nothing that has not been sent can be acknowledged, so this is not a
		// real answer. Ignoring it matters: acting on it would drop segments
		// that are still in flight and leave the stream short of bytes nobody
		// will ever resend.
		return
	}
	if seqDiff(ack, c.sndUna) > 0 {
		c.sndUna = ack
	}

	progressed := false
	for len(c.unacked) > 0 {
		end := c.unacked[0].seq + uint32(len(c.unacked[0].data))
		if seqDiff(ack, end) < 0 {
			break
		}
		c.unacked = c.unacked[1:]
		progressed = true
	}

	if progressed || seqDiff(ack, c.lastAckAt) > 0 {
		c.lastAckAt = ack
		c.dupAcks = 0
	} else if len(c.unacked) > 0 && seg.typ == typeAck {
		// Nothing has moved. Repeated acknowledgements of the same offset, or an
		// acknowledgement that reports buffered data beyond the oldest
		// unacknowledged segment, both mean that segment is missing and has to go
		// again. The delay between attempts keeps a burst of stale
		// acknowledgements from resending it many times over.
		head := c.unacked[0]
		headEnd := head.seq + uint32(len(head.data))
		c.dupAcks++
		if (c.dupAcks >= dupAckLimit || seqDiff(seg.seq, headEnd) > 0) &&
			now.Sub(head.sent) >= fastRetransmitDelay {
			c.dupAcks = 0
			c.retransmitHeadLocked(now, true)
		}
	}

	if c.sentFin && !c.finAcked && seqDiff(ack, c.finSeq) >= 0 {
		c.finAcked = true
	}

	// The peer may have opened room in its receive window, or acknowledged the
	// bytes a writer is waiting on.
	c.signal()
}

func (c *stream) finReceivedLocked(seg segment, now time.Time) {
	if !c.haveFin {
		end := c.rcvBase + uint32(c.recvBuf.len())
		if int64(seqDiff(seg.seq, end)) > recvWindow {
			// A finish that far ahead of the data received cannot be honest, and
			// waiting for the bytes it implies would never end.
			return
		}
		c.haveFin = true
		c.finSeq = seg.seq
	}
	// Acknowledge immediately: the peer's Close is waiting on this.
	c.scheduleAckLocked(now, true)
	c.signal()
}

// maybeWindowUpdateLocked schedules an acknowledgement when reading has opened
// enough of the receive window that the sender would otherwise stall.
func (c *stream) maybeWindowUpdateLocked() {
	free := c.freeWindowLocked()
	if free == 0 {
		return
	}
	if c.ackedWindow == 0 || int(free) >= int(c.ackedWindow)+windowStep/windowScale {
		c.ackPending = true
		c.ackDue = time.Now()
	}
}

// freeWindowLocked is the receive window in header units.
func (c *stream) freeWindowLocked() uint16 {
	free := recvWindow - c.recvBuf.len() - c.oooBytes
	if free < 0 {
		free = 0
	}
	units := free / windowScale
	if units > maxWindow {
		units = maxWindow
	}
	return uint16(units)
}

// windowLocked reports the receive window and records it as the last value
// advertised, which is what a window update is measured against.
func (c *stream) windowLocked() uint16 {
	w := c.freeWindowLocked()
	c.ackedWindow = w
	return w
}

// windowBytes converts an advertised window into bytes.
func windowBytes(units uint16) int { return int(units) * windowScale }

// envelopeLocked builds a datagram carrying the current acknowledgement and
// receive window.
func (c *stream) envelopeLocked(typ msgType, seq uint32, payload []byte) []byte {
	return marshalSegment(nil, c.token, segment{
		typ:     typ,
		session: c.session,
		seq:     seq,
		ack:     c.rcvBase + uint32(c.recvBuf.len()),
		window:  c.windowLocked(),
		payload: payload,
	})
}

// sendLocked transmits a datagram. mu must be held.
func (c *stream) sendLocked(b []byte) {
	c.lastActivity = time.Now()
	c.tx.writeTo(b, c.peer)
}

// sendAckLocked sends an acknowledgement that carries the current receive
// position, the window, and the highest offset buffered out of order.
func (c *stream) sendAckLocked(now time.Time) {
	c.ackPending = false
	c.pendingSegs = 0
	c.lastAckSent = now
	c.sendLocked(marshalSegment(nil, c.token, segment{
		typ:     typeAck,
		session: c.session,
		seq:     c.oooHigh,
		ack:     c.rcvBase + uint32(c.recvBuf.len()),
		window:  c.windowLocked(),
	}))
}

// retransmitHeadLocked resends the oldest unacknowledged segment and counts the
// attempt against the retransmission limit. A segment is only resent a few times
// in response to acknowledgements; after that the timeout path owns it, so a
// burst of stale acknowledgements cannot burn the whole retransmission budget.
func (c *stream) retransmitHeadLocked(now time.Time, fromAck bool) {
	head := c.unacked[0]
	if fromAck {
		if head.fastTries >= fastRetransmitTries {
			return
		}
		head.fastTries++
	}
	if head.retries >= c.cfg.RetransmitLimit {
		c.failLocked(fmt.Errorf("reliable: peer stopped acknowledging after %d retransmissions", head.retries))
		return
	}
	head.retries++
	head.sent = now
	if c.logger != nil {
		c.logger.Printf("reliable: retransmit seq=%d attempt=%d", head.seq, head.retries)
	}
	c.sendLocked(c.envelopeLocked(typeData, head.seq, head.data))
}
