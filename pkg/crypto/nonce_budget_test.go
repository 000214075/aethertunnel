package crypto

import (
	"errors"
	"testing"
)

// A NonceCache budget can run out while every nonce it holds is still fresh.
// claim used the same value for "seen before" and "no room left", so
// VerifyIdentity mapped the exhausted budget to ErrIdentityReplayed and sent an
// operator looking for a replayed handshake. The two refusals are now distinct.
func TestAnExhaustedNonceCacheIsReportedAsABudgetNotAReplay(t *testing.T) {
	identity, nonce, stamp, signature := signedIdentity(t)
	seen := NewNonceCache(1)

	// The first assertion spends the whole budget for this key.
	if err := VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(), Nonce: nonce, Timestamp: stamp,
		Signature: signature, Seen: seen,
	}); err != nil {
		t.Fatalf("the first verify: %v", err)
	}

	// A fresh nonce is refused because the budget is spent, not because it was
	// seen before: the two carry different errors now.
	fresh := make([]byte, IdentityNonceSize)
	copy(fresh, nonce)
	fresh[0] ^= 0xff
	if err := VerifyIdentity(IdentityCheckInput{
		PublicKey: identity.PublicKey(), Nonce: fresh, Timestamp: stamp,
		Signature: identity.SignChallenge(fresh, stamp), Seen: seen,
	}); !errors.Is(err, ErrIdentityScopeFull) {
		t.Fatalf("an exhausted cache returned %v, want ErrIdentityScopeFull", err)
	}
}
