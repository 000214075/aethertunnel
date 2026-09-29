package server

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// startUpgradeBackend serves a minimal HTTP/1.1 endpoint that answers an
// Upgrade: test request with 101 Switching Protocols and then echoes whatever
// bytes follow on the upgraded connection.
func startUpgradeBackend(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start the upgrade backend: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

				// Read the request headers, then answer the protocol switch.
				request := make([]byte, 0, 4096)
				buf := make([]byte, 1024)
				for !bytes.Contains(request, []byte("\r\n\r\n")) {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					request = append(request, buf[:n]...)
				}
				if !bytes.Contains(request, []byte("Upgrade: test")) {
					_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
					return
				}
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"))

				// Echo the upgraded bytes back.
				echo := make([]byte, 4096)
				for {
					n, err := conn.Read(echo)
					if err != nil {
						return
					}
					if _, err := conn.Write(echo[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

// TestHTTPProxyCarriesAProtocolSwitch covers a WebSocket-style upgrade: the
// reverse proxy has to hijack the visitor connection, which its response wrapper
// used to make impossible (the wrapper hid the underlying Hijacker).
func TestHTTPProxyCarriesAProtocolSwitch(t *testing.T) {
	backend := startUpgradeBackend(t)
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"upgrade": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "upgrade", Type: protocol.ProxyTypeHTTP, LocalAddr: backend,
		Domains: []string{"upgrade.example.com"},
	})
	waitForListener(t, rs.server, "upgrade")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Server.HTTPPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the shared http port: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	request := "GET /socket HTTP/1.1\r\nHost: upgrade.example.com\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("send the upgrade request: %v", err)
	}

	// Read the response status line and headers.
	response := make([]byte, 0, 4096)
	buf := make([]byte, 1024)
	for !bytes.Contains(response, []byte("\r\n\r\n")) {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read the upgrade response: %v", err)
		}
		response = append(response, buf[:n]...)
	}
	if !bytes.Contains(response, []byte("101 Switching Protocols")) {
		t.Fatalf("the proxy did not switch protocols, got %q", response)
	}

	payload := []byte("bytes-on-the-upgraded-socket")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write on the upgraded socket: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("the upgraded socket echoed %q, want %q", echoed, payload)
	}
	_ = conn.Close()

	// The upgraded bytes have to reach the counters, not vanish the moment the
	// connection is hijacked.
	group, err := rs.server.tunnels.Get("upgrade")
	if err != nil {
		t.Fatalf("look up the proxy: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var total, in, out int64
	for {
		_, total, in, out = group.Totals()
		if total == 1 && in == int64(len(payload)) && out == int64(len(payload)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the upgraded bytes were not counted: total=%d in=%d out=%d, want 1/%d/%d",
				total, in, out, len(payload), len(payload))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
