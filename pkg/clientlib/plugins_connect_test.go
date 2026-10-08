package clientlib

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A visitor may pipeline the first tunnelled bytes right behind its CONNECT
// headers. The HTTP server has already read those into the reader it returns
// from Hijack, so a tunnel that reads the raw socket drops them and the target
// sees an empty request.
func TestAConnectTunnelKeepsBytesPipelinedBehindTheHeaders(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the target: %v", err)
	}
	defer target.Close()
	received := make(chan string, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.Client.DialTimeoutSecs = 5
	c.cfg.Client.IdleTimeoutSecs = 5

	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, Plugin: "http_proxy"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.handleProxyConnect(w, r, proxy)
	}))
	defer server.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()

	const pipelined = "GET / HTTP/1.1\r\nHost: service\r\n\r\n"
	// One write, so the request headers and the tunnelled bytes arrive together
	// and the server's reader holds the second half.
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n%s",
		target.Addr(), target.Addr(), pipelined); err != nil {
		t.Fatalf("write the CONNECT: %v", err)
	}

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("the CONNECT answer is %q, want a 200", strings.TrimSpace(status))
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read the end of the CONNECT answer: %v", err)
	}

	select {
	case got := <-received:
		if !strings.Contains(got, pipelined) {
			t.Fatalf("the target received %q, want the pipelined request %q", got, pipelined)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the target never received the bytes pipelined behind the CONNECT headers")
	}
}
