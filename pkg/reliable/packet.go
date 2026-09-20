package reliable

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
)

// Wire format: every datagram is header || payload || MAC, where the MAC is
// HMAC-SHA256 over the preceding bytes keyed by Config.Token. A datagram that
// does not authenticate is dropped before anything else looks at it, so a peer
// without the token cannot be mistaken for the far end and random traffic
// cannot reach the state machine.
//
// Header fields, all big endian:
//
//	offset  0  type     1 byte
//	offset  1  flags    1 byte, reserved
//	offset  2  session  4 bytes, derived from both handshake nonces
//	offset  6  seq      4 bytes, byte offset of the payload in the stream
//	offset 10  ack      4 bytes, next byte offset held in order by the sender
//	offset 14  window   2 bytes, free receive space in windowScale byte units
//
// In a standalone ack the seq field is unused for data, so it carries the
// highest byte offset the sender of the ack has buffered out of order. That is
// what lets the far side tell "nothing to send" apart from "a gap is holding up
// data I already have", and retransmit without waiting for a timeout.
const (
	headerSize = 16
	macSize    = 32
	nonceSize  = 16

	// windowScale is the number of bytes one unit of the window field covers.
	// The field is 16 bits, and a byte count would cap the flow-control window
	// at 64 KiB, which is below what a tunnel wants on a slow path.
	windowScale = 128
	// maxWindow is the largest window value the 16 bit field can carry.
	maxWindow = 65535
)

// macDomain keeps these MACs from being interchangeable with any other use of
// the same token.
var macDomain = []byte("aethertunnel/reliable/v1")

// msgType identifies a datagram.
type msgType uint8

const (
	typeHello    msgType = 1
	typeHelloAck msgType = 2
	typeData     msgType = 3
	typeAck      msgType = 4
	typeFin      msgType = 5
)

func (t msgType) String() string {
	switch t {
	case typeHello:
		return "hello"
	case typeHelloAck:
		return "hello-ack"
	case typeData:
		return "data"
	case typeAck:
		return "ack"
	case typeFin:
		return "fin"
	default:
		return "unknown"
	}
}

// segment is a decoded datagram. payload aliases the receive buffer, so it is
// only valid until the next read.
type segment struct {
	typ     msgType
	session uint32
	seq     uint32
	ack     uint32
	window  uint16
	payload []byte
}

// marshalSegment appends a complete, authenticated datagram to dst.
func marshalSegment(dst, token []byte, seg segment) []byte {
	start := len(dst)
	dst = append(dst, make([]byte, headerSize)...)
	dst[start] = byte(seg.typ)
	dst[start+1] = 0
	binary.BigEndian.PutUint32(dst[start+2:], seg.session)
	binary.BigEndian.PutUint32(dst[start+6:], seg.seq)
	binary.BigEndian.PutUint32(dst[start+10:], seg.ack)
	binary.BigEndian.PutUint16(dst[start+14:], seg.window)
	dst = append(dst, seg.payload...)
	return appendMAC(dst, token, start)
}

// appendMAC appends the HMAC-SHA256 of dst[start:] keyed by token.
func appendMAC(dst, token []byte, start int) []byte {
	m := hmac.New(sha256.New, token)
	m.Write(macDomain)
	m.Write(dst[start:])
	return m.Sum(dst)
}

// parseSegment decodes one datagram, returning false for anything too short, not
// authentic, or of an unknown type. The payload it reports aliases b.
func parseSegment(token, b []byte) (segment, bool) {
	if len(b) < headerSize+macSize {
		return segment{}, false
	}
	body := b[:len(b)-macSize]

	m := hmac.New(sha256.New, token)
	m.Write(macDomain)
	m.Write(body)
	var want [macSize]byte
	sum := m.Sum(want[:0])
	if subtle.ConstantTimeCompare(sum, b[len(b)-macSize:]) != 1 {
		return segment{}, false
	}

	typ := msgType(b[0])
	switch typ {
	case typeHello, typeHelloAck, typeData, typeAck, typeFin:
	default:
		return segment{}, false
	}

	return segment{
		typ:     typ,
		session: binary.BigEndian.Uint32(b[2:]),
		seq:     binary.BigEndian.Uint32(b[6:]),
		ack:     binary.BigEndian.Uint32(b[10:]),
		window:  binary.BigEndian.Uint16(b[14:]),
		payload: body[headerSize:],
	}, true
}

// buildHello builds the datagram that starts a punch. nonce is the sender's.
func buildHello(token, nonce []byte) []byte {
	return marshalSegment(nil, token, segment{typ: typeHello, payload: nonce})
}

// buildHelloAck answers a hello. It carries the sender's own nonce and echoes
// the peer's, so a peer that never received a hello can still derive the
// session from the acknowledgement alone.
func buildHelloAck(token, nonce, peerNonce []byte, session uint32) []byte {
	payload := make([]byte, 0, 2*nonceSize)
	payload = append(payload, nonce...)
	payload = append(payload, peerNonce...)
	return marshalSegment(nil, token, segment{typ: typeHelloAck, session: session, payload: payload})
}

// sessionID mixes both handshake nonces into the identifier carried by every
// later datagram. Mixing is symmetric, so neither peer has to know which of the
// two started the punch.
func sessionID(a, b []byte) uint32 {
	var mixed [nonceSize]byte
	for i := range mixed {
		mixed[i] = a[i] ^ b[i]
	}
	sum := sha256.Sum256(mixed[:])
	return binary.BigEndian.Uint32(sum[:4])
}

// seqDiff reports a-b as a signed value, so comparisons between 32 bit sequence
// numbers stay correct across the wrap-around.
func seqDiff(a, b uint32) int32 { return int32(a - b) }
