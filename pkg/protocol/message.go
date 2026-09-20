// Package protocol defines the wire format shared by the client and the server.
//
// Frame layout (all integers big endian):
//
//	+--------+--------+------------------+-------------------+
//	| type:1 | flags:1| length:4         | payload:length    |
//	+--------+--------+------------------+-------------------+
//
// length may be zero (heartbeats carry no payload) and is validated against a
// configurable maximum before anything is allocated, so a peer cannot make the
// process allocate arbitrary memory by lying in the header.
//
// When a Cipher is supplied, payloads are sealed and flagEncrypted is set. The
// length field then counts the sealed bytes. Encryption is negotiated out of
// band: both sides must be configured identically, which is why the auth response
// reports the server's encryption setting so a mismatch produces a clear error
// instead of a stream of authentication failures.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// ProtocolVersion is bumped whenever the frame layout or the message set changes
// in a way that breaks older peers.
const ProtocolVersion = 3

// MessageType identifies a frame.
type MessageType uint8

const (
	TypeAuthRequest   MessageType = 1 // client -> server, JSON AuthRequest
	TypeAuthResponse  MessageType = 2 // server -> client, JSON AuthResponse
	TypeHeartbeat     MessageType = 3 // client -> server, empty
	TypeHeartbeatAck  MessageType = 4 // server -> client, empty
	TypeRegisterProxy MessageType = 5 // client -> server, JSON ProxySpec
	TypeProxyList     MessageType = 6 // server -> client, JSON []ProxyStatus
	TypeDataRequest   MessageType = 7 // server -> client, JSON DataRequest: please open a stream
	TypeDataOpen      MessageType = 8 // client -> server, JSON DataOpen, then raw bytes
	TypeDataOpenAck   MessageType = 9 // server -> client, JSON DataOpenAck, then raw bytes
	TypeError         MessageType = 10
)

// String makes logs readable.
func (t MessageType) String() string {
	switch t {
	case TypeAuthRequest:
		return "auth-request"
	case TypeAuthResponse:
		return "auth-response"
	case TypeHeartbeat:
		return "heartbeat"
	case TypeHeartbeatAck:
		return "heartbeat-ack"
	case TypeRegisterProxy:
		return "register-proxy"
	case TypeProxyList:
		return "proxy-list"
	case TypeDataRequest:
		return "data-request"
	case TypeDataOpen:
		return "data-open"
	case TypeDataOpenAck:
		return "data-open-ack"
	case TypeError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

const (
	flagEncrypted uint8 = 1 << 0
	flagPadded    uint8 = 1 << 1
)

// DefaultMaxPayload bounds a single frame. Data is streamed after a DataOpen
// handshake rather than sent as frames, so this only needs to fit control
// messages.
const DefaultMaxPayload = 1 << 20 // 1 MiB

// ErrPayloadTooLarge is returned when a peer announces an oversized frame.
var ErrPayloadTooLarge = errors.New("protocol: announced payload exceeds the configured maximum")

// Message is a decoded frame.
type Message struct {
	Type      MessageType
	Encrypted bool
	Payload   []byte
}

// FramerOptions configures frame-level encryption and length padding.
type FramerOptions struct {
	// MaxPayload bounds a single frame payload. Zero selects DefaultMaxPayload.
	MaxPayload uint32
	// PadTo rounds the on-wire payload up to a multiple of this many bytes, so a
	// passive observer learns less from the frame length. Zero disables padding.
	// The receiver unpads from the flag alone, so the two ends do not have to
	// agree on this value.
	PadTo int
	// Jitter adds a random delay in [0, Jitter) before each write.
	Jitter time.Duration
}

// Framer reads and writes frames on a single connection.
//
// WriteFrame is safe for concurrent use (the client sends heartbeats while
// forwarding data); ReadFrame must be called from one goroutine.
type Framer struct {
	conn       io.ReadWriter
	cipher     *crypto.Cipher
	maxPayload uint32
	padTo      int
	jitter     time.Duration

	writeMu sync.Mutex
	header  [6]byte
}

// NewFramer wraps conn. cipher may be nil to disable encryption.
func NewFramer(conn io.ReadWriter, cipher *crypto.Cipher, maxPayload uint32) *Framer {
	return NewFramerWithOptions(conn, cipher, FramerOptions{MaxPayload: maxPayload})
}

// NewFramerWithOptions wraps conn with encryption, padding and jitter.
func NewFramerWithOptions(conn io.ReadWriter, cipher *crypto.Cipher, opts FramerOptions) *Framer {
	if opts.MaxPayload == 0 {
		opts.MaxPayload = DefaultMaxPayload
	}
	if opts.PadTo < 0 {
		opts.PadTo = 0
	}
	return &Framer{
		conn:       conn,
		cipher:     cipher,
		maxPayload: opts.MaxPayload,
		padTo:      opts.PadTo,
		jitter:     opts.Jitter,
	}
}

// MaxPayload returns the configured frame limit.
func (f *Framer) MaxPayload() uint32 { return f.maxPayload }

// WriteFrame encodes and sends one frame with a single Write call, so two frames
// written concurrently cannot interleave on the wire.
func (f *Framer) WriteFrame(msg *Message) error {
	payload := msg.Payload
	var flags uint8

	if f.cipher.Enabled() {
		sealed, err := f.cipher.Seal(payload)
		if err != nil {
			return err
		}
		payload = sealed
		flags |= flagEncrypted
	}

	if f.padTo > 0 && len(payload) > 0 {
		padded, err := pad(payload, f.padTo)
		if err != nil {
			return err
		}
		payload = padded
		flags |= flagPadded
	}

	if uint32(len(payload)) > f.maxPayload {
		return fmt.Errorf("protocol: refusing to send %d byte frame (limit %d)", len(payload), f.maxPayload)
	}

	header := make([]byte, 6, 6+len(payload))
	header[0] = byte(msg.Type)
	header[1] = flags
	binary.BigEndian.PutUint32(header[2:], uint32(len(payload)))
	frame := append(header, payload...)

	f.writeMu.Lock()
	defer f.writeMu.Unlock()

	if f.jitter > 0 {
		time.Sleep(time.Duration(rand.Int63n(int64(f.jitter))))
	}

	n, err := f.conn.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// pad prefixes the real length and fills up to a multiple of padTo.
//
// The prefix is inside the padded region, so the frame length carries no
// information about the payload size beyond which bucket it fell into.
func pad(payload []byte, padTo int) ([]byte, error) {
	body := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(body, uint32(len(payload)))
	copy(body[4:], payload)

	remainder := len(body) % padTo
	if remainder == 0 {
		return body, nil
	}
	return append(body, make([]byte, padTo-remainder)...), nil
}

// unpad reverses pad.
func unpad(payload []byte) ([]byte, error) {
	if len(payload) < 4 {
		return nil, errors.New("protocol: padded frame is too short to contain a length prefix")
	}
	length := binary.BigEndian.Uint32(payload[:4])
	if int(length) > len(payload)-4 {
		return nil, fmt.Errorf("protocol: padded frame declares %d bytes but carries %d",
			length, len(payload)-4)
	}
	return payload[4 : 4+length], nil
}

// WriteJSON marshals v and sends it as a frame of the given type.
func (f *Framer) WriteJSON(msgType MessageType, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("protocol: marshal %s: %w", msgType, err)
	}
	return f.WriteFrame(&Message{Type: msgType, Payload: payload})
}

// ReadFrame reads one frame, decrypting and size-checking it.
func (f *Framer) ReadFrame() (*Message, error) {
	if _, err := io.ReadFull(f.conn, f.header[:]); err != nil {
		return nil, err
	}
	msgType := MessageType(f.header[0])
	flags := f.header[1]
	length := binary.BigEndian.Uint32(f.header[2:])

	if length > f.maxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, length)
	}

	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(f.conn, payload); err != nil {
			return nil, err
		}
	}

	encrypted := flags&flagEncrypted != 0
	if encrypted != f.cipher.Enabled() {
		return nil, fmt.Errorf(
			"protocol: encryption mismatch: frame says encrypted=%v but this side has encryption=%v; "+
				"both peers must use the same [encryption] configuration",
			encrypted, f.cipher.Enabled())
	}
	// The sender seals first and pads second, so the receiver must strip the
	// padding before it can authenticate the ciphertext.
	if flags&flagPadded != 0 {
		unpadded, err := unpad(payload)
		if err != nil {
			return nil, err
		}
		payload = unpadded
	}
	if encrypted {
		plaintext, err := f.cipher.Open(payload)
		if err != nil {
			return nil, err
		}
		payload = plaintext
	}

	return &Message{Type: msgType, Encrypted: encrypted, Payload: payload}, nil
}

