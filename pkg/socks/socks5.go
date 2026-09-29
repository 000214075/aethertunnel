// Package socks implements the parts of SOCKS5 (RFC 1928) that a tunnel endpoint
// needs: reading a CONNECT or UDP ASSOCIATE request from a visitor, answering it,
// carrying UDP datagrams in the protocol's wrapping, and deciding which targets
// the client behind the tunnel may dial.
//
// BIND is refused with the reply code the protocol defines for it, so a client
// that asks for it gets a clear answer instead of a hang.
package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Protocol constants from RFC 1928.
const (
	Version     = 0x05
	MethodNoReq = 0x00
	MethodNone  = 0xFF

	CmdConnect = 0x01
	CmdBind    = 0x02
	CmdAssoc   = 0x03

	AtypIPv4   = 0x01
	AtypDomain = 0x03
	AtypIPv6   = 0x04

	ReplySucceeded           = 0x00
	ReplyGeneralFailure      = 0x01
	ReplyNotAllowed          = 0x02
	ReplyNetworkUnreachable  = 0x03
	ReplyHostUnreachable     = 0x04
	ReplyConnectionRefused   = 0x05
	ReplyTTLExpired          = 0x06
	ReplyCommandNotSupported = 0x07
	ReplyAddressNotSupported = 0x08
)

// Errors reported while reading a request.
var (
	ErrNotSOCKS5        = errors.New("socks: the client did not offer SOCKS5")
	ErrNoAuthMethod     = errors.New("socks: the client offered no method this server accepts")
	ErrUnsupported      = errors.New("socks: only CONNECT and UDP ASSOCIATE are supported")
	ErrAddressTooLong   = errors.New("socks: the requested domain name is too long")
	ErrUnknownAddress   = errors.New("socks: the request uses an address type this server does not know")
	ErrBadAddress       = errors.New("socks: the requested host cannot be written as host:port")
	ErrPortMissing      = errors.New("socks: the request has no port")
	ErrShortUDPDatagram = errors.New("socks: the UDP datagram is truncated")
	ErrFragment         = errors.New("socks: the UDP datagram asks for fragmentation")
)

