package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/oidc"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestOIDCAuthentication walks the [oidc] auth path: a verified access token
// replaces the static token, a garbage token with a wrong static token is
// refused, and the static token keeps working — the [oidc] section adds a
// credential, it does not remove one.
func TestOIDCAuthentication(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the issuer key: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer jwks.Close()

	cfg := testConfig(t, false)
	cfg.OIDC = &config.OIDCConfig{
		Issuer:   "https://issuer.example",
		Audience: "aethertunnel",
		JWKSURL:  jwks.URL + "/jwks",
	}
	rs := startServer(t, cfg)

	authenticate := func(token string) protocol.AuthResponse {
		t.Helper()
		client, err := newTestClient(t, rs.addr, false)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.close()
		response, err := client.authenticate("test-agent", token)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return response
	}

	token, err := oidc.SignToken("RS256", "k1", rsaKey, nil, map[string]any{
		"iss": "https://issuer.example",
		"aud": "aethertunnel",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if response := authenticate(token); !response.OK {
		t.Fatalf("a verified oidc token was refused: %s", response.Error)
	}

	if response := authenticate("garbage"); response.OK || !strings.Contains(response.Error, "invalid auth token") {
		t.Fatalf("garbage answered ok=%v error=%q", response.OK, response.Error)
	}

	if response := authenticate(testToken); !response.OK {
		t.Fatalf("the static token stopped working beside [oidc]: %s", response.Error)
	}
}