// ReadJSON reads a frame of the expected type and decodes its payload into v.
func (f *Framer) ReadJSON(expected MessageType, v any) error {
	msg, err := f.ReadFrame()
	if err != nil {
		return err
	}
	if msg.Type != expected {
		return fmt.Errorf("protocol: expected %s, got %s", expected, msg.Type)
	}
	if len(msg.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(msg.Payload, v); err != nil {
		return fmt.Errorf("protocol: decode %s: %w", expected, err)
	}
	return nil
}

// --- payload types ------------------------------------------------------------

// AuthRequest is sent by the client to open a session.
type AuthRequest struct {
	Token          string `json:"token"`
	ClientVersion  string `json:"client_version"`
	Protocol       int    `json:"protocol"`
	Encryption     string `json:"encryption"`
	EncryptionSalt string `json:"encryption_salt,omitempty"`
}

// AuthResponse reports whether the session was accepted.
type AuthResponse struct {
	OK               bool   `json:"ok"`
	Session          string `json:"session,omitempty"`
	ServerVersion    string `json:"server_version"`
	Protocol         int    `json:"protocol"`
	Encryption       string `json:"encryption"`
	Error            string `json:"error,omitempty"`
	HeartbeatSecs    int    `json:"heartbeat_seconds,omitempty"`
	ProtocolMismatch bool   `json:"protocol_mismatch,omitempty"`
}

// ProxySpec describes a tunnel a client wants to publish.
type ProxySpec struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	LocalAddr  string `json:"local_addr"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// ProxyStatus is one row of the proxy table reported to the dashboard.
type ProxyStatus struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	LocalAddr   string `json:"local_addr"`
	RemotePort  int    `json:"remote_port,omitempty"`
	ClientID    string `json:"client_id"`
	Active      int64  `json:"active_connections"`
	TotalOpened int64  `json:"total_connections"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
}

// DataRequest tells the client that a public connection is waiting and asks it to
// dial back with a matching DataOpen carrying the same StreamID.
type DataRequest struct {
	Proxy    string `json:"proxy"`
	StreamID string `json:"stream_id"`
}

// DataOpen asks the server to open a new data stream for a proxy. After the ack
// both sides speak raw bytes (optionally wrapped by the cipher's record layer).
type DataOpen struct {
	Session  string `json:"session"`
	Proxy    string `json:"proxy"`
	StreamID string `json:"stream_id"`
}

// DataOpenAck accepts or rejects a data stream.
type DataOpenAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ErrorPayload carries a human-readable failure.
type ErrorPayload struct {
	Error string `json:"error"`
}
