package net

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// TestPrefixedConnRelayHalfClose pins the half-close contract the tunnel's
// relay depends on: a connection whose first bytes a peek consumed must still
// carry a full request, its half-close, and the reply that the far end only
// produces after it saw the end of the request.
func TestPrefixedConnRelayHalfClose(t *testing.T) {
	visitor := connectedPair(t) // visitor.local: the test; visitor.remote: the relay
	far := connectedPair(t)     // far.local: the relay; far.remote: the service

	// The far service: echo after EOF, like the tunnel test's counter.
	go func() {
		data, err := io.ReadAll(far.remote)
		if err != nil {
			return
		}
		_, _ = far.remote.Write([]byte("read " + count(len(data)) + " bytes"))
		time.Sleep(50 * time.Millisecond)
		_ = far.remote.Close()
	}()

	buffered := bufio.NewReader(visitor.remote)
	// The visitor writes while the server's peek is still waiting, exactly as
	// a real visitor does against the accepted connection.
	go func() {
		payload := bytes.Repeat([]byte("x"), 4096)
		if _, err := visitor.local.Write(payload); err != nil {
			return
		}
		_ = visitor.local.(*net.TCPConn).CloseWrite()
	}()
	if _, err := buffered.Peek(len(UpgradePath) + 4); err != nil {
		t.Fatalf("peek: %v", err)
	}
	wrapped := &prefixedConn{Conn: visitor.remote, reader: buffered}
	go Pipe(wrapped, far.local, 0)

	_ = visitor.local.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := io.ReadAll(visitor.local)
	if err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if want := "read 4096 bytes"; string(reply) != want {
		t.Fatalf("the reply is %q, want %q", reply, want)
	}
}

// connectedPair is one loopback connection: local is the end the test or the
// relay holds, remote the end the peer holds.
type pairEnd struct {
	local  net.Conn
	remote net.Conn
}

func connectedPair(t *testing.T) pairEnd {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	dialed := make(chan net.Conn, 1)
	go func() {
		client, err := net.Dial("tcp", listener.Addr().String())
		if err == nil {
			dialed <- client
		}
	}()
	remote, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	select {
	case local := <-dialed:
		t.Cleanup(func() { remote.Close(); local.Close() })
		return pairEnd{local: local, remote: remote}
	case <-time.After(5 * time.Second):
		t.Fatal("the pair did not connect")
		return pairEnd{}
	}
}

func count(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}
