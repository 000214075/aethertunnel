//go:build unix

package clientlib

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestReloadWatcherRereadsTheFileOnSIGHUP is the hot-reload path end to end,
// signal and all: the client re-reads its file, and the new proxy is on the
// dispatch list — and on the wire — without the process restarting. It needs a
// real SIGHUP, which is a Unix thing; the reload logic itself is covered
// portably in reload_test.go.
func TestReloadWatcherRereadsTheFileOnSIGHUP(t *testing.T) {
	if os.Getpid() <= 1 {
		t.Skip("no pid to signal")
	}
	// Delivering a self-directed SIGHUP on darwin/arm64 crashes inside the Go
	// runtime itself ("fatal error: signal_recv: inconsistent state", seen on
	// three consecutive CI runs), where the os/signal machinery cannot be
	// trusted with it; the reload logic the signal drives is covered on every
	// platform in reload_test.go.
	if runtime.GOOS == "darwin" {
		t.Skip("the darwin os/signal runtime can crash on a self-directed SIGHUP")
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
