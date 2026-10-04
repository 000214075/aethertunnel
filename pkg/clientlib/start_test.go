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
