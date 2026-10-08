package net

import (
	"net"
	"sync"

	"github.com/golang/snappy"
)

// CompressConn wraps a byte stream in snappy: writes compress and reads
// decompress. The streaming writer emits a self-contained chunk per Write —
// the same property frp's compression relies on — so a relay whose copies end
// early never strands buffered bytes, and an interactive protocol never waits
// for a block to fill before its bytes cross.
//
// The wrapper sits outside the encryption layer: payloads are compressed while
// they are still plaintext, which is where compression actually gains bytes.
func CompressConn(inner net.Conn) net.Conn {
	return &compressedConn{
		Conn:   inner,
		reader: snappy.NewReader(inner),
		writer: snappy.NewWriter(inner),
	}
}

type compressedConn struct {
	net.Conn
	reader *snappy.Reader
	writer *snappy.Writer

	// snappy's reader and writer each carry per-call state (the writer's output
	// buffer, the reader's block buffer), so neither tolerates a second caller.
	// net.Conn allows concurrent calls, and the wrappers around this one hold a
	// lock for exactly this reason; without it two writers corrupt the stream, which
	// the reader then reports as "snappy: corrupt input".
	readMu  sync.Mutex
	writeMu sync.Mutex
}

func (c *compressedConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		// A caller probing with an empty buffer must not be made to wait for the
		// next block: snappy's reader fills its buffer before it looks at len(p),
		// so the call would block on the socket instead of returning 0, nil. Every
		// other wrapper in this tree guards this case.
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.reader.Read(p)
}

func (c *compressedConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writer.Write(p)
}

// Close closes the underlying connection; the snappy writers carry no buffer
// of their own between writes, so nothing needs flushing first.
func (c *compressedConn) Close() error {
	return c.Conn.Close()
}

// CloseWrite forwards the half-close the underlying connection may support.
// It has to be explicit: net.Conn does not carry CloseWrite, so a wrapper
// chain that ends in a TCP connection keeps its half-close semantics instead
// of having the relay fall back to a full close, which would truncate the
// reply of any protocol that signals "request finished" with a shutdown.
func (c *compressedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	// The connection underneath has no half-close of its own, so it is closed
	// whole. Returning nil would swallow this side's end-of-stream and leave a
	// peer that answers only once the request has ended waiting out its timeout.
	return c.Conn.Close()
}
