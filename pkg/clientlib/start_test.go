package clientlib

import (
	"io"
	"log"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// selectByStart keeps the proxies and visitors client.start names, in the
// order the configuration lists them, and drops the rest.
func TestSelectByStartKeepsOnlyTheNamedEntries(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.Start = []string{"web", "dns-visitor"}
	cfg.Proxies = []config.ProxyConfig{
		{Name: "web", Type: config.ProxyTypeTCP},
		{Name: "ssh", Type: config.ProxyTypeTCP},
		{Name: "files", Type: config.ProxyTypeHTTP},
	}
	cfg.Visitors = []config.VisitorConfig{
		{Name: "dns-visitor", Type: "sudp"},
		{Name: "other", Type: "stcp"},
	}

	proxies, visitors := selectByStart(cfg, log.New(io.Discard, "", 0))
	if len(proxies) != 1 || proxies[0].Name != "web" {
		t.Fatalf("the selected proxies are %v, want [web]", names(proxies))
	}
	if len(visitors) != 1 || visitors[0].Name != "dns-visitor" {
		t.Fatalf("the selected visitors are %v, want [dns-visitor]", visitorNames(visitors))
	}
}

// An empty start list keeps every entry, which is what a configuration without
// the key means.
func TestSelectByStartKeepsEverythingWithoutAList(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxies = []config.ProxyConfig{
		{Name: "web", Type: config.ProxyTypeTCP},
		{Name: "ssh", Type: config.ProxyTypeTCP},
	}
	cfg.Visitors = []config.VisitorConfig{{Name: "dns-visitor", Type: "sudp"}}

	proxies, visitors := selectByStart(cfg, log.New(io.Discard, "", 0))
	if len(proxies) != 2 || len(visitors) != 1 {
		t.Fatalf("an empty start list selected %v and %v, want everything", names(proxies), visitorNames(visitors))
	}
}

// A start list naming nothing leaves nothing to run, and the lists come back
// empty rather than nil so registerProxies publishes no tunnel.
func TestSelectByStartCanSelectNothing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.Start = []string{"ghost"}
	cfg.Proxies = []config.ProxyConfig{{Name: "web", Type: config.ProxyTypeTCP}}

	proxies, visitors := selectByStart(cfg, log.New(io.Discard, "", 0))
	if len(proxies) != 0 || len(visitors) != 0 {
		t.Fatalf("selecting only a ghost kept %v and %v, want nothing", names(proxies), visitorNames(visitors))
	}
}

func names(proxies []config.ProxyConfig) []string {
	out := make([]string, 0, len(proxies))
	for _, proxy := range proxies {
		out = append(out, proxy.Name)
	}
	return out
}

func visitorNames(visitors []config.VisitorConfig) []string {
	out := make([]string, 0, len(visitors))
	for _, visitor := range visitors {
		out = append(out, visitor.Name)
	}
	return out
}

// An entry carrying enabled = false is left out of the dispatch lists, alone and
// together with a start list.
func TestSelectByStartLeavesDisabledEntriesStopped(t *testing.T) {
	off := false
	on := true
	cfg := &config.Config{}
	cfg.Proxies = []config.ProxyConfig{
		{Name: "web", Type: config.ProxyTypeTCP},
		{Name: "ssh", Type: config.ProxyTypeTCP, Enabled: &off},
		{Name: "files", Type: config.ProxyTypeHTTP, Enabled: &on},
	}
	cfg.Visitors = []config.VisitorConfig{
		{Name: "dns-visitor", Type: "sudp", Enabled: &off},
	}

	proxies, visitors := selectByStart(cfg, log.New(io.Discard, "", 0))
	if got := names(proxies); len(got) != 2 || got[0] != "web" || got[1] != "files" {
		t.Fatalf("the enabled proxies are %v, want [web files]", got)
	}
	if len(visitors) != 0 {
		t.Fatalf("a disabled visitor is dispatched: %v", visitorNames(visitors))
	}

	// A start list naming an entry that is on still selects it, and naming the
	// disabled one selects nothing.
	cfg.Client.Start = []string{"files"}
	proxies, _ = selectByStart(cfg, log.New(io.Discard, "", 0))
	if got := names(proxies); len(got) != 1 || got[0] != "files" {
		t.Fatalf("the selection is %v, want [files]", got)
	}

	cfg.Client.Start = []string{"ssh"}
	proxies, _ = selectByStart(cfg, log.New(io.Discard, "", 0))
	if len(proxies) != 0 {
		t.Fatalf("a start list naming a disabled proxy selected %v", names(proxies))
	}
}

// The labels a proxy carries travel to the server in its registration frame.
func TestProxySpecCarriesAnnotations(t *testing.T) {
	proxy := config.ProxyConfig{
		Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080,
		Annotations: map[string]string{"env": "prod"},
	}
	spec := proxySpec(proxy, "ada")
	if spec.Annotations["env"] != "prod" {
		t.Fatalf("the registration frame carries %v, want the label", spec.Annotations)
	}
	if spec.User != "ada" {
		t.Fatalf("the registration frame names %q, want the publisher's identity", spec.User)
	}
}
