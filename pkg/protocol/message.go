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
//
// Version 4 added the padding flag (bit 1) and message types 16-18, none of
// which a version 3 peer understands: it would read a padded payload's length
// prefix as data and reject the frame.
const ProtocolVersion = 4

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

	TypeError MessageType = 10

	// TypeUDPPacket carries exactly one datagram as its payload. It is only sent
	// on a data connection that was accepted for a udp/sudp proxy, where the
	// frame boundary preserves the datagram boundary.
	TypeUDPPacket MessageType = 11
	// TypeVisitorConnect is the first frame of a visitor connection: it asks the
	// server to pair this connection with the owner of a private proxy.
	TypeVisitorConnect MessageType = 12
	// TypeP2PPrepare asks the owner of a proxy to open a punch socket.
	TypeP2PPrepare MessageType = 13
	// TypeP2PPeer carries the peer's observed address during hole punching. The
	// same payload is returned in the server's reply to a UDP punch datagram.
	TypeP2PPeer MessageType = 14
	// TypeP2PFallback asks the server for the relayed path after a failed punch.
	TypeP2PFallback MessageType = 15
	// TypeVisitorChallenge carries the nonce a visitor must prove knowledge of
	// the proxy secret against, for auth_method = "nizk".
	TypeVisitorChallenge MessageType = 16
	// TypeVisitorProve carries the visitor's proof.
	TypeVisitorProve MessageType = 17
	// TypeVPNPacket carries exactly one IP packet as its payload, in either
	// direction, on a control connection that was given a tunnel address. The frame
	// boundary preserves the packet boundary.
	TypeVPNPacket MessageType = 18
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
	case TypeUDPPacket:
		return "udp-packet"
	case TypeVisitorConnect:
		return "visitor-connect"
	case TypeP2PPrepare:
		return "p2p-prepare"
	case TypeP2PPeer:
		return "p2p-peer"
	case TypeP2PFallback:
		return "p2p-fallback"
	case TypeVisitorChallenge:
		return "visitor-challenge"
	case TypeVisitorProve:
		return "visitor-prove"
	case TypeVPNPacket:
		return "vpn-packet"
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

// Close closes the underlying connection when it implements io.Closer. It is
// used by relays that need to unblock a reader on the other side; the framer must
// not be used afterwards.
func (f *Framer) Close() error {
	if closer, ok := f.conn.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

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
	// KEX carries the client's X25519 public key followed by its ML-KEM-768
	// encapsulation key when [encryption].post_quantum is on.
	KEX []byte `json:"kex,omitempty"`
	// Identity is the client's Ed25519 public key, and IdentityNonce,
	// IdentityTime and IdentitySignature are the assertion it signs. See
	// crypto.IdentityChallenge for the exact message.
	Identity          []byte `json:"identity,omitempty"`
	IdentityNonce     []byte `json:"identity_nonce,omitempty"`
	IdentityTime      int64  `json:"identity_time,omitempty"`
	IdentitySignature []byte `json:"identity_signature,omitempty"`
	// VPN asks the server for an address on its layer-3 tunnel subnet. The server
	// answers with VPNAddress when it can provide one.
	VPN bool `json:"vpn,omitempty"`
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
	// P2PPort is the server's UDP rendezvous port for xtcp hole punching. Zero
	// means the server does not offer hole punching.
	P2PPort int `json:"p2p_port,omitempty"`
	// KEX carries the server's X25519 public key followed by the ML-KEM-768
	// ciphertext when the client offered one.
	KEX []byte `json:"kex,omitempty"`
	// IdentityRequired tells the client that the server refuses sessions without
	// a valid identity assertion, so the error is actionable.
	IdentityRequired bool `json:"identity_required,omitempty"`
	// VPNAddress is the tunnel address the server assigned. Empty means the
	// session carries no layer-3 traffic, either because the client did not ask or
	// because the server has no tunnel configured.
	VPNAddress string `json:"vpn_address,omitempty"`
	// VPNMask is the subnet mask for VPNAddress, as a dotted quad.
	VPNMask string `json:"vpn_mask,omitempty"`
	// VPNMTU is the packet size limit the server's interface accepts.
	VPNMTU int `json:"vpn_mtu,omitempty"`
}

// Tunnel types a client may publish. "tcp" and "udp" take a public port on the
// server; "http" and "https" are reached through the server's shared virtual-host
// listener and are selected by the request's Host header; "stcp", "sudp" and
// "xtcp" are private and reachable only by a visitor that knows the secret.
const (
	ProxyTypeTCP   = "tcp"
	ProxyTypeUDP   = "udp"
	ProxyTypeHTTP  = "http"
	ProxyTypeHTTPS = "https"
	ProxyTypeSTCP  = "stcp"
	ProxyTypeSUDP  = "sudp"
	ProxyTypeXTCP  = "xtcp"
)

