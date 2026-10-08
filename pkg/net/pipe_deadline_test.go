package net

import (
	"io"
	"os"
	"testing"
	"time"
)

// deadlineConn is a reader/writer that enforces the deadlines it is given, the way a
// real socket does, and can be told to take a while to answer either direction.
type deadlineConn struct {
	data     []byte
	read     bool
	readDur  time.Duration
	writeDur time.Duration

	readDeadline  time.Time
	writeDeadline time.Time
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error  { c.readDeadline = t; return nil }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error { c.writeDeadline = t; return nil }

func (c *deadlineConn) Read(p []byte) (int, error) {
	time.Sleep(c.readDur)
	if !c.readDeadline.IsZero() && time.Now().After(c.readDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, c.data), nil
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	time.Sleep(c.writeDur)
	if !c.writeDeadline.IsZero() && time.Now().After(c.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return len(p), nil
}

// The idle timeout bounds both directions of a relayed stream. Setting the write
// deadline to the read's absolute instant meant a read that used most of the window
// left the write with nothing, so a write the destination would have accepted failed
// with a timeout, the direction returned and Pipe closed both connections — a
// truncated stream from a slow but healthy source.
func TestASlowReadDoesNotEatTheWriteDeadline(t *testing.T) {
	const idle = 600 * time.Millisecond
	src := &deadlineConn{data: []byte("payload"), readDur: 400 * time.Millisecond}
	dst := &deadlineConn{writeDur: 400 * time.Millisecond}

	if got := copyDirection(dst, src, idle); got != int64(len("payload")) {
		t.Fatalf("copyDirection copied %d bytes, want %d", got, len("payload"))
	}
}
