package server

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestRouteByHTTPUserSplitsAHostname covers the third routing dimension: two
// proxies share one hostname and location, and the basic-auth username the
// visitor presents decides which one serves the request. A route named for a
// user wins over a route that named none, and the named proxy's own
// credentials still apply.
func TestRouteByHTTPUserSplitsAHostname(t *testing.T) {
	aliceAddr, aliceSeen := headerOrigin(t, "alice-body")
	openAddr, _ := headerOrigin(t, "open-body")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"alice": framedStreamHandler(aliceAddr),
		"open":  framedStreamHandler(openAddr),
	})
	agent.register(protocol.ProxySpec{
		Name: "alice", Type: protocol.ProxyTypeHTTP, LocalAddr: aliceAddr,
		Domains:         []string{"users.example"},
		RouteByHTTPUser: "alice",
		HTTPUser:        "alice",
		HTTPPassword:    "alicepass",
	})
	agent.register(protocol.ProxySpec{
		Name: "open", Type: protocol.ProxyTypeHTTP, LocalAddr: openAddr,
		Domains: []string{"users.example"},
	})

	fetchWith := func(authorization string) (string, int) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.Server.HTTPPort))+"/", nil)
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		request.Host = "users.example"
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer answer.Body.Close()
		body := new(strings.Builder)
		buf := make([]byte, 512)
		for {
			n, err := answer.Body.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return body.String(), answer.StatusCode
	}

	// Alice's credentials route to her proxy and pass its own check.
	aliceAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:alicepass"))
	body, status := fetchWith(aliceAuth)
	if status != 200 || !strings.Contains(body, "alice-body") {
		t.Fatalf("alice's request answered %d %q, want her proxy", status, body)
	}
	if !strings.Contains(aliceSeen.String(), "users.example") {
		t.Fatalf("alice's origin saw host %q", aliceSeen.String())
	}

	// A visitor without credentials falls through to the catch-all proxy.
	body, status = fetchWith("")
	if status != 200 || !strings.Contains(body, "open-body") {
		t.Fatalf("the anonymous request answered %d %q, want the catch-all proxy", status, body)
	}

	// Bob's username misses alice's route and lands on the catch-all too.
	body, status = fetchWith("Basic " + base64.StdEncoding.EncodeToString([]byte("bob:whatever")))
	if status != 200 || !strings.Contains(body, "open-body") {
		t.Fatalf("bob's request answered %d %q, want the catch-all proxy", status, body)
	}

	// Alice's username with a wrong password routes to her proxy and is
	// rejected by its credential check — routing picked it, auth refused it.
	body, status = fetchWith("Basic " + base64.StdEncoding.EncodeToString([]byte("alice:wrong")))
	if status != 401 {
		t.Fatalf("alice with a wrong password answered %d %q, want 401 from her proxy", status, body)
	}
}
