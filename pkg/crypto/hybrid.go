package crypto

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Hybrid post-quantum key agreement: X25519 together with ML-KEM-768.
//
// The two shared secrets are concatenated and run through HKDF-SHA256, so the
// session key is at least as strong as the stronger of the two mechanisms. A
// future quantum computer that breaks X25519 therefore does not recover the
// session, because it would still have to break ML-KEM-768; and a break of
// ML-KEM-768 alone does not help either, because X25519 is still in the mix.
//
// Wire format of a public key blob ("hybrid public key"):
//
//	x25519 public key : 32 bytes
//	ML-KEM-768 key or ciphertext : MLKEM768Size
//
// The exchanging parties are the client, which offers an X25519 public key and an
// ML-KEM-768 encapsulation key, and the server, which answers with its own X25519
// public key and the ML-KEM-768 ciphertext.
const (
	// X25519Size is the length of an X25519 public key and of the shared secret.
	X25519Size = 32
	// MLKEM768EncapKeySize is the length of an ML-KEM-768 encapsulation key.
	MLKEM768EncapKeySize = mlkem.EncapsulationKeySize768
	// MLKEM768CiphertextSize is the length of an ML-KEM-768 ciphertext.
	MLKEM768CiphertextSize = mlkem.CiphertextSize768
	// SessionKeySize is the length of the derived session key.
	SessionKeySize = 32
)

// HybridPublicKeySize is the length of the client's public key blob.
const HybridPublicKeySize = X25519Size + MLKEM768EncapKeySize

// HybridResponseSize is the length of the server's response blob.
const HybridResponseSize = X25519Size + MLKEM768CiphertextSize

// hybridInfo domain-separates the session key from every other use of HKDF in
// this package.
const hybridInfo = "aethertunnel/v3/hybrid-kex"

// Errors reported by the hybrid key agreement.
var (
	ErrHybridKeySize = errors.New("crypto: hybrid key material has the wrong length")
	ErrHybridEmpty   = errors.New("crypto: hybrid key agreement produced an empty secret")
)

// hybridShared derives the session key from the two shared secrets and the two
// public blobs, so the key is bound to the exact exchange that produced it.
func hybridShared(x25519Secret, mlkemSecret, clientPub, serverPub []byte) ([]byte, error) {
	if len(x25519Secret) != X25519Size {
		return nil, ErrHybridKeySize
	}
	if len(mlkemSecret) != mlkem.SharedKeySize {
		return nil, ErrHybridKeySize
	}

	hash := sha256.New
	clientSum := sha256.Sum256(clientPub)
	serverSum := sha256.Sum256(serverPub)

	salt := make([]byte, 0, sha256.Size*2)
	salt = append(salt, clientSum[:]...)
	salt = append(salt, serverSum[:]...)

	ikm := make([]byte, 0, len(x25519Secret)+len(mlkemSecret))
	ikm = append(ikm, x25519Secret...)
	ikm = append(ikm, mlkemSecret...)

	reader := hkdf.New(hash, ikm, salt, []byte(hybridInfo))
	key := make([]byte, SessionKeySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("crypto: derive the hybrid session key: %w", err)
	}
	return key, nil
}

// HybridClientInit produces the client's public key blob and the secret the
// client keeps. It performs no I/O; the response blob from the server is fed to
// HybridClientFinish.
func HybridClientInit() (publicKey []byte, state []byte, err error) {
	curve := ecdh.X25519()
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: x25519 key: %w", err)
	}
	mlkemKey, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: ml-kem key: %w", err)
	}

	publicKey = make([]byte, 0, HybridPublicKeySize)
	publicKey = append(publicKey, privateKey.PublicKey().Bytes()...)
	publicKey = append(publicKey, mlkemKey.EncapsulationKey().Bytes()...)

	state = make([]byte, 0, X25519Size+len(privateKey.Bytes()))
	state = append(state, privateKey.Bytes()...)
	state = append(state, mlkemKey.Bytes()...)
	return publicKey, state, nil
}

