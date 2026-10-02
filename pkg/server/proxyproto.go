package server

import (
	"fmt"
	"net"
)

// ProxyHeaderV1 renders a PROXY protocol v1 header line for the connection's
// addresses, or the UNKNOWN form when either address is not an IPv4 endpoint.
// The local service receives the header as the first bytes of its stream and
// can log and filter on the real visitor.
func ProxyHeaderV1(remote, local net.Addr) string {
	src, srcOK := remote.(*net.TCPAddr)
	dst, dstOK := local.(*net.TCPAddr)
	if !srcOK || !dstOK || src.IP.To4() == nil || dst.IP.To4() == nil {
		return "PROXY UNKNOWN\r\n"
	}
	return fmt.Sprintf("PROXY TCP4 %s %s %d %d\r\n", src.IP, dst.IP, src.Port, dst.Port)
}

// proxyHeaderV2Signature opens every PROXY protocol v2 header.
var proxyHeaderV2Signature = []byte{
	0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A,
}

// ProxyHeaderV2 renders the binary PROXY protocol v2 header for the
// connection's addresses, or the UNKNOWN form when either address is not an
// IPv4 endpoint. v2 is what a service that parses the binary protocol
// expects; the wire form is the 16-byte signature and command block, the
// family byte, the address length and the addresses themselves.
func ProxyHeaderV2(remote, local net.Addr) []byte {
	src, srcOK := remote.(*net.TCPAddr)
	dst, dstOK := local.(*net.TCPAddr)
	if !srcOK || !dstOK || src.IP.To4() == nil || dst.IP.To4() == nil {
		header := make([]byte, 0, 16)
		header = append(header, proxyHeaderV2Signature...)
		header = append(header, 0x20, 0x00, 0x00, 0x00) // v2, LOCAL, no address
		return header
	}
	header := make([]byte, 0, 28)
	header = append(header, proxyHeaderV2Signature...)
	header = append(header, 0x21, 0x11, 0x00, 0x0C) // v2, PROXY, TCP4, 12 address bytes
	header = append(header, src.IP.To4()...)
	header = append(header, dst.IP.To4()...)
	header = append(header, byte(src.Port>>8), byte(src.Port))
	header = append(header, byte(dst.Port>>8), byte(dst.Port))
	return header
}

// proxyHeaderConn is a connection whose reads yield a fixed header once, then
// the underlying bytes — the form a PROXY protocol header takes on the stream
// a local service receives. Writes pass through untouched.
type proxyHeaderConn struct {
	net.Conn
	header []byte
	off    int
}

func (c *proxyHeaderConn) Read(p []byte) (int, error) {
	if c.off < len(c.header) {
		n := copy(p, c.header[c.off:])
		c.off += n
		return n, nil
	}
	return c.Conn.Read(p)
}
