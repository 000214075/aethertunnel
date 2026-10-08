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
	"io"
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

// errLedgerAlreadyLocked reports that another process holds the ledger's
// single-writer lock, which the platform-specific lockLedgerFile returns.
var errLedgerAlreadyLocked = errors.New("ledger: the file is locked by another writer")

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

// Ledger is an append-only chain of entries. The file on disk holds the whole
// chain; memory holds the most recent ledgerRetainEntries plus what the earlier
// entries summed to, so a server that runs for months keeps a bounded working
// set while the dashboard still reports the full chain's count, head and
// per-client totals — the last of these bounded too, see ledgerMaxClientTotals.
type Ledger struct {
	mu      sync.Mutex
	key     *SigningKey
	pub     ed25519.PublicKey
	path    string
	file    *os.File // nil when the ledger is memory only
	lock    *os.File // the single-writer lock's sidecar file; nil when the ledger is memory only
	entries []Entry  // the retained tail of the chain; the whole chain while it is shorter
	count   int      // entries in the whole chain, including the trimmed ones
	totals  map[string]Totals
	retain  int
	closed  bool
	// broken is set when a failed append could not be rolled back: the file
	// still holds the bytes of a record the in-memory chain does not contain,
	// so every later write would reuse that record's index and PrevHash and
	// the next start's verification would refuse the file.
	broken bool
}

// ledgerRetainEntries is how much of the chain stays in memory. The
// dashboard's limit parameter is capped at 5000, so a tail of this size
// answers every request the API can make.
const ledgerRetainEntries = 5120

// ledgerMaxClientTotals bounds how many client ids keep an aggregate of their
// own. A client_id is the client's to choose, up to 128 bytes long
// (pkg/server/server.go bounds its length, not how many there are), so a client
// that renames itself on every session would otherwise grow this map — and every
// dashboard response that copies it — for the life of the process, which is
// exactly what the retention window keeps the entries from doing.
const ledgerMaxClientTotals = 1024

// overflowClientID is the bucket the traffic of the client ids past
// ledgerMaxClientTotals is summed into, so the totals still account for every
// byte even though the map is bounded. A client that names itself exactly this
// has its traffic merged with theirs; the aggregate is right either way.
const overflowClientID = "other"

// addTotals folds one entry into a per-client totals map, keeping the map
// bounded: an id that is already there always keeps its own bucket, and a new
// one past the cap joins overflowClientID.
func addTotals(totals map[string]Totals, clientID string, bytesIn, bytesOut int64) {
	if _, known := totals[clientID]; !known && len(totals) >= ledgerMaxClientTotals {
		clientID = overflowClientID
	}
	sum := totals[clientID]
	sum.BytesIn += bytesIn
	sum.BytesOut += bytesOut
	sum.Entries++
	totals[clientID] = sum
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

	l := &Ledger{key: key, pub: pub, path: path, retain: ledgerRetainEntries, totals: map[string]Totals{}}
	if path == "" {
		return l, nil
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("ledger: %w", err)
		}
	}

	// The file does not exist, so the OpenFile below creates it: only then does
	// its directory entry need the flush at the end of this function.
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)

	// The single-writer lock is taken before the file is read, and held for the
	// Ledger's whole life: two instances configured with one ledger path, each
	// inheriting the same signing key, otherwise load the same head and write
	// records carrying one Index and PrevHash, breaking the chain on the spot.
	// Locking after the read would also let the second instance parse a
	// half-written line while the first is appending and report a corruption
	// error in place of the real reason it must not start.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	// The exclusive single-writer lock lives on a sidecar lock file, not on the
	// ledger itself. The platform locks differ in strength: unix flock is
	// advisory and leaves readers alone, while Windows LockFileEx is mandatory —
	// an exclusive lock over the ledger's bytes would block every read of them
	// while the writer runs, including --verify-ledger next to a running server,
	// which is what the offline audit is for. The lock file carries no bytes;
	// the handle that holds its lock is the single writer, and the file is
	// relocked by whoever starts after this process lets it go.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("ledger: open the lock for %s: %w", path, err)
	}
	if err := lockLedgerFile(lock); err != nil {
		closeBoth(file, lock)
		if errors.Is(err, errLedgerAlreadyLocked) {
			return nil, fmt.Errorf("ledger: %s is already being written by another instance", path)
		}
		return nil, fmt.Errorf("ledger: lock %s: %w", path, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		closeBoth(file, lock)
		return nil, fmt.Errorf("ledger: read %s: %w", path, err)
	}
	if err := l.load(data); err != nil {
		closeBoth(file, lock)
		return nil, err
	}

	if len(data) > 0 && data[len(data)-1] != '\n' {
		// A chain whose last record is not newline-terminated — a hand edit, or a
		// crash that lost the final byte — loads fine, but the next Append would
		// concatenate its record onto that line and make the whole file
		// unreadable, so the server would refuse to start afterwards. Terminating
		// the line first keeps the record that is already there.
		if err := writeAll(file, []byte{'\n'}); err != nil {
			closeBoth(file, lock)
			return nil, fmt.Errorf("ledger: terminate the last line of %s: %w", path, err)
		}
		if err := file.Sync(); err != nil {
			closeBoth(file, lock)
			return nil, fmt.Errorf("ledger: flush %s: %w", path, err)
		}
	}
	if created {
		// The file's own Sync makes its contents durable, but the name that
		// points at the inode can still be sitting in the directory's page
		// cache, so a crash right after the ledger is created can lose the whole
		// file and the chain in it.
		if err := syncParentDir(path); err != nil {
			closeBoth(file, lock)
			return nil, fmt.Errorf("ledger: flush the directory of %s: %w", path, err)
		}
	}
	l.file = file
	l.lock = lock
	return l, nil
}

