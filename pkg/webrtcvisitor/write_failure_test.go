package webrtcvisitor

import (
	"errors"
	"io"
	"net"
	"testing"
)

// Write used to report net.ErrClosed for every way the connection could end, so a
// writer never learned that the receive buffer had overflowed — the failure Read
// reports — and the relay blamed the wrong side.
func TestWriteReportsTheFailureThatEndedTheConnection(t *testing.T) {
	gone := &Conn{wait: make(chan struct{}), remoteGone: true, goneErr: errReceiveOverflow}
	if _, err := gone.Write([]byte("x")); !errors.Is(err, errReceiveOverflow) {
		t.Fatalf("Write on a connection that overflowed = %v, want errReceiveOverflow", err)
	}

	closed := &Conn{wait: make(chan struct{}), closed: true}
	if _, err := closed.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write on a closed connection = %v, want net.ErrClosed", err)
	}

	// An ordinary remote close reads as io.EOF, the way Read reports it.
	remote := &Conn{wait: make(chan struct{}), remoteGone: true}
	if _, err := remote.Write([]byte("x")); !errors.Is(err, io.EOF) {
		t.Fatalf("Write to a remote that left = %v, want io.EOF", err)
	}
}
