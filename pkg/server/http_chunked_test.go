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
)

// startBodyEchoHTTPService serves a handler that drains the request body and
// reports how many bytes it read, so a test knows the body really crossed the
// tunnel rather than being discarded before it arrived.
func startBodyEchoHTTPService(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start the body echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "read %d", n)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// TestHTTPProxyCountsAChunkedRequestBody covers a body sent with no
// Content-Length. It used to be billed as zero bytes because the accounting read
// r.ContentLength, which is -1 for a chunked body; now it reads what the reverse
// proxy actually drained.
func TestHTTPProxyCountsAChunkedRequestBody(t *testing.T) {
	backend := startBodyEchoHTTPService(t)
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"upload": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "upload", Type: protocol.ProxyTypeHTTP, LocalAddr: backend,
		Domains: []string{"upload.example.com"},
	})
	waitForListener(t, rs.server, "upload")

	payload := []byte(strings.Repeat("x", 4096))
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(payload)
		_ = writer.Close()
	}()

	// An *io.PipeReader has no known length, so the client sends the body
	// chunked.
	request, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.Server.HTTPPort), reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Host = "upload.example.com"

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(got), fmt.Sprintf("read %d", len(payload))) {
		t.Fatalf("the backend did not receive the whole body: %q", got)
	}

	group, err := rs.server.tunnels.Get("upload")
	if err != nil {
		t.Fatalf("look up the proxy: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, total, _, out := group.Totals()
		if total == 1 && out == int64(len(payload)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the chunked body was not billed: total=%d out=%d, want 1/%d",
				total, out, len(payload))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
