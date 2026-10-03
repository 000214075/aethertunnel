package net

// A minimal RFC 6455 WebSocket that carries the tunnel's byte stream over
// binary frames. It exists so [transport].protocol = "websocket" can ride the
// same port as every other connection: the server peeks each accepted
// connection for the upgrade request and hands the upgraded ones to the same
// control path, while everything else flows through untouched — frp's
// transport.protocol = "websocket" shape without a new dependency.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// UpgradePath is the request path a client uses to ask for the tunnel over
// websocket; the server recognises the connection by these first bytes.
const UpgradePath = "/~!aethertunnel"

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opBinary       = 0x2
	opContinuation = 0x0
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxMessagePayload bounds one message. The tunnel's own framing keeps its
// frames far under this; the cap only stops a malformed peer from making the
// reader allocate without limit.
const maxMessagePayload = 8 << 20

var errNotWebsocket = errors.New("not a websocket upgrade request")

// WebsocketDial performs the client half of the upgrade on a connection that
// is already established — and already inside TLS when the transport is TLS —
// and returns the connection carrying binary frames. The host header names the
// server the way the dialer reached it.
func WebsocketDial(conn net.Conn, host string, timeout time.Duration) (net.Conn, error) {
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket: key: %w", err)
	}
	request := "GET " + UpgradePath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket: send the request: %w", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket: read the response: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket: the server answered %s", response.Status)
	}
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		_ = conn.Close()
		return nil, errors.New("websocket: the answer is not an upgrade")
	}
	accept := response.Header.Get("Sec-WebSocket-Accept")
	want := websocketAcceptKey(base64.StdEncoding.EncodeToString(key))
	if accept != want {
		_ = conn.Close()
		return nil, errors.New("websocket: the accept key does not match the request")
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Time{})
	}
	// The bytes after the 101 come through the reader itself: the response
	// body Go returns for a 101 is an empty stub, so it must not be the
	// connection's read path.
	return &wsConn{Conn: conn, reader: reader, client: true}, nil
}

// AcceptOrPass is the server half of the same-port arrangement: it peeks the
// connection's first bytes, and when they ask for the tunnel's upgrade path it
// completes the handshake and returns the upgraded connection. Every other
// connection comes back with a replay of the peeked bytes in front of it, so
// the caller's read path is unchanged. The timeout bounds how long a silent
// connection may hold the peek open; the error return is only for a peer that
// asked for the upgrade and then failed it.
func AcceptOrPass(conn net.Conn, timeout time.Duration) (net.Conn, error) {
	buffered := bufio.NewReader(conn)
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	// Four bytes decide: the upgrade request is the only stream that opens
	// with "GET " (the tunnel's own frames start with a type byte), and
	// deciding that early keeps a short first frame — the kind a port scan or
	// a refused probe sends — on the same fast refusal path it had before the
	// sniffing existed, instead of waiting for a full request line.
	head, err := buffered.Peek(len("GET "))
	if err != nil || string(head) != "GET " {
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Time{})
		}
		return &prefixedConn{Conn: conn, reader: buffered}, nil
	}
	rest, err := buffered.Peek(len("GET " + UpgradePath))
	if err != nil || string(rest) != "GET "+UpgradePath {
		// The rest of the request never arrived, or arrived wrong; the
		// framer's own refusal is the honest answer for both.
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Time{})
		}
		return &prefixedConn{Conn: conn, reader: buffered}, nil
	}

	request, err := http.ReadRequest(buffered)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket: read the request: %w", err)
	}
	if !isTunnelUpgrade(request) {
		answer := "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
		_, _ = io.WriteString(conn, answer)
		_ = conn.Close()
		return nil, errNotWebsocket
	}
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + websocketAcceptKey(request.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := io.WriteString(conn, response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Time{})
	}
	return &wsConn{Conn: conn, reader: buffered, client: false}, nil
}