// closeBoth releases the handles of a half-built ledger when New fails after the
// lock was taken: the data file and the sidecar lock file go together, so the
// lock never outlives the failed start.
func closeBoth(file, lock *os.File) {
	_ = file.Close()
	_ = lock.Close()
}

// syncParentDir flushes the directory entry of a file this process created, so
// the name that points at the inode survives a crash as the inode does. It is
// platform-split (see syncdir_windows.go): Windows has no equivalent directory
// flush, and calling one there failed every first creation of a ledger.

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
	if l.broken {
		return Entry{}, errors.New("ledger: a failed append could not be rolled back, so the file holds bytes the chain does not; refusing to write past them")
	}

	entry := Entry{
		Index:    uint64(l.count),
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
		// Where this record starts, so a failed write or flush can be rolled back.
		// The entry is not added to the chain on failure, so leaving the bytes in
		// the file would make the next append reuse this index and PrevHash — a
		// duplicate or a torn line that makes the next start's verification fail
		// and the server refuse to come up.
		before, err := l.sizeLocked()
		if err != nil {
			return Entry{}, fmt.Errorf("ledger: %s: %w", l.path, err)
		}
		if err := writeAll(l.file, line); err != nil {
			return Entry{}, l.appendFailedLocked(fmt.Errorf("ledger: append to %s: %w", l.path, err), before)
		}
		if err := l.file.Sync(); err != nil {
			return Entry{}, l.appendFailedLocked(fmt.Errorf("ledger: flush %s: %w", l.path, err), before)
		}
	}

	l.entries = append(l.entries, entry)
	l.count++
	addTotals(l.totals, clientID, bytesIn, bytesOut)
	// Trim the tail once it outgrows the retention window. The slice shares
	// its backing array with what was dropped until the next growth, so the
	// working set stays within a constant factor of the tail.
	if len(l.entries) > l.retain {
		l.entries = l.entries[len(l.entries)-l.retain:]
	}
	return entry, nil
}

