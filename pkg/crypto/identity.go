package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Identity is a long-term Ed25519 key that authenticates a client without
// revealing the shared auth token.
//
// The client signs a challenge built from a nonce it generated and the current
// time. The server checks the signature against an allow list, refuses a
// timestamp outside IdentityWindow, and refuses a nonce it has already seen,
// which is what stops a captured handshake from being replayed.
const (
	// IdentityWindow is how far a client's clock may differ from the server's.
	IdentityWindow = 120 * time.Second
	// IdentityNonceSize is the length of the client's nonce.
	IdentityNonceSize = 32
	// IdentitySignatureSize is the length of an Ed25519 signature.
	IdentitySignatureSize = ed25519.SignatureSize
	// identityDomain separates identity signatures from every other signature
	// made with the same key.
	identityDomain = "aethertunnel/v3/identity\x00"
)

// Errors reported while checking an identity.
var (
	ErrIdentityKeySize     = errors.New("crypto: an Ed25519 public key is 32 bytes")
	ErrIdentitySignature   = errors.New("crypto: the identity signature does not verify")
	ErrIdentityStale       = errors.New("crypto: the identity timestamp is outside the accepted window")
	ErrIdentityReplayed    = errors.New("crypto: the identity nonce has already been used")
	ErrIdentityScopeFull   = errors.New("crypto: the identity nonce cache has no room for another nonce from this key")
	ErrIdentityNotAllowed  = errors.New("crypto: the identity is not in the allowed list")
	ErrIdentityNonceLength = errors.New("crypto: the identity nonce has the wrong length")
)

// IdentityChallenge builds the exact message an identity holder signs.
//
// The encoding is domain-separated and length-prefixed so that no other
// structure signed by the same key can be presented as a challenge. The
// challenge deliberately does not name the server: the address a client dials
// and the address a server binds are often different strings for the same
// server, and a mismatch there would turn a correct configuration into an
// authentication failure. Freshness comes from the nonce and the timestamp, and
// the auth token is still required on top, so a handshake captured for one
// server is of no use at another.
func IdentityChallenge(nonce []byte, timestamp int64) []byte {
	out := make([]byte, 0, len(identityDomain)+1+len(nonce)+8)
	out = append(out, identityDomain...)
	out = append(out, byte(len(nonce)))
	out = append(out, nonce...)
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(timestamp))
	out = append(out, stamp[:]...)
	return out
}

// Identity is a client's long-term key.
type Identity struct {
	key ed25519.PrivateKey
}

// NewIdentity generates a fresh identity.
func NewIdentity() (*Identity, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("crypto: generate an identity: %w", err)
	}
	return &Identity{key: key}, nil
}

// IdentityFromSeed rebuilds an identity from a 32-byte seed.
func IdentityFromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("crypto: an identity seed is %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Identity{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// LoadIdentity reads a hex-encoded seed from path, creating the file with a fresh
// identity when it does not exist yet.
func LoadIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		seed, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil {
			return nil, fmt.Errorf("crypto: identity file %s is not hex: %w", path, decodeErr)
		}
		return IdentityFromSeed(seed)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("crypto: read identity %s: %w", path, err)
	}

	identity, err := NewIdentity()
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("crypto: create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(identity.Seed())), 0o600); err != nil {
		return nil, fmt.Errorf("crypto: write identity %s: %w", path, err)
	}
	return identity, nil
}

// Seed returns the private seed, for persistence.
func (i *Identity) Seed() []byte { return i.key.Seed() }

// PublicKey returns the verification key.
func (i *Identity) PublicKey() ed25519.PublicKey {
	return i.key.Public().(ed25519.PublicKey)
}

// PublicKeyHex renders the public key the way the configuration lists it.
func (i *Identity) PublicKeyHex() string { return hex.EncodeToString(i.PublicKey()) }

// Sign signs a challenge.
func (i *Identity) Sign(challenge []byte) []byte { return ed25519.Sign(i.key, challenge) }

