package clientlib

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// refusedAddr hands back a loopback address whose listener is already closed,
// so dialing it fails at once with "connection refused".
func refusedAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open a listener: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}
	return addr
}

func loginFailExitConfig(addr string, loginFailExit bool) *config.Config {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = addr
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1
	cfg.Client.LoginFailExit = loginFailExit
	return cfg
}

// With client.login_fail_exit set, a client whose very first login is refused
// stops instead of retrying forever, and Run reports why.
func TestLoginFailExitStopsTheClientOnARefusedFirstLogin(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a refused first login with login_fail_exit set")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run is still retrying after a refused first login with login_fail_exit set")
	}
}

// The default keeps retrying a refused login: a client without login_fail_exit
// behaves as before and never gives up on its own.
func TestWithoutLoginFailExitTheClientKeepsRetrying(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	select {
	case err := <-done:
		t.Fatalf("Run stopped on its own after a refused login: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// login_fail_exit only guards the very first login: a client whose session was
// established once keeps reconnecting when the session drops.
func TestLoginFailExitKeepsReconnectingAnEstablishedSession(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: cfg.Client.ServerAddr}
	c.baseCtx = context.Background()
	c.sessionEstablished.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()

	// The first refused session comes back around for a reconnect instead of
	// stopping the client; the reconnect wait is jitter(1s), so nothing ends
	// the run inside this window except a wrongly taken give-up path.
	select {
	case <-done:
		t.Fatal("run stopped a client whose session was already established")
	case <-time.After(300 * time.Millisecond):
	}
}

// A refused first login is reported through the logger before Run returns.
func TestLoginFailExitNamesTheReasonWhenItStops(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	buf := &bytes.Buffer{}
	err := Run(context.Background(), cfg, log.New(buf, "", 0))
	if err == nil {
		t.Fatal("Run returned nil for a refused first login with login_fail_exit set")
	}
	if want := "login failed and client.login_fail_exit is set"; !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Errorf("the log names the stop reason %q, got:\n%s", want, buf.String())
	}
}
