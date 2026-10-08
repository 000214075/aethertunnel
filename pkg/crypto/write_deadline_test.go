package crypto

import (
	"testing"
	"time"
)

// A relay that bounds a stalled destination with a write deadline needs the
// call to reach the underlying connection through the Stream; a Stream that
// silently drops it leaves the relay's writer parked on a peer that stopped
// reading, with nothing to end the wait.
func TestStreamForwardsTheWriteDeadlineToTheUnderlyingConnection(t *testing.T) {
	rec := &deadlineRecorder{}
	stream := NewStream(rec, nil)

	at := time.Now().Add(time.Second)
	if err := stream.SetWriteDeadline(at); err != nil {
		t.Fatalf("SetWriteDeadline = %v, want nil", err)
	}
	if len(rec.writes) != 1 || !rec.writes[0].Equal(at) {
		t.Fatalf("the write deadline did not reach the connection: %v", rec.writes)
	}

	if err := stream.SetDeadline(at); err != nil {
		t.Fatalf("SetDeadline = %v, want nil", err)
	}
	if len(rec.reads) != 1 || !rec.reads[0].Equal(at) {
		t.Fatalf("SetDeadline did not reach the read side: %v", rec.reads)
	}
	if len(rec.writes) != 2 || !rec.writes[1].Equal(at) {
		t.Fatalf("SetDeadline did not reach the write side: %v", rec.writes)
	}
}

type deadlineRecorder struct {
	reads  []time.Time
	writes []time.Time
}

func (d *deadlineRecorder) Read([]byte) (int, error)    { return 0, nil }
func (d *deadlineRecorder) Write(p []byte) (int, error) { return len(p), nil }

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.reads = append(d.reads, t)
	return nil
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.writes = append(d.writes, t)
	return nil
}