// Nonce returns a fresh random nonce.
func Nonce() ([]byte, error) {
	nonce := make([]byte, IdentityNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	return nonce, nil
}

// SignChallenge builds and signs the challenge for a handshake.
func (i *Identity) SignChallenge(nonce []byte, timestamp int64) []byte {
	return i.Sign(IdentityChallenge(nonce, timestamp))
}

// ParseIdentityKey accepts a 32-byte key in hex.
func ParseIdentityKey(text string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("crypto: identity key is not hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, ErrIdentityKeySize
	}
	return ed25519.PublicKey(raw), nil
}

// IdentityCheckInput is one identity assertion to verify.
type IdentityCheckInput struct {
	PublicKey ed25519.PublicKey
	Nonce     []byte
	Timestamp int64
	Signature []byte

	// Allowed, when non-empty, is the set of accepted public keys. A key that is
	// not in it is refused.
	Allowed []ed25519.PublicKey
	// Now is the reference time. Zero selects time.Now.
	Now time.Time
	// Seen, when set, records nonces so a replay is refused.
	Seen *NonceCache
}

// VerifyIdentity checks one identity assertion.
func VerifyIdentity(in IdentityCheckInput) error {
	if len(in.PublicKey) != ed25519.PublicKeySize {
		return ErrIdentityKeySize
	}
	if len(in.Nonce) != IdentityNonceSize {
		return ErrIdentityNonceLength
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	stamp := time.Unix(in.Timestamp, 0)
	if delta := now.Sub(stamp); delta > IdentityWindow || delta < -IdentityWindow {
		return ErrIdentityStale
	}

	challenge := IdentityChallenge(in.Nonce, in.Timestamp)
	if !ed25519.Verify(in.PublicKey, challenge, in.Signature) {
		return ErrIdentitySignature
	}
	if len(in.Allowed) > 0 {
		// Only a holder of the key's private half reaches this test, so the refusal
		// cannot be used to learn which client keys the server authorizes. With the
		// membership test first, a peer that already held the server's auth token
		// could tell a listed key from an unlisted one by the error it was given —
		// the oracle the visitor path avoids by proving the proxy secret before it
		// checks allow_users.
		ok := false
		for _, key := range in.Allowed {
			if key.Equal(in.PublicKey) {
				ok = true
				break
			}
		}
		if !ok {
			return ErrIdentityNotAllowed
		}
	}
	if in.Seen != nil {
		if err := in.Seen.claim(string(in.PublicKey), in.Nonce, now); err != nil {
			return err
		}
	}
	return nil
}

// NonceCache remembers the nonces seen during a window so a captured handshake
// cannot be replayed. Entries expire with the window, and the window's entry
// budget is per claiming key, so the cache stays small no matter how many
// identities share it.
type NonceCache struct {
	mu    sync.Mutex
	seen  map[string]map[string]time.Time
	limit int
	// swept is when the whole cache was last swept. Expired entries are
	// reclaimed per bucket on every claim, but a key that is claimed once never
	// visits its scope again, so its bucket would otherwise live for the life of
	// the process and the bucket count would grow with the number of distinct
	// keys. The sweep is what bounds the buckets, not the per-bucket cleanup.
	swept time.Time
}

// NewNonceCache creates a cache. A limit of zero selects 4096 entries per key.
func NewNonceCache(limit int) *NonceCache {
	if limit <= 0 {
		limit = 4096
	}
	return &NonceCache{seen: make(map[string]map[string]time.Time), limit: limit}
}

// claim records a nonce under the given scope — the claiming client's public
// key — and reports whether it was new. An expired entry is reclaimed, so a
// nonce becomes usable again only after the window has passed — by which time
// the timestamp check has already refused the message. The scope and the entry
// budget keep the refusal a per-identity matter: one client flooding the cache
// with valid assertions would otherwise hold the one shared cache full and
// shut every other identity client out of the server.
//
// The two refusals are distinct errors: a nonce that was already claimed is a
// replay, while an exhausted budget is a client that claims faster than the
// window drains. Reporting the second as the first told an operator to look for
// a replayed handshake when the cause was load.
func (c *NonceCache) claim(scope string, nonce []byte, now time.Time) error {
	key := string(nonce)

	c.mu.Lock()
	defer c.mu.Unlock()

	// At most one sweep per window keeps the cost amortised across claims. A
	// bucket whose newest entry is older than the window holds no nonce that can
	// still be valid, so the whole bucket goes.
	if now.Sub(c.swept) > IdentityWindow {
		for s, b := range c.seen {
			idle := true
			for _, at := range b {
				if now.Sub(at) <= 2*IdentityWindow {
					idle = false
					break
				}
			}
			if idle {
				delete(c.seen, s)
			}
		}
		c.swept = now
	}

	bucket := c.seen[scope]
	for existing, at := range bucket {
		if now.Sub(at) > 2*IdentityWindow {
			delete(bucket, existing)
		}
	}
	if _, ok := bucket[key]; ok {
		return ErrIdentityReplayed
	}
	if c.limit > 0 && len(bucket) >= c.limit {
		// Refusing rather than evicting keeps a flood of valid handshakes from
		// pushing out a nonce an attacker is about to replay.
		return ErrIdentityScopeFull
	}
	if bucket == nil {
		bucket = make(map[string]time.Time)
		c.seen[scope] = bucket
	}
	bucket[key] = now
	return nil
}

// Len reports how many nonces are remembered, across every claiming key.
func (c *NonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, bucket := range c.seen {
		total += len(bucket)
	}
	return total
}

// fingerprint is a short identifier for a public key, used in log lines.
func fingerprint(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}
