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
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// --- stream accounting ---------------------------------------------------------
//
// Every number the dashboard shows for "active conns" and for a client's streams
// comes from these counters, so a stream that ends has to give its count back. The
// release used to be missing on the two paths that serve a visitor, which left
// aethertunnel_streams_active and the per-proxy active count climbing for the life
// of the process.

// streamCounts reads what the dashboard would show: per proxy, per session and in
// the aggregate.
func streamCounts(t *testing.T, rs *runningServer, proxy string) (group int64, sessions int64, totals int64) {
	t.Helper()

	group0, err := rs.server.tunnels.Get(proxy)
	if err != nil {
		t.Fatalf("look up %q: %v", proxy, err)
	}
	active, _, _, _ := group0.Totals()
	for _, session := range rs.server.sessions.List() {
		sessions += session.ActiveStreams()
	}
	return active, sessions, rs.server.metrics.streamsActive.Load()
}

// echoThroughProxy sends one payload through the published port and fails the test
// when the answer does not come back.
func echoThroughProxy(t *testing.T, port int, payload string) {
	t.Helper()

	answer := echoOnce(fmt.Sprintf("127.0.0.1:%d", port), payload)
	if answer != payload {
		t.Fatalf("echo returned %q, want %q", answer, payload)
	}
}

// waitForNoActiveStreams waits for the proxy, the sessions and the metric to agree
// that nothing is running. The visitor closing its connection and the server
// finishing the copy are two different moments, so an assertion taken immediately
// after a request would still see the stream it just ended.
func waitForNoActiveStreams(t *testing.T, rs *runningServer, proxy string) (int64, int64, int64) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var group, sessions, totals int64
	for {
		group, sessions, totals = streamCounts(t, rs, proxy)
		if group == 0 && sessions == 0 && totals == 0 {
			return group, sessions, totals
		}
		if time.Now().After(deadline) {
			return group, sessions, totals
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStreamCountersReturnToZeroWhenAStreamIsServed(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	})
	waitForListener(t, rs.server, "echo")

	for round := 0; round < 3; round++ {
		echoThroughProxy(t, publicPort, fmt.Sprintf("round-%d", round))
	}

	// The streams have all ended, so nothing may still be counted as running.
	group, sessions, totals := waitForNoActiveStreams(t, rs, "echo")
	if group != 0 || sessions != 0 || totals != 0 {
		t.Errorf("after three finished streams: proxy=%d session(s)=%d metric=%d, want 0 everywhere",
			group, sessions, totals)
	}

	// The completed count is a counter, so it has to have moved by three.
	proxy, err := rs.server.tunnels.Get("echo")
	if err != nil {
		t.Fatalf("look up echo: %v", err)
	}
	var completed int64
	for _, member := range proxy.Members() {
		completed += member.Total.Load()
	}
	if completed != 3 {
		t.Errorf("the members report %d completed streams, want 3", completed)
	}

	var perSession int64
	for _, session := range rs.server.sessions.List() {
		perSession += session.totalStreams.Load()
	}
	if perSession != 3 {
		t.Errorf("the sessions report %d completed streams, want 3", perSession)
	}
}

func TestStreamCountersReturnToZeroForASocks5Stream(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	policy, err := socks.NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	agent.setDialTarget(func(target string) (net.Conn, error) { return policy.Dial(target, 5*time.Second) })
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	for round := 0; round < 2; round++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if code := socksRequest(t, conn, echo); code != socks.ReplySucceeded {
			t.Fatalf("round %d: reply code %d, want %d", round, code, socks.ReplySucceeded)
		}
		conn.Close()
	}

	group, sessions, totals := waitForNoActiveStreams(t, rs, "exit")
	if group != 0 || sessions != 0 || totals != 0 {
		t.Errorf("after two finished socks5 streams: proxy=%d session(s)=%d metric=%d, want 0 everywhere",
			group, sessions, totals)
	}
	if got := rs.server.metrics.socksRequests.Load(); got != 2 {
		t.Errorf("the socks5 counter is %d, want 2", got)
	}
}

// A stream that is open has to be visible, otherwise the "active" number would be
// meaningless even once it returns to zero.
func TestAnOpenStreamIsCountedWhileItRuns(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	})
	waitForListener(t, rs.server, "echo")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("held")); err != nil {
		t.Fatalf("write: %v", err)
	}
	answer := make([]byte, len("held"))
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read: %v", err)
	}

	group, sessions, totals := streamCounts(t, rs, "echo")
	if group != 1 || sessions != 1 || totals != 1 {
		t.Errorf("while one stream is open: proxy=%d session(s)=%d metric=%d, want 1 everywhere",
			group, sessions, totals)
	}

	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		group, sessions, totals = streamCounts(t, rs, "echo")
		if group == 0 && sessions == 0 && totals == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the counters did not return to zero after the stream ended: proxy=%d session(s)=%d metric=%d",
				group, sessions, totals)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The http path releases its stream through the reverse proxy's transport, which is
// a different route, so it needs its own check.
func TestStreamCountersReturnToZeroForAnHTTPRequest(t *testing.T) {
	service := startHTTPService(t, "counted")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"counted.example.com"},
	})
	waitForListener(t, rs.server, "web")

	body, err := httpRequestThroughProxy(cfg.Server.HTTPPort, "counted.example.com")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !strings.Contains(body, "counted") {
		t.Fatalf("the body reads %q", body)
	}

	group, sessions, totals := waitForNoActiveStreams(t, rs, "web")
	if group != 0 || sessions != 0 || totals != 0 {
		t.Errorf("after a finished http request: proxy=%d session(s)=%d metric=%d, want 0 everywhere",
			group, sessions, totals)
	}
}

// httpRequestThroughProxy sends one request through the shared http listener,
// selecting the tunnel by Host header.
func httpRequestThroughProxy(port int, host string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return "", err
	}
	request.Host = host

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("status %d", response.StatusCode)
	}
	return string(body), nil
}
