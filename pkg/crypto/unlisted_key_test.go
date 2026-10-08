package crypto

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

// The allow list is consulted only after the signature has been verified. With the
// membership test first, a peer that already held the server's auth token could
// tell a listed key from an unlisted one by the error it was handed, which is an
// oracle for the identities the server authorizes.
func TestAnUnlistedKeyWithABadSignatureReportsTheSignature(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)
	other, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	broken := append([]byte(nil), signature...)
	broken[0] ^= 0xff

	err = VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(),
		Nonce:     nonce,
		Timestamp: stamp,
		Signature: broken,
		Allowed:   []ed25519.PublicKey{other.PublicKey()},
	})
	if !errors.Is(err, ErrIdentitySignature) {
		t.Fatalf("verify returned %v, want ErrIdentitySignature: the allow list answered for an unproven key", err)
	}
}
