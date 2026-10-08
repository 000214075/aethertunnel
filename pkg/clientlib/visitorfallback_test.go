package clientlib

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// fallbackAnswer listens on a loopback port and answers every caller with one
// line, so a test can tell where a connection ended up.
func fallbackAnswer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the fallback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("fallback-answer\n"))
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// A visitor with fallback_to connects its caller straight to that address once
// the tunnel cannot be opened, so the local service stays reachable while the
// server is down.
func TestVisitorFallbackConnectsTheCallerDirectly(t *testing.T) {
	cfg := config.VisitorConfig{
		Name:       "ssh",
		Type:       config.ProxyTypeSTCP,
		ServerName: "ssh",
		// The tunnel is pointed at a closed port, so opening it fails at once.
		FallbackTo:        fallbackAnswer(t),
		FallbackTimeoutMs: 500,
	}
	c := &client{
		cfg:    &config.Config{},
		logger: log.New(io.Discard, "", 0),
		target: refusedAddr(t),
	}
	c.baseCtx = context.Background()

	caller, visited := net.Pipe()
	defer caller.Close()

	done := make(chan struct{})
	go func() {
		c.handleVisitorTCP(context.Background(), cfg, visited)
		close(done)
	}()

	_ = caller.SetDeadline(time.Now().Add(5 * time.Second))
	answer, err := bufio.NewReader(caller).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the caller's connection: %v", err)
	}
	if answer != "fallback-answer\n" {
		t.Fatalf("the caller was answered %q, want the fallback's line", answer)
	}

	_ = caller.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the visitor did not finish after the caller closed")
	}
}

// Without a fallback the caller is refused, which is the behaviour a visitor
// had before the key existed.
func TestWithoutFallbackTheCallerIsRefused(t *testing.T) {
	cfg := config.VisitorConfig{Name: "ssh", Type: config.ProxyTypeSTCP, ServerName: "ssh"}
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0), target: refusedAddr(t)}
	c.baseCtx = context.Background()

	caller, visited := net.Pipe()
	defer caller.Close()

	done := make(chan struct{})
	go func() {
		c.handleVisitorTCP(context.Background(), cfg, visited)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the visitor did not finish opening the tunnel")
	}

	_ = caller.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := caller.Read(make([]byte, 1)); err == nil {
		t.Fatal("the caller was answered without a tunnel and without a fallback")
	}
}
