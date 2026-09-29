package obfs

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// fuzzConn feeds a fixed byte slice to the record reader without blocking, and
// swallows whatever the wrapper writes back. A real socket would block whenever
// the reader stops early — an incomplete record is exactly that case — and a fuzz
// target that leaks a blocked goroutine per input runs out of memory long before
// it finds a bug.
type fuzzConn struct{ r *bytes.Reader }

func (c *fuzzConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fuzzConn) Close() error                     { return nil }
func (c *fuzzConn) LocalAddr() net.Addr              { return fuzzAddr{} }
func (c *fuzzConn) RemoteAddr() net.Addr             { return fuzzAddr{} }
func (c *fuzzConn) SetDeadline(time.Time) error      { return nil }
func (c *fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fuzzConn) SetWriteDeadline(time.Time) error { return nil }

type fuzzAddr struct{}

func (fuzzAddr) Network() string { return "fuzz" }
func (fuzzAddr) String() string  { return "fuzz" }

// FuzzRecordRead covers the record reader of the tls-record disguise, which is what
// a peer's bytes go through once the connection is open. The disguise adds no
// authentication, so this is a parser of whatever arrives, and it must reject a
// stream that is not record-framed rather than panic on it.
func FuzzRecordRead(f *testing.F) {
	f.Add([]byte{})
	// One application-data record carrying three bytes.
	f.Add([]byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x03, 'a', 'b', 'c'})
	// A record whose header claims 65535 bytes that are not there.
	f.Add([]byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0xff, 0xff, 'a'})
	// A zero-length record, which the reader skips rather than reporting as end of
	// stream, followed by a real one.
	f.Add([]byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x00,
		tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x01, 'x'})
	// A plain stream that is not record-framed at all.
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		conn, err := Wrap(&fuzzConn{r: bytes.NewReader(data)}, DisguiseTLSRecord, Dialer)
		if err != nil {
			t.Fatalf("Wrap: %v", err)
		}

		buf := make([]byte, 64)
		total := 0
		for i := 0; i < 16; i++ {
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			if n < 0 || n > len(buf) {
				t.Fatalf("Read reported %d bytes into a %d byte buffer", n, len(buf))
			}
			total += n
		}
		// A reader cannot hand out more bytes than it was given, which is the
		// property that keeps a disguised stream from inventing plaintext.
		if total > len(data) {
			t.Fatalf("Read produced %d bytes from %d", total, len(data))
		}
	})
}
