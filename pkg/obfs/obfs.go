// Package obfs wraps a connection so that what a passive observer sees on the wire
// is not the tunnel's own frame header.
//
// The wrapper is a byte-stream transform, not a protocol: it adds no handshake and
// no key material, so it hides the shape of the traffic rather than its contents.
// Encryption is the job of [encryption] and [transport], and this package is
// useful only alongside them.
package obfs

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Disguises a connection can be wrapped in.
const (
	// DisguiseNone leaves the connection exactly as it is.
	DisguiseNone = "none"
	// DisguiseTLSRecord makes every write look like one or more TLS 1.2
	// application-data records.
	//
	// This defeats a detector that looks at the first bytes of a connection, which
	// is what a plain protocol-identification rule does. It does not survive a
	// detector that models a TLS handshake, because no handshake happens: there is
	// no ClientHello, no certificate and no key exchange.
	DisguiseTLSRecord = "tls-record"
)

// Disguises lists the values [obfuscation].disguise accepts.
var Disguises = []string{DisguiseNone, DisguiseTLSRecord}

// recordHeaderLen is the length of a TLS record header: content type, protocol
// version and payload length.
const recordHeaderLen = 5

// maxRecordPayload is the largest payload a single TLS record may carry.
const maxRecordPayload = 16384

// tlsRecordContentTypeApplicationData is the content type a data record uses. The
// number is from RFC 5246 section 6.2.1.
const tlsRecordContentTypeApplicationData = 0x17

// ErrNotRecord reports a stream that does not begin with the record header this
// wrapper writes, which is what a peer that is not wrapped looks like.
var ErrNotRecord = errors.New("obfs: the stream is not carrying record-framed data")

// IsDisguise reports whether name is a disguise this package implements.
func IsDisguise(name string) bool {
	for _, candidate := range Disguises {
		if candidate == name {
			return true
		}
	}
	return false
}

// Wrap applies a disguise to a connection.
//
// The returned connection keeps the underlying connection's deadlines and addresses,
// so it can be used everywhere the original could. The underlying connection is
// closed by closing the returned one.
func Wrap(conn net.Conn, disguise string) (net.Conn, error) {
	switch disguise {
	case "", DisguiseNone:
		return conn, nil
	case DisguiseTLSRecord:
		return &recordConn{Conn: conn}, nil
	default:
		return nil, fmt.Errorf("obfs: %q is not a disguise this build implements (use %s or %s)",
			disguise, DisguiseNone, DisguiseTLSRecord)
	}
}

// recordConn writes and reads a stream of TLS application-data records.
type recordConn struct {
	net.Conn

	// writeMu serialises whole writes. Two concurrent writers would otherwise be
	// able to put one record header in front of another writer's payload, which
	// produces a stream neither reader can parse.
	writeMu sync.Mutex

	// readMu guards the record reader state, since the framer may read from one
	// goroutine while another only ever writes.
	readMu    sync.Mutex
	remaining uint16
	header    [recordHeaderLen]byte
}

// CloseWrite half-closes the underlying connection when it supports it, so a wrapped stream
// keeps the half-close its caller asked for: the record layer adds a header to each write
// and holds nothing that needs finalising, and the peer reads a clean end of stream at a
// record boundary.
func (c *recordConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}

// Write sends the bytes as one or more records.
func (c *recordConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxRecordPayload {
			chunk = chunk[:maxRecordPayload]
		}

		var header [recordHeaderLen]byte
		header[0] = tlsRecordContentTypeApplicationData
		header[1] = 0x03 // TLS 1.2, which is what a current client offers
		header[2] = 0x03
		header[3] = byte(len(chunk) >> 8)
		header[4] = byte(len(chunk))

		if _, err := writeAll(c.Conn, header[:]); err != nil {
			return total, err
		}
		n, err := writeAll(c.Conn, chunk)
		total += n
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// Read returns bytes from the current record, moving to the next record when it is
// exhausted.
func (c *recordConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	for c.remaining == 0 {
		if _, err := io.ReadFull(c.Conn, c.header[:]); err != nil {
			return 0, err
		}
		length, err := parseHeader(c.header[:])
		if err != nil {
			return 0, err
		}
		// An empty record is legal to write and carries no data, so it is skipped
		// rather than reported as the end of the stream.
		c.remaining = length
	}

	if int(c.remaining) < len(p) {
		p = p[:c.remaining]
	}
	n, err := io.ReadFull(c.Conn, p)
	c.remaining -= uint16(n)
	return n, err
}

// parseHeader validates a record header and returns its payload length.
func parseHeader(header []byte) (uint16, error) {
	if len(header) < recordHeaderLen {
		return 0, fmt.Errorf("%w: the header is %d bytes, want %d", ErrNotRecord, len(header), recordHeaderLen)
	}
	if header[0] != tlsRecordContentTypeApplicationData {
		return 0, fmt.Errorf("%w: the first byte is %#02x, want %#02x for an application-data record",
			ErrNotRecord, header[0], tlsRecordContentTypeApplicationData)
	}
	if header[1] != 0x03 || header[2] != 0x03 {
		return 0, fmt.Errorf("%w: the protocol version is %#02x%02x, want 0303", ErrNotRecord, header[1], header[2])
	}
	return uint16(header[3])<<8 | uint16(header[4]), nil
}

// writeAll writes every byte, tolerating short writes.
func writeAll(conn net.Conn, data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n, err := conn.Write(data)
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
		data = data[n:]
	}
	return written, nil
}
