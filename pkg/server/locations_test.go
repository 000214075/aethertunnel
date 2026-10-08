package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// headerOrigin serves one marker and reports the Host it saw.
func headerOrigin(t *testing.T, marker string) (string, *strings.Builder) {
	t.Helper()
	seen := &strings.Builder{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.WriteString(r.Host + " " + r.URL.Path)
		fmt.Fprint(w, marker)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), seen
}

// TestLocationsSplitAHostname covers path routing on one hostname: the proxy
// whose location prefix is the longest match takes the request, and the proxy
// without locations catches whatever is left.
func TestLocationsSplitAHostname(t *testing.T) {
	apiAddr, apiSeen := headerOrigin(t, "api-body")
	webAddr, _ := headerOrigin(t, "web-body")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"api": framedStreamHandler(apiAddr),
		"web": framedStreamHandler(webAddr),
	})
	agent.register(protocol.ProxySpec{
		Name: "api", Type: protocol.ProxyTypeHTTP, LocalAddr: apiAddr,
		Domains:   []string{"split.example"},
		Locations: []string{"/api"},
	})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: webAddr,
		Domains: []string{"split.example"},
	})

	if body := getViaHTTP(t, cfg.Server.HTTPPort, "split.example", "/api/users"); !strings.Contains(body, "api-body") {
		t.Fatalf("/api/users reached %q, want the api proxy", body)
	}
	if !strings.Contains(apiSeen.String(), "split.example") {
		t.Fatalf("the api origin saw host %q", apiSeen.String())
	}
	if body := getViaHTTP(t, cfg.Server.HTTPPort, "split.example", "/index.html"); !strings.Contains(body, "web-body") {
		t.Fatalf("/index.html reached %q, want the web proxy", body)
	}
	if body := getViaHTTP(t, cfg.Server.HTTPPort, "split.example", "/apiv2/items"); !strings.Contains(body, "api-body") {
		t.Fatalf("/apiv2/items reached %q, want the api proxy (frp matches by prefix)", body)
	}
}

// TestHostHeaderRewriteReachesTheOrigin covers host_header_rewrite on an http
// proxy: the origin sees the rewritten Host while the visitor keeps addressing
// the published one.
func TestHostHeaderRewriteReachesTheOrigin(t *testing.T) {
	originAddr, seen := headerOrigin(t, "rewrite-body")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(originAddr)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: originAddr,
		Domains:           []string{"original.example"},
		HostHeaderRewrite: "rewritten.example",
	})

	if body := getViaHTTP(t, cfg.Server.HTTPPort, "original.example", "/"); !strings.Contains(body, "rewrite-body") {
		t.Fatalf("the request did not reach the origin: %q", body)
	}
	if !strings.Contains(seen.String(), "rewritten.example") {
		t.Fatalf("the origin saw host %q, want the rewritten one", seen.String())
	}
}

// getViaHTTP fetches one path with a forged Host through the server's shared
// http listener.
func getViaHTTP(t *testing.T, port int, host, path string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", fmt.Sprint(port))+path, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Host = host
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s with host %s: %v", path, host, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return string(body)
}
