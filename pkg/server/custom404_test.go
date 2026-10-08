package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestCustom404PageAnswersUnknownHosts covers the vhost miss path: a request
// for a hostname nobody published is answered with the configured page, one
// for a published host still reaches the tunnel, and an unreadable page falls
// back to the built-in answer.
func TestCustom404PageAnswersUnknownHosts(t *testing.T) {
	service := startHTTPService(t, "page-origin")
	page := filepath.Join(t.TempDir(), "404.html")
	if err := os.WriteFile(page, []byte("<h1>nothing here</h1>"), 0o600); err != nil {
		t.Fatalf("write the page: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.Custom404Page = page
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"known.example"},
	})

	request := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.Server.HTTPPort))+"/", nil)
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		req.Host = host
		answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET with host %s: %v", host, err)
		}
		return answer
	}

	// A known hostname reaches the tunnel.
	known := request("known.example")
	body, _ := io.ReadAll(known.Body)
	known.Body.Close()
	if known.StatusCode != http.StatusOK || !strings.Contains(string(body), "page-origin") {
		t.Fatalf("the known host answered %d %q", known.StatusCode, body)
	}

	// An unknown hostname is answered with the custom page and a 404.
	unknown := request("unknown.example")
	body, _ = io.ReadAll(unknown.Body)
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "nothing here") {
		t.Fatalf("the unknown host answered %d %q, want 404 with the custom page", unknown.StatusCode, body)
	}

	// A page that cannot be read falls back to the built-in answer.
	if err := os.Remove(page); err != nil {
		t.Fatalf("remove the page: %v", err)
	}
	fallback := request("unknown.example")
	body, _ = io.ReadAll(fallback.Body)
	fallback.Body.Close()
	if fallback.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no tunnel is registered") {
		t.Fatalf("the fallback answered %d %q, want the built-in 404", fallback.StatusCode, body)
	}
}