// isTunnelUpgrade checks the two headers that say this HTTP request really is
// the tunnel's websocket handshake and not a stray GET.
func isTunnelUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != UpgradePath {
		return false
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	if r.Header.Get("Sec-WebSocket-Key") == "" {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// websocketAcceptKey computes the Sec-WebSocket-Accept answer for a key, the
// one check that ties the response to this request.
func websocketAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// prefixedConn replays what a peek consumed before the connection itself, so
// the raw path sees its stream exactly as it was written.
type prefixedConn struct {
	net.Conn
	reader io.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// CloseWrite forwards the half-close the underlying connection may support.
// It has to be explicit: net.Conn does not carry CloseWrite, so a wrapper that
// hides a *net.TCPConn behind it would otherwise turn every half-close into a
// full close and cut off the reply a service only sends after it has seen the
// end of the request.
func (c *prefixedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

// wsConn speaks one RFC 6455 connection in binary frames. Client-side writes
// are masked as the protocol demands; server-side writes are not. Reads
// assemble continuation frames, answer pings and treat a close as EOF.
type wsConn struct {
	net.Conn
	reader io.Reader
	client bool

	pending    []byte
	assembling []byte
}

func (c *wsConn) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	for {
		payload, opcode, fin, err := c.readFrame()
		if err != nil {
			return 0, err
		}
		switch opcode {
		case opBinary, opContinuation:
			if opcode == opBinary {
				if fin {
					return deliver(p, payload, &c.pending)
				}
				c.assembling = append(c.assembling[:0], payload...)
				continue
			}
			// A continuation frame continues the message the first binary
			// frame started; one with nothing to continue is a protocol
			// error.
			if c.assembling == nil {
				return 0, errors.New("websocket: continuation frame with nothing to continue")
			}
			c.assembling = append(c.assembling, payload...)
			if len(c.assembling) > maxMessagePayload {
				return 0, errors.New("websocket: message exceeds the payload limit")
			}
			if !fin {
				continue
			}
			message := c.assembling
			c.assembling = nil
			return deliver(p, message, &c.pending)
		case opPing:
			_ = c.writeFrame(opPong, payload)
		case opPong:
			// A keepalive answer; nothing to do.
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("websocket: unexpected opcode 0x%x", opcode)
		}
	}
}

// deliver hands a finished message to the caller, stashing what does not fit
// for the next Read.
func deliver(p []byte, message []byte, pending *[]byte) (int, error) {
	if len(p) < len(message) {
		*pending = append((*pending)[:0], message[len(p):]...)
		message = message[:len(p)]
	}
	n := copy(p, message)
	return n, nil
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 0xFFFF:
		header = header[:2]
		header[1] = 126
		header = binary.BigEndian.AppendUint16(header, uint16(length))
	default:
		header = header[:2]
		header[1] = 127
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}
	if c.client {
		header[1] |= 0x80
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		header = append(header, key[:]...)
		masked := make([]byte, length)
		for i, b := range payload {
			masked[i] = b ^ key[i%4]
		}
		if _, err := c.Conn.Write(header); err != nil {
			return err
		}
		_, err := c.Conn.Write(masked)
		return err
	}
	if _, err := c.Conn.Write(header); err != nil {
		return err
	}
	_, err := c.Conn.Write(payload)
	return err
}

func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.writeFrame(opBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite half-closes the underlying connection when it can. The websocket
// protocol has no half-close of its own; the forwarding only makes sure a
// wrapper chain that ends in a TCP connection keeps its half-close semantics
// instead of falling back to a full close.
func (c *wsConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

func (c *wsConn) Close() error {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.writeFrame(opClose, nil)
	return c.Conn.Close()
}

// readFrame reads one frame's header and payload. It reports whether the FIN
// bit says the message is complete, and enforces the direction rule on
// masking: the client masks, the server does not — a peer that breaks the rule
// gets an error rather than a guess about who it is.
func (c *wsConn) readFrame() ([]byte, byte, bool, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		return nil, 0, false, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			return nil, 0, false, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			return nil, 0, false, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	if length > maxMessagePayload {
		return nil, 0, false, errors.New("websocket: frame exceeds the payload limit")
	}
	// The protocol demands masked frames from the client and unmasked frames
	// from the server; a peer that breaks the rule is answered with an error
	// rather than a guess about who it is.
	if masked == c.client {
		return nil, 0, false, errors.New("websocket: frame masking does not match the peer's role")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return nil, 0, false, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return nil, 0, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, fin, nil
}
