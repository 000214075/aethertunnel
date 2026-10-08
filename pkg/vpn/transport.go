package vpn

import (
	"errors"
	"io"
	"sync"
)

// ErrBufferFull reports that a packet was dropped because the transport's receive
// buffer is full.
var ErrBufferFull = errors.New("vpn: the receive buffer is full")

// DefaultReceiveBuffer is how many packets a ChannelTransport holds before it starts
// dropping.
//
// Dropping is the right answer when the peer cannot keep up: TCP inside the tunnel
// retransmits, and a buffer that grows without bound would add latency to every
// connection rather than only to the one that is flooding.
const DefaultReceiveBuffer = 64

// ChannelTransport is a Transport built on a send function and a receive buffer.
//
// It exists so a caller that already has its own framing — the tunnel's control
// connection, in this program — does not need a Transport implementation of its own:
// it supplies a function that writes one packet as a frame, and hands each received
// frame to Deliver.
type ChannelTransport struct {
	send func(packet []byte) error
	in   chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

// NewChannelTransport builds a transport whose outbound packets are written by send.
// A nil send makes Send fail, which is what a transport that can only receive looks
// like.
func NewChannelTransport(send func(packet []byte) error) *ChannelTransport {
	return &ChannelTransport{
		send:   send,
		in:     make(chan []byte, DefaultReceiveBuffer),
		closed: make(chan struct{}),
	}
}

// Deliver hands one received packet to the tunnel. It never blocks: it returns false
// when the buffer is full or the transport is closed, so the frame reader can keep
// up with a peer that is sending faster than the device accepts.
func (t *ChannelTransport) Deliver(packet []byte) bool {
	select {
	case <-t.closed:
		return false
	default:
	}

	select {
	case t.in <- packet:
		return true
	case <-t.closed:
		return false
	default:
		return false
	}
}

// Send writes one packet out.
func (t *ChannelTransport) Send(packet []byte) error {
	if t.send == nil {
		return errors.New("vpn: this transport cannot send")
	}
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
		return t.send(packet)
	}
}

// Receive returns the next packet, or fails once the transport is closed.
func (t *ChannelTransport) Receive() ([]byte, error) {
	select {
	case <-t.closed:
		return nil, io.ErrClosedPipe
	case packet := <-t.in:
		return packet, nil
	}
}

// Close stops the transport and unblocks a Receive that is in progress. It is
// idempotent.
func (t *ChannelTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

// Closed reports whether Close has been called.
func (t *ChannelTransport) Closed() bool {
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}

// Buffered is how many received packets are waiting.
func (t *ChannelTransport) Buffered() int { return len(t.in) }
