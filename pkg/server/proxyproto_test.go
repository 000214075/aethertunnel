package server

import (
	"io"
	"net"
	"strings"
	"testing"
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
