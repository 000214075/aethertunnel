package net

import (
	"io"
	"net"
	"sync"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// pickTestConn is an io.ReadWriter with nothing behind it: the session under test
// only hands its framers around, and closeFramers closes what it was given.
type pickTestConn struct{}

func (pickTestConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (pickTestConn) Write(p []byte) (int, error) { return len(p), nil }
func (pickTestConn) Close() error                { return nil }

func pickTestFramer() *protocol.Framer {
	return protocol.NewFramerWithOptions(pickTestConn{}, nil, protocol.FramerOptions{})
}

// pick reads the number of paths, drops the lock, and takes it again. A session
// that ends in that window — the reaper, a shutdown, a failed write — has its
// framers replaced with nil, so indexing there panicked the whole process. Both
// branches go through the bounds-checked accessor for that reason, which is what
// this exercises: the toggling goroutine reproduces the close the single-shot
// version of this test cannot reach.
func TestPickNeverIndexesClosedPaths(t *testing.T) {
	session := &datagramSession{
		addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65000},
		done: make(chan struct{}),
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			session.setFramers([]*protocol.Framer{pickTestFramer()}, nil)
			session.closeFramers()
		}
	}()

	for i := 0; i < 200000; i++ {
		session.pick()
	}
	close(stop)
	wg.Wait()

	// The session is left empty, which is the state a torn-down one is in: the
	// same call has to keep answering nil rather than panicking.
	session.closeFramers()
	if framer := session.pick(); framer != nil {
		t.Fatal("a session whose paths are gone returned a framer")
	}
}