// joinHostPort renders a target as host:port, refusing a host that would not
// survive being taken apart again.
//
// A target crosses the tunnel as one string and every consumer reads it with
// net.SplitHostPort: the server compares it against the proxy's allow list, the
// client resolves and dials what it parsed, and the same string comes back the
// other way. A domain name carrying a bracket breaks that — net.JoinHostPort only
// brackets a host containing a colon, so "a[" becomes "a[:80", which
// net.SplitHostPort refuses — and the request would reach the client as a string
// it can only turn down with a complaint about its own parsing. Neither '[' nor
// ']' can appear in a host name, so refusing them here cannot refuse a target a
// visitor meant.
func joinHostPort(host string, port int) (string, error) {
	if strings.ContainsAny(host, "[]") {
		return "", fmt.Errorf("%w: the host %q carries a bracket", ErrBadAddress, host)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// Request is one accepted request.
type Request struct {
	// Command is CmdConnect or CmdAssoc.
	Command byte
	// Target is the address the visitor asked to reach, as host:port. It is set
	// for a CONNECT request; a domain name is kept as a name so the client behind
	// the tunnel resolves it, not the visitor.
	Target string
	// ClientAddr is the address the visitor says it will send UDP datagrams
	// from, as host:port. It is set for a UDP ASSOCIATE request and is commonly
	// 0.0.0.0:0, which means "any address".
	ClientAddr string
}

// ReadRequest performs the method negotiation and reads one request, answering
// the negotiation while it does. The connection is left positioned at the first
// byte the visitor sends after the request.
func ReadRequest(conn net.Conn, timeout time.Duration) (Request, error) {
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return Request{}, fmt.Errorf("socks: read the greeting: %w", err)
	}
	if header[0] != Version {
		_ = writeMethod(conn, MethodNone)
		return Request{}, ErrNotSOCKS5
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return Request{}, fmt.Errorf("socks: read the methods: %w", err)
	}
	accepts := false
	for _, method := range methods {
		if method == MethodNoReq {
			accepts = true
			break
		}
	}
	if !accepts {
		_ = writeMethod(conn, MethodNone)
		return Request{}, ErrNoAuthMethod
	}
	if err := writeMethod(conn, MethodNoReq); err != nil {
		return Request{}, err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return Request{}, fmt.Errorf("socks: read the request: %w", err)
	}
	if request[0] != Version {
		return Request{}, ErrNotSOCKS5
	}
	command := request[1]
	switch command {
	case CmdConnect, CmdAssoc:
	default:
		_ = WriteReply(conn, ReplyCommandNotSupported)
		return Request{}, ErrUnsupported
	}

	host, err := readAddress(conn, request[3])
	if err != nil {
		_ = WriteReply(conn, ReplyAddressNotSupported)
		return Request{}, err
	}

	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return Request{}, fmt.Errorf("socks: read the port: %w", err)
	}
	number := binary.BigEndian.Uint16(port)
	// A CONNECT request names a service to reach, so port 0 has no meaning. A
	// UDP ASSOCIATE request names the address the visitor will send datagrams
	// from, and 0.0.0.0:0 is the standard "any address".
	if command == CmdConnect && number == 0 {
		return Request{}, ErrPortMissing
	}

	parsed := Request{Command: command}
	address, err := joinHostPort(host, int(number))
	if err != nil {
		_ = WriteReply(conn, ReplyAddressNotSupported)
		return Request{}, err
	}
	if command == CmdConnect {
		parsed.Target = address
	} else {
		parsed.ClientAddr = address
	}
	return parsed, nil
}

// WriteReply answers a request. The bound address is reported as 0.0.0.0:0, which
// is what a client that only cares about the reply code expects.
func WriteReply(conn net.Conn, code byte) error {
	return WriteReplyBound(conn, code, net.IPv4zero, 0)
}

// WriteReplyBound answers a request with a reply that names a real bound
// address, which is what a UDP ASSOCIATE reply needs: the visitor has to know
// where to send its datagrams.
func WriteReplyBound(conn net.Conn, code byte, ip net.IP, port int) error {
	reply := []byte{Version, code, 0x00}
	if v4 := ip.To4(); v4 != nil {
		reply = append(reply, AtypIPv4)
		reply = append(reply, v4...)
	} else {
		reply = append(reply, AtypIPv6)
		reply = append(reply, ip.To16()...)
	}
	reply = binary.BigEndian.AppendUint16(reply, uint16(port))
	_, err := conn.Write(reply)
	return err
}

// ReplyFor maps a dial failure onto the closest SOCKS5 reply code, so a visitor
// learns whether the name, the network or the service was the problem.
func ReplyFor(err error) byte {
	if err == nil {
		return ReplySucceeded
	}
	var dns *net.DNSError
	text := err.Error()
	switch {
	case errors.As(err, &dns):
		return ReplyHostUnreachable
	case strings.Contains(text, "refused"):
		return ReplyConnectionRefused
	case strings.Contains(text, "not allowed"):
		return ReplyNotAllowed
	case strings.Contains(text, "no such host"), strings.Contains(text, "no route"),
		strings.Contains(text, "unreachable"), strings.Contains(text, "timeout"),
		strings.Contains(text, "i/o timeout"):
		return ReplyHostUnreachable
	default:
		return ReplyGeneralFailure
	}
}

func writeMethod(conn net.Conn, method byte) error {
	_, err := conn.Write([]byte{Version, method})
	return err
}

