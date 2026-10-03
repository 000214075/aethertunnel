package server

import "testing"

func TestSelectByRouteIsolated(t *testing.T) {
	alice := &ProxyGroup{Name: "alice", RouteByHTTPUser: "alice"}
	open := &ProxyGroup{Name: "open"}
	candidates := []*ProxyGroup{alice, open}
	if got := selectByRoute(candidates, "/", "alice"); got != alice {
		t.Fatalf("alice's request got %q", got.Name)
	}
	if got := selectByRoute(candidates, "/", ""); got != open {
		t.Fatalf("an anonymous request got %q", got.Name)
	}
	if got := selectByRoute(candidates, "/", "bob"); got != open {
		t.Fatalf("bob's request got %q", got.Name)
	}
}
