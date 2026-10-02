package clientlib

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestApplyProxyReloadConvergesTheServer walks one reload across the control
// connection: a removed proxy is withdrawn, an added one is registered, and the
// dispatch list serves the new set at once.
func TestApplyProxyReloadConvergesTheServer(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	alpha := config.ProxyConfig{Name: "alpha", Type: config.ProxyTypeTCP, LocalPort: 8001}
	beta := config.ProxyConfig{Name: "beta", Type: config.ProxyTypeTCP, LocalPort: 8002}
	gamma := config.ProxyConfig{Name: "gamma", Type: config.ProxyTypeTCP, LocalPort: 8003}
	c.setProxyList([]config.ProxyConfig{alpha, beta})

	exchanged := make(chan error, 1)
	go func() {
		// The server side: a withdrawal for alpha, then a registration for
		// gamma. beta stays silent — it was not part of the diff.
		var withdraw protocol.ProxyWithdraw
		if err := peer.ReadJSON(protocol.TypeProxyWithdraw, &withdraw); err != nil {
			exchanged <- err
			return
		}
		if withdraw.Name != "alpha" {
			exchanged <- errors.New("the withdrawal names " + withdraw.Name)
			return
		}
		if err := peer.WriteJSON(protocol.TypeProxyWithdrawAck, protocol.ProxyWithdrawAck{Name: "alpha", OK: true}); err != nil {
			exchanged <- err
			return
		}
		var spec protocol.ProxySpec
		exchanged <- peer.ReadJSON(protocol.TypeRegisterProxy, &spec)
	}()

	newCfg := &config.Config{Proxies: []config.ProxyConfig{beta, gamma}}
	c.applyReload(newCfg)

	if _, err := c.findProxy("alpha"); err == nil {
		t.Fatal("alpha is still dispatchable after the reload removed it")
	}
	if _, err := c.findProxy("gamma"); err != nil {
		t.Fatalf("gamma is not dispatchable: %v", err)
	}
	if _, err := c.findProxy("beta"); err != nil {
		t.Fatalf("beta lost its dispatch entry though the reload kept it: %v", err)
	}
	select {
	case err := <-exchanged:
		if err != nil {
			t.Fatalf("the reload exchange failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reload exchange did not finish")
	}
}

// TestReloadWatcherRereadsTheFileOnSIGHUP is the hot-reload path end to end,
// signal and all: the client re-reads its file, and the new proxy is on the
// dispatch list — and on the wire — without the process restarting.
func TestReloadWatcherRereadsTheFileOnSIGHUP(t *testing.T) {
	if os.Getpid() <= 1 {
		t.Skip("no pid to signal")
	}
	c, peer, _ := newWithdrawHarness(t)
	c.baseCtx = testContext(t)

	file := filepath.Join(t.TempDir(), "client.toml")
	first := "[client]\nserver_addr = \"127.0.0.1:1\"\nauth_token = \"token\"\n"
	if err := os.WriteFile(file, []byte(first), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	c.cfg.SourceFile = file
	c.startReloadWatcher()

	updated := first + "\n[[proxies]]\nname = \"late\"\ntype = \"tcp\"\nlocal_port = 9001\n"
	if err := os.WriteFile(file, []byte(updated), 0o600); err != nil {
		t.Fatalf("rewrite the config: %v", err)
	}

	registered := make(chan protocol.ProxySpec, 1)
	go func() {
		var spec protocol.ProxySpec
		if err := peer.ReadJSON(protocol.TypeRegisterProxy, &spec); err != nil {
			return
		}
		registered <- spec
	}()

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("raise SIGHUP: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.findProxy("late"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reload never applied the new proxy")
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case spec := <-registered:
		if spec.Name != "late" {
			t.Fatalf("the server was told about %q, want late", spec.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new proxy never reached the wire")
	}
}

// testContext gives a harness client the lifetime its watchers hang off.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
