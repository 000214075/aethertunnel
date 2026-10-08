package net

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"
)

// slowConn is the stream a compressed connection writes to: it buffers what it is
// given and takes a moment over each write, so two writers have a window in which to
// interleave if nothing serialises them.
type slowConn struct {
	net.Conn
	mu    sync.Mutex
	buf   bytes.Buffer
	sleep time.Duration
}

func (c *slowConn) Write(p []byte) (int, error) {
	time.Sleep(c.sleep)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *slowConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Read(p)
}

// net.Conn permits concurrent calls, and snappy's writer keeps per-call state, so two
// writers through one CompressConn used to produce a stream the reader refused as
// corrupt.
func TestConcurrentCompressedWritesProduceOneReadableStream(t *testing.T) {
	underlying := &slowConn{sleep: time.Millisecond}
	conn := CompressConn(underlying)

	const perWrite = 4096
	const writesEach = 4

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fill := range []byte{'A', 'B'} {
		fill := fill
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < writesEach; i++ {
				if _, err := conn.Write(bytes.Repeat([]byte{fill}, perWrite)); err != nil {
					t.Errorf("Write(%c): %v", fill, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	decoded, err := io.ReadAll(snappy.NewReader(underlying))
	if err != nil {
		t.Fatalf("the compressed stream is not readable: %v", err)
	}
	if want := perWrite * writesEach * 2; len(decoded) != want {
		t.Fatalf("decoded %d bytes, want %d", len(decoded), want)
	}
}
