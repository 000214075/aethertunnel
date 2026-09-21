// Package socks implements the parts of SOCKS5 (RFC 1928) that a tunnel endpoint
// needs: reading a CONNECT request from a visitor, answering it, and deciding
// which targets the client behind the tunnel may dial.
//
// Only the CONNECT command is implemented. UDP ASSOCIATE and BIND are refused
// with the reply code the protocol defines for them, so a client that asks for
// one gets a clear answer instead of a hang.
package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
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
	ErrNotSOCKS5      = errors.New("socks: the client did not offer SOCKS5")
	ErrNoAuthMethod   = errors.New("socks: the client offered no method this server accepts")
	ErrUnsupported    = errors.New("socks: only the CONNECT command is supported")
	ErrAddressTooLong = errors.New("socks: the requested domain name is too long")
	ErrUnknownAddress = errors.New("socks: the request uses an address type this server does not know")
	ErrPortMissing    = errors.New("socks: the request has no port")
)

// Request is one accepted CONNECT request.
type Request struct {
	// Target is the address the visitor asked for, as host:port. A domain name is
	// kept as a name: the client behind the tunnel resolves it, so the visitor
	// does not have to.
	Target string
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
	if request[1] != CmdConnect {
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
	if binary.BigEndian.Uint16(port) == 0 {
		return Request{}, ErrPortMissing
	}

	return Request{Target: net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port)))}, nil
}

// WriteReply answers a request. The bound address is reported as 0.0.0.0:0, which
// is what a client that only cares about the reply code expects.
func WriteReply(conn net.Conn, code byte) error {
	reply := []byte{Version, code, 0x00, AtypIPv4, 0, 0, 0, 0, 0, 0}
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
