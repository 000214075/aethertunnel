package crypto

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// recordReader is an io.ReadWriter over a fixed byte slice: the Stream under test
// only reads from it.
type recordReader struct{ *bytes.Reader }

func (recordReader) Write(b []byte) (int, error) { return len(b), nil }

// A record of exactly Overhead() bytes is a valid, authenticated empty plaintext:
// Open succeeds, there is nothing to copy, and Read used to return (0, nil) with a
// non-empty p. Every caller that loops on a read — io.Copy, pkg/net/pipe.go — then
// read again at once, spinning on a stream that never yields a byte and holding
// back whatever is queued behind the empty record.
func TestAnEmptyEncryptedRecordDoesNotSpinTheReader(t *testing.T) {
	cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	empty, err := cipher.Seal(nil)
	if err != nil {
		t.Fatalf("Seal(nil): %v", err)
	}
	if len(empty) != cipher.Overhead() {
		t.Fatalf("an empty record is %d bytes, want Overhead()=%d", len(empty), cipher.Overhead())
	}
	text, err := cipher.Seal([]byte("after the empty record"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var wire bytes.Buffer
	write := func(record []byte) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(record)))
		wire.Write(hdr[:])
		wire.Write(record)
	}
	write(empty)
	write(text)

	stream := NewStream(recordReader{Reader: bytes.NewReader(wire.Bytes())}, cipher)
	buf := make([]byte, 64)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n == 0 {
		t.Fatal("Read returned (0, nil) for an authenticated empty record, which makes every caller loop")
	}
	if got := string(buf[:n]); got != "after the empty record" {
		t.Fatalf("Read returned %q, want the record after the empty one", got)
	}
}

// A bucket is only revisited by the scope that created it, so a key claimed once
// would leave its bucket resident for the life of the process and the bucket count
// would grow with the number of distinct keys. The window sweep removes buckets
// whose newest entry is older than the window.
func TestAnIdleNonceCacheBucketIsSweptAway(t *testing.T) {
	cache := NewNonceCache(0)
	base := time.Now().Add(-10 * time.Minute)
	if err := cache.claim("key-a", []byte("nonce-a"), base); err != nil {
		t.Fatalf("the first claim of a fresh nonce was refused: %v", err)
	}
	if len(cache.seen) != 1 {
		t.Fatalf("the cache holds %d bucket(s) after one claim, want one", len(cache.seen))
	}

	// A different key, more than a window later: key-a's bucket is idle past the
	// window and cannot hold a nonce that is still valid.
	if err := cache.claim("key-b", []byte("nonce-b"), base.Add(3*IdentityWindow)); err != nil {
		t.Fatalf("the first claim of a second key was refused: %v", err)
	}
	if _, ok := cache.seen["key-a"]; ok {
		t.Fatal("the idle bucket of key-a survived the sweep")
	}
	if len(cache.seen) != 1 {
		t.Fatalf("the cache holds %d buckets after the sweep, want one", len(cache.seen))
	}
}
