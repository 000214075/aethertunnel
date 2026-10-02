package net

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestCompressConnRoundTrip carries a compressible payload through a pair of
// wrapped ends over one net.Pipe and back, and proves the wire actually
// carried fewer bytes than the payload.
func TestCompressConnRoundTrip(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	wire := &countingConn{Conn: serverSide}
	compressed := CompressConn(clientSide)
	plain := CompressConn(wire)

	payload := []byte(strings.Repeat("aethertunnel-compression-check ", 4000))

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		if _, err := compressed.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(plain, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	<-sent
	if !bytes.Equal(got, payload) {
		t.Fatal("the round trip did not restore the payload")
	}
	if wire.written >= int64(len(payload)) {
		t.Fatalf("the wire carried %d bytes for a %d-byte payload: nothing was compressed",
			wire.written, len(payload))
	}
}

// TestCompressConnSurvivesManySmallWrites pins the property the streaming
// format must keep: every Write is self-contained, so a peer that writes and
// reads interactively never waits for a block to fill.
func TestCompressConnSurvivesManySmallWrites(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	compressed := CompressConn(clientSide)
	plain := CompressConn(serverSide)

	go func() {
		for i := 0; i < 50; i++ {
			if _, err := compressed.Write([]byte("msg")); err != nil {
				return
			}
		}
	}()
	plain.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 150)
	if _, err := io.ReadFull(plain, got); err != nil {
		t.Fatalf("interleaved small writes stalled: %v", err)
	}
	if string(got) != strings.Repeat("msg", 50) {
		t.Fatalf("the stream carried %q", got)
	}
}

// countingConn counts what actually reached the wire.
type countingConn struct {
	net.Conn
	written int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	return c.Conn.Write(p)
}
