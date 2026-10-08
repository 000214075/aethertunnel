package crypto

import (
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
)

// Schnorr proof of knowledge over NIST P-256.
//
// A visitor that holds a private proxy's secret key proves that it knows the
// secret, without sending it. The prover publishes Y = g^x for the secret x and
// shows that it knows x; the proof is made non-interactive with the Fiat-Shamir
// transform over a context string that the caller fills with the proxy name and a
// fresh server nonce, so a captured proof cannot be replayed against a different
// proxy or a different handshake.
//
// The proof is a single 97-byte blob:
//
//	R : uncompressed P-256 point (65 bytes)
//	z : scalar, big endian, left padded to 32 bytes
//
// Verification accepts when z*G == R + c*Y, where c is the challenge hash of the
// context, the public key and the commitment.
const (
	// SchnorrPointSize is the size of an uncompressed P-256 point.
	SchnorrPointSize = 65
	// SchnorrScalarSize is the size of a field scalar.
	SchnorrScalarSize = 32
	// SchnorrProofSize is the size of a proof.
	SchnorrProofSize = SchnorrPointSize + SchnorrScalarSize
	// SchnorrPublicKeySize is the size of a public key.
	SchnorrPublicKeySize = SchnorrPointSize
)

// Errors reported while proving or verifying.
var (
	ErrSchnorrPublicKey = errors.New("crypto: the Schnorr public key is not a valid P-256 point")
	ErrSchnorrProofSize = errors.New("crypto: the Schnorr proof has the wrong length")
	ErrSchnorrProof     = errors.New("crypto: the Schnorr proof does not verify")
	ErrSchnorrSecret    = errors.New("crypto: a Schnorr secret is required")
)

// schnorrDomain separates these hashes from every other hash in this package.
const schnorrDomain = "aethertunnel/v3/schnorr\x00"

// SchnorrSecretFromBytes maps an arbitrary secret onto the scalar field. The
// secret never has to be a valid scalar: a hash of it is reduced modulo the group
// order, and zero is replaced by one so that the public key is never the point at
// infinity.
func SchnorrSecretFromBytes(secret []byte) (*big.Int, error) {
	if len(secret) == 0 {
		return nil, ErrSchnorrSecret
	}
	sum := sha256.Sum256(append([]byte("aethertunnel/v3/schnorr-secret\x00"), secret...))
	scalar := new(big.Int).SetBytes(sum[:])
	n := elliptic.P256().Params().N
	scalar.Mod(scalar, n)
	if scalar.Sign() == 0 {
		scalar.SetInt64(1)
	}
	return scalar, nil
}

// SchnorrPublicKey returns g^x for the given secret, in the encoding that
// SchnorrVerify expects.
func SchnorrPublicKey(secret []byte) ([]byte, error) {
	scalar, err := SchnorrSecretFromBytes(secret)
	if err != nil {
		return nil, err
	}
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(scalar.Bytes())
	return elliptic.Marshal(curve, x, y), nil
}

// SchnorrProve returns a proof of knowledge of secret, bound to context.
func SchnorrProve(secret, context []byte) ([]byte, error) {
	scalar, err := SchnorrSecretFromBytes(secret)
	if err != nil {
		return nil, err
	}
	curve := elliptic.P256()
	n := curve.Params().N

	px, py := curve.ScalarBaseMult(scalar.Bytes())
	public := elliptic.Marshal(curve, px, py)

	nonce, err := rand.Int(rand.Reader, n)
	if err != nil {
		return nil, fmt.Errorf("crypto: schnorr nonce: %w", err)
	}
	if nonce.Sign() == 0 {
		nonce.SetInt64(1)
	}
	commitX, commitY := curve.ScalarBaseMult(nonce.Bytes())
	commit := elliptic.Marshal(curve, commitX, commitY)

	challenge := schnorrChallenge(commit, public, context)

	// z = r + c*x mod n
	z := new(big.Int).Mul(challenge, scalar)
	z.Add(z, nonce)
	z.Mod(z, n)

	proof := make([]byte, 0, SchnorrProofSize)
	proof = append(proof, commit...)
	proof = append(proof, leftPad(z.Bytes(), SchnorrScalarSize)...)
	return proof, nil
}

// SchnorrVerify checks a proof against the public key and the context it was
// bound to. It returns nil when the proof is sound.
func SchnorrVerify(public, context, proof []byte) error {
	curve := elliptic.P256()
	n := curve.Params().N

	if len(public) != SchnorrPublicKeySize {
		return ErrSchnorrPublicKey
	}
	if len(proof) != SchnorrProofSize {
		return ErrSchnorrProofSize
	}

	px, py := elliptic.Unmarshal(curve, public)
	if px == nil {
		return ErrSchnorrPublicKey
	}
	commitX, commitY := elliptic.Unmarshal(curve, proof[:SchnorrPointSize])
	if commitX == nil {
		return ErrSchnorrProof
	}
	z := new(big.Int).SetBytes(proof[SchnorrPointSize:])
	if z.Sign() == 0 || z.Cmp(n) >= 0 {
		return ErrSchnorrProof
	}

	challenge := schnorrChallenge(proof[:SchnorrPointSize], public, context)

	// Accept when z*G equals R + c*Y.
	wantX, wantY := curve.ScalarBaseMult(z.Bytes())
	cx, cy := curve.ScalarMult(px, py, challenge.Bytes())
	gotX, gotY := curve.Add(commitX, commitY, cx, cy)
	if gotX == nil || gotY == nil {
		return ErrSchnorrProof
	}

	if subtle.ConstantTimeCompare(elliptic.Marshal(curve, gotX, gotY), elliptic.Marshal(curve, wantX, wantY)) != 1 {
		return ErrSchnorrProof
	}
	return nil
}

// schnorrChallenge is the Fiat-Shamir hash. Every input is length-prefixed so
// that no two different transcripts can hash to the same challenge.
func schnorrChallenge(commit, public, context []byte) *big.Int {
	h := sha256.New()
	h.Write([]byte(schnorrDomain))
	h.Write([]byte{byte(len(commit))})
	h.Write(commit)
	h.Write([]byte{byte(len(public))})
	h.Write(public)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(context)))
	h.Write(length[:])
	h.Write(context)

	challenge := new(big.Int).SetBytes(h.Sum(nil))
	return challenge.Mod(challenge, elliptic.P256().Params().N)
}

// leftPad returns b padded on the left with zero bytes to exactly size bytes.
func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b[len(b)-size:]
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
