package socks

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// fuzzConn feeds a fixed byte slice to a parser without blocking, and swallows
// whatever the parser writes back. A real socket would block when the parser
// stops reading early — an oversized or malformed request is exactly that case —
// and a fuzz target that leaks a blocked goroutine per input runs out of memory
// before it finds a bug.
type fuzzConn struct{ r *bytes.Reader }

func (c *fuzzConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fuzzConn) Close() error                     { return nil }
func (c *fuzzConn) LocalAddr() net.Addr              { return fuzzAddr{} }
func (c *fuzzConn) RemoteAddr() net.Addr             { return fuzzAddr{} }
func (c *fuzzConn) SetDeadline(time.Time) error      { return nil }
func (c *fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fuzzConn) SetWriteDeadline(time.Time) error { return nil }

type fuzzAddr struct{}

func (fuzzAddr) Network() string { return "fuzz" }
func (fuzzAddr) String() string  { return "fuzz" }

// FuzzReadRequest parses what any visitor can put on the public socks5 port
// before authentication, since a socks5 exit has no handshake in front of it.
func FuzzReadRequest(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{Version, 1, MethodNoReq, Version, CmdConnect, 0, AtypIPv4, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{Version, 1, MethodNoReq, Version, CmdAssoc, 0, AtypDomain, 3, 'a', 'b', 'c', 0, 0})
	f.Add([]byte{Version, 1, MethodNoReq, Version, CmdConnect, 0, AtypIPv6,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 53})

	f.Fuzz(func(t *testing.T, data []byte) {
		request, err := ReadRequest(&fuzzConn{r: bytes.NewReader(data)}, time.Second)
		if err != nil {
			return
		}
		// A request that parsed has to name exactly one address, and it has to be a
		// host:port pair: a CONNECT names the service to reach, an ASSOCIATE names
		// the source the visitor will send from.
		address := request.Target
		if request.Command == CmdAssoc {
			if request.Target != "" {
				t.Fatalf("a UDP ASSOCIATE request parsed with a target %q", request.Target)
			}
			address = request.ClientAddr
		}
		if address == "" {
			t.Fatalf("command %d parsed with no address", request.Command)
		}
		if _, _, err := net.SplitHostPort(address); err != nil {
			t.Fatalf("parsed address %q is not host:port: %v", address, err)
		}
	})
}

// FuzzParseUDPDatagram parses what any visitor can send to the relay address a
// socks5 UDP ASSOCIATE handed out.
func FuzzParseUDPDatagram(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, AtypIPv4, 127, 0, 0, 1, 0, 80, 'h', 'i'})
	f.Add([]byte{0, 0, 1, AtypIPv4, 127, 0, 0, 1, 0, 80}) // a fragment
	f.Add([]byte{1, 0, 0, AtypIPv4, 127, 0, 0, 1, 0, 80}) // a non-zero reserved byte
	f.Add([]byte{0, 0, 0, AtypDomain, 3, 'a', 'b', 'c', 0, 80, 'x'})

	wrapped, err := WrapUDPDatagram("127.0.0.1:80", []byte("payload"))
	if err != nil {
		f.Fatalf("build a datagram: %v", err)
	}
	f.Add(wrapped)

	f.Fuzz(func(t *testing.T, packet []byte) {
		addr, data, err := ParseUDPDatagram(packet)
		if err != nil {
			return
		}
		// A parsed datagram reports an address inside the packet and data that the
		// packet really carries, so a caller can trust the length it is handed.
		if _, _, err := net.SplitHostPort(addr); err != nil {
			t.Fatalf("parsed address %q is not host:port: %v", addr, err)
		}
		if len(data) > len(packet) {
			t.Fatalf("parsed %d bytes of data out of a %d byte datagram", len(data), len(packet))
		}
	})
}
