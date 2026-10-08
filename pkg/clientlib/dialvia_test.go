package clientlib

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDialViaConnectTunnelsThroughAnHTTPProxy drives the CONNECT handshake the
// way a real HTTP proxy sees it: the request names the target and carries the
// credentials, the answer is 200, and the bytes after it belong to the tunnel.
func TestDialViaConnectTunnelsThroughAnHTTPProxy(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyListener.Close()

	target := "backend.example:443"
	type proxySaw struct {
		method, host, auth string
	}
	saw := make(chan proxySaw, 1)
	go func() {
		conn, err := proxyListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			saw <- proxySaw{method: "unreadable: " + err.Error()}
			return
		}
		saw <- proxySaw{method: request.Method, host: request.Host, auth: request.Header.Get("Proxy-Authorization")}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, _ = io.Copy(conn, conn) // echo the tunnel's bytes back
	}()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialVia(context.Background(), "http://user:pass@"+proxyListener.Addr().String(), dialer, target)
	if err != nil {
		t.Fatalf("dialVia: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(reply) != "ping" {
		t.Fatalf("the tunnel carried %q, want ping", reply)
	}

	select {
	case got := <-saw:
		if got.method != http.MethodConnect {
			t.Fatalf("the proxy saw a %q request, want CONNECT", got.method)
		}
		if got.host != target {
			t.Fatalf("the CONNECT names %q, want %q", got.host, target)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
		if got.auth != wantAuth {
			t.Fatalf("Proxy-Authorization is %q, want %q", got.auth, wantAuth)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy saw no CONNECT request")
	}
}

// TestDialViaSOCKS5WalksTheHandshake speaks the server side of SOCKS5: the
// greeting is answered without auth, the connect request names the target,
// and the bytes after the reply belong to the tunnel.
func TestDialViaSOCKS5WalksTheHandshake(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyListener.Close()

	target := "backend.example:443"
	saw := make(chan string, 1)
	go func() {
		conn, err := proxyListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		greeting := make([]byte, 3) // VER NMETHODS METHODS
		if _, err := io.ReadFull(conn, greeting); err != nil {
			saw <- "greeting: " + err.Error()
			return
		}
		if greeting[0] != 5 {
			saw <- "greeting version is not 5"
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			saw <- "greeting reply: " + err.Error()
			return
		}

		head := make([]byte, 4) // VER CMD RSV ATYP
		if _, err := io.ReadFull(conn, head); err != nil {
			saw <- "request head: " + err.Error()
			return
		}
		var tailLen int
		switch head[3] {
		case 1:
			tailLen = 4 + 2 // IPv4 + port
		case 4:
			tailLen = 16 + 2 // IPv6 + port
		default:
			length := make([]byte, 1)
			if _, err := io.ReadFull(conn, length); err != nil {
				saw <- "name length: " + err.Error()
				return
			}
			tailLen = int(length[0]) + 2
		}
		tail := make([]byte, tailLen)
		if _, err := io.ReadFull(conn, tail); err != nil {
			saw <- "request tail: " + err.Error()
			return
		}
		name := tail[:tailLen-2]
		port := tail[tailLen-2:]
		saw <- string(name) + ":" + strconv.Itoa(int(port[0])<<8|int(port[1]))

		// BIND.ADDR/PORT of all zeroes: the client does not read them.
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		_, _ = io.Copy(conn, conn) // echo the tunnel's bytes back
	}()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialVia(context.Background(), "socks5://"+proxyListener.Addr().String(), dialer, target)
	if err != nil {
		t.Fatalf("dialVia: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(reply) != "ping" {
		t.Fatalf("the tunnel carried %q, want ping", reply)
	}

	select {
	case got := <-saw:
		if got != target {
			t.Fatalf("the proxy saw a request for %q, want %q", got, target)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy saw no SOCKS5 request")
	}
}

func TestDialViaRejectsUnknownSchemes(t *testing.T) {
	dialer := &net.Dialer{Timeout: time.Second}
	_, err := dialVia(context.Background(), "ftp://proxy.lan", dialer, "backend.example:443")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("dialVia = %v, want an unsupported-scheme error", err)
	}
}
