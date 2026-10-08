package net

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// frameConn serves the given bytes to reads and collects writes, so a test can
// drive readFrame without a peer in either direction.
type frameConn struct {
	reader *bytes.Reader
	writes bytes.Buffer
}

func (c *frameConn) Read(p []byte) (int, error)      { return c.reader.Read(p) }
func (c *frameConn) Write(b []byte) (int, error)     { return c.writes.Write(b) }
func (c *frameConn) Close() error                    { return nil }
func (c *frameConn) LocalAddr() net.Addr             { return nil }
func (c *frameConn) RemoteAddr() net.Addr            { return nil }
func (c *frameConn) SetDeadline(time.Time) error     { return nil }
func (c *frameConn) SetReadDeadline(time.Time) error { return nil }
func (c *frameConn) SetWriteDeadline(time.Time) error {
	return nil
}

// serverFrame drives a client-side connection, which is the side that expects
// unmasked frames: the frame under test is then refused only by the check it is
// meant to trip, not by the masking rule.
func serverFrame(frame []byte) *wsConn {
	conn := &frameConn{reader: bytes.NewReader(frame)}
	return &wsConn{Conn: conn, reader: conn, client: true}
}

// RFC 6455 5.2: the reserved bits belong to extensions a connection negotiated,
// and this wrapper negotiates none. A frame that sets one must fail the
// connection rather than have its payload delivered.
func TestAFrameWithAReservedBitIsRefused(t *testing.T) {
	// FIN and RSV1 with a binary opcode and an empty payload.
	conn := serverFrame([]byte{0x80 | 0x40 | 0x02, 0x00})
	if _, _, _, err := conn.readFrame(); err == nil {
		t.Fatal("a frame with RSV1 set was accepted")
	}
}

// RFC 6455 5.5: control frames cannot be fragmented.
func TestAFragmentedControlFrameIsRefused(t *testing.T) {
	// FIN clear, opcode ping, empty payload.
	conn := serverFrame([]byte{0x09, 0x00})
	if _, _, _, err := conn.readFrame(); err == nil {
		t.Fatal("a fragmented ping was accepted")
	}
}

// RFC 6455 5.2: the payload length must use the shortest form that fits, and a
// receiver must fail the connection when it does not.
func TestANonMinimalLengthIsRefused(t *testing.T) {
	// The frames carry their whole payload, so the only thing that can refuse them
	// is the length check: a truncated frame would error either way.
	short := serverFrame([]byte{0x82, 0x7E, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'})
	if _, _, _, err := short.readFrame(); err == nil {
		t.Fatal("a 5-byte payload sent in the 16-bit form was accepted")
	}
	long := serverFrame([]byte{0x82, 0x7F, 0, 0, 0, 0, 0, 0, 0, 0x05, 'h', 'e', 'l', 'l', 'o'})
	if _, _, _, err := long.readFrame(); err == nil {
		t.Fatal("a 5-byte payload sent in the 64-bit form was accepted")
	}
}

// The writer has to stay on the minimal forms the checks above demand, or the
// tunnel would refuse its own frames.
func TestTheWriterUsesTheShortestLengthForm(t *testing.T) {
	for _, size := range []int{0, 125, 126, 65535, 65536} {
		conn := &frameConn{reader: bytes.NewReader(nil)}
		if err := (&wsConn{Conn: conn}).writeFrame(opBinary, make([]byte, size)); err != nil {
			t.Fatalf("write %d bytes: %v", size, err)
		}
		frame := conn.writes.Bytes()
		if len(frame) < 2 {
			t.Fatalf("the %d-byte frame produced %d bytes", size, len(frame))
		}
		want := byte(size)
		switch {
		case size < 126:
		case size <= 0xFFFF:
			want = 0x7E
		default:
			want = 0x7F
		}
		if frame[1] != want {
			t.Fatalf("a %d-byte payload was written with length form 0x%02x, want 0x%02x", size, frame[1], want)
		}
	}
}
