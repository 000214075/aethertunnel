package crypto

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// The per-session key exists so no long-lived key ever seals bulk traffic with
// random nonces: AES-GCM bounds random nonces at 2^32 invocations per key
// (NIST SP 800-38D). The derivation therefore has to be deterministic in the
// inputs both ends share (the token and the session salt) and different for
// every other combination, and the result has to work as an AEAD key.
func TestSessionKeyBindsTheSharedSecretToTheSessionSalt(t *testing.T) {
	salt, err := SessionSalt()
	if err != nil {
		t.Fatalf("session salt: %v", err)
	}
	if len(salt) != SessionSaltLen {
		t.Fatalf("the salt is %d bytes, want %d", len(salt), SessionSaltLen)
	}
	other, err := SessionSalt()
	if err != nil {
		t.Fatalf("second salt: %v", err)
	}
	if bytes.Equal(salt, other) {
		t.Fatal("two drawn salts are identical")
	}

	key, err := SessionKey("shared-token", salt)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	again, err := SessionKey("shared-token", salt)
	if err != nil {
		t.Fatalf("second derivation: %v", err)
	}
	if !bytes.Equal(key, again) {
		t.Fatal("the same secret and salt derived different keys")
	}
	if otherSalt, err := SessionKey("shared-token", other); err != nil || bytes.Equal(key, otherSalt) {
		t.Fatal("a different salt did not derive a different key")
	}
	if otherSecret, err := SessionKey("another-token", salt); err != nil || bytes.Equal(key, otherSecret) {
		t.Fatal("a different secret did not derive a different key")
	}
	if _, err := SessionKey("", salt); err == nil {
		t.Fatal("an empty secret was accepted")
	}
	if _, err := SessionKey("shared-token", nil); err == nil {
		t.Fatal("an empty salt was accepted")
	}

	// Two ends that reach the same key can seal and open, and a session that
	// derived a different key cannot open what this one sealed.
	cipher, err := NewCipherFromKey(AlgorithmAES256GCM, key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	sealed, err := cipher.Seal([]byte("payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := cipher.Open(sealed)
	if err != nil || string(opened) != "payload" {
		t.Fatalf("roundtrip: %v %q", err, opened)
	}
	wrong, err := SessionKey("another-token", salt)
	if err != nil {
		t.Fatalf("wrong key: %v", err)
	}
	wrongCipher, err := NewCipherFromKey(AlgorithmAES256GCM, wrong)
	if err != nil {
		t.Fatalf("wrong cipher: %v", err)
	}
	if _, err := wrongCipher.Open(sealed); err == nil {
		t.Fatal("a record under one session key opened under another")
	}
}

// The replay cache is shared by every identity client of the server, so it is
// keyed by the claiming client's public key: without that scoping, one listed
// client filling the cache with valid assertions would refuse every other
// identity client for as long as the window runs.
func TestTheNonceCacheScopesReplayByClaimingKey(t *testing.T) {
	cache := NewNonceCache(2)
	now := time.Now()

	// One identity fills the cache with valid assertions.
	if err := cache.claim("identity-a", []byte("one"), now); err != nil {
		t.Fatalf("the first claim was refused: %v", err)
	}
	if err := cache.claim("identity-a", []byte("two"), now); err != nil {
		t.Fatalf("the second claim was refused: %v", err)
	}
	// A full cache must not lock other identities out.
	if err := cache.claim("identity-b", []byte("three"), now); err != nil {
		t.Fatalf("a full cache kept another identity's nonce out: %v", err)
	}
	// Within one identity the refusal stays: a replay is a replay.
	if err := cache.claim("identity-a", []byte("one"), now); !errors.Is(err, ErrIdentityReplayed) {
		t.Fatalf("a repeated nonce returned %v, want ErrIdentityReplayed", err)
	}
	// The budget is per claiming key, so identity-a's full bucket refuses its
	// own overflow while identity-b keeps claiming.
	if err := cache.claim("identity-a", []byte("four"), now); !errors.Is(err, ErrIdentityScopeFull) {
		t.Fatalf("a claim past the limit returned %v, want ErrIdentityScopeFull", err)
	}
	if cache.Len() != 3 {
		t.Fatalf("cache holds %d entries, want 3", cache.Len())
	}
}
