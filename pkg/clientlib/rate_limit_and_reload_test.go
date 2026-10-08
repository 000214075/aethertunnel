package clientlib

import (
	"context"
	"io"
	"log"
	"math"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// The burst is one second's worth of the rate. On a 32-bit build a rate past
// 2^31 bytes per second wrapped int(limit) negative, and a limiter whose
// burst is below one grants nothing, so wait let the connection pass
// unthrottled — the cap has to hold on every platform the module builds for.
func TestTheBurstOfARateLimitNeverWrapsNegative(t *testing.T) {
	got := burstFor(3_000_000_000)
	if got <= 0 {
		t.Fatalf("a 3 GB/s rate produced a burst of %d, want a positive burst", got)
	}
	if got > math.MaxInt32 {
		t.Fatalf("the burst %d does not fit the 32-bit int the limiter is built with", got)
	}

	conn := newLimitedConn(3_000_000_000, nil)
	if conn == nil {
		t.Fatal("newLimitedConn refused a valid rate")
	}
	limited := conn.(*limitedConn)
	if burst := limited.limiter.Burst(); burst != math.MaxInt32 {
		t.Fatalf("the limiter's burst is %d, want the %d cap", burst, math.MaxInt32)
	}
}

// reWithdrawUnhealthy used to read the health state and write the withdrawal
// outside the reload lock, so a reload that replaced a proxy mid-pass could
// have its fresh registration withdrawn by the pass. It takes the reload
// lock for the whole pass now, which shows up as blocking while it is held.
func TestReWithdrawUnhealthyWaitsForTheReloadLock(t *testing.T) {
	c := &client{logger: log.New(io.Discard, "", 0)}

	c.reloadMu.Lock()
	finished := make(chan struct{})
	go func() {
		c.reWithdrawUnhealthy()
		close(finished)
	}()

	select {
	case <-finished:
		t.Fatal("the re-withdraw pass ran while the reload lock was held elsewhere")
	case <-time.After(200 * time.Millisecond):
	}

	c.reloadMu.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-withdraw pass did not run after the lock was released")
	}
}

// The DHT startup wait used to sleep through the caller's cancellation: a
// name that resolves within the grace kept Run alive for the full 30 seconds
// after a stop had already been asked for. A cancelled context now ends the
// wait at once, with the same clean (nil) result every other cancelled run
// produces.
func TestACancelledContextEndsTheDhtStartupWait(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.AuthToken = "dht-cancel-token-0123456789ab"
	cfg.DHT.Enabled = true
	cfg.DHT.Discover = "nothing-announces-this-name"
	cfg.DHT.ListenAddr = "127.0.0.1:0"
	cfg.ApplyClientDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	// The DHT node binds and the first lookups fail fast; the wait is in its
	// retry loop well inside a second.
	time.Sleep(time.Second)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run reported %v after the cancellation, want the clean nil a stopped client returns", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run was still in the DHT startup wait 10s after the cancellation")
	}
}
