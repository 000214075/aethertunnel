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
