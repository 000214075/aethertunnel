package crypto

import (
	"bytes"
	"testing"
)

func TestHybridAgreementProducesTheSameSessionKey(t *testing.T) {
	clientPublic, clientState, err := HybridClientInit()
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	if len(clientPublic) != HybridPublicKeySize {
		t.Fatalf("client public key is %d bytes, want %d", len(clientPublic), HybridPublicKeySize)
	}

	response, serverKey, err := HybridServerFinish(clientPublic)
	if err != nil {
		t.Fatalf("server finish: %v", err)
	}
	if len(response) != HybridResponseSize {
		t.Fatalf("server response is %d bytes, want %d", len(response), HybridResponseSize)
	}

	clientKey, err := HybridClientFinish(clientState, response)
	if err != nil {
		t.Fatalf("client finish: %v", err)
	}
	if !bytes.Equal(clientKey, serverKey) {
		t.Fatal("the two sides derived different session keys")
	}
	if len(clientKey) != SessionKeySize {
		t.Fatalf("session key is %d bytes, want %d", len(clientKey), SessionKeySize)
	}
}

func TestHybridSessionsAreIndependent(t *testing.T) {
	keys := make([][]byte, 4)
	for i := range keys {
		clientPublic, clientState, err := HybridClientInit()
		if err != nil {
			t.Fatalf("client init: %v", err)
		}
		response, _, err := HybridServerFinish(clientPublic)
		if err != nil {
			t.Fatalf("server finish: %v", err)
		}
		key, err := HybridClientFinish(clientState, response)
		if err != nil {
			t.Fatalf("client finish: %v", err)
		}
		keys[i] = key
	}

	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				t.Fatalf("sessions %d and %d produced the same key", i, j)
			}
		}
	}
}

func TestHybridRejectsMalformedMaterial(t *testing.T) {
	clientPublic, clientState, err := HybridClientInit()
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	response, _, err := HybridServerFinish(clientPublic)
	if err != nil {
		t.Fatalf("server finish: %v", err)
	}

	if _, err := HybridClientFinish(clientState[:10], response); err == nil {
		t.Fatal("a short client state was accepted")
	}
	if _, err := HybridClientFinish(clientState, response[:10]); err == nil {
		t.Fatal("a short server response was accepted")
	}
	if _, _, err := HybridServerFinish(clientPublic[:10]); err == nil {
		t.Fatal("a short client public key was accepted")
	}

	// An all-zero X25519 public key is a low-order point, so the agreement must
	// be refused rather than producing a predictable secret.
	zeroed := append([]byte(nil), clientPublic...)
	for i := 0; i < X25519Size; i++ {
		zeroed[i] = 0
	}
	if _, _, err := HybridServerFinish(zeroed); err == nil {
		t.Fatal("an all-zero X25519 key was accepted")
	}
}

func TestTamperedMLKEMCiphertextYieldsADifferentKey(t *testing.T) {
	clientPublic, clientState, err := HybridClientInit()
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	response, serverKey, err := HybridServerFinish(clientPublic)
	if err != nil {
		t.Fatalf("server finish: %v", err)
	}

	tampered := append([]byte(nil), response...)
	tampered[len(tampered)-1] ^= 0x01

	clientKey, err := HybridClientFinish(clientState, tampered)
	if err != nil {
		t.Fatalf("client finish: %v", err)
	}
	// ML-KEM decapsulation never fails outright; it derives an unrelated secret.
	// What matters is that the two sides no longer agree, so the session fails
	// closed at the first authenticated frame.
	if bytes.Equal(clientKey, serverKey) {
		t.Fatal("a tampered ciphertext produced the server's key")
	}
}

func TestSessionKeyIDIsStableAndHidesTheKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, SessionKeySize)
	first := SessionKeyID(key)
	if first != SessionKeyID(key) {
		t.Fatal("the key identifier is not stable")
	}
	if len(first) != 16 {
		t.Fatalf("the key identifier is %d characters, want 16", len(first))
	}
	if bytes.Contains([]byte(first), key) {
		t.Fatal("the key identifier contains the key")
	}

	other := append([]byte(nil), key...)
	other[0] ^= 0xff
	if first == SessionKeyID(other) {
		t.Fatal("two different keys share an identifier")
	}
}

func TestCipherFromSessionKeyEncrypts(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, SessionKeySize)
	cipher, err := NewCipherFromKey(AlgorithmXChaCha20Poly1305, key)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	if !cipher.Enabled() {
		t.Fatal("the cipher reports itself disabled")
	}

	sealed, err := cipher.Seal([]byte("post-quantum payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("post-quantum payload")) {
		t.Fatal("the plaintext is visible in the sealed record")
	}
	opened, err := cipher.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != "post-quantum payload" {
		t.Fatalf("round trip returned %q", opened)
	}

	if _, err := NewCipherFromKey(AlgorithmXChaCha20Poly1305, key[:16]); err == nil {
		t.Fatal("a short session key was accepted")
	}
	if cipher, err := NewCipherFromKey(AlgorithmNone, key); err != nil || cipher != nil {
		t.Fatal("the none algorithm should disable encryption")
	}
}
