package webrtcvisitor

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// net.Conn requires a write made after its deadline to fail. The deadline was
// checked only after the send buffer had been found full, so a write whose
// deadline had passed still succeeded whenever the channel happened to be
// empty — while Read reported the expired deadline first, so the two directions
// disagreed about what the same deadline means.
func TestAWritePastItsDeadlineFailsWithAnEmptyBuffer(t *testing.T) {
	conn := &Conn{
		channel:   &webrtc.DataChannel{},
		wait:      make(chan struct{}),
		writeDead: time.Now().Add(-time.Second),
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write past its deadline = %v, want os.ErrDeadlineExceeded", err)
	}
}
