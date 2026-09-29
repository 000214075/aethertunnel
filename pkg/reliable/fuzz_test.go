package reliable

import (
	"testing"
)

// FuzzParseSegment covers the datagram parser of the hole-punched transport. The
// HMAC is checked before any field is read, so an unauthenticated peer cannot reach
// the parsing — but the token is handed to a visitor by the rendezvous server, so
// the parser is still fed bytes chosen by the far end, and a length or type that
// makes it panic would take down the process that runs it.
func FuzzParseSegment(f *testing.F) {
	token := []byte("fuzz-reliable-token")

	f.Add([]byte{})
	f.Add(marshalSegment(nil, token, segment{typ: typeHello}))
	f.Add(marshalSegment(nil, token, segment{typ: typeHello, payload: make([]byte, nonceSize)}))
	f.Add(marshalSegment(nil, token, segment{typ: typeData, session: 7, seq: 1, ack: 2, window: 3, payload: []byte("data")}))
	// The same datagram with the wrong token, which must not authenticate.
	f.Add(marshalSegment(nil, []byte("another-token"), segment{typ: typeData, payload: []byte("data")}))
	// An unknown type with a valid MAC, which the type switch has to reject.
	f.Add(marshalSegment(nil, token, segment{typ: 200, payload: []byte("data")}))

	f.Fuzz(func(t *testing.T, b []byte) {
		seg, ok := parseSegment(token, b)
		if !ok {
			return
		}
		// A datagram that parsed carried a type this transport knows, and the payload
		// it reports is inside the buffer it was given. The payload aliases b, so a
		// caller that reads it must not be able to walk off the end.
		switch seg.typ {
		case typeHello, typeHelloAck, typeData, typeAck, typeFin:
		default:
			t.Fatalf("a datagram of unknown type %d parsed", seg.typ)
		}
		if len(seg.payload) > len(b) {
			t.Fatalf("a %d byte datagram reported a %d byte payload", len(b), len(seg.payload))
		}
		// The payload has to be the tail of the datagram: header and MAC are stripped,
		// nothing else.
		if len(seg.payload) != len(b)-headerSize-macSize {
			t.Fatalf("a %d byte datagram reported a %d byte payload, want %d",
				len(b), len(seg.payload), len(b)-headerSize-macSize)
		}
	})
}
