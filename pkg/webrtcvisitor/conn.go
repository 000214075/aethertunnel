package webrtcvisitor

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// bufferedAmountHigh is where Write starts waiting for the channel to drain.
// SCTP applies no backpressure of its own by default: without this check a
// remote that stops reading while the local service keeps producing grows the
// association's send queue without bound, on both ends.
const bufferedAmountHigh = 1 << 20

// maxBuffered is how many unread bytes may wait in the receive buffer. SCTP
// applies no backpressure to the receiving side either: the message callback
// runs as fast as the peer sends, so a remote that keeps streaming while the
// relay is blocked writing downstream grew this buffer for as long as the
// session lasted.
const maxBuffered = 1 << 20

// errReceiveOverflow is what Read reports when the peer sent more than
// maxBuffered bytes that nothing had read.
var errReceiveOverflow = errors.New("webrtc: the peer sent more than the receive buffer holds")

// Conn presents a WebRTC data channel as a net.Conn, which is all the relay
// code on both ends asks of a data path. The channel reports messages through
// a callback, so incoming bytes land in a buffer that Read drains; the relay
// on each side is the only reader.
type Conn struct {
	channel *webrtc.DataChannel
	pc      *webrtc.PeerConnection

	mu         sync.Mutex
	buf        []byte
	closed     bool
	remoteGone bool
	// goneErr is what Read reports for the failure that ended the connection:
	// nil for an ordinary close, which reads as io.EOF.
	goneErr   error
	readDead  time.Time
	writeDead time.Time
	wait      chan struct{}
}

// newConn wraps a channel after wiring the message callback and the two
// liveness callbacks: a channel the peer closed, and a peer connection whose
// state left the established path, both have to wake a blocked Read — a relay
// parked in Read on a dead channel would otherwise strand its goroutine, its
// socket and everything upstream of it until the whole session ends.
func newConn(pc *webrtc.PeerConnection, channel *webrtc.DataChannel) *Conn {
	c := &Conn{
		channel: channel,
		pc:      pc,
		wait:    make(chan struct{}),
	}
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		c.mu.Lock()
		if len(c.buf)+len(message.Data) > maxBuffered {
			c.mu.Unlock()
			// The peer streams faster than the relay drains and there is no way to tell
			// it to stop, so the connection is failed rather than grown: the relay then
			// reports a broken data path, which is what this is.
			c.fail(errReceiveOverflow)
			return
		}
		c.buf = append(c.buf, message.Data...)
		ch := c.wait
		c.wait = make(chan struct{})
		c.mu.Unlock()
		close(ch)
	})
	channel.OnClose(func() {
		c.markRemoteGone()
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// Disconnected is deliberately not terminal: ICE can re-establish
			// it, and declaring the remote gone would kill a recoverable
			// relay. Only failed and closed have no way back.
			c.markRemoteGone()
		}
	})
	return c
}

// markRemoteGone records that the remote side is unreachable and wakes every
// blocked reader with EOF.
func (c *Conn) markRemoteGone() { c.fail(nil) }

// fail ends the connection with the error Read should report, nil for an
// ordinary close, and wakes every blocked reader.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	already := c.remoteGone || c.closed
	c.remoteGone = true
	if c.goneErr == nil {
		c.goneErr = err
	}
	ch := c.wait
	c.wait = make(chan struct{})
	c.mu.Unlock()
	if !already {
		close(ch)
	}
}

