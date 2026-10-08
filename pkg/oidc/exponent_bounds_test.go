package oidc

import (
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
)

// An RSA exponent is a small number — 65537 is three bytes — but nothing
// bounded the decode, so an overlong one silently wrapped a uint64 and became
// a key that simply never verified, reported as a generic signature failure.
// The entry is now refused by name at parse time.
func TestAJWKWithAnOverlongExponentIsRefusedByName(t *testing.T) {
	modulus := make([]byte, 128)
	modulus[0] = 0x80
	modulusB64 := base64.RawURLEncoding.EncodeToString(modulus)
	overlong := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 0x01, 0x00, 0x01, 0, 0, 0, 0})

	key, err := publicKeyFromJWK("RSA", "", modulusB64, overlong, "", "")
	if err == nil {
		t.Fatalf("an overlong exponent produced a key %+v instead of an error", key)
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("unexpected error for an overlong exponent: %v", err)
	}

	// Five bytes is past the bound as well: the exponent itself is irrelevant
	// once the encoding cannot be one.
	justPast := base64.RawURLEncoding.EncodeToString([]byte{1, 0x01, 0x00, 0x01, 0x00})
	if _, err := publicKeyFromJWK("RSA", "", modulusB64, justPast, "", ""); err == nil {
		t.Fatal("a five-byte exponent was accepted")
	}

	// The real world still parses: 65537, the exponent every issuer ships.
	key, err = publicKeyFromJWK("RSA", "", modulusB64, "AQAB", "", "")
	if err != nil {
		t.Fatalf("the ordinary exponent was refused: %v", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("an RSA JWK produced %T", key)
	}
	if rsaKey.E != 65537 {
		t.Fatalf("the exponent decoded to %d, want 65537", rsaKey.E)
	}
	if rsaKey.N.Cmp(big.NewInt(0)) == 0 {
		t.Fatal("the modulus decoded to zero")
	}
}
