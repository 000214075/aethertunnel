package socks

// ServeConn runs the server half of one SOCKS5 conversation on a connection —
// the form the socks5 plugin needs: the visitor speaks SOCKS5 to the public
// port, the bytes cross the tunnel untouched, and this end answers the
// handshake, dials the target and relays. It differs from the socks5 proxy
// type's server side in two ways: it can require username/password
// authentication (RFC 1929, the plugin_user/plugin_password pair), and it
// refuses UDP ASSOCIATE because the tunneled form has no UDP relay to offer.

import (
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"time"
)

const methodUserPass = 0x02

// ServerOptions configures one ServeConn conversation.
type ServerOptions struct {
	// Username and Password, both non-empty, require RFC 1929
	// username/password authentication; both empty answer any visitor without
	// credentials. The comparison is constant-time.
	Username string
	Password string
	// Dial opens the CONNECT target. It is called only after the request
	// passed the command and address checks, so the caller can put its target
	// policy here.
	Dial func(target string) (net.Conn, error)
	// Handshake bounds the greeting, the authentication and the request
	// together; zero means no bound.
	Handshake time.Duration
}

// ServeConn runs greeting, authentication and request, dials the CONNECT
// target through opts.Dial, answers the request and returns the target
// connection for the caller to relay. On any refusal the reply carries the
// matching error code and the connection is left closed. A UDP ASSOCIATE or
// BIND request is refused with ReplyCommandNotSupported.
func ServeConn(conn net.Conn, opts ServerOptions) (net.Conn, error) {
	if opts.Handshake > 0 {
		_ = conn.SetDeadline(time.Now().Add(opts.Handshake))
	}
	if err := serveGreeting(conn, opts); err != nil {
		_ = conn.Close()
		return nil, err
	}
	request, err := readCommand(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if opts.Handshake > 0 {
		_ = conn.SetDeadline(time.Time{})
	}
	target, err := opts.Dial(request.Target)
	if err != nil {
		_ = WriteReply(conn, ReplyFor(err))
		_ = conn.Close()
		return nil, err
	}
	if err := WriteReply(conn, ReplySucceeded); err != nil {
		_ = target.Close()
		_ = conn.Close()
		return nil, err
	}
	return target, nil
}

// serveGreeting negotiates the method and, when the options demand it, the
// RFC 1929 username/password exchange. A visitor that offers only methods the
// options do not accept is refused with 0xFF, the no-acceptable-methods
// answer.
func serveGreeting(conn net.Conn, opts ServerOptions) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return errors.New("socks: read the greeting: " + err.Error())
	}
	if header[0] != Version {
		return ErrNotSOCKS5
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return errors.New("socks: read the methods: " + err.Error())
	}
	var chosen byte
	switch {
	case opts.Username != "":
		chosen = methodUserPass
	case len(methods) > 0:
		chosen = MethodNoReq
	default:
		chosen = MethodNone
	}
	offered := false
	for _, method := range methods {
		if method == chosen {
			offered = true
			break
		}
	}
	if !offered {
		_ = writeMethod(conn, MethodNone)
		return ErrNoAuthMethod
	}
	if err := writeMethod(conn, chosen); err != nil {
		return err
	}
	if chosen == methodUserPass {
		return readCredentials(conn, opts)
	}
	return nil
}

// readCredentials checks the RFC 1929 username/password subnegotiation: a
// version byte, the name, and the password. A failed check answers 0x01 and
// refuses the visitor without revealing whether the name or the password was
// wrong.
func readCredentials(conn net.Conn, opts ServerOptions) error {
	ver := make([]byte, 1)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return errors.New("socks: read the auth version: " + err.Error())
	}
	if ver[0] != 1 {
		return errors.New("socks: the auth subnegotiation is not RFC 1929")
	}
	ulen := make([]byte, 1)
	if _, err := io.ReadFull(conn, ulen); err != nil {
		return errors.New("socks: read the username length: " + err.Error())
	}
	username := make([]byte, ulen[0])
	if _, err := io.ReadFull(conn, username); err != nil {
		return errors.New("socks: read the username: " + err.Error())
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return errors.New("socks: read the password length: " + err.Error())
	}
	password := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, password); err != nil {
		return errors.New("socks: read the password: " + err.Error())
	}
	nameOK := subtle.ConstantTimeCompare(username, []byte(opts.Username)) == 1
	passOK := subtle.ConstantTimeCompare(password, []byte(opts.Password)) == 1
	if !nameOK || !passOK {
		_, _ = conn.Write([]byte{1, 0x01})
		return ErrNoAuthMethod
	}
	_, err := conn.Write([]byte{1, 0x00})
	return err
}

// drainRequestTail reads and discards the address and port that follow the
// request header, per the address type it names. Best effort: a caller that
// sent a truncated request gets the refusal whenever the bytes stop coming.
func drainRequestTail(conn net.Conn, atyp byte) {
	var fixed int
	switch atyp {
	case AtypIPv4:
		fixed = 4
	case AtypIPv6:
		fixed = 16
	case AtypDomain:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		fixed = int(length[0])
	default:
		return
	}
	tail := make([]byte, fixed+2)
	_, _ = io.ReadFull(conn, tail)
}

// readCommand reads one request and accepts CONNECT only: the tunneled form
// has no UDP relay to offer and no listener for BIND to announce.
func readCommand(conn net.Conn) (Request, error) {
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return Request{}, errors.New("socks: read the request: " + err.Error())
	}
	if request[0] != Version {
		return Request{}, ErrNotSOCKS5
	}
	if request[1] != CmdConnect {
		// The refusal goes out only after the rest of the request is drained:
		// closing with unread bytes queued makes Windows reset the connection,
		// and the visitor would never see the command-not-supported answer
		// Linux delivers fine.
		drainRequestTail(conn, request[3])
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
		return Request{}, errors.New("socks: read the port: " + err.Error())
	}
	number := int(port[0])<<8 | int(port[1])
	if number == 0 {
		return Request{}, ErrPortMissing
	}
	parsed, err := joinHostPort(host, number)
	if err != nil {
		_ = WriteReply(conn, ReplyAddressNotSupported)
		return Request{}, err
	}
	return Request{Command: CmdConnect, Target: parsed}, nil
}
