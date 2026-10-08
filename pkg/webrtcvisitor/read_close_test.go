package webrtcvisitor

import (
	"errors"
	"net"
	"testing"
)

// Read used to look at the receive buffer before the local close flag, so a
// connection its own side had closed still delivered bytes that were buffered when
// the close happened. The reliable transport reports its local close before it
// touches its buffer; this now does the same. The remote close stays after the
// buffer, so the peer's last bytes are not truncated.
func TestReadReportsALocalCloseBeforeDrainingTheBuffer(t *testing.T) {
	c := &Conn{buf: []byte("buffered"), closed: true, wait: make(chan struct{})}

	p := make([]byte, 16)
	n, err := c.Read(p)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read on a locally closed conn returned (%d, %v), want net.ErrClosed", n, err)
	}
	if n != 0 {
		t.Fatalf("Read on a locally closed conn returned %d buffered byte(s)", n)
	}
}
