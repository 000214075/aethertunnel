// Package obfs wraps a connection so that what a passive observer sees on the wire
// is not the tunnel's own frame header.
//
// Two kinds of wrapper live here. The record disguise is a byte-stream transform:
// it adds no handshake and no key material, so it hides the shape of the traffic
// rather than its contents, and it is useful only alongside [encryption] and
// [transport]. The session disguise puts the connection inside a real TLS session:
// a genuine handshake, real encryption, an anonymous certificate.
package obfs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
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
	// DisguiseTLSSession puts the connection inside a real TLS session.
	//
	// Every connection performs a genuine TLS 1.2-or-newer handshake — ClientHello,
	// key exchange, alerts where they belong — and everything afterwards is real
	// TLS-encrypted. The certificate the listener answers with is freshly minted
	// and self-signed per connection, so the server stays anonymous and two
	// connections share nothing an observer could correlate; proving which server
	// this is remains the identity layer's job. A detector that models TLS sessions
	// sees a session that is one.
	DisguiseTLSSession = "tls-session"
)

// Disguises lists the values [obfuscation].disguise accepts.
var Disguises = []string{DisguiseNone, DisguiseTLSRecord, DisguiseTLSSession}

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

// Endpoint tells Wrap which end of the connection it is wrapping. The record
// disguise is symmetric, but the session disguise performs a TLS handshake, and
// a handshake has two different roles.
type Endpoint int

const (
	// Dialer is the end that dials: it drives the TLS handshake as the client.
	Dialer Endpoint = iota
	// Listener is the end that accepted the connection: it answers the handshake
	// as the server, with a fresh self-signed certificate.
	Listener
)

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
func Wrap(conn net.Conn, disguise string, endpoint Endpoint) (net.Conn, error) {
	switch disguise {
	case "", DisguiseNone:
		return conn, nil
	case DisguiseTLSRecord:
		return &recordConn{Conn: conn}, nil
	case DisguiseTLSSession:
		return wrapSession(conn, endpoint)
	default:
		return nil, fmt.Errorf("obfs: %q is not a disguise this build implements (use %s)",
			disguise, strings.Join(Disguises, ", "))
	}
}

// sessionALPN is the application-layer protocol the session disguise negotiates,
// so a probe that completes the handshake sees a name that says what carries the
// traffic inside.
const sessionALPN = "aethertunnel/4"

// wrapSession puts the connection inside a real TLS session. The listener mints a
// fresh self-signed certificate for every connection; the dialer drives the
// handshake without verifying it, because the certificate is anonymous by design
// and proving which server this is belongs to the identity layer.
func wrapSession(conn net.Conn, endpoint Endpoint) (net.Conn, error) {
	config := &tls.Config{
		NextProtos: []string{sessionALPN},
		MinVersion: tls.VersionTLS12,
	}
	if endpoint == Listener {
		certificate, err := selfSignedCertificate()
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("obfs: mint the session disguise's certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
		return &sessionConn{Conn: tls.Server(conn, config)}, nil
	}
	// The certificate is anonymous by design; authentication is the protocol's
	// identity layer, checked after the handshake.
	config.InsecureSkipVerify = true
	return &sessionConn{Conn: tls.Client(conn, config)}, nil
}

// sessionConn carries a TLS session over the underlying connection.
type sessionConn struct {
	*tls.Conn
}

// CloseWrite sends close_notify and half-closes the connection underneath, which
// is exactly what the embedded *tls.Conn does. This wrapper used to close the
// whole connection instead, on the belief that TLS has no half-close: it does,
// and a peer that answers only once it has seen the end of the request lost the
// reply, which is what half-closing exists to avoid.
func (c *sessionConn) CloseWrite() error {
	return c.Conn.CloseWrite()
}

// selfSignedCertificate mints the certificate one session-disguised connection
// answers the handshake with. A fresh key pair per connection means two
// connections share nothing an observer could correlate.
func selfSignedCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "aethertunnel"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * 365 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
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
