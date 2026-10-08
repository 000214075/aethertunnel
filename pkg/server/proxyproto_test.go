package server

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProxyHeaderV1(t *testing.T) {
	remote := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 4444}
	local := &net.TCPAddr{IP: net.IPv4(198, 51, 100, 2), Port: 8080}
	header := ProxyHeaderV1(remote, local)
	want := "PROXY TCP4 203.0.113.9 198.51.100.2 4444 8080\r\n"
	if header != want {
		t.Fatalf("header is %q, want %q", header, want)
	}

	ipv6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4444}
	if header := ProxyHeaderV1(ipv6, local); !strings.HasPrefix(header, "PROXY UNKNOWN") {
		t.Fatalf("an IPv6 visitor produced %q, want the UNKNOWN form", header)
	}
	if header := ProxyHeaderV1(&net.UnixAddr{Name: "x"}, local); !strings.HasPrefix(header, "PROXY UNKNOWN") {
		t.Fatalf("a unix visitor produced %q, want the UNKNOWN form", header)
	}
}

func TestProxyHeaderConnYieldsTheHeaderOnce(t *testing.T) {
	reader, writer := net.Pipe()
	defer writer.Close()
	wrapped := &proxyHeaderConn{Conn: reader, header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 9 10\r\n")}

	go func() {
		// the visitor's own bytes arrive after the header has been read out
		writer.Write([]byte("hello"))
		writer.Close()
	}()

	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read through the header: %v", err)
	}
	want := "PROXY TCP4 1.2.3.4 5.6.7.8 9 10\r\nhello"
	if string(got) != want {
		t.Fatalf("the stream is %q, want %q", got, want)
	}
}

func TestProxyHeaderV2(t *testing.T) {
	remote := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 4444}
	local := &net.TCPAddr{IP: net.IPv4(198, 51, 100, 2), Port: 8080}
	header := ProxyHeaderV2(remote, local)

	signature := string([]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A})
	if !strings.HasPrefix(string(header), signature) {
		t.Fatalf("the header does not open with the v2 signature: % x", header[:16])
	}
	if len(header) != 28 {
		t.Fatalf("a TCP4 v2 header is %d bytes, want 28", len(header))
	}
	// ver/cmd 0x21 (v2, PROXY), family 0x11 (TCP4), length 0x0010.
	if header[12] != 0x21 || header[13] != 0x11 || header[14] != 0x00 || header[15] != 0x0C {
		t.Fatalf("the command block is % x, want 21 11 00 0c", header[12:16])
	}
	if !bytes.Equal(header[16:20], []byte{203, 0, 113, 9}) || !bytes.Equal(header[20:24], []byte{198, 51, 100, 2}) {
		t.Fatalf("the addresses are % x % x", header[16:20], header[20:24])
	}
	if header[24] != 0x11 || header[25] != 0x5C || header[26] != 0x1F || header[27] != 0x90 {
		t.Fatalf("the ports are % x, want 11 5c 1f 90 (4444, 8080)", header[24:28])
	}

	ipv6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4444}
	unknown := ProxyHeaderV2(ipv6, local)
	if len(unknown) != 16 || unknown[12] != 0x20 {
		t.Fatalf("an IPv6 visitor produced %d bytes with command byte % x, want the 16-byte UNKNOWN form", len(unknown), unknown[12])
	}
}

// flynet.Pipe finishes each direction with CloseWrite when the type offers one and
// with a full close otherwise, so a wrapper that hides it turns the half-close of a
// proxy_protocol tunnel's local service into a full close and cuts off the reply the
// service only sends after it has seen the end of the request. net.Conn does not
// carry the method, so the wrapper has to forward it by hand.
func TestProxyHeaderConnForwardsTheHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	visitor, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer visitor.Close()

	serverSide := <-accepted
	defer serverSide.Close()

	wrapped := &proxyHeaderConn{Conn: visitor, header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 9 10\r\n")}
	if err := wrapped.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite through the wrapper: %v", err)
	}

	// The peer sees the end of the stream on its read side...
	_ = serverSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(serverSide); err != nil {
		t.Fatalf("read to the half-close: %v", err)
	}
	// ...and can still answer, which is what a full close would have prevented.
	if _, err := serverSide.Write([]byte("answer")); err != nil {
		t.Fatalf("the peer cannot answer after the half-close: %v", err)
	}
	// Read the answer from the connection itself: the wrapper answers a read with
	// the PROXY header first, which is not what this test is about.
	answer := make([]byte, len("answer"))
	_ = visitor.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(visitor, answer); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if string(answer) != "answer" {
		t.Fatalf("the answer is %q, want %q", answer, "answer")
	}
}
