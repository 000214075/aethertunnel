package net

import (
	"bufio"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// handshakeClient and handshakeServer pair one WebsocketDial with one
// AcceptOrPass over conn, so each test states only what it checks.
func handshakeClient(t *testing.T, conn net.Conn, host string) net.Conn {
	t.Helper()
	ws, err := WebsocketDial(conn, host, 5*time.Second)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	return ws
}

func handshakeServer(t *testing.T, conn net.Conn) net.Conn {
	t.Helper()
	ws, err := AcceptOrPass(conn, 5*time.Second)
	if err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return ws
}

func TestWebsocketCarriesBinaryBothWays(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	t.Cleanup(func() { serverRaw.Close(); clientRaw.Close() })
	_ = serverRaw.SetDeadline(time.Now().Add(10 * time.Second))
	_ = clientRaw.SetDeadline(time.Now().Add(10 * time.Second))

	serverDone := make(chan net.Conn, 1)
	go func() {
		ws, err := AcceptOrPass(serverRaw, 5*time.Second)
		if err != nil {
			return
		}
		serverDone <- ws
	}()
	clientWS := handshakeClient(t, clientRaw, "tunnel.example")
	serverWS := <-serverDone

	payload := make([]byte, 200000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("random payload: %v", err)
	}
	go func() {
		_, _ = clientWS.Write(payload)
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(serverWS, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d differs: the tunnel is not byte-exact", i)
		}
		break
	}

	answer := []byte("over-and-back")
	go func() { _, _ = serverWS.Write(answer) }()
	reply := make([]byte, len(answer))
	if _, err := io.ReadFull(clientWS, reply); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(reply) != string(answer) {
		t.Fatalf("the way back carried %q, want %q", reply, answer)
	}
}

func TestWebsocketAnswersPingAndCloses(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	t.Cleanup(func() { serverRaw.Close(); clientRaw.Close() })
	_ = clientRaw.SetDeadline(time.Now().Add(10 * time.Second))

	serverDone := make(chan string, 1)
	go func() {
		ws, err := AcceptOrPass(serverRaw, 5*time.Second)
		if err != nil {
			serverDone <- "accept: " + err.Error()
			return
		}
		buf := make([]byte, 64)
		n, err := ws.Read(buf)
		if err != nil {
			serverDone <- "read after the ping: " + err.Error()
			return
		}
		if string(buf[:n]) != "after-the-ping" {
			serverDone <- "payload: " + string(buf[:n])
			return
		}
		// The close frame ends the next read.
		if _, err := ws.Read(buf); err == nil {
			serverDone <- "the close did not end the read"
			return
		}
		close(serverDone)
	}()
	clientWS := handshakeClient(t, clientRaw, "tunnel.example")

	// The pong the server answers with needs a reader on this side; this test
	// plays a client that only sends, so a discard drain keeps the synchronous
	// pipe from wedging the read loop that answers the ping.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := clientWS.Read(buf); err != nil {
				return
			}
		}
	}()

	// A masked ping written as a raw frame is answered with a pong and the
	// connection keeps working: the next binary frame still arrives.
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("mask key: %v", err)
	}
	masked := []byte{'p', 'i', 'n', 'g'}
	for i := range masked {
		masked[i] ^= key[i%4]
	}
	frame := append([]byte{0x80 | 0x9, 0x80 | 4}, key[:]...)
	frame = append(frame, masked...)
	if _, err := clientRaw.Write(frame); err != nil {
		t.Fatalf("write the ping: %v", err)
	}
	if _, err := clientWS.Write([]byte("after-the-ping")); err != nil {
		t.Fatalf("write after the ping: %v", err)
	}

	// A close frame ends the far read with EOF.
	if err := clientWS.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case problem, ok := <-serverDone:
		if ok {
			t.Fatalf("the server side reports: %s", problem)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close was not noticed")
	}
}

func TestAcceptOrPassReplaysRawBytes(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	t.Cleanup(func() { serverRaw.Close(); clientRaw.Close() })
	_ = clientRaw.SetDeadline(time.Now().Add(10 * time.Second))

	raw := []byte("\x00\x01binary-frame-that-is-no-get\x00")
	go func() { _, _ = clientRaw.Write(raw) }()

	carried, err := AcceptOrPass(serverRaw, 5*time.Second)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	got := make([]byte, len(raw))
	if _, err := io.ReadFull(carried, got); err != nil {
		t.Fatalf("read the replayed bytes: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("the raw path saw %q, want %q", got, raw)
	}
}

func TestWebsocketDialRejectsWrongAcceptKey(t *testing.T) {
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
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_ = request.Body.Close()
				// A 101 whose accept key does not answer the request, held
				// long enough for the client to read it.
				_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
				time.Sleep(5 * time.Second)
			}()
		}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := WebsocketDial(conn, "tunnel.example", 5*time.Second); err == nil ||
		!strings.Contains(err.Error(), "accept key") {
		t.Fatalf("WebsocketDial = %v, want an accept-key mismatch", err)
	}
}
