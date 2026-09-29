package webrtcvisitor

import (
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Conn presents a WebRTC data channel as a net.Conn, which is all the relay
// code on both ends asks of a data path. The channel reports messages through
// a callback, so incoming bytes land in a buffer that Read drains; the relay
// on each side is the only reader.
type Conn struct {
	channel *webrtc.DataChannel
	pc      *webrtc.PeerConnection

	mu     sync.Mutex
	buf    []byte
	closed bool
	wait   chan struct{}
}

// newConn wraps a channel after wiring the message callback.
func newConn(pc *webrtc.PeerConnection, channel *webrtc.DataChannel) *Conn {
	c := &Conn{
		channel: channel,
		pc:      pc,
		wait:    make(chan struct{}),
	}
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		c.mu.Lock()
		c.buf = append(c.buf, message.Data...)
		c.mu.Unlock()
		close(c.signal())
	})
	return c
}

// signal hands waiters a fresh closed channel; the next wait allocates another.
func (c *Conn) signal() chan struct{} {
	ch := c.wait
	c.wait = make(chan struct{})
	return ch
}

// waitChan is the channel a blocked reader sleeps on.
func (c *Conn) waitChan() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wait
}

// Read drains received bytes, blocking while the buffer is empty and the
// channel is open.
func (c *Conn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.buf) > 0 {
			n := copy(p, c.buf)
			c.buf = c.buf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		ch := c.wait
		c.mu.Unlock()

		<-ch
	}
}

// Write sends the bytes to the remote side.
func (c *Conn) Write(p []byte) (int, error) {
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
	c.mu.Unlock()
	if !already {
		close(c.signal())
	}
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

// SetDeadline is accepted and ignored: data channels have no deadlines.
func (c *Conn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline is accepted and ignored.
func (c *Conn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline is accepted and ignored.
func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

// channelAddr is a placeholder address: WebRTC hides the network path behind
// ICE, so the honest answer is the channel's identity, not an IP pair.
type channelAddr struct{ label string }

func (a channelAddr) Network() string { return "webrtc" }
func (a channelAddr) String() string  { return "webrtc:" + a.label }
