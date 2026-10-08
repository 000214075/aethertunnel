package server

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A client that puts its first payload bytes in the same write as the CONNECT
// headers must not lose them.
//
// The server parses the request through a buffered reader, and that reader pulls
// in whatever the socket had ready — which is the payload, in this case.
// Relaying from the raw connection instead of through the buffer drops those
// bytes silently: the tunnel opens, the far end answers, and the first thing the
// client sent is simply gone. A proxy client that speaks before it reads (a
// TLS-less protocol with a greeting, or a request already queued) is exactly
// that shape.
func TestTCPMuxKeepsBytesPipelinedAfterTheConnectHeaders(t *testing.T) {
	backend := echoService(t)

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
		Domains:     []string{"pipelined"},
	})

	muxAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort))
	conn, err := net.DialTimeout("tcp", muxAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the mux: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := "early-bytes"
	// One write: the CONNECT, its headers, and the payload behind them. The
	// reader on this side is kept, because the answer comes back through it.
	request := "CONNECT pipelined:9999 HTTP/1.1\r\nHost: pipelined\r\n\r\n" + payload
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want 200", response.StatusCode)
	}

	// The backend echoes, so what comes back is what the tunnel carried.
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, reply); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(reply) != payload {
		t.Fatalf("the tunnel carried %q, want %q (the bytes sent with the CONNECT were dropped)", reply, payload)
	}
}

// halfCloseService reads a request until the client's write side closes, then
// answers with what it read. Plenty of protocols are shaped like that — the
// answer only exists once the request's end has arrived — so a visitor that
// half-closes its side must still get that answer back through the mux.
func halfCloseService(t *testing.T) string {
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
				body, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("read " + strconv.Itoa(len(body)) + " bytes"))
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func TestTCPMuxForwardsTheVisitorsHalfClose(t *testing.T) {
	backend := halfCloseService(t)

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
		Domains:     []string{"halfclose"},
	})

	conn, response := connectThrough(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort)), "halfclose:9999")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want 200", response.StatusCode)
	}
	payload := "request-with-a-half-close"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	halfCloser, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the visitor's connection cannot half-close")
	}
	if err := halfCloser.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	// The answer only exists if the backend saw the end of the request, so this
	// read carries the half-close's verdict as well as the tunnel's.
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	want := "read " + strconv.Itoa(len(payload)) + " bytes"
	if string(answer) != want {
		t.Fatalf("the backend answered %q, want %q", answer, want)
	}
}

// maxConnectRequestBytes bounds the CONNECT request alone, and handle stays on
// the relay for the whole stream. A release that only ran when handle returned
// therefore counted every relayed byte against the cap, and a tunnel past 64 KiB
// ended with "the CONNECT request is too large" — a payload bound that no
// configuration mentions.
func TestTCPMuxRelaysMoreThanTheRequestCap(t *testing.T) {
	backend := echoService(t)

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
		Domains:     []string{"bigger"},
	})

	conn, response := connectThrough(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort)), "bigger:9999")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want 200", response.StatusCode)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	payload := make([]byte, maxConnectRequestBytes+64<<10)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	// Written on its own goroutine: the echo only returns as the tunnel carries
	// it, and a full write of a payload this size can outrun the socket buffers.
	go func() { _, _ = conn.Write(payload) }()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read through the tunnel after %d bytes: %v", len(payload), err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the tunnel did not echo the whole payload back")
	}
}
