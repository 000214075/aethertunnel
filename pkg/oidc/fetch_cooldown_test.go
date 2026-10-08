package oidc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A rotation that coincides with an issuer hiccup: the first token's lookup
// fails to fetch the key set, and the second token inside the cooldown used to
// be told "the issuer publishes no key named ..." — a claim about a key set
// the fetcher never saw. The cooldown has to name the failed refresh instead,
// or the operator goes looking for a kid that was never wrong.
func TestACooldownAfterAFailedFetchNamesTheFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	v, err := NewVerifierWithJWKS(srv.URL, "https://issuer.example", "audience")
	if err != nil {
		t.Fatalf("NewVerifierWithJWKS: %v", err)
	}

	if _, err := v.signingKey("kid-1"); err == nil {
		t.Fatal("the first lookup succeeded although the issuer answers 500")
	}

	_, err = v.signingKey("kid-2")
	if err == nil {
		t.Fatal("the second lookup succeeded although the issuer answers 500")
	}
	if !strings.Contains(err.Error(), "could not be refreshed") {
		t.Fatalf("the cooldown error does not name the failed refresh: %v", err)
	}
	if strings.Contains(err.Error(), "publishes no key") {
		t.Fatalf("the cooldown error claims the issuer publishes no such key: %v", err)
	}
}