// readAddress reads the address from a request, returning it as a host string.
func readAddress(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case AtypIPv4:
		buf := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", fmt.Errorf("socks: read the IPv4 address: %w", err)
		}
		return net.IP(buf).String(), nil
	case AtypIPv6:
		buf := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", fmt.Errorf("socks: read the IPv6 address: %w", err)
		}
		return net.IP(buf).String(), nil
	case AtypDomain:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", fmt.Errorf("socks: read the domain length: %w", err)
		}
		if length[0] == 0 {
			return "", ErrAddressTooLong
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", fmt.Errorf("socks: read the domain name: %w", err)
		}
		return string(name), nil
	default:
		return "", ErrUnknownAddress
	}
}

// WrapUDPDatagram builds one SOCKS5 UDP datagram (RFC 1928 section 7): two
// reserved zero bytes, a zero fragment byte, the address and port the data is
// bound to, and the data. The address is the destination for an outbound
// datagram and the source for a reply.
func WrapUDPDatagram(addr string, data []byte) ([]byte, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks: %q is not host:port: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks: %q has no usable port: %w", addr, err)
	}

	atyp, address, err := encodeAddress(host)
	if err != nil {
		return nil, err
	}

	packet := make([]byte, 0, 4+len(address)+len(data))
	packet = append(packet, 0, 0, 0, atyp) // reserved, reserved, fragment, atyp
	packet = append(packet, address...)
	packet = binary.BigEndian.AppendUint16(packet, uint16(port))
	packet = append(packet, data...)
	return packet, nil
}

// ParseUDPDatagram reads one SOCKS5 UDP datagram back into the address it names
// and the data it carries. It refuses a non-zero reserved field and a non-zero
// fragment field, because this relay does not reassemble fragments.
func ParseUDPDatagram(packet []byte) (string, []byte, error) {
	if len(packet) < 4 {
		return "", nil, ErrShortUDPDatagram
	}
	if packet[0] != 0 || packet[1] != 0 {
		return "", nil, errors.New("socks: the UDP datagram's reserved bytes are not zero")
	}
	if packet[2] != 0 {
		return "", nil, ErrFragment
	}

	host, length, err := decodeAddress(packet[3], packet[4:])
	if err != nil {
		return "", nil, err
	}
	rest := packet[4+length:]
	if len(rest) < 2 {
		return "", nil, ErrShortUDPDatagram
	}
	port := int(binary.BigEndian.Uint16(rest[:2]))
	address, err := joinHostPort(host, port)
	if err != nil {
		return "", nil, err
	}
	return address, rest[2:], nil
}

// encodeAddress turns a host into the SOCKS5 address type and its bytes.
func encodeAddress(host string) (byte, []byte, error) {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return AtypIPv4, v4, nil
		}
		return AtypIPv6, ip.To16(), nil
	}
	if len(host) == 0 || len(host) > 255 {
		return 0, nil, ErrAddressTooLong
	}
	out := make([]byte, 1, 1+len(host))
	out[0] = byte(len(host))
	out = append(out, host...)
	return AtypDomain, out, nil
}

// decodeAddress reads a SOCKS5 address out of buf, returning the host and how
// many bytes it consumed.
func decodeAddress(atyp byte, buf []byte) (string, int, error) {
	switch atyp {
	case AtypIPv4:
		if len(buf) < net.IPv4len {
			return "", 0, ErrShortUDPDatagram
		}
		return net.IP(buf[:net.IPv4len]).String(), net.IPv4len, nil
	case AtypIPv6:
		if len(buf) < net.IPv6len {
			return "", 0, ErrShortUDPDatagram
		}
		return net.IP(buf[:net.IPv6len]).String(), net.IPv6len, nil
	case AtypDomain:
		if len(buf) < 1 {
			return "", 0, ErrShortUDPDatagram
		}
		length := int(buf[0])
		if length == 0 || len(buf) < 1+length {
			return "", 0, ErrShortUDPDatagram
		}
		return string(buf[1 : 1+length]), 1 + length, nil
	default:
		return "", 0, ErrUnknownAddress
	}
}