// HybridClientFinish takes the server's response blob and returns the session
// key. clientState is what HybridClientInit returned.
func HybridClientFinish(clientState, serverResponse []byte) ([]byte, error) {
	if len(clientState) != X25519Size+mlkem.SeedSize {
		return nil, ErrHybridKeySize
	}
	if len(serverResponse) != HybridResponseSize {
		return nil, ErrHybridKeySize
	}

	privateKey, err := ecdh.X25519().NewPrivateKey(clientState[:X25519Size])
	if err != nil {
		return nil, fmt.Errorf("crypto: x25519 key: %w", err)
	}
	mlkemKey, err := mlkem.NewDecapsulationKey768(clientState[X25519Size:])
	if err != nil {
		return nil, fmt.Errorf("crypto: ml-kem key: %w", err)
	}

	serverPub := serverResponse[:X25519Size]
	ciphertext := serverResponse[X25519Size:]
	if len(ciphertext) != MLKEM768CiphertextSize {
		return nil, ErrHybridKeySize
	}

	peer, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		return nil, fmt.Errorf("crypto: x25519 peer key: %w", err)
	}
	x25519Secret, err := privateKey.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("crypto: x25519 agreement: %w", err)
	}
	mlkemSecret, err := mlkemKey.Decapsulate(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto: ml-kem decapsulation: %w", err)
	}

	clientPub := make([]byte, 0, HybridPublicKeySize)
	clientPub = append(clientPub, privateKey.PublicKey().Bytes()...)
	clientPub = append(clientPub, mlkemKey.EncapsulationKey().Bytes()...)

	return hybridShared(x25519Secret, mlkemSecret, clientPub, serverResponse)
}

// HybridServerFinish takes the client's public key blob and returns the server's
// response blob together with the session key.
func HybridServerFinish(clientPublicKey []byte) (response []byte, sessionKey []byte, err error) {
	if len(clientPublicKey) != HybridPublicKeySize {
		return nil, nil, ErrHybridKeySize
	}

	curve := ecdh.X25519()
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: x25519 key: %w", err)
	}

	clientX25519, err := curve.NewPublicKey(clientPublicKey[:X25519Size])
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: x25519 client key: %w", err)
	}
	x25519Secret, err := privateKey.ECDH(clientX25519)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: x25519 agreement: %w", err)
	}

	encapsulationKey, err := mlkem.NewEncapsulationKey768(clientPublicKey[X25519Size:])
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: ml-kem client key: %w", err)
	}
	mlkemSecret, ciphertext := encapsulationKey.Encapsulate()

	response = make([]byte, 0, HybridResponseSize)
	response = append(response, privateKey.PublicKey().Bytes()...)
	response = append(response, ciphertext...)

	sessionKey, err = hybridShared(x25519Secret, mlkemSecret, clientPublicKey, response)
	if err != nil {
		return nil, nil, err
	}
	return response, sessionKey, nil
}

// NewCipherFromKey builds a Cipher from an already derived 32-byte session key,
// which is how a post-quantum session is protected once both peers have agreed on
// a key.
func NewCipherFromKey(algorithm string, key []byte) (*Cipher, error) {
	if algorithm == "" || algorithm == AlgorithmNone {
		return nil, nil
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: session key must be %d bytes, got %d", keySize, len(key))
	}
	return newCipherWithKey(algorithm, key)
}

// SessionKeyID renders a short, non-reversible fingerprint of a session key. It
// is used to log and compare which key a session is using without revealing it.
func SessionKeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("aethertunnel/v3/key-id"), key...))
	return fmt.Sprintf("%x", sum[:8])
}

// StreamKey derives the key that protects one data connection from the control
// session's key and the stream identifier both ends already share.
//
// Deriving per stream rather than reusing one key has two effects: two streams of
// the same session never share a keystream, and a data connection opened by a
// client that has not run the session handshake cannot produce a valid key. Both
// ends can compute it without another round trip because the stream identifier
// travels in the clear inside the stream's own handshake.
func StreamKey(sessionKey []byte, streamID string) ([]byte, error) {
	if len(sessionKey) != SessionKeySize {
		return nil, ErrHybridKeySize
	}
	if streamID == "" {
		return nil, errors.New("crypto: a stream identifier is required")
	}

	reader := hkdf.New(sha256.New, sessionKey, []byte(streamID), []byte(streamKeyInfo))
	key := make([]byte, SessionKeySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("crypto: derive the stream key: %w", err)
	}
	return key, nil
}

// streamKeyInfo domain-separates stream keys from the session key itself.
const streamKeyInfo = "aethertunnel/v3/stream-key"