// sizeLocked is the file's length, which is the offset a failed write is rolled
// back to. The caller holds the lock. An unreadable length is an error rather
// than a zero: truncating to a length that was never read would drop the whole
// chain, not the partial record the rollback exists for.
func (l *Ledger) sizeLocked() (int64, error) {
	if l.file == nil {
		return 0, nil
	}
	info, err := l.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// appendFailedLocked rolls the file back to offset after a failed append and
// returns why the append failed, with the rollback failure joined in when there
// is one. Swallowing that failure is what makes the ledger unwritable rather
// than merely behind: the record's bytes stay in the file while count and
// headLocked() do not advance.
func (l *Ledger) appendFailedLocked(appendErr error, offset int64) error {
	if err := l.rollbackLocked(offset); err != nil {
		return errors.Join(appendErr, err)
	}
	return appendErr
}

// rollbackLocked drops everything written past offset. The caller holds the lock.
// O_APPEND makes the offset irrelevant to the next write, so only the file's
// length has to be restored.
//
// A truncate that fails is reported instead of ignored: the ledger is then
// marked broken, and later appends refuse rather than writing a record whose
// index the file already holds.
func (l *Ledger) rollbackLocked(offset int64) error {
	if l.file == nil {
		return nil
	}
	if err := l.file.Truncate(offset); err != nil {
		l.broken = true
		return fmt.Errorf("ledger: roll back %s to %d: %w", l.path, offset, err)
	}
	return nil
}

// Snapshot returns the retained tail of the chain, its head hash, the length
// of the whole chain and the whole chain's per-client totals from a single
// locked read, so a report published while an entry is being appended cannot
// pair a head with entries that do not reach it.
func (l *Ledger) Snapshot() (entries []Entry, head string, count int, totals map[string]Totals) {
	if l == nil {
		return nil, "", 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries = make([]Entry, len(l.entries))
	copy(entries, l.entries)
	if len(l.entries) > 0 {
		head = l.entries[len(l.entries)-1].Hash
	}
	totals = make(map[string]Totals, len(l.totals))
	for id, sum := range l.totals {
		totals[id] = sum
	}
	return entries, head, l.count, totals
}

// Entries returns a copy of the retained tail of the chain in order: the
// whole chain while it is shorter than the retention window. Older entries
// stay in the ledger file, which ReadFile can parse.
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

// Len returns the number of entries in the whole chain, including any that
// memory no longer holds.
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
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
	lock := l.lock
	l.file = nil
	l.lock = nil
	err := file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	// The lock is released by the handle that holds it; the sidecar file itself
	// stays on disk at 0 bytes for the next writer to lock.
	if lock != nil {
		if lockErr := lock.Close(); err == nil {
			err = lockErr
		}
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

// ReadFile parses a JSONL ledger file into entries without verifying it. Pair it
// with Verify to check a chain that arrived from somewhere else.
func ReadFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: read %s: %w", path, err)
	}
	entries, _, err := parse(data)
	return entries, err
}

// load parses a JSONL file and adopts it when the chain verifies.
func (l *Ledger) load(data []byte) error {
	entries, lines, err := parse(data)
	if err != nil {
		return fmt.Errorf("ledger: %s: %w", l.path, err)
	}
	verified, err := verifyChain(entries, l.pub)
	if err != nil {
		return fmt.Errorf("ledger: %s: line %d: %w", l.path, lines[verified], err)
	}

	l.count = len(entries)
	l.totals = Summarise(entries)
	if len(entries) > l.retain {
		entries = entries[len(entries)-l.retain:]
	}
	l.entries = entries
	return nil
}

// parse decodes JSONL entries, skipping blank lines and returning the file line
// number each entry came from so a failure can be reported against the file the
// operator has to edit.
func parse(data []byte) ([]Entry, []int, error) {
	lines := strings.Split(string(data), "\n")
	// A file written by Append ends with a newline, which leaves a final empty
	// element that is not an entry.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	entries := make([]Entry, 0, len(lines))
	numbers := make([]int, 0, len(lines))
	for i, line := range lines {
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		entries = append(entries, entry)
		numbers = append(numbers, i+1)
	}
	return entries, numbers, nil
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
//
// A prefix reaching past the retained tail is rebuilt from the ledger file,
// so proofs keep working for entries memory no longer holds.
func Proof(l *Ledger, index int) ([]Entry, error) {
	if l == nil {
		return nil, errors.New("ledger: nil ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if index < 0 || index >= l.count {
		return nil, fmt.Errorf("ledger: index %d out of range for %d entries", index, l.count)
	}
	// Memory holds the most recent entries only once the chain outgrew the
	// retention window, so a slice of it is the chain's tail, not the prefix
	// Proof promises: Verify would refuse it at its first index. The in-memory
	// path is therefore taken only while memory still starts at index 0, and
	// every other prefix is rebuilt from the file below.
	if index < len(l.entries) && l.entries[0].Index == 0 {
		prefix := make([]Entry, index+1)
		copy(prefix, l.entries[:index+1])
		return prefix, nil
	}
	if l.path == "" {
		return nil, fmt.Errorf("ledger: index %d is outside the %d entries held in memory (chain of %d) and the ledger has no file to read it from", index, len(l.entries), l.count)
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, fmt.Errorf("ledger: read %s: %w", l.path, err)
	}
	chain, _, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("ledger: %s: %w", l.path, err)
	}
	if index >= len(chain) {
		return nil, fmt.Errorf("ledger: %s holds %d entries, fewer than index %d", l.path, len(chain), index)
	}
	prefix := make([]Entry, index+1)
	copy(prefix, chain[:index+1])
	return prefix, nil
}

// Summarise totals the traffic per client over a chain that has already been
// verified. Totals are keyed by client id and include entries for clients whose
// usage is zero.
func Summarise(entries []Entry) map[string]Totals {
	totals := make(map[string]Totals)
	for i := range entries {
		entry := entries[i]
		addTotals(totals, entry.ClientID, entry.BytesIn, entry.BytesOut)
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