// ProxySpec describes a tunnel a client wants to publish.
type ProxySpec struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	LocalAddr  string `json:"local_addr"`
	RemotePort int    `json:"remote_port,omitempty"`
	// Domains selects the Host values that reach an http/https tunnel.
	Domains []string `json:"domains,omitempty"`
	// SecretKey gates a private proxy. It is sent on the control connection,
	// which is encrypted only when [encryption] is enabled on both peers.
	SecretKey string `json:"secret_key,omitempty"`
	// AuthMethod is "secret" or "nizk".
	AuthMethod string `json:"auth_method,omitempty"`
	// Group pools this proxy with every other registration of the same name and
	// group, so the server publishes one endpoint and balances across members.
	Group string `json:"group,omitempty"`
	// Multipath is how many parallel data connections carry one datagram
	// session. Zero or one means a single path.
	Multipath int `json:"multipath,omitempty"`
}

// VisitorConnect is the first frame of a visitor connection. A visitor is a
// second client that wants to reach a private proxy without the traffic passing
// through a public port on the server.
type VisitorConnect struct {
	Proxy string `json:"proxy"`
	// Secret is the proxy's secret key, sent only when the proxy uses
	// auth_method = "secret".
	Secret string `json:"secret,omitempty"`
	// Type is the transport the visitor wants: "tcp", "udp" or "xtcp".
	Type string `json:"type,omitempty"`
	// AuthToken is the server's auth token, so a visitor connection is
	// authenticated exactly like a control connection.
	AuthToken string `json:"auth_token,omitempty"`
	// KEX carries the visitor's hybrid public key when
	// [encryption].post_quantum is on.
	KEX []byte `json:"kex,omitempty"`
	// The identity assertion, identical in form to the one a control connection
	// carries, so a server that requires an identity requires it here too.
	Identity          []byte `json:"identity,omitempty"`
	IdentityNonce     []byte `json:"identity_nonce,omitempty"`
	IdentityTime      int64  `json:"identity_time,omitempty"`
	IdentitySignature []byte `json:"identity_signature,omitempty"`
}

// VisitorChallenge is the server's reply when a proxy uses auth_method = "nizk":
// the visitor must prove knowledge of the secret against this nonce.
type VisitorChallenge struct {
	Proxy string `json:"proxy"`
	Nonce []byte `json:"nonce"`
	// KEX is the server's hybrid response, when the visitor offered one.
	KEX []byte `json:"kex,omitempty"`
}

// VisitorProve is the visitor's answer to a challenge.
type VisitorProve struct {
	Proof []byte `json:"proof"`
}

// VisitorProofContext builds the context a visitor's Schnorr proof is bound to.
// Binding the proxy name stops a proof for one proxy from being replayed against
// another, and the server's nonce stops it from being replayed at all.
func VisitorProofContext(proxy string, nonce []byte) []byte {
	out := make([]byte, 0, len(visitorProofDomain)+1+len(proxy)+1+len(nonce))
	out = append(out, visitorProofDomain...)
	out = append(out, byte(len(proxy)))
	out = append(out, proxy...)
	out = append(out, byte(len(nonce)))
	out = append(out, nonce...)
	return out
}

const visitorProofDomain = "aethertunnel/v3/visitor-proof\x00"

// P2PPrepare asks the owner of a proxy to open a UDP socket for hole punching.
type P2PPrepare struct {
	Proxy string `json:"proxy"`
	Token string `json:"token"`
}

// P2PPeer reports the address the rendezvous server observed for the other end
// of a hole-punch attempt, or, when it carries only a token, the token itself.
type P2PPeer struct {
	Token string `json:"token"`
	Addr  string `json:"addr,omitempty"`
	// KEX is the server's hybrid response, when the visitor offered one.
	KEX []byte `json:"kex,omitempty"`
}

// P2PFallback asks the server for the relayed path after a punch failed.
type P2PFallback struct {
	Proxy string `json:"proxy"`
	Token string `json:"token"`
}

// ProxyStatus is one row of the proxy table reported to the dashboard.
type ProxyStatus struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	LocalAddr  string   `json:"local_addr"`
	RemotePort int      `json:"remote_port,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	ClientID   string   `json:"client_id"`
	// GroupMembers is how many clients share this published name. One means the
	// proxy is served by a single client.
	GroupMembers int   `json:"group_members,omitempty"`
	Active       int64 `json:"active_connections"`
	TotalOpened  int64 `json:"total_connections"`
	BytesIn      int64 `json:"bytes_in"`
	BytesOut     int64 `json:"bytes_out"`
}

// DataRequest tells the client that a public connection is waiting and asks it to
// dial back with a matching DataOpen carrying the same StreamID.
type DataRequest struct {
	Proxy    string `json:"proxy"`
	StreamID string `json:"stream_id"`
	// Visitor marks a stream that was requested by a visitor connection rather
	// than by a visit to a public port.
	Visitor bool `json:"visitor,omitempty"`
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
	// KEX is the server's hybrid response, when the peer offered one. It is
	// present on the first reply of a data or visitor connection.
	KEX []byte `json:"kex,omitempty"`
}

// ErrorPayload carries a human-readable failure.
type ErrorPayload struct {
	Error string `json:"error"`
}
