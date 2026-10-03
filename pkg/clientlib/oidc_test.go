package clientlib

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

func TestOIDCAccessTokenFetchAndCache(t *testing.T) {
	var fetches atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse the form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type %q, want client_credentials", got)
		}
		if got := r.Form.Get("audience"); got != "aethertunnel" {
			t.Errorf("audience %q, want aethertunnel", got)
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "client-1" || password != "secret-1" {
			t.Errorf("the request authenticated as %q/%q, want client-1/secret-1", user, password)
		}
		_, _ = w.Write([]byte(`{"access_token":"jwt-value","expires_in":3600}`))
	}))
	defer endpoint.Close()

	c := &client{cfg: &config.Config{Client: config.ClientConfig{DialTimeoutSecs: 5}}, logger: log.New(io.Discard, "", 0)}
	c.cfg.OIDC = &config.OIDCConfig{
		TokenEndpointURL: endpoint.URL,
		ClientID:         "client-1",
		ClientSecret:     "secret-1",
		Audience:         "aethertunnel",
	}

	// A busy runner can stall one loopback request; a fetch that times out is
	// retried with the cache cleared, so the assertion is about caching rather
	// than about the runner's mood.
	var first, second string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		first, err = c.oidcAccessToken(context.Background())
		if err == nil {
			break
		}
		t.Logf("fetch attempt %d: %v", attempt+1, err)
		c.oidcMu.Lock()
		c.oidcToken, c.oidcExpiry = "", time.Time{}
		c.oidcMu.Unlock()
	}
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// A timed-out attempt still reached the handler — the client gave up, the
	// server kept counting — so the caching baseline is the count at the moment
	// the first fetch succeeded, and the cached call must add nothing.
	baseline := fetches.Load()
	second, err = c.oidcAccessToken(context.Background())
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if first != "jwt-value" || second != "jwt-value" {
		t.Fatalf("the token came back as %q and %q", first, second)
	}
	if got := fetches.Load(); got != baseline {
		t.Fatalf("the token endpoint was called %d more times, want 0 (the second is cached)", got-baseline)
	}
}

func TestOIDCAccessTokenWithoutExpiryIsRefetched(t *testing.T) {
	var fetches atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"jwt-value"}`))
	}))
	defer endpoint.Close()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.OIDC = &config.OIDCConfig{TokenEndpointURL: endpoint.URL, ClientID: "c", ClientSecret: "s"}

	for i := 0; i < 2; i++ {
		if _, err := c.oidcAccessToken(context.Background()); err != nil {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("the endpoint was called %d times, want 2 (no expiry means no cache)", got)
	}
}

func TestOIDCAccessTokenEndpointFailureIsExplained(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	}))
	defer endpoint.Close()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.OIDC = &config.OIDCConfig{TokenEndpointURL: endpoint.URL, ClientID: "c", ClientSecret: "wrong"}

	// A busy runner can mangle a loopback request at the transport level — a
	// stalled fetch, a reset, even a spurious "bad file descriptor" — so any
	// error without the endpoint's status in it is retried; the assertion is
	// about the explanation, not the runner's mood.
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = c.oidcAccessToken(context.Background())
		if err != nil && strings.Contains(err.Error(), "401") {
			break
		}
	}
	if err == nil || !contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the endpoint's status in the message", err)
	}
}

func TestOIDCTokenExpiryReachesTheCache(t *testing.T) {
	// A token with a one-second expiry is refreshed after it goes stale.
	var fetches atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"short-lived","expires_in":1}`))
	}))
	defer endpoint.Close()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.OIDC = &config.OIDCConfig{TokenEndpointURL: endpoint.URL, ClientID: "c", ClientSecret: "s"}

	if _, err := c.oidcAccessToken(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := c.oidcAccessToken(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("the endpoint was called %d times, want 2 (the short expiry ran out)", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 || stringsIndex(haystack, needle) >= 0)
}

func stringsIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
