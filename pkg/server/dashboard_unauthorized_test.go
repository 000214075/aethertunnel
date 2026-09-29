package server

import (
	"net/http"
	"testing"
)

// TestAnUnauthorizedDashboardRequestIsCounted covers the refusal that left no trace
// anywhere.
//
// Every refusal on the control port is on a counter and in the audit log, but the
// dashboard listener answered a missing or wrong token with a 401 and nothing else:
// auth_failures_total stayed put (its help text says "a wrong token", which is what
// this is), control_rejected_total stayed put, the audit log grew by no line, and the
// server logged nothing at all. Five requests against a running server changed
// nothing an operator could see. This listener has no rate limit of its own and the
// shipped Service publishes it, so a wrong-token client — a scanner that found the
// port, or a scraper with a rotated credential — was invisible.
func TestAnUnauthorizedDashboardRequestIsCounted(t *testing.T) {
	dashboard, base := newTestDashboard(t, nil)
	metrics := dashboard.server.metrics

	before := metrics.dashboardRefused.Load()
	authBefore := metrics.authFailures.Load()
	rejectedBefore := metrics.controlRejected.Load()
	acceptedBefore := metrics.controlAccepted.Load()

	// No token, a wrong token, and the wrong token on every kind of endpoint: the
	// /api ones that go through withAuth and /metrics, which checks its own token.
	for _, tc := range []struct{ path, token string }{
		{"/api/status", ""},
		{"/api/status", "not-the-dashboard-token"},
		{"/api/config", "not-the-dashboard-token"},
		{"/metrics", "not-the-metrics-token"},
		{"/metrics", ""},
	} {
		resp := doDashboardRequest(t, http.MethodGet, base+tc.path, tc.token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with %q = %d, want 401", tc.path, tc.token, resp.StatusCode)
		}
	}
	if got := metrics.dashboardRefused.Load(); got != before+5 {
		t.Errorf("the unauthorized-request counter is %d after five refused requests, want %d",
			got, before+5)
	}

	// A request that does present the token must not move it.
	for _, tc := range []struct{ path, token string }{
		{"/api/status", "dashboard-secret"},
		{"/metrics", "metrics-secret"},
		{"/metrics", "dashboard-secret"}, // the dashboard token is accepted here too
		{"/healthz", ""},                 // public on purpose
	} {
		resp := doDashboardRequest(t, http.MethodGet, base+tc.path, tc.token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s with %q = %d, want 200", tc.path, tc.token, resp.StatusCode)
		}
	}
	if got := metrics.dashboardRefused.Load(); got != before+5 {
		t.Errorf("the unauthorized-request counter is %d after requests that carried the "+
			"token, want it to stay at %d", got, before+5)
	}

	// The control-port series keep their own meaning: an HTTP request is not a
	// control connection, so none of them may move.
	if got := metrics.authFailures.Load(); got != authBefore {
		t.Errorf("auth_failures_total is %d after dashboard requests, want it to stay at %d",
			got, authBefore)
	}
	if got := metrics.controlRejected.Load(); got != rejectedBefore {
		t.Errorf("control_rejected_total is %d after dashboard requests, want it to stay at %d",
			got, rejectedBefore)
	}
	if got := metrics.controlAccepted.Load(); got != acceptedBefore {
		t.Errorf("control_connections_total is %d after dashboard requests, want it to stay at %d",
			got, acceptedBefore)
	}
}
