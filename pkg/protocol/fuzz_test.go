package protocol

import (
	"bytes"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// fuzzConn feeds a fixed byte slice to a Framer without blocking. net.Pipe would
// block the writer whenever ReadFrame returns before consuming every byte — the
// frame-length limit is exactly such a case — and a fuzz target that leaks a
// blocked goroutine per input runs out of memory long before it finds a bug.
type fuzzConn struct{ r *bytes.Reader }

func (c *fuzzConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error) { return len(p), nil }

// FuzzReadFrame reads frames off a byte string that is what an unauthenticated
// peer can put on the control port: anything at all. The reader must reject what
// it cannot parse and never panic, because everything before the handshake is
// attacker-controlled.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0, 0})
	// A header that claims the whole 1 MiB limit with no payload behind it.
	f.Add([]byte{1, 0, 0x00, 0x10, 0x00, 0x00})
	// A padded frame whose length prefix says more than the frame carries.
	f.Add([]byte{1, 0x02, 0, 0, 0, 4, 0xff, 0xff, 0xff, 0xff})

	sealed, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "fuzz-passphrase", "fuzz-salt")
	if err != nil {
		f.Fatalf("build a cipher: %v", err)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// The same hostile bytes go through the plain reader and through the
		// encrypting, padding one, so the padding and decryption paths see them too.
		for _, framer := range []*Framer{
			NewFramerWithOptions(&fuzzConn{r: bytes.NewReader(data)}, nil, FramerOptions{PadTo: 8}),
			NewFramerWithOptions(&fuzzConn{r: bytes.NewReader(data)}, sealed, FramerOptions{PadTo: 8}),
		} {
			// Bounded: the reader refuses a frame larger than the limit outright, so a
			// few calls drain anything the input could hold.
			for i := 0; i < 4; i++ {
				if _, err := framer.ReadFrame(); err != nil {
					break
				}
			}
		}
	})
}

// FuzzDecodePunchResponse parses a datagram any host can send to the client's
// punch socket while it waits for the rendezvous server's answer.
func FuzzDecodePunchResponse(f *testing.F) {
	f.Add([]byte{})
	f.Add(EncodePunchResponse(testToken, "127.0.0.1:1"))
	f.Add(append(EncodePunchResponse(testToken, "127.0.0.1:1"), 'x'))
	// A length byte that promises more than the datagram carries.
	f.Add([]byte{PunchResponseMagic[0], PunchResponseMagic[1], PunchResponseMagic[2], PunchResponseMagic[3],
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		PunchTokenLen, 'x'})

	f.Fuzz(func(t *testing.T, data []byte) {
		token, peer, ok := DecodePunchResponse(data)
		if !ok {
			return
		}
		// A successful decode is a promise: the token and the address it reports
		// are inside the datagram it was handed.
		if len(token) != PunchTokenLen {
			t.Fatalf("decoded token %q is %d bytes, want %d", token, len(token), PunchTokenLen)
		}
		if len(data) < len(PunchResponseMagic)+PunchTokenLen+1+len(peer) {
			t.Fatalf("decoded a %d byte address out of a %d byte datagram", len(peer), len(data))
		}
	})
}

// FuzzDecodePunchRequest parses a datagram any host can send to the rendezvous
// port, which is the only server-side port with no handshake in front of it.
func FuzzDecodePunchRequest(f *testing.F) {
	f.Add([]byte{})
	f.Add(EncodePunchRequest(PunchRoleVisitor, testToken))
	f.Add(EncodePunchRequest(PunchRoleOwner, testToken))
	f.Add([]byte{PunchRequestMagic[0], PunchRequestMagic[1], PunchRequestMagic[2], PunchRequestMagic[3], 'X'})

	f.Fuzz(func(t *testing.T, data []byte) {
		role, token, ok := DecodePunchRequest(data)
		if !ok {
			return
		}
		if role != PunchRoleVisitor && role != PunchRoleOwner {
			t.Fatalf("decoded role %q, want %q or %q", string(role), PunchRoleVisitor, PunchRoleOwner)
		}
		if len(token) != PunchTokenLen {
			t.Fatalf("decoded token %q is %d bytes, want %d", token, len(token), PunchTokenLen)
		}
		if len(data) != PunchRequestLen {
			t.Fatalf("accepted a %d byte datagram, want exactly %d", len(data), PunchRequestLen)
		}
	})
}
