// Package ledger keeps a tamper-evident, append-only record of the bandwidth
// each client used on the tunnel server.
//
// Entries form a hash chain: every entry commits to the previous entry's hash,
// and every entry is signed with Ed25519. A third party holding only the public
// key can verify that a published usage report was not altered. Detecting a
// truncated report additionally needs the head hash of the full chain, which is
// published out of band and compared against the head of the report (see Head).
package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Domain separators keep a ledger hash and a ledger signature from being
// confused with any other use of SHA-256 or of the same Ed25519 key.
const (
	hashDomain      = "aethertunnel/ledger/v1/entry-hash\x00"
	signatureDomain = "aethertunnel/ledger/v1/entry-signature\x00"
)

// Entry is one signed usage record.
type Entry struct {
	Index     uint64    `json:"index"`
	Time      time.Time `json:"time"`
	ClientID  string    `json:"client_id"`
	Proxy     string    `json:"proxy"`
	BytesIn   int64     `json:"bytes_in"`  // received from the client
	BytesOut  int64     `json:"bytes_out"` // sent to the client
	PrevHash  string    `json:"prev_hash"` // hex, empty for the first entry
	Hash      string    `json:"hash"`      // hex
	Signature string    `json:"signature"` // hex, Ed25519 over the signing payload
}

// Totals aggregates one client's usage.
type Totals struct {
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
	Entries  int   `json:"entries"`
}

// SigningKey is the Ed25519 key an operator holds.
//
// A key is immutable once built, so every method is safe for concurrent use and
// a single key can back several ledgers.
type SigningKey struct {
	priv ed25519.PrivateKey
}

// NewSigningKey generates a new key.
func NewSigningKey() (*SigningKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ledger: generate key: %w", err)
	}
	return &SigningKey{priv: priv}, nil
}

