package net

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A caller that probes with an empty buffer is entitled to 0, nil at once. Both
// wrappers below used to hand the empty buffer to a reader that fills its own
// block buffer first, so the call parked on the socket until the peer sent
// something — which is not what reading nothing means, and which every other
// wrapper in this package already guards against.
func TestAnEmptyReadReturnsAtOnce(t *testing.T) {
	t.Run("compressed", func(t *testing.T) {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()

		conn := CompressConn(right)
		done := make(chan struct{})
		go func() {
			defer close(done)
			n, err := conn.Read(nil)
			if n != 0 || err != nil {
				t.Errorf("Read(nil) = %d, %v, want 0, nil", n, err)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Read with an empty buffer blocked on a compressed connection")
		}
	})

	t.Run("websocket", func(t *testing.T) {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()

		conn := &wsConn{Conn: left, reader: left}
		done := make(chan struct{})
		go func() {
			defer close(done)
			n, err := conn.Read([]byte{})
			if n != 0 || err != nil {
				t.Errorf("Read([]byte{}) = %d, %v, want 0, nil", n, err)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Read with an empty buffer blocked on a websocket connection")
		}
	})
}

// shortWriteConn accepts a write and reports one byte fewer, the way an
// underlying connection that violates io.Writer's contract behaves.
type shortWriteConn struct {
	net.Conn
	writes bytes.Buffer
}

func (c *shortWriteConn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, _ := c.writes.Write(b[:len(b)-1])
	return n, nil
}

// A frame written in two pieces goes out as one unit: a short write of the header
// with no error reported used to be taken as success, which puts a truncated
// header on the wire and desynchronises the frame stream. Every other frame
// writer here reports io.ErrShortWrite for exactly that.
func TestAFrameWriteReportsAShortWrite(t *testing.T) {
	conn := &shortWriteConn{}
	ws := &wsConn{Conn: conn}
	if err := ws.writeFrame(opBinary, []byte("payload")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeFrame over a connection that short-writes = %v, want io.ErrShortWrite", err)
	}
}
