// Package crypto provides the optional AEAD protection used for control messages
// and for tunnelled byte streams.
//
// Two things are deliberately different from the v1 code that this replaces:
//
//  1. The key is never used raw. Callers pass a passphrase (typically the shared
//     auth token) and the key is derived with HKDF-SHA256, so any passphrase length
//     works and the wire key is not the same string that is sent as a credential.
//  2. Ciphers are selected by name and validated at construction time, so a
//     misconfiguration fails immediately instead of on the first packet.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Algorithm names accepted by NewCipher.
const (
	AlgorithmXChaCha20Poly1305 = "xchacha20-poly1305"
	AlgorithmAES256GCM         = "aes-256-gcm"
	AlgorithmNone              = "none"
)

// keySize is the key length required by every supported AEAD.
const keySize = 32

// hkdfInfo domain-separates this key from any other use of the same passphrase.
const hkdfInfo = "aethertunnel/v3/aead"

// ErrAuthFailed is returned by Open when a record does not authenticate. Callers
// must treat it as fatal for the connection: it means the peer is not holding the
// key, or the record was tampered with.
var ErrAuthFailed = errors.New("crypto: message authentication failed")

// Cipher is an AEAD with a fixed 32-byte key and the framing helpers used on the
// wire. A nil *Cipher is valid and means "no encryption"; every method copes.
type Cipher struct {
	aead      cipher.AEAD
	algorithm string
}

// NewCipher builds a Cipher from a passphrase and a salt.
//
// The passphrase is run through HKDF-SHA256 with the given salt, so short tokens
// and long passphrases both work. Passing AlgorithmNone (or an empty algorithm)
// with an empty passphrase returns (nil, nil): encryption disabled.
func NewCipher(algorithm, passphrase, salt string) (*Cipher, error) {
	if algorithm == "" {
		algorithm = AlgorithmNone
	}
	if algorithm == AlgorithmNone {
		return nil, nil
	}
	if passphrase == "" {
		return nil, errors.New("crypto: encryption is enabled but no passphrase is configured")
	}

	key := make([]byte, keySize)
	reader := hkdf.New(sha256.New, []byte(passphrase), []byte(salt), []byte(hkdfInfo))
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("crypto: derive key: %w", err)
	}

	var (
		aead cipher.AEAD
		err  error
	)
	switch algorithm {
	case AlgorithmXChaCha20Poly1305:
		aead, err = chacha20poly1305.NewX(key)
	case AlgorithmAES256GCM:
		block, blockErr := aes.NewCipher(key)
		if blockErr != nil {
			return nil, fmt.Errorf("crypto: aes: %w", blockErr)
		}
		aead, err = cipher.NewGCM(block)
	default:
		return nil, fmt.Errorf("crypto: unsupported algorithm %q (supported: %s, %s, %s)",
			algorithm, AlgorithmXChaCha20Poly1305, AlgorithmAES256GCM, AlgorithmNone)
	}
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}

	return &Cipher{aead: aead, algorithm: algorithm}, nil
}

// Enabled reports whether this cipher actually encrypts.
func (c *Cipher) Enabled() bool { return c != nil && c.aead != nil }

// Algorithm returns the configured algorithm name.
func (c *Cipher) Algorithm() string {
	if !c.Enabled() {
		return AlgorithmNone
	}
	return c.algorithm
}

// Overhead is the number of bytes Seal adds on top of the plaintext.
func (c *Cipher) Overhead() int {
	if !c.Enabled() {
		return 0
	}
	return c.aead.Overhead() + c.aead.NonceSize()
}

// Seal encrypts plaintext and returns nonce||ciphertext. When encryption is
// disabled it returns the plaintext unchanged.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	if !c.Enabled() {
		return plaintext, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. It returns ErrAuthFailed when the record does not
// authenticate.
func (c *Cipher) Open(record []byte) ([]byte, error) {
	if !c.Enabled() {
		return record, nil
	}
	if len(record) < c.aead.NonceSize() {
		return nil, ErrAuthFailed
	}
	nonce, ciphertext := record[:c.aead.NonceSize()], record[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrAuthFailed
	}
	return plaintext, nil
}

