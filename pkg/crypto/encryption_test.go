package crypto

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestShortPassphraseIsAccepted(t *testing.T) {
	// The v1 code used the passphrase bytes directly as the key, so anything that
	// was not exactly 32 bytes made every call fail with "bad key length".
	for _, passphrase := range []string{"x", "change-me", strings.Repeat("a", 200)} {
		cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, passphrase, "salt")
		if err != nil {
			t.Fatalf("NewCipher(%q): %v", passphrase, err)
		}
		sealed, err := cipher.Seal([]byte("hello"))
		if err != nil {
			t.Fatalf("Seal with passphrase %q: %v", passphrase, err)
		}
		plaintext, err := cipher.Open(sealed)
		if err != nil {
			t.Fatalf("Open with passphrase %q: %v", passphrase, err)
		}
		if string(plaintext) != "hello" {
			t.Fatalf("round trip produced %q", plaintext)
		}
	}
}

func TestCipherIsDeterminedByPassphraseAndSalt(t *testing.T) {
	a, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt-a")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt-b")
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := a.Seal([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err != nil {
		t.Fatalf("same passphrase and salt must interoperate: %v", err)
	}
	if _, err := c.Open(sealed); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("a different salt must not decrypt, got %v", err)
	}
}

func TestTamperedCiphertextIsRejected(t *testing.T) {
	cipher, err := NewCipher(AlgorithmAES256GCM, "passphrase", "salt")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.Seal([]byte("important"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, err := cipher.Open(sealed); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampering must be detected, got %v", err)
	}
}

func TestDisabledCipherIsAPassthrough(t *testing.T) {
	cipher, err := NewCipher(AlgorithmNone, "", "")
	if err != nil {
		t.Fatalf("NewCipher(none): %v", err)
	}
	if cipher.Enabled() {
		t.Fatal("algorithm none must not enable encryption")
	}
	if cipher.Algorithm() != AlgorithmNone {
		t.Fatalf("Algorithm = %q", cipher.Algorithm())
	}
	sealed, err := cipher.Seal([]byte("plain"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) != "plain" {
		t.Fatalf("disabled cipher altered the data: %q", sealed)
	}
}

func TestUnknownAlgorithmIsRejected(t *testing.T) {
	if _, err := NewCipher("rot13", "passphrase", "salt"); err == nil {
		t.Fatal("expected an error for an unknown algorithm")
	}
	if _, err := NewCipher(AlgorithmXChaCha20Poly1305, "", "salt"); err == nil {
		t.Fatal("expected an error when encryption is on but the passphrase is empty")
	}
}

func TestEqualTokens(t *testing.T) {
	if !EqualTokens("abcdef", "abcdef") {
		t.Fatal("identical tokens must compare equal")
	}
	if EqualTokens("abcdef", "abcdeg") {
		t.Fatal("different tokens must not compare equal")
	}
	if EqualTokens("abcdef", "abcde") {
		t.Fatal("different lengths must not compare equal")
	}
}

func TestStreamRoundTrip(t *testing.T) {
	cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt")
	if err != nil {
		t.Fatal(err)
	}

	// A pipe stands in for a socket: what one Stream writes, the other reads.
	left, right := net.Pipe()
	writer := NewStream(left, cipher)
	reader := NewStream(right, cipher)

	payload := bytes.Repeat([]byte("aether"), 20000) // 120 KB: several records
	go func() {
		if _, err := writer.Write(payload[:40000]); err != nil {
			t.Errorf("write: %v", err)
		}
		if _, err := writer.Write(payload[40000:]); err != nil {
			t.Errorf("write: %v", err)
		}
		left.Close()
	}()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream round trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestLargeWriteIsSplitIntoRecords(t *testing.T) {
	cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt")
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	writer := NewStream(left, cipher)
	reader := NewStream(right, cipher)

	payload := bytes.Repeat([]byte("z"), MaxRecordSize+1234)
	go func() {
		n, err := writer.Write(payload)
		if err != nil {
			t.Errorf("write: %v", err)
		}
		if n != len(payload) {
			t.Errorf("write consumed %d bytes, want %d", n, len(payload))
		}
		left.Close()
	}()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
}

func TestOversizedRecordOnTheWireIsRefused(t *testing.T) {
	cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, "passphrase", "salt")
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	reader := NewStream(right, cipher)

	// A peer that announces an enormous record must be rejected before the
	// allocation, not after.
	header := []byte{0x7f, 0xff, 0xff, 0xff}
	go func() { _, _ = left.Write(header) }()

	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("expected an error for an out-of-range record length")
	}
}

func TestUnencryptedStreamIsAPassthrough(t *testing.T) {
	cipher, err := NewCipher(AlgorithmNone, "", "")
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()

	go func() {
		stream := NewStream(left, cipher)
		_, _ = stream.Write([]byte("raw bytes on the wire"))
		left.Close()
	}()

	got, err := io.ReadAll(NewStream(right, cipher))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "raw bytes on the wire" {
		t.Fatalf("passthrough altered the data: %q", got)
	}
}
