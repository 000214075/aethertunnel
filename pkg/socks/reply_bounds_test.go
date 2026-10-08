package socks

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A CONNECT to port 0 is refused with a reply. RFC 1928 requires an answer to
// every request, and the reader used to return the error with nothing written, so
// the visitor saw the connection close and could not tell a refusal from a dead
// server.
func TestAConnectToPortZeroIsAnswered(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x00}
	go func() { _, _ = client.Write(script) }()

	replies := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		total := 0
		for {
			n, err := client.Read(buf[total:])
			if err != nil {
				break
			}
			total += n
		}
		replies <- append([]byte(nil), buf[:total]...)
	}()

	if _, err := ReadRequest(server, 2*time.Second); !errors.Is(err, ErrPortMissing) {
		t.Fatalf("want ErrPortMissing, got %v", err)
	}
	_ = server.Close()

	got := <-replies
	if len(got) < 12 {
		t.Fatalf("the reader answered %d bytes (%v), want the method reply and a request reply", len(got), got)
	}
	wantPrefix := []byte{Version, MethodNoReq, Version, ReplyGeneralFailure, 0x00, AtypIPv4}
	if !bytes.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Errorf("the answer starts %v, want %v", got[:len(wantPrefix)], wantPrefix)
	}
}

// A domain name of length zero is invalid rather than long, and the error used to
// say the name was too long, sending an operator after a name that was never sent.
func TestAnEmptyDomainNameIsRefusedAsSuch(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypDomain, 0x00}
	if _, _, err := serve(t, script); !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("want ErrEmptyAddress, got %v", err)
	}
}

// An address that is neither IPv4 nor IPv6 must not leave the reply without the
// address its header promises: the visitor would read the port out of whatever
// followed.
func TestWriteReplyBoundFallsBackToTheWildcardForAnUnusableAddress(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 10)
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.ReadFull(client, buf)
		done <- buf[:n]
	}()

	if err := WriteReplyBound(server, ReplySucceeded, net.IP{1, 2, 3}, 4100); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := <-done
	want := []byte{Version, ReplySucceeded, 0x00, AtypIPv4, 0, 0, 0, 0, 0x10, 0x04}
	if !bytes.Equal(got, want) {
		t.Errorf("reply is %v, want %v", got, want)
	}
}

// A port that does not fit the reply's two bytes is refused rather than truncated
// into a port the caller never named.
func TestWriteReplyBoundRefusesAPortThatDoesNotFit(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	// Drain the writes: net.Pipe is synchronous, and without a reader a reply the
	// sink should never have written would block here instead of failing.
	go func() {
		buf := make([]byte, 32)
		for {
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	for _, port := range []int{-1, 65536, 70000} {
		if err := WriteReplyBound(server, ReplySucceeded, net.IPv4zero, port); err == nil {
			t.Errorf("port %d was written as if it fit the reply", port)
		}
	}
}