// SigningKeyFromSeed rebuilds a key from a 32-byte seed.
func SigningKeyFromSeed(seed []byte) (*SigningKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ledger: seed has length %d, want %d", len(seed), ed25519.SeedSize)
	}
	return &SigningKey{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicKey returns the verification key.
func (k *SigningKey) PublicKey() ed25519.PublicKey {
	if k == nil || len(k.priv) != ed25519.PrivateKeySize {
		return nil
	}
	pub, _ := k.priv.Public().(ed25519.PublicKey)
	if len(pub) != ed25519.PublicKeySize {
		return nil
	}
	out := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(out, pub)
	return out
}

// Seed returns the private seed (for persistence). The caller owns the copy.
func (k *SigningKey) Seed() []byte {
	if k == nil || len(k.priv) != ed25519.PrivateKeySize {
		return nil
	}
	return k.priv.Seed()
}

// sign returns the Ed25519 signature of payload, or nil without a usable key.
func (k *SigningKey) sign(payload []byte) []byte {
	if k == nil || len(k.priv) != ed25519.PrivateKeySize {
		return nil
	}
	return ed25519.Sign(k.priv, payload)
}

// Ledger is an append-only chain of entries.
type Ledger struct {
	mu      sync.Mutex
	key     *SigningKey
	pub     ed25519.PublicKey
	path    string
	file    *os.File // nil when the ledger is memory only
	entries []Entry
	closed  bool
}

// New creates a ledger. path is an optional JSONL file that entries are
// appended to; an empty path keeps the ledger in memory only.
//
// When the file already exists its entries are loaded and verified against the
// key, so a restart continues the chain from the stored head and index. A
// corrupt file (unreadable line, broken link, or bad signature) is an error and
// nothing is appended afterwards.
func New(path string, key *SigningKey) (*Ledger, error) {
	if key == nil {
		return nil, errors.New("ledger: a signing key is required")
	}
	pub := key.PublicKey()
	if pub == nil {
		return nil, errors.New("ledger: signing key is not usable")
	}

	l := &Ledger{key: key, pub: pub, path: path}
	if path == "" {
		return l, nil
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := l.load(data); err != nil {
			return nil, err
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("ledger: read %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("ledger: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	l.file = file
	return l, nil
}

// Append signs and appends one record, returning the stored entry.
//
// The record is written and flushed as a single JSON line before it becomes
// visible in the chain, so a crash cannot leave two records interleaved.
func (l *Ledger) Append(clientID, proxy string, bytesIn, bytesOut int64) (Entry, error) {
	if l == nil {
		return Entry{}, errors.New("ledger: nil ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Entry{}, errors.New("ledger: ledger is closed")
	}

	entry := Entry{
		Index:    uint64(len(l.entries)),
		Time:     time.Now().UTC(),
		ClientID: clientID,
		Proxy:    proxy,
		BytesIn:  bytesIn,
		BytesOut: bytesOut,
		PrevHash: l.headLocked(),
	}
	entry.Hash = hashEntry(entry)
	entry.Signature = hex.EncodeToString(l.key.sign(signaturePayload(entry.Hash)))

	if l.file != nil {
		line, err := json.Marshal(entry)
		if err != nil {
			return Entry{}, fmt.Errorf("ledger: marshal: %w", err)
		}
		line = append(line, '\n')
		if err := writeAll(l.file, line); err != nil {
			return Entry{}, fmt.Errorf("ledger: append to %s: %w", l.path, err)
		}
		if err := l.file.Sync(); err != nil {
			return Entry{}, fmt.Errorf("ledger: flush %s: %w", l.path, err)
		}
	}

	l.entries = append(l.entries, entry)
	return entry, nil
}

// Entries returns a copy of the chain in order.
func (l *Ledger) Entries() []Entry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Len returns the number of entries.
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Head returns the hash of the last entry, or "" when the ledger is empty.
//
// Publishing the head hash (and the entry count) out of band is what makes a
// truncated report detectable: a verifier compares it with the head of the
// chain it was given.
func (l *Ledger) Head() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.headLocked()
}

// Close flushes and closes the file, if any. It is idempotent.
func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	err := file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("ledger: close %s: %w", l.path, err)
	}
	return nil
}

// headLocked is Head without locking.
func (l *Ledger) headLocked() string {
	if len(l.entries) == 0 {
		return ""
	}
	return l.entries[len(l.entries)-1].Hash
}

// load parses a JSONL file and adopts it when the chain verifies.
func (l *Ledger) load(data []byte) error {
	lines := strings.Split(string(data), "\n")
	// A file written by Append ends with a newline, which leaves a final empty
	// element that is not an entry.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	entries := make([]Entry, 0, len(lines))
	source := make([]int, 0, len(lines))
	for i, line := range lines {
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return fmt.Errorf("ledger: %s: line %d: %w", l.path, i+1, err)
		}
		entries = append(entries, entry)
		source = append(source, i+1)
	}

	verified, err := verifyChain(entries, l.pub)
	if err != nil {
		line := 0
		if verified < len(source) {
			line = source[verified]
		} else if len(source) > 0 {
			line = source[len(source)-1]
		}
		return fmt.Errorf("ledger: %s: line %d: %w", l.path, line, err)
	}

	l.entries = entries
	return nil
}

// Verify checks a chain end to end: every hash links to the previous entry,
// the first entry has an empty PrevHash, indices are consecutive from zero,
// and every signature verifies under pub. It returns the number of verified
// entries and the first problem found, or (len, nil) when the chain is sound.
//
// An empty chain is valid. Malformed entries produce errors, never panics.
func Verify(entries []Entry, pub ed25519.PublicKey) (int, error) {
	if len(pub) != ed25519.PublicKeySize {
		return 0, fmt.Errorf("ledger: public key has length %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	return verifyChain(entries, pub)
}

// verifyChain is Verify after the public key length has been checked. The
// returned error names the index of the first entry that does not check out.
func verifyChain(entries []Entry, pub ed25519.PublicKey) (int, error) {
	prev := ""
	for i := range entries {
		entry := entries[i]
		if entry.Index != uint64(i) {
			return i, fmt.Errorf("entry %d: index is %d", i, entry.Index)
		}
		if entry.PrevHash != prev {
			return i, fmt.Errorf("entry %d: prev hash is %q, want %q", i, entry.PrevHash, prev)
		}
		computed := hashEntry(entry)
		if computed != entry.Hash {
			return i, fmt.Errorf("entry %d: hash is %q, recomputed %q", i, entry.Hash, computed)
		}
		signature, err := hex.DecodeString(entry.Signature)
		if err != nil {
			return i, fmt.Errorf("entry %d: signature is not hex: %w", i, err)
		}
		if len(signature) != ed25519.SignatureSize {
			return i, fmt.Errorf("entry %d: signature has length %d, want %d", i, len(signature), ed25519.SignatureSize)
		}
		if !ed25519.Verify(pub, signaturePayload(entry.Hash), signature) {
			return i, fmt.Errorf("entry %d: signature does not verify under the given public key", i)
		}
		prev = entry.Hash
	}
	return len(entries), nil
}

// Proof returns a copy of the chain entries [0, index], so a holder of the
// public key can check one entry's inclusion without the rest of the ledger:
// Verify the prefix and compare its last hash, or Ledger.Head of the full
// chain, with the published value for that index. It errors when index is out
// of range.
func Proof(l *Ledger, index int) ([]Entry, error) {
	if l == nil {
		return nil, errors.New("ledger: nil ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if index < 0 || index >= len(l.entries) {
		return nil, fmt.Errorf("ledger: index %d out of range for %d entries", index, len(l.entries))
	}
	prefix := make([]Entry, index+1)
	copy(prefix, l.entries[:index+1])
	return prefix, nil
}

// Summarise totals the traffic per client over a chain that has already been
// verified. Totals are keyed by client id and include entries for clients whose
// usage is zero.
func Summarise(entries []Entry) map[string]Totals {
	totals := make(map[string]Totals)
	for i := range entries {
		entry := entries[i]
		sum := totals[entry.ClientID]
		sum.BytesIn += entry.BytesIn
		sum.BytesOut += entry.BytesOut
		sum.Entries++
		totals[entry.ClientID] = sum
	}
	return totals
}

// hashEntry returns the hex SHA-256 of the canonical encoding
//
//	hashDomain
//	index     8 bytes big endian
//	time      UTC RFC3339Nano, length-prefixed
//	clientID  length-prefixed
//	proxy     length-prefixed
//	bytesIn   8 bytes big endian, two's complement
//	bytesOut  8 bytes big endian, two's complement
//	prevHash  length-prefixed
//
// where a length-prefixed field is its uint64 big-endian byte length followed
// by its bytes. The encoding is injective: any byte string of a field, including
// one holding a separator or an embedded newline, has exactly one encoding, so
// two different records cannot hash to the same digest. Time is always reduced
// to UTC first, so a record hashes identically wherever it is verified.
func hashEntry(entry Entry) string {
	h := sha256.New()
	_, _ = h.Write([]byte(hashDomain))
	writeUint64(h, entry.Index)
	writeField(h, entry.Time.UTC().Format(time.RFC3339Nano))
	writeField(h, entry.ClientID)
	writeField(h, entry.Proxy)
	writeUint64(h, uint64(entry.BytesIn))
	writeUint64(h, uint64(entry.BytesOut))
	writeField(h, entry.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// signaturePayload is what Ed25519 signs: a domain separator followed by the
// entry hash, so the signature is bound to this structure.
func signaturePayload(hashHex string) []byte {
	payload := make([]byte, 0, len(signatureDomain)+len(hashHex))
	payload = append(payload, signatureDomain...)
	return append(payload, hashHex...)
}

func writeField(h hash.Hash, field string) {
	writeUint64(h, uint64(len(field)))
	_, _ = h.Write([]byte(field))
}

func writeUint64(h hash.Hash, value uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	_, _ = h.Write(buf[:])
}

// writeAll writes the whole buffer, tolerating short writes.
func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("short write")
		}
		data = data[n:]
	}
	return nil
}
