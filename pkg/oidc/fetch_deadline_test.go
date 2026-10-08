package oidc

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Fetching the key set gives up on its own deadline. The two constructors here
// set a client timeout, but WithClient can hand a verifier one that never times
// out, and the GET then ran on context.Background(): every caller waiting on the
// fetch gate waited as long as the issuer kept the connection open without
// answering, which an unauthenticated client's made-up kid is enough to make it
// do.
func TestFetchingTheKeySetGivesUpOnItsOwnDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// LIFO: the handler has to be released before Close waits for it.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	restore := keyFetchTimeout
	keyFetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { keyFetchTimeout = restore })

	verifier, err := NewVerifierWithJWKS(server.URL, "https://issuer.example", "aud", WithClient(server.Client()))
	if err != nil {
		t.Fatalf("NewVerifierWithJWKS: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := verifier.fetchKeys()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fetchKeys reported success on an issuer that never answered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fetchKeys waited with no deadline of its own")
	}
}