// Read drains received bytes, blocking while the buffer is empty and the
// channel is open. A read deadline, the peer closing the channel and the peer
// connection leaving its established state all end the wait.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		// The io.Reader convention: a zero-length read returns at once. Without
		// this the loop below parks on the wait channel until a datagram arrives,
		// so a caller that passed an empty slice would hang instead.
		return 0, nil
	}
	for {
		c.mu.Lock()
		// A local close is checked before the buffer, the way the reliable
		// transport checks its own localClosed first: Close was called, so the
		// reader asked to stop and buffered bytes are not delivered. The remote
		// close stays after the buffer on purpose: the peer's last bytes arrive
		// just before it closes the channel, and dropping them would truncate the
		// stream — which is why the reliable transport reads its buffer before it
		// reports the peer's FIN.
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if len(c.buf) > 0 {
			n := copy(p, c.buf)
			c.buf = c.buf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.remoteGone {
			err := c.goneErr
			c.mu.Unlock()
			if err == nil {
				return 0, io.EOF
			}
			return 0, err
		}
		// The wait channel is captured in the same critical section that saw
		// the buffer empty: a message landing between the check and the
		// capture swaps in a fresh channel and closes this one, so the
		// reader is woken by exactly the data it must not sleep past.
		ch := c.wait
		var deadline <-chan time.Time
		var timer *time.Timer
		if !c.readDead.IsZero() {
			d := time.Until(c.readDead)
			if d <= 0 {
				c.mu.Unlock()
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			deadline = timer.C
		}
		c.mu.Unlock()

		if deadline == nil {
			<-ch
			continue
		}
		select {
		case <-ch:
			timer.Stop()
		case <-deadline:
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// Write sends the bytes to the remote side. It waits for the channel's send
// buffer to drain below its high-water mark first: the data channel applies no
// backpressure of its own, and a local service that produces faster than the
// path carries would otherwise queue without limit. A write deadline bounds
// that wait, so a remote that stops reading cannot hang the caller forever.
func (c *Conn) Write(p []byte) (int, error) {
	for {
		c.mu.Lock()
		closed := c.closed
		remoteGone := c.remoteGone
		goneErr := c.goneErr
		hasDeadline := !c.writeDead.IsZero()
		var remaining time.Duration
		if hasDeadline {
			remaining = time.Until(c.writeDead)
		}
		c.mu.Unlock()
		// The failure that ended the connection is reported the way Read reports
		// it, so a writer learns why: net.ErrClosed alone hid the receive-buffer
		// overflow that actually ended it.
		if closed {
			return 0, net.ErrClosed
		}
		if remoteGone {
			if goneErr != nil {
				return 0, goneErr
			}
			return 0, io.EOF
		}
		// The deadline is checked before the buffer: a write past its deadline has
		// to fail whatever the channel is doing, which is how Read reports it.
		if hasDeadline && remaining <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		if c.channel.BufferedAmount() <= bufferedAmountHigh {
			break
		}
		// Sleep in short slices so a deadline set mid-wait is noticed
		// promptly and the sleep never runs past it.
		sleep := 10 * time.Millisecond
		if hasDeadline && remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
	if err := c.channel.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close closes the channel and the peer connection, and wakes a blocked read.
func (c *Conn) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	if !already {
		close(c.wait)
		c.wait = make(chan struct{})
	}
	c.mu.Unlock()
	if err := c.channel.Close(); err != nil {
		_ = c.pc.Close()
		return err
	}
	return c.pc.Close()
}

// LocalAddr names the channel; a data channel has no network address of its own.
func (c *Conn) LocalAddr() net.Addr { return channelAddr{label: c.channel.Label()} }

// RemoteAddr names the peer connection.
func (c *Conn) RemoteAddr() net.Addr { return channelAddr{label: "webrtc-peer"} }

// SetDeadline sets both the read and the write deadline: Read waits for a
// message or the read deadline, Write for the send buffer to drain or the
// write deadline.
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return c.SetReadDeadline(t)
}

// SetReadDeadline bounds how long Read may wait for the remote side.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDead = t
	ch := c.wait
	c.wait = make(chan struct{})
	c.mu.Unlock()
	close(ch)
	return nil
}

// SetWriteDeadline bounds how long Write may wait for the channel to drain.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDead = t
	c.mu.Unlock()
	return nil
}

// channelAddr is a placeholder address: WebRTC hides the network path behind
// ICE, so the honest answer is the channel's identity, not an IP pair.
type channelAddr struct{ label string }

func (a channelAddr) Network() string { return "webrtc" }
func (a channelAddr) String() string  { return a.label }
