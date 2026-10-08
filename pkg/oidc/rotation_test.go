package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Two tokens carrying the same newly rotated kid, arriving while the first fetch is
// in flight, must both verify. The cooldown check used to run before the in-flight
// wait, so the second caller read the attemptAt the first had just stamped as "too
// soon to ask again" and refused a key the issuer really publishes.
func TestAConcurrentRotationVerificationWaitsForTheFetchInFlight(t *testing.T) {
	first, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the first key: %v", err)
	}
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the rotated key: %v", err)
	}

	var mu sync.Mutex
	rotated := false
	// The seeding fetch must answer; the rotation fetch is the one held open.
	park := false
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		useSecond, shouldPark := rotated, park
		park = false
		mu.Unlock()
		if shouldPark {
			// The rotation fetch, held open so the second caller arrives while it
			// is still running.
			parked <- struct{}{}
			<-release
		}
		key, kid := first, "key-one"
		if useSecond {
			key, kid = second, "key-two"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kid": kid, "alg": "RS256", "kty": "RSA",
			"n": base64url(key.N.Bytes()), "e": base64url(bigEndian(uint64(key.E))),
		}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("build the verifier: %v", err)
	}
	claims := goodClaims("https://issuer.example", "aethertunnel")

	// Seed the cache with the key that is about to be rotated away.
	oldToken, err := SignToken("RS256", "key-one", first, nil, claims)
	if err != nil {
		t.Fatalf("sign the first token: %v", err)
	}
	if err := verifier.Verify(oldToken); err != nil {
		t.Fatalf("the token before the rotation was refused: %v", err)
	}

	// The seeding fetch just stamped the unknown-kid cooldown, and the rotation is
	// meant to be the first thing since then that needs a fresh set: backdate the
	// stamp instead of shortening the cooldown, which is what hid this.
	verifier.mu.Lock()
	verifier.attemptAt = time.Now().Add(-time.Minute)
	verifier.mu.Unlock()

	// The issuer rotates, and the real cooldown is left in place — the tests that
	// shorten it are exactly why this went unnoticed.
	mu.Lock()
	rotated = true
	park = true
	mu.Unlock()
	newToken, err := SignToken("RS256", "key-two", second, nil, claims)
	if err != nil {
		t.Fatalf("sign the rotated token: %v", err)
	}

	firstCall := make(chan error, 1)
	go func() { firstCall <- verifier.Verify(newToken) }()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the rotation fetch never reached the issuer")
	}

	secondCall := make(chan error, 1)
	go func() { secondCall <- verifier.Verify(newToken) }()
	select {
	case err := <-secondCall:
		close(release)
		t.Fatalf("the second caller did not wait for the fetch in flight: %v", err)
	case <-time.After(500 * time.Millisecond):
		// Still waiting, which is the point.
	}
	close(release)

	for _, call := range []chan error{firstCall, secondCall} {
		select {
		case err := <-call:
			if err != nil {
				t.Fatalf("a token signed with the rotated key was refused: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a verification never returned after the fetch completed")
		}
	}
}
