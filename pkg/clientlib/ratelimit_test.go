package clientlib

import (
	"net"
	"testing"
	"time"
)

// The relay reads into a 32 KiB buffer, so reserving len(p) up front charged every
// read as if it were full: a stream of small records was throttled to a fraction of
// the configured rate. The charge has to follow the bytes that actually moved.
func TestASmallReadIsChargedForWhatItRead(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// One second's burst, so a few bytes are free and a 32 KiB reservation is not.
	conn := newLimitedConn(1000, client)

	done := make(chan int, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, _ := conn.Read(buf)
		done <- n
	}()
	// net.Pipe is unbuffered, so the write waits for the read above to take it.
	if _, err := server.Write([]byte("ten bytes!")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case n := <-done:
		if n != 10 {
			t.Fatalf("read %d bytes, want 10", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a 10-byte read waited on a 32 KiB reservation")
	}
}
