package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issuers publish every key of the set in one JWKS, including the shapes this
// verifier cannot build (an OKP entry beside the RSA/EC signing keys). One
// unusable entry must not cost the verifier the usable ones: the old code
// refused the whole set, and every login failed until the issuer republished.
func TestAJwksWithAnUnusableKeyAlongsideUsableOnesKeepsTheUsableKeys(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	set := map[string]any{"keys": []any{
		map[string]any{
			"kid": "rsa-1", "alg": "RS256", "kty": "RSA",
			"n": base64url(rsaKey.N.Bytes()),
			"e": base64url(bigEndian(uint64(rsaKey.E))),
		},
		map[string]any{
			"kid": "okp-1", "alg": "EdDSA", "kty": "OKP", "crv": "Ed25519",
			"x": base64url([]byte("a key this verifier cannot build")),
		},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(server.Close)

	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("NewVerifierWithJWKS: %v", err)
	}
	keys, err := verifier.fetchKeys()
	if err != nil {
		t.Fatalf("fetchKeys refused the whole set: %v", err)
	}
	if _, ok := keys["rsa-1"]; !ok {
		t.Fatalf("the usable key is missing from %v", keys)
	}
	if _, ok := keys["okp-1"]; ok {
		t.Fatalf("the unusable key was installed: %v", keys)
	}
}

// A set with nothing usable in it is still an error, and the error says that
// rather than naming one entry as if the rest were fine.
func TestAJwksWithOnlyUnusableKeysIsRefusedWithTheReason(t *testing.T) {
	set := map[string]any{"keys": []any{
		map[string]any{
			"kid": "okp-1", "alg": "EdDSA", "kty": "OKP", "crv": "Ed25519",
			"x": base64url([]byte("a key this verifier cannot build")),
		},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(server.Close)

	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("NewVerifierWithJWKS: %v", err)
	}
	_, err = verifier.fetchKeys()
	if err == nil {
		t.Fatal("a set with no usable key was accepted")
	}
	if !strings.Contains(err.Error(), "no usable signing keys") {
		t.Fatalf("the error %q does not say that no key is usable", err)
	}
}
