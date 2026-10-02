package net

import (
	"net"

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
}

func (c *compressedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *compressedConn) Write(p []byte) (int, error) {
	return c.writer.Write(p)
}

// Close closes the underlying connection; the snappy writers carry no buffer
// of their own between writes, so nothing needs flushing first.
func (c *compressedConn) Close() error {
	return c.Conn.Close()
}
