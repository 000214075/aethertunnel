package crypto

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// These targets cover what a peer can put on the wire before it is authenticated:
// the server runs every one of them on bytes a stranger chose, and each runs in the
// goroutine that serves a connection. A panic in any of them ends the process and
// every tunnel it was carrying, so the property they assert is mainly that nothing
// panics, and secondarily that a call which reports success was given input of the
// shape it requires.

// FuzzSchnorrVerify covers the NIZK visitor challenge, which reads an uncompressed
// P-256 point and a scalar straight out of the frame. Point decoding is where a
// hand-rolled parser goes wrong, so it is handed every byte string that has the
// right length and every one that does not.
func FuzzSchnorrVerify(f *testing.F) {
	f.Add([]byte{}, []byte{}, []byte{})

	// A real proof, so the corpus reaches the group arithmetic rather than stopping
	// at the length checks.
	public, err := SchnorrPublicKey([]byte("fuzz-secret"))
	if err != nil {
		f.Fatalf("public key: %v", err)
	}
	proof, err := SchnorrProve([]byte("fuzz-secret"), []byte("fuzz-context"))
	if err != nil {
		f.Fatalf("proof: %v", err)
	}
	f.Add(public, []byte("fuzz-context"), proof)
	// The same proof against the context it was not bound to, which must not verify.
	f.Add(public, []byte("other-context"), proof)
	// The generator and the all-zero point, neither of which is a valid key here.
	f.Add(make([]byte, SchnorrPublicKeySize), []byte("fuzz-context"), proof)

	f.Fuzz(func(t *testing.T, public, context, proof []byte) {
		if err := SchnorrVerify(public, context, proof); err != nil {
			return
		}
		// A proof that verified was of the required shape, which is what lets a
		// caller treat a nil error as "this peer knows the secret".
		if len(public) != SchnorrPublicKeySize {
			t.Fatalf("a %d byte public key verified", len(public))
		}
		if len(proof) != SchnorrProofSize {
			t.Fatalf("a %d byte proof verified", len(proof))
		}
		// The same proof under a different context must not verify: that binding is
		// what stops a captured proof from being replayed against another proxy.
		other := append(append([]byte(nil), context...), 'x')
		if err := SchnorrVerify(public, other, proof); err == nil {
			t.Fatal("a proof verified against a context it was not bound to")
		}
	})
}

// FuzzHybridServerFinish covers the post-quantum key agreement, which parses the
// client's X25519 key and ML-KEM encapsulation key before anything about the peer
// is known. Both halves are expected to reject bad material with an error rather
// than a panic.
func FuzzHybridServerFinish(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, HybridPublicKeySize))

	public, _, err := HybridClientInit()
	if err != nil {
		f.Fatalf("client init: %v", err)
	}
	f.Add(public)
	f.Add(append(append([]byte(nil), public...), 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		// The real shape, so the parser reaches both key decoders instead of stopping
		// at the length check.
		sized := make([]byte, HybridPublicKeySize)
		copy(sized, data)

		for _, input := range [][]byte{data, sized} {
			response, sessionKey, err := HybridServerFinish(input)
			if err != nil {
				continue
			}
			// A successful agreement produced a response of the one size the client
			// can parse, and a session key of the size the ciphers take.
			if len(response) != HybridResponseSize {
				t.Fatalf("the response is %d bytes, want %d", len(response), HybridResponseSize)
			}
			if len(sessionKey) != SessionKeySize {
				t.Fatalf("the session key is %d bytes, want %d", len(sessionKey), SessionKeySize)
			}
		}
	})
}

// FuzzCipherOpen covers the record decryption every frame goes through when
// encryption is on, which is reached with attacker bytes as soon as the connection
// is open.
func FuzzCipherOpen(f *testing.F) {
	cipher, err := NewCipher(AlgorithmXChaCha20Poly1305, "fuzz-passphrase", "fuzz-salt")
	if err != nil {
		f.Fatalf("cipher: %v", err)
	}
	sealed, err := cipher.Seal([]byte("fuzz-plaintext"))
	if err != nil {
		f.Fatalf("seal: %v", err)
	}

	f.Add([]byte{})
	f.Add(sealed)
	f.Add(sealed[:len(sealed)-1])
	// A record with one bit of its ciphertext flipped, which must not authenticate.
	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 0x01
	f.Add(flipped)

	f.Fuzz(func(t *testing.T, record []byte) {
		plaintext, err := cipher.Open(record)
		if err != nil {
			if plaintext != nil {
				t.Fatalf("Open returned %d bytes alongside the error %v", len(plaintext), err)
			}
			return
		}
		// An authenticated record only ever shrinks: Open strips the nonce and the
		// tag, so it cannot report more plaintext than it was given.
		if len(plaintext) > len(record) {
			t.Fatalf("Open turned %d bytes into %d", len(record), len(plaintext))
		}
		// What came out goes back in unchanged.
		again, err := cipher.Seal(plaintext)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		round, err := cipher.Open(again)
		if err != nil {
			t.Fatalf("a record built from opened plaintext did not open: %v", err)
		}
		if string(round) != string(plaintext) {
			t.Fatalf("round trip changed the plaintext: %q became %q", plaintext, round)
		}
	})
}

// FuzzVerifyIdentity covers the identity assertion a visitor may send with its
// handshake: a public key, a nonce, a timestamp and a signature, all chosen by the
// peer.
func FuzzVerifyIdentity(f *testing.F) {
	identity, err := NewIdentity()
	if err != nil {
		f.Fatalf("identity: %v", err)
	}
	nonce, err := Nonce()
	if err != nil {
		f.Fatalf("nonce: %v", err)
	}
	const timestamp = int64(1700000000)

	f.Add(int64(0), []byte{}, []byte{}, []byte{})
	f.Add(timestamp, []byte(identity.PublicKey()), nonce, identity.SignChallenge(nonce, timestamp))
	// The right key with a signature over a different timestamp.
	f.Add(timestamp+1, []byte(identity.PublicKey()), nonce, identity.SignChallenge(nonce, timestamp))

	f.Fuzz(func(t *testing.T, timestamp int64, key, nonce, signature []byte) {
		// The clock is pinned to the assertion's own timestamp, so the freshness
		// window never decides the answer and the signature is what is under test.
		now := time.Unix(timestamp, 0)

		err := VerifyIdentity(IdentityCheckInput{
			PublicKey: identity.PublicKey(),
			Nonce:     nonce,
			Timestamp: timestamp,
			Signature: signature,
			Now:       now,
		})
		if err == nil && len(nonce) != IdentityNonceSize {
			// A verified assertion carried a nonce of the one length the field
			// allows, which is what keeps the replay cache keyed consistently.
			t.Fatalf("a %d byte nonce verified", len(nonce))
		}

		// The caller's key is whatever the peer claimed. A key of the wrong length is
		// refused whatever else is presented, so nothing downstream sees a short one.
		err = VerifyIdentity(IdentityCheckInput{
			PublicKey: ed25519.PublicKey(key),
			Nonce:     nonce,
			Timestamp: timestamp,
			Signature: signature,
			Now:       now,
		})
		if err == nil && len(key) != ed25519.PublicKeySize {
			t.Fatalf("a %d byte public key verified", len(key))
		}
	})
}
