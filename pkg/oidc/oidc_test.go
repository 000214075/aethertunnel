package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// keyPair is one issuer key published through the test JWKS endpoint.
type keyPair struct {
	kid     string
	alg     string
	private any
}

// jwksServer serves a fixed key set and counts how often the verifier had to
// come back for it.
func jwksServer(t *testing.T, keys []keyPair) (*httptest.Server, *int) {
	t.Helper()
	fetches := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		set := map[string]any{"keys": []any{}}
		entries := []any{}
		for _, key := range keys {
			entry := map[string]any{"kid": key.kid, "alg": key.alg}
			switch typed := key.private.(type) {
			case *rsa.PrivateKey:
				entry["kty"] = "RSA"
				entry["n"] = base64url(typed.N.Bytes())
				entry["e"] = base64url(bigEndian(uint64(typed.E)))
			case *ecdsa.PrivateKey:
				entry["kty"] = "EC"
				entry["crv"] = typed.Curve.Params().Name
				size := (typed.Curve.Params().BitSize + 7) / 8
				entry["x"] = base64url(typed.X.Bytes())
				entry["y"] = base64url(typed.Y.Bytes())
				_ = size
			}
			entries = append(entries, entry)
		}
		set["keys"] = entries
		_ = json.NewEncoder(w).Encode(set)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": "http://" + r.Host + "/jwks"})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &fetches
}

// The claims a matching token carries: issued now by the expected issuer for
// the expected audience, expiring in an hour.
func goodClaims(issuer, audience string) map[string]any {
	return map[string]any{
		"iss": issuer,
		"aud": audience,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func TestVerifyAcceptsRSClaims(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the rsa key: %v", err)
	}
	server, _ := jwksServer(t, []keyPair{{kid: "rsa-1", alg: "RS256", private: rsaKey}})
	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("build the verifier: %v", err)
	}

	token, err := SignToken("RS256", "rsa-1", rsaKey, nil, goodClaims("https://issuer.example", "aethertunnel"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verifier.Verify(token); err != nil {
		t.Fatalf("a well-formed token was refused: %v", err)
	}
}

func TestVerifyAcceptsECAndAudienceArrays(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the ec key: %v", err)
	}
	server, _ := jwksServer(t, []keyPair{{kid: "ec-1", alg: "ES256", private: ecKey}})
	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("build the verifier: %v", err)
	}

	claims := goodClaims("https://issuer.example", "aethertunnel")
	claims["aud"] = []any{"other-app", "aethertunnel"}
	token, err := SignToken("ES256", "ec-1", ecKey, nil, claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verifier.Verify(token); err != nil {
		t.Fatalf("an ES256 token with an audience list was refused: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the rsa key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the second key: %v", err)
	}
	server, _ := jwksServer(t, []keyPair{
		{kid: "rsa-1", alg: "RS256", private: rsaKey},
		{kid: "other", alg: "RS256", private: otherKey},
	})
	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "aethertunnel")
	if err != nil {
		t.Fatalf("build the verifier: %v", err)
	}

	sign := func(kid string, key *rsa.PrivateKey, claims map[string]any) string {
		token, err := SignToken("RS256", kid, key, nil, claims)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}

	t.Run("a signature from a foreign key", func(t *testing.T) {
		// The token claims the published kid but was signed by a key the
		// issuer never published.
		token, err := SignToken("RS256", "rsa-1", otherKey, nil, goodClaims("https://issuer.example", "aethertunnel"))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("Verify = %v, want a signature failure", err)
		}
	})
	t.Run("a wrong audience", func(t *testing.T) {
		token := sign("rsa-1", rsaKey, goodClaims("https://issuer.example", "another-app"))
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "audience") {
			t.Fatalf("Verify = %v, want an audience failure", err)
		}
	})
	t.Run("a wrong issuer", func(t *testing.T) {
		token := sign("rsa-1", rsaKey, goodClaims("https://someone-else.example", "aethertunnel"))
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "issuer") {
			t.Fatalf("Verify = %v, want an issuer failure", err)
		}
	})
	t.Run("an expired token", func(t *testing.T) {
		claims := goodClaims("https://issuer.example", "aethertunnel")
		claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
		token := sign("rsa-1", rsaKey, claims)
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("Verify = %v, want an expiry failure", err)
		}
	})
	t.Run("a token that is not valid yet", func(t *testing.T) {
		claims := goodClaims("https://issuer.example", "aethertunnel")
		claims["nbf"] = time.Now().Add(time.Hour).Unix()
		token := sign("rsa-1", rsaKey, claims)
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "not valid yet") {
			t.Fatalf("Verify = %v, want a not-yet failure", err)
		}
	})
	t.Run("an unknown key id", func(t *testing.T) {
		token := sign("nobody", rsaKey, goodClaims("https://issuer.example", "aethertunnel"))
		if err := verifier.Verify(token); err == nil || !strings.Contains(err.Error(), "no key named") {
			t.Fatalf("Verify = %v, want an unknown-kid failure", err)
		}
	})
	t.Run("not a jwt at all", func(t *testing.T) {
		if err := verifier.Verify("static-token"); err == nil {
			t.Fatal("a static token verified as a JWT")
		}
	})
}

func TestVerifySkipsIssuerAndExpiryWhenAsked(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the rsa key: %v", err)
	}
	server, _ := jwksServer(t, []keyPair{{kid: "rsa-1", alg: "RS256", private: rsaKey}})
	verifier, err := NewVerifierWithJWKS(server.URL+"/jwks", "https://issuer.example", "",
		WithSkipIssuer(), WithSkipExpiry())
	if err != nil {
		t.Fatalf("build the verifier: %v", err)
	}
	claims := map[string]any{
		"iss": "https://whoever.example",
		"exp": time.Now().Add(-time.Hour).Unix(),
	}
	token, err := SignToken("RS256", "rsa-1", rsaKey, nil, claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verifier.Verify(token); err != nil {
		t.Fatalf("a skipped-check verifier refused the token: %v", err)
	}
}

func TestDiscoveryFindsTheJWKS(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the rsa key: %v", err)
	}
	server, fetches := jwksServer(t, []keyPair{{kid: "rsa-1", alg: "RS256", private: rsaKey}})
	verifier, err := NewVerifier(context.Background(), server.URL, "aethertunnel")
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	token, err := SignToken("RS256", "rsa-1", rsaKey, nil, goodClaims(server.URL, "aethertunnel"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verifier.Verify(token); err != nil {
		t.Fatalf("the discovered verifier refused the token: %v", err)
	}
	// A second verification reuses the cached keys.
	if err := verifier.Verify(token); err != nil {
		t.Fatalf("the second verification failed: %v", err)
	}
	if *fetches != 1 {
		t.Fatalf("the key set was fetched %d times, want 1", *fetches)
	}
}

func base64url(raw []byte) string {
	return strings.TrimRight(base64RawURL().EncodeToString(raw), "=")
}

func base64RawURL() *base64.Encoding {
	return base64.RawURLEncoding
}

func bigEndian(v uint64) []byte {
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	i := 0
	for i < 7 && out[i] == 0 {
		i++
	}
	return out[i:]
}
