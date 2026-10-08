package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// connectAnswerService reads a request's headers and answers with the request
// line it saw, so a test can tell whether the backend was handed the CONNECT
// itself or a stream that starts after it.
func connectAnswerService(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if _, err := c.Write([]byte("saw: " + strings.TrimSpace(line))); err != nil {
					return
				}
				// Keep the connection open after answering: a service does not
				// hang up the moment it has spoken, and closing right after a
				// write races the relay's teardown (the answer can be lost).
				_, _ = io.Copy(io.Discard, reader)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// TestTCPMuxPassthroughHandsTheConnectToTheTunnel covers
// server.tcpmux_passthrough: with it on, the multiplexer still reads the request
// to route by its authority, but answers nothing itself and relays the request
// as written — which is what frp's tcpmuxPassthrough does, and what lets a
// service that speaks CONNECT itself (an HTTP proxy behind the tunnel) work.
func TestTCPMuxPassthroughHandsTheConnectToTheTunnel(t *testing.T) {
	backend := connectAnswerService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.TCPMuxPort = freePort(t)
	cfg.Server.TCPMuxPassthrough = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeTCPMux, LocalAddr: backend,
		Multiplexer: config.MultiplexerHTTPConnect,
		Domains:     []string{"passthrough"},
	})

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the mux: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// The visitor writes a CONNECT as it would to any proxy and waits for the
	// answer — which in passthrough has to come from the backend.
	if _, err := conn.Write([]byte("CONNECT passthrough:9999 HTTP/1.1\r\nHost: passthrough\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	want := "saw: CONNECT passthrough:9999 HTTP/1.1"
	answer := make([]byte, len(want))
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if string(answer) != want {
		t.Fatalf("the answer is %q, want %q: the backend did not receive the CONNECT itself", answer, want)
	}
}

// Without the key the multiplexer answers the CONNECT itself, so the backend
// sees the stream from behind the request, which is what a service that expects
// raw bytes needs.
func TestTCPMuxAnswersTheConnectItselfByDefault(t *testing.T) {
	backend := connectAnswerService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.TCPMuxPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeTCPMux, LocalAddr: backend,
		Multiplexer: config.MultiplexerHTTPConnect,
		Domains:     []string{"answered"},
	})

	muxAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort))
	conn, response := connectThrough(t, muxAddr, "answered:9999")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want the server's own 200", response.StatusCode)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "saw: hello"
	answer := make([]byte, len(want))
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read the backend's answer: %v", err)
	}
	if string(answer) != want {
		t.Fatalf("the backend saw %q, want %q", answer, want)
	}
}

// connectEchoService plays a backend that answers the CONNECT itself — the shape of
// an HTTP proxy behind the tunnel — and then echoes every byte that follows the
// headers, so a test can tell whether the visitor's pipelined payload arrived once
// or twice.
func connectEchoService(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				_ = request.Body.Close()
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
					return
				}
				// Whatever the visitor pipelined behind the request is payload, and
				// it has to arrive here exactly once.
				_, _ = io.Copy(c, reader)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// A visitor is free to put its first payload bytes in the same write as the
// CONNECT. The multiplexer records the request by reading through a recorder, and
// the recorder sits under the buffered reader, so what it holds already includes
// the bytes the buffer read ahead: replaying the buffer's remainder as well handed
// the backend that tail twice, which corrupts a stream whose service speaks
// CONNECT itself.
func TestTCPMuxPassthroughDeliversPipelinedBytesOnce(t *testing.T) {
	backend := connectEchoService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.TCPMuxPort = freePort(t)
	cfg.Server.TCPMuxPassthrough = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeTCPMux, LocalAddr: backend,
		Multiplexer: config.MultiplexerHTTPConnect,
		Domains:     []string{"once"},
	})

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the mux: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	const payload = "pipelined-payload"
	if _, err := conn.Write([]byte("CONNECT once:9999 HTTP/1.1\r\nHost: once\r\n\r\n" + payload)); err != nil {
		t.Fatalf("write CONNECT with payload: %v", err)
	}

	established := "HTTP/1.1 200 Connection established\r\n\r\n"
	answer := make([]byte, len(established))
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read the backend's answer: %v", err)
	}
	if string(answer) != established {
		t.Fatalf("the backend answered %q, want the CONNECT answer", answer)
	}

	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read the echoed payload: %v", err)
	}
	if string(echoed) != payload {
		t.Fatalf("the backend echoed %q, want %q", echoed, payload)
	}

	// A second copy would already be in flight, so a short read that times out is
	// what says the payload arrived once.
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := conn.Read(make([]byte, len(payload))); n != 0 || err == nil {
		t.Fatalf("the backend received the pipelined payload more than once (%d more bytes, %v)", n, err)
	}
}
