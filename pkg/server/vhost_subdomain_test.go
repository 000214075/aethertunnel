package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestASubdomainHostPublishesAProxyWithoutDomains covers the routing mode in which
// the proxy name is the hostname: a proxy that declares no domains is published as
// <name>.<server.subdomain_host>.
//
// It is the only way to serve an http proxy whose hostname the client does not know,
// and it used to be unreachable: the client's configuration validation refused a
// domain-less proxy before the server could decide whether it could route it.
func TestASubdomainHostPublishesAProxyWithoutDomains(t *testing.T) {
	service := startHTTPService(t, "subdomain-service")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.SubdomainHost = "tunnel.example"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
	})

	base := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Server.HTTPPort)

	cases := []struct {
		host string
		want int
		body string
	}{
		{"web.tunnel.example", http.StatusOK, "subdomain-service|host=web.tunnel.example"},
		{"WEB.TUNNEL.EXAMPLE", http.StatusOK, "subdomain-service|host=WEB.TUNNEL.EXAMPLE"},
		{"other.tunnel.example", http.StatusNotFound, ""},
		{"web.other.example", http.StatusNotFound, ""},
		{"tunnel.example", http.StatusNotFound, ""},
	}

	for _, tc := range cases {
		request, err := http.NewRequest(http.MethodGet, base, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Host = tc.host

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()

		if response.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d (%s)", tc.host, response.StatusCode, tc.want, body)
		}
		if tc.body != "" && string(body) != tc.body {
			t.Fatalf("%s: body %q, want %q", tc.host, body, tc.body)
		}
	}
}

// TestAProxyWithoutDomainsIsRefusedWithoutASubdomainHost documents what happens when
// the server cannot work out a hostname for a proxy: it refuses the registration and
// says why, instead of publishing an endpoint that nothing can reach. This is the
// answer a client gets when it publishes a domain-less proxy to a server that has
// server.subdomain_host unset.
func TestAProxyWithoutDomainsIsRefusedWithoutASubdomainHost(t *testing.T) {
	service := startHTTPService(t, "no-subdomain")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	refusal := agent.expectRefused("web")
	if !strings.Contains(refusal, "subdomain_host") {
		t.Fatalf("the refusal does not name the setting that is missing: %s", refusal)
	}
}
