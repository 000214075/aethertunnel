package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A refresh that fails once — one 500, one network blip — used to slam the
// whole verifier shut for as long as the cooldown ran: even a token whose
// signing key was still sitting in the cache was refused, although the same
// token a second earlier had verified. A key the issuer published keeps
// verifying until the issuer can be asked again, and revocation still lands as
// soon as a refresh succeeds.
func TestAFailedRefreshKeepsServingTheCachedSigningKey(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	replacement, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("replacement key: %v", err)
	}

	var mode atomic.Int32 // 0 = serve the key set, 1 = answer 500
	var rotated atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		keys := []map[string]any{rsaJWK("kid-1", rsaKey)}
		if rotated.Load() {
			keys = []map[string]any{rsaJWK("kid-2", replacement)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	v, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	cached, err := v.signingKey("kid-1")
	if err != nil {
		t.Fatalf("the first lookup failed although the issuer answers: %v", err)
	}

	// Age the set past its refresh interval and fail one refresh.
	stale := time.Now().Add(-2 * jwksRefreshInterval)
	v.mu.Lock()
	v.fetchedAt = stale
	v.attemptAt = stale
	v.mu.Unlock()
	mode.Store(1)

	// This caller performs the failed fetch: it must still be handed the key
	// the issuer published, instead of an error.
	served, err := v.signingKey("kid-1")
	if err != nil {
		t.Fatalf("a failed refresh refused a key the cache holds: %v", err)
	}
	if served != cached {
		t.Fatal("the fallback served a key other than the cached one")
	}

	// Callers inside the cooldown afterwards: the cached key keeps serving here
	// too, which is the window that used to refuse everything.
	for i := 0; i < 2; i++ {
		served, err = v.signingKey("kid-1")
		if err != nil {
			t.Fatalf("the cooldown refused a key the cache holds: %v", err)
		}
		if served != cached {
			t.Fatal("the cooldown served a key other than the cached one")
		}
	}

	// An unknown kid during the same outage is still refused, and the error
	// names the refresh failure rather than pretending to know the key set.
	if _, err := v.signingKey("kid-9"); err == nil || !strings.Contains(err.Error(), "could not be refreshed") {
		t.Fatalf("an unknown kid during the outage answered %v, want the refresh failure", err)
	}

	// Recovery: the issuer rotates, the new set no longer names kid-1, and the
	// first successful refresh makes that stick.
	mode.Store(0)
	rotated.Store(true)
	v.mu.Lock()
	v.attemptAt = stale
	v.mu.Unlock()
	if _, err := v.signingKey("kid-1"); err == nil || !strings.Contains(err.Error(), "publishes no key") {
		t.Fatalf("a revoked key kept verifying after a successful refresh: %v", err)
	}
	if _, err := v.signingKey("kid-2"); err != nil {
		t.Fatalf("the rotated key was not served: %v", err)
	}
}

func rsaJWK(kid string, key *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kid": kid, "alg": "RS256", "kty": "RSA",
		"n": base64url(key.N.Bytes()),
		"e": base64url(bigEndian(uint64(key.E))),
	}
}