// SealString and OpenString are convenience wrappers for text payloads.
func (c *Cipher) SealString(plaintext string) (string, error) {
	sealed, err := c.Seal([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return string(sealed), nil
}

// OpenString reverses SealString.
func (c *Cipher) OpenString(record string) (string, error) {
	plaintext, err := c.Open([]byte(record))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// EqualTokens compares two secrets in constant time, so that comparing an auth
// token does not leak its prefix through timing.
func EqualTokens(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// --- framed stream encryption -------------------------------------------------
//
// A Stream wraps an ordered, reliable byte stream (a TCP connection after the
// handshake) with per-record AEAD. Each record is
//
//	[len:4 big endian][sealed payload of len bytes]
//
// where the sealed payload is nonce||ciphertext produced by Seal. Records may be
// written concurrently from several goroutines; reads must come from one.

// MaxRecordSize caps the size of a single decrypted record, bounding the memory a
// peer can make us allocate.
const MaxRecordSize = 64 * 1024

// Stream adds optional AEAD to a byte stream. It implements io.ReadWriteCloser and
// forwards SetReadDeadline to the underlying connection when that connection
// supports deadlines, so the tunnel's idle timeout still applies through it.
type Stream struct {
	conn io.ReadWriter
	aead *Cipher

	writeMu sync.Mutex
	readBuf []byte // holds the plaintext of the record currently being consumed
	lenBuf  [4]byte
}

// NewStream wraps conn. A disabled cipher makes the stream a passthrough, which is
// what the "encryption off" configuration should cost.
func NewStream(conn io.ReadWriter, aead *Cipher) *Stream {
	return &Stream{conn: conn, aead: aead}
}

// Read returns decrypted bytes. It buffers at most one record at a time.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !s.aead.Enabled() {
		return s.conn.Read(p)
	}
	if len(s.readBuf) == 0 {
		if _, err := io.ReadFull(s.conn, s.lenBuf[:]); err != nil {
			return 0, err
		}
		length := binary.BigEndian.Uint32(s.lenBuf[:])
		if length == 0 || int(length) > MaxRecordSize+s.aead.Overhead() {
			return 0, fmt.Errorf("crypto: record length %d out of range", length)
		}
		record := make([]byte, length)
		if _, err := io.ReadFull(s.conn, record); err != nil {
			return 0, err
		}
		plaintext, err := s.aead.Open(record)
		if err != nil {
			return 0, err
		}
		s.readBuf = plaintext
	}
	n := copy(p, s.readBuf)
	s.readBuf = s.readBuf[n:]
	return n, nil
}

// Write encrypts p as one or more records.
//
// A write larger than MaxRecordSize is split rather than refused: io.Writer
// requires that a successful write consumes all of p, and a caller that happens to
// use a 64 KiB+ buffer (io.Copy with a custom buffer, for instance) must not get an
// error it cannot act on.
func (s *Stream) Write(p []byte) (int, error) {
	if !s.aead.Enabled() {
		return s.conn.Write(p)
	}
	if len(p) == 0 {
		return 0, nil
	}

	total := 0
	for total < len(p) {
		end := total + MaxRecordSize
		if end > len(p) {
			end = len(p)
		}
		if err := s.writeRecord(p[total:end]); err != nil {
			return total, err
		}
		total = end
	}
	return total, nil
}

// writeRecord seals and sends one record of at most MaxRecordSize bytes.
func (s *Stream) writeRecord(p []byte) error {
	sealed, err := s.aead.Seal(p)
	if err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	frame := make([]byte, 4+len(sealed))
	binary.BigEndian.PutUint32(frame, uint32(len(sealed)))
	copy(frame[4:], sealed)
	if _, err := s.conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying connection when it is closeable. The AEAD layer
// holds no state that needs flushing, so there is nothing else to do.
func (s *Stream) Close() error {
	if closer, ok := s.conn.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// SetReadDeadline forwards to the underlying connection so that an idle timeout
// keeps working when the stream is encrypted.
func (s *Stream) SetReadDeadline(t time.Time) error {
	if setter, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		return setter.SetReadDeadline(t)
	}
	return nil
}
