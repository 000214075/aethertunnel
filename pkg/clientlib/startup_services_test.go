package clientlib

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// stopStartupServices and the apply paths both start long-lived services, and
// every apply path holds reloadMu for its whole run. Stopping outside that lock
// let an apply install a visitor listener — or a health probe — after the stop
// had gone past it, and that listener then outlived Run, so a login_fail_exit
// retry could not rebind the port. This holds reloadMu the way an apply does and
// checks that the stop waits for it and then cancels what the apply installed.
func TestStoppingStartupServicesWaitsForAnApplyAndCancelsWhatItInstalled(t *testing.T) {
	c, _ := testClient(t, 0)

	c.reloadMu.Lock()
	stopped := make(chan struct{})
	go func() {
		c.stopStartupServices()
		close(stopped)
	}()

	select {
	case <-stopped:
		c.reloadMu.Unlock()
		t.Fatal("stopStartupServices ran while a configuration apply held reloadMu: a listener the " +
			"apply installs afterwards outlives Run and keeps its port bound")
	case <-time.After(200 * time.Millisecond):
	}

	// The apply finishes: it installs the visitor context it started listeners
	// under, then releases the lock.
	ctx, cancel := context.WithCancel(context.Background())
	c.visitorsMu.Lock()
	c.visitorsCtx, c.visitorsCancel = ctx, cancel
	c.visitorsMu.Unlock()
	c.reloadMu.Unlock()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopStartupServices did not finish after the apply released reloadMu")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the visitor context a concurrent apply installed outlived the stop: its listener " +
			"keeps the port bound and the next Run cannot rebind it")
	}
}

// expires_in is multiplied by time.Second into a time.Duration of nanoseconds,
// so an endpoint reporting an absurd one used to overflow the multiplication and
// land the expiry in the wrong century: the token was then cached for the life of
// the process, and every connection after its real expiry failed with "invalid
// auth token" until a restart, with nothing in the log saying why. Past the cap
// the answer is treated like one that states no expiry.
func TestAnOIDCExpiresInPastTheCapIsNotCached(t *testing.T) {
	var fetches atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"jwt-value","expires_in":300000000000}`))
	}))
	defer endpoint.Close()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.OIDC = &config.OIDCConfig{TokenEndpointURL: endpoint.URL, ClientID: "c", ClientSecret: "s"}

	var baseline int64
	for i := 0; i < 2; i++ {
		if _, err := fetchTokenRetryingStalls(c); err != nil {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
		if i == 0 {
			baseline = fetches.Load()
		}
	}
	if got := fetches.Load() - baseline; got < 1 {
		t.Fatalf("the endpoint was called %d more times, want at least 1 (an unusable expires_in means no cache)", got)
	}
	if !c.oidcExpiry.IsZero() {
		t.Fatalf("a token the endpoint dated %s ahead was cached anyway", time.Until(c.oidcExpiry))
	}
}
