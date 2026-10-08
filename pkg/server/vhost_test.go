package server

import (
	"io"
	"log"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A withdrawn hostname has to leave the router entirely. The lookup treats a present
// key as a match and stops there, so an entry emptied in place used to answer for the
// withdrawn proxy forever — instead of falling through to a wildcard or the subdomain
// convention — and the maps kept one key per hostname every client ever registered.
func TestAWithdrawnDomainLeavesNoEntryBehind(t *testing.T) {
	router := newVhostRouter("http", &config.Config{}, log.New(io.Discard, "", 0), newMetrics())

	exact := &ProxyGroup{Name: "exact", Domains: []string{"site.example.com"}}
	binding, err := router.add(exact)
	if err != nil {
		t.Fatalf("add the exact hostname: %v", err)
	}
	if got := router.lookup("site.example.com"); len(got) != 1 || got[0] != exact {
		t.Fatalf("the published hostname resolved to %v", got)
	}

	binding.remove()
	if _, present := router.exact["site.example.com"]; present {
		t.Errorf("the emptied exact entry stayed in the map: %v", router.exact)
	}

	// A wildcard that covers the withdrawn name answers it, which the stale entry
	// used to prevent.
	wildcard := &ProxyGroup{Name: "wildcard", Domains: []string{"*.example.com"}}
	wildcardBinding, err := router.add(wildcard)
	if err != nil {
		t.Fatalf("add the wildcard: %v", err)
	}
	defer wildcardBinding.remove()
	if got := router.lookup("site.example.com"); len(got) != 1 || got[0] != wildcard {
		t.Fatalf("the withdrawn name resolved to %v, want the wildcard proxy", got)
	}

	// The same holds for wildcards: a withdrawn longer suffix must not shadow the
	// shorter one that really covers the name.
	deeper := &ProxyGroup{Name: "deeper", Domains: []string{"*.a.example.com"}}
	deeperBinding, err := router.add(deeper)
	if err != nil {
		t.Fatalf("add the longer wildcard: %v", err)
	}
	deeperBinding.remove()
	if _, present := router.wildcard["a.example.com"]; present {
		t.Errorf("the emptied wildcard entry stayed in the map: %v", router.wildcard)
	}
	if got := router.lookup("x.a.example.com"); len(got) != 1 || got[0] != wildcard {
		t.Fatalf("the withdrawn wildcard resolved to %v, want the shorter wildcard", got)
	}
}

// The subdomain convention lowercases the Host header before it strips the suffix
// and looks the name up, so a proxy name with a capital letter has to be stored
// lowercased or it is published and then unreachable.
func TestASubdomainConventionNameIsReachableWhateverItsCase(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.SubdomainHost = "example.com"
	router := newVhostRouter("http", cfg, log.New(io.Discard, "", 0), newMetrics())

	group := &ProxyGroup{Name: "Web"}
	binding, err := router.add(group)
	if err != nil {
		t.Fatalf("add the named proxy: %v", err)
	}
	if got := router.lookup("web.example.com"); len(got) != 1 || got[0] != group {
		t.Fatalf("the convention hostname resolved to %v, want the proxy", got)
	}

	binding.remove()
	if _, present := router.exact["web"]; present {
		t.Errorf("the emptied convention entry stayed in the map: %v", router.exact)
	}
}
