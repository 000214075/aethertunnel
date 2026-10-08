package clientlib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A stale probe's withdrawal used to go out unchecked: a reload that replaced
// the proxy while the old probe was between its own currency check and the
// frame write — or inside the server round trip — withdrew the replacement,
// and nothing published the name again until the next reconnect. The
// withdrawal now re-checks the probe under reloadMu, the same lock the reload
// takes.
func TestAStaleProbeDoesNotWithdrawAProxyAReloadJustPublished(t *testing.T) {
	var logs logSink
	c := &client{logger: log.New(&logs, "", 0)}

	ctxOld, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	ctxNew, cancelNew := context.WithCancel(context.Background())
	defer cancelNew()

	c.healthStopsMu.Lock()
	c.healthStops = map[string]healthProbe{"web": {ctx: ctxNew, cancel: cancelNew}}
	c.healthStopsMu.Unlock()

	state := &healthState{maxFail: 3}
	proxy := config.ProxyConfig{Name: "web"}

	// The registry already holds the replacement's probe, so the old one is
	// out of the game no matter when its result was produced.
	c.withdrawUnhealthy(ctxOld, proxy, state)
	if strings.Contains(logs.String(), "failed its health check") {
		t.Fatalf("a stale probe started a withdrawal; the log reads:\n%s", logs.String())
	}

	// A cancelled probe is equally out of the game even while it is still the
	// registered one.
	cancelOld()
	c.withdrawUnhealthy(ctxOld, proxy, state)
	if strings.Contains(logs.String(), "failed its health check") {
		t.Fatalf("a cancelled probe started a withdrawal; the log reads:\n%s", logs.String())
	}

	// And a current, live probe still withdraws — here it can only report,
	// because the test client holds no control connection.
	c.healthStopsMu.Lock()
	c.healthStops["web"] = healthProbe{ctx: ctxNew, cancel: cancelNew}
	c.healthStopsMu.Unlock()
	c.withdrawUnhealthy(ctxNew, proxy, state)
	if !strings.Contains(logs.String(), "failed its health check") {
		t.Fatal("a current probe was prevented from withdrawing")
	}
}

// The SIGHUP that schedules a reload is snapshotted before a failing Run tears
// the client's services down, so the reload goroutine can outlive the client
// it was handed: applying it bound visitor listeners and started health probes
// under the caller's context, which outlives Run. A reload for a stopped
// client must apply nothing.
func TestAReloadForAStoppedClientDoesNotStartServices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.toml")
	cfgText := `
[client]
server_addr = "127.0.0.1:1"
auth_token = "reload-flag-token-0123456"

[[visitors]]
name = "database"
type = "stcp"
server_name = "database"
bind_addr = "127.0.0.1"
bind_port = 18432
secret_key = "reload-test-second-secret"
`
	if err := os.WriteFile(path, []byte(cfgText), 0o644); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	base := &config.Config{}
	base.Client.AuthToken = "reload-flag-token-0123456"
	base.ApplyClientDefaults()

	c := &client{cfg: base, logger: log.New(io.Discard, "", 0)}
	c.baseCtx = context.Background()
	c.serviceCtx, c.serviceCancel = context.WithCancel(context.Background())
	c.stopStartupServices()

	if err := c.reloadFromFile(path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	c.visitorsMu.Lock()
	started := c.visitorsCancel != nil
	c.visitorsMu.Unlock()
	if started {
		t.Fatal("a reload for a stopped client started the visitor listeners")
	}
}

// A server that accepts the connection and then says nothing used to be
// reported as "closed it without answering" — a claim about a close the
// server never made, which sent the operator looking for a refusal the
// server's log does not hold. A dial_timeout expiry says what happened.
func TestASilentServerIsReportedAsUnansweredNotClosed(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer silent.Close()
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever reading or writing.
			go func() { time.Sleep(10 * time.Second); _ = conn.Close() }()
		}
	}()

	cfg := &config.Config{}
	cfg.Client.ServerAddr = silent.Addr().String()
	cfg.Client.AuthToken = "silent-server-token-01234"
	cfg.Client.DialTimeoutSecs = 1
	cfg.ApplyClientDefaults()

	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: cfg.Client.ServerAddr}
	c.baseCtx = context.Background()

	err = c.runSession(context.Background())
	if err == nil {
		t.Fatal("runSession succeeded although the server never answers")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) && !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("the error is not the authentication deadline: %v", err)
	}
	if strings.Contains(err.Error(), "closed it without answering") {
		t.Fatalf("the timeout is still described as a server close: %v", err)
	}
	if !strings.Contains(err.Error(), "did not answer the authentication request") {
		t.Fatalf("the timeout does not say the server did not answer: %v", err)
	}
}

// An admin-API entry write that was in flight when Run failed can still be
// holding the admin server's Shutdown window: applying it started health
// probes and bound visitor listeners under the caller's context, which
// outlives Run. A dispatch for a stopped client must start nothing — the
// same rule a SIGHUP reload that outlived the client is held to.
func TestAStoreDispatchForAStoppedClientStartsNothing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:1"
	cfg.Client.AuthToken = "dispatch-stopped-token-01234"
	cfg.Visitors = []config.VisitorConfig{{
		Name:       "database",
		Type:       "stcp",
		ServerName: "database",
		BindAddr:   "127.0.0.1",
		BindPort:   freeVisitorPort(t),
		SecretKey:  "reload-test-second-secret",
	}}
	cfg.ApplyClientDefaults()

	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	c.baseCtx = context.Background()
	c.serviceCtx, c.serviceCancel = context.WithCancel(context.Background())
	c.setFileLists(nil, cfg.Visitors)
	c.stopStartupServices()

	c.refreshDispatch()

	c.visitorsMu.Lock()
	started := c.visitorsCancel != nil
	c.visitorsMu.Unlock()
	if started {
		t.Fatal("a dispatch for a stopped client started the visitor listeners")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Visitors[0].BindPort)
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("the visitor listener is bound on %s although the client has stopped", addr)
	}
}

// freeVisitorPort hands out a port that is (almost certainly) not in use and
// marks it so a second test would get a different one.
func freeVisitorPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// The server routes datagrams to the session's fresh address as soon as the
// auth response is written, so packets arrive before the device is up — at
// packet rate. One log line per packet would bury the log, so the branch
// shares the buffer-full case's once-per-interval throttle.
func TestAVpnPacketStormWithoutATunnelDoesNotFloodTheLog(t *testing.T) {
	var logs logSink
	c := &client{logger: log.New(&logs, "", 0)}

	for i := 0; i < 200; i++ {
		c.deliverVPN([]byte("packet"))
	}
	if got := strings.Count(logs.String(), "no tunnel address"); got != 1 {
		t.Fatalf("200 packets without a tunnel device wrote %d log lines, want 1:\n%s",
			got, logs.String())
	}
}
