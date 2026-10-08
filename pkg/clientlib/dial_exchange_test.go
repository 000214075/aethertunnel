package clientlib

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// registerTestProbe stands in for a running health check: it registers ctx as the
// probe for name, which is what a re-registration requires before it writes.
func registerTestProbe(c *client, name string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	c.healthStopsMu.Lock()
	if c.healthStops == nil {
		c.healthStops = map[string]healthProbe{}
	}
	c.healthStops[name] = healthProbe{ctx: ctx, cancel: cancel}
	c.healthStopsMu.Unlock()
	return ctx
}

// A dial through an intermediary stops at the caller's deadline. The visitor
// fallback window and a shutdown both ride on the caller's context, and the dial
// used to build its own from context.Background(), so a proxy that accepted the
// connection and then said nothing held the caller for the whole dial timeout.
func TestADialViaExchangeStopsAtTheCallersDeadline(t *testing.T) {
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
			// Accepted and never answered: the CONNECT exchange never finishes.
			_ = conn
		}
	}()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.Client.DialTimeoutSecs = 8
	c.cfg.Client.DialVia = "socks5h://" + silent.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.dialRaw(ctx, &net.Dialer{Timeout: 8 * time.Second}, "server.example:7000"); err == nil {
		t.Fatal("the dial succeeded against an intermediary that never answered")
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the dial took %s, want it to stop at the caller's 300ms deadline", took)
	}
}

// A re-registration is not written once the probe that asked for it has been
// stopped: a reload cancels the probe and withdraws the proxy, and a
// registration landing after that withdrawal leaves the endpoint published for a
// name the dispatch list no longer carries.
func TestAReRegistrationIsRefusedOnceTheProbeIsStopped(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080, RemotePort: 7000}
	probeCtx := registerTestProbe(c, proxy.Name)

	// What a reload does before it withdraws the proxy.
	c.stopHealthCheck(proxy.Name)

	// The reader starts first: net.Pipe writes block until someone reads, so a
	// write that does happen would otherwise hang this test instead of failing it.
	read := make(chan error, 1)
	go func() {
		_, err := peer.ReadFrame()
		read <- err
	}()
	if err := c.registerProxyAgain(probeCtx, proxy); err != nil {
		t.Fatalf("registerProxyAgain: %v", err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a registration was written for a probe that was already stopped")
		}
	case <-time.After(300 * time.Millisecond):
		// Nothing arrived, which is the point: the write was skipped.
	}
}

// Nothing in the stop path may touch the context the embedder owns: an embedder
// keeps its context alive across Run calls, and cancelling it here would end
// everything the application has tied to it.
func TestStoppingTheServicesLeavesTheCallersContextAlone(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.baseCtx = base
	c.serviceCtx, c.serviceCancel = context.WithCancel(base)

	c.stopStartupServices()

	if c.serviceCtx.Err() == nil {
		t.Fatal("stopStartupServices left the service context alive, so its goroutines keep running")
	}
	if base.Err() != nil {
		t.Fatal("stopStartupServices cancelled the caller's context")
	}
}
