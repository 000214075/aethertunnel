package crypto

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func signedIdentity(t *testing.T) (*Identity, []byte, int64, []byte) {
	t.Helper()

	identity, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	nonce, err := Nonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	now := time.Now()
	signature := identity.SignChallenge(nonce, now.Unix())
	return identity, nonce, now.Unix(), signature
}

func TestIdentityRoundTrip(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)

	err := VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp,
		Signature: signature,
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp,
		Signature: signature,
		Allowed:   []ed25519.PublicKey{identity.PublicKey()},
	})
	if err != nil {
		t.Fatalf("verify with an allow list: %v", err)
	}
}

func TestIdentityRejectsATamperedAssertion(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)

	// The signature covers the nonce, so a different nonce must not verify.
	other, err := Nonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     other,
		Timestamp: stamp,
		Signature: signature,
	})
	if !errors.Is(err, ErrIdentitySignature) {
		t.Fatalf("a different nonce returned %v, want ErrIdentitySignature", err)
	}

	// Nor does a different timestamp.
	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp + 1,
		Signature: signature,
	})
	if !errors.Is(err, ErrIdentitySignature) {
		t.Fatalf("a different timestamp returned %v, want ErrIdentitySignature", err)
	}
}

func TestIdentityRejectsAStaleTimestamp(t *testing.T) {
	identity, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	nonce, err := Nonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}

	old := time.Now().Add(-10 * IdentityWindow)
	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: old.Unix(),
		Signature: identity.SignChallenge(nonce, old.Unix()),
	})
	if !errors.Is(err, ErrIdentityStale) {
		t.Fatalf("verify returned %v, want ErrIdentityStale", err)
	}

	future := time.Now().Add(10 * IdentityWindow)
	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: future.Unix(),
		Signature: identity.SignChallenge(nonce, future.Unix()),
	})
	if !errors.Is(err, ErrIdentityStale) {
		t.Fatalf("a future timestamp returned %v, want ErrIdentityStale", err)
	}
}

func TestIdentityRejectsAReplay(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)
	seen := NewNonceCache(16)

	in := IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp,
		Signature: signature,
		Seen:      seen,
	}
	if err := VerifyIdentity(in); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if err := VerifyIdentity(in); !errors.Is(err, ErrIdentityReplayed) {
		t.Fatalf("second verify returned %v, want ErrIdentityReplayed", err)
	}
}

func TestIdentityRejectsAKeyThatIsNotAllowed(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)
	other, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}

	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp,
		Signature: signature,
		Allowed:   []ed25519.PublicKey{other.PublicKey()},
	})
	if !errors.Is(err, ErrIdentityNotAllowed) {
		t.Fatalf("verify returned %v, want ErrIdentityNotAllowed", err)
	}
}

func TestIdentityRejectsMalformedAssertions(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)

	if err := VerifyIdentity(IdentityCheckInput{PublicKey: identity.PublicKey()[:8], Nonce: nonce, Timestamp: stamp, Signature: signature}); !errors.Is(err, ErrIdentityKeySize) {
		t.Fatalf("short key returned %v", err)
	}
	if err := VerifyIdentity(IdentityCheckInput{PublicKey: identity.PublicKey(), Nonce: nonce[:4], Timestamp: stamp, Signature: signature}); !errors.Is(err, ErrIdentityNonceLength) {
		t.Fatalf("short nonce returned %v", err)
	}
	if err := VerifyIdentity(IdentityCheckInput{PublicKey: identity.PublicKey(), Nonce: nonce, Timestamp: stamp, Signature: signature[:8]}); !errors.Is(err, ErrIdentitySignature) {
		t.Fatalf("short signature returned %v", err)
	}
}

func TestIdentityPersistsAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.key")

	first, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the identity file was not created: %v", err)
	}

	second, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !bytes.Equal(first.PublicKey(), second.PublicKey()) {
		t.Fatal("reloading the file produced a different identity")
	}

	if err := os.WriteFile(path, []byte("not hex at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadIdentity(path); err == nil {
		t.Fatal("a corrupt identity file was accepted")
	}
}

func TestNonceCacheForgetsExpiredEntries(t *testing.T) {
	cache := NewNonceCache(8)
	now := time.Now()

	if !cache.claim([]byte("nonce-one"), now) {
		t.Fatal("the first claim was refused")
	}
	if cache.claim([]byte("nonce-one"), now) {
		t.Fatal("a repeated claim was accepted")
	}
	if cache.Len() != 1 {
		t.Fatalf("cache holds %d entries, want 1", cache.Len())
	}

	// After two windows the entry is gone, so the cache does not grow forever.
	later := now.Add(3 * IdentityWindow)
	if !cache.claim([]byte("nonce-two"), later) {
		t.Fatal("a second claim was refused")
	}
	if cache.Len() != 1 {
		t.Fatalf("cache holds %d entries, want 1 after expiry", cache.Len())
	}
}

func TestNonceCacheRefusesWhenFull(t *testing.T) {
	cache := NewNonceCache(2)
	now := time.Now()

	if !cache.claim([]byte("a"), now) || !cache.claim([]byte("b"), now) {
		t.Fatal("the first two claims were refused")
	}
	if cache.claim([]byte("c"), now) {
		t.Fatal("a claim past the limit was accepted")
	}
}

func TestParseIdentityKey(t *testing.T) {
	identity, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	key, err := ParseIdentityKey(identity.PublicKeyHex())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !key.Equal(identity.PublicKey()) {
		t.Fatal("the parsed key differs from the original")
	}
	if _, err := ParseIdentityKey("zz"); err == nil {
		t.Fatal("non-hex input was accepted")
	}
	if _, err := ParseIdentityKey("00112233"); err == nil {
		t.Fatal("a short key was accepted")
	}
}
