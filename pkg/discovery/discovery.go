// Package discovery publishes and resolves AetherTunnel proxy names through the
// Kademlia DHT in pkg/dht, so a client can find the server that publishes a proxy
// without being configured with that server's address.
//
// A server stores a small JSON record under "<namespace>/proxy/<name>"; any node
// that can reach the DHT resolves the same key to read it. The record carries its
// own expiry because the DHT has no remote delete: a server that stops announcing
// a name leaves a copy behind on its peers until the TTL runs out, and readers
// treat a record whose expiry has passed as absent rather than as a live address.
//
// A record may be signed. The DHT stores values at keys that anyone on the network
// can write, so an unsigned record says only that some node claimed an address. A
// publisher that holds an Ed25519 key signs each record with it and names the
// public key in the record; a reader that is given the key it trusts, or that
// simply wants any record to be signed, checks the signature before using the
// address. Signing does not hide the metadata: the record, the name and the
// address stay visible.
package discovery

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/dht"
)

const (
	// DefaultNamespace prefixes every key this package writes.
	DefaultNamespace = "aethertunnel"
	// DefaultAnnounceTTL is how long an announcement stays valid on a reader,
	// as written into the record by its publisher.
	DefaultAnnounceTTL = 90 * time.Second
	// DefaultRepublishInterval is the interval that goes with DefaultAnnounceTTL:
	// a third of it, which is what a third of any chosen TTL comes to. The
	// interval has to stay below the TTL, or a running server lets its own
	// announcements lapse.
	DefaultRepublishInterval = 30 * time.Second
	// DefaultLookupTimeout bounds a single resolution from start to finish.
	DefaultLookupTimeout = 5 * time.Second
)

// ErrStale reports a record that exists but whose announcing server has stopped
// refreshing it.
var ErrStale = errors.New("discovery: the announcement is stale")

// ErrUnsigned reports an unsigned record read by a node that requires signatures.
var ErrUnsigned = errors.New("discovery: the announcement carries no signature")

// ErrUntrustedPublisher reports a record signed by a key the reader was not given.
var ErrUntrustedPublisher = errors.New("discovery: the announcement is signed by a key that is not trusted")

// ErrBadSignature reports a record whose signature does not check out, which means
// the stored value was written or altered by a node other than the publisher.
var ErrBadSignature = errors.New("discovery: the announcement's signature does not verify")

// signatureDomain is the first line of the signed bytes. It keeps a signature
// produced for an announcement from being replayed as a signature over anything
// else that happens to be signed with the same key.
const signatureDomain = "aethertunnel announcement v1"

// Signer is the Ed25519 key a publisher signs its announcements with.
// *crypto.Identity satisfies it.
type Signer interface {
	PublicKey() ed25519.PublicKey
	Sign(message []byte) []byte
}

// Record is the value stored under a proxy key.
//
// The two timestamps are exact rather than second-resolution, because a short
// announce TTL has to be comparable with sub-second precision.
type Record struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Server  string    `json:"server"`
	Domains []string  `json:"domains,omitempty"`
	Updated time.Time `json:"updated"`
	Expires time.Time `json:"expires"`

	// PublicKey is the hex Ed25519 key the record was signed with, in the same
	// form as identity.allowed_keys. Empty on an unsigned record.
	PublicKey string `json:"public_key,omitempty"`
	// Signature is the hex signature over SigningPayload. Empty on an unsigned
	// record.
	Signature string `json:"signature,omitempty"`

	// Verified is set by Lookup when the record carried a signature that checked
	// out against the key in the record. It is not part of the stored value.
	Verified bool `json:"-"`
}

// Fresh reports whether the record was still current at now.
func (r Record) Fresh(now time.Time) bool { return now.Before(r.Expires) }

// SigningPayload is the byte string a record's signature covers: every field a
// reader acts on, in the order they are encoded. Changing the name, the type, the
// address, the domains or either timestamp therefore invalidates the signature,
// so an altered copy of a valid record is rejected.
func (r Record) SigningPayload() []byte {
	r.Signature = ""
	body, err := json.Marshal(r)
	if err != nil {
		// The type holds no value json cannot encode.
		return nil
	}
	out := make([]byte, 0, len(signatureDomain)+len(body)+1)
	out = append(out, signatureDomain...)
	out = append(out, '\n')
	return append(out, body...)
}

// sign fills in the key and the signature. A record published without a signer
// keeps both fields empty.
func (r *Record) sign(signer Signer) {
	if signer == nil {
		r.PublicKey = ""
		r.Signature = ""
		return
	}
	r.PublicKey = hex.EncodeToString(signer.PublicKey())
	r.Signature = hex.EncodeToString(signer.Sign(r.SigningPayload()))
}

// verify checks the record against the reader's policy: an unsigned record when
// signatures are required, a malformed key or signature, a signature that does not
// verify, and a valid signature from a key the reader was not given are all
// refused. It reports whether the record was signed and verified, so a reader can
// tell a trusted record from an unsigned one that its policy allows.
func (r *Record) verify(requireSigned bool, trusted []ed25519.PublicKey) (bool, error) {
	if r.PublicKey == "" && r.Signature == "" {
		if requireSigned {
			return false, ErrUnsigned
		}
		if len(trusted) > 0 {
			// A named key is what the reader trusts; an unsigned record cannot
			// show that it came from one.
			return false, ErrUnsigned
		}
		return false, nil
	}
	if r.PublicKey == "" || r.Signature == "" {
		return false, fmt.Errorf("%w: the record names a key but no signature, or the other way round", ErrBadSignature)
	}

	raw, err := hex.DecodeString(r.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return false, fmt.Errorf("%w: the record's public key is not a %d-byte Ed25519 key in hex",
			ErrBadSignature, ed25519.PublicKeySize)
	}
	key := ed25519.PublicKey(raw)

	signature, err := hex.DecodeString(r.Signature)
	if err != nil {
		return false, fmt.Errorf("%w: the signature is not hex: %v", ErrBadSignature, err)
	}
	if !ed25519.Verify(key, r.SigningPayload(), signature) {
		return false, ErrBadSignature
	}

	if len(trusted) > 0 && !trusts(trusted, key) {
		return false, fmt.Errorf("%w: %s", ErrUntrustedPublisher, r.PublicKey)
	}
	r.Verified = true
	return true, nil
}

func trusts(trusted []ed25519.PublicKey, key ed25519.PublicKey) bool {
	for _, candidate := range trusted {
		if candidate.Equal(key) {
			return true
		}
	}
	return false
}

// Config configures a node. The zero value is usable and joins no DHT: with an
// empty Bootstrap list the node is a one-node table that still stores and
// resolves its own records, which is what a single-server deployment needs.
type Config struct {
	// ListenAddr is the UDP address to bind. Empty or ":0" selects a port.
	ListenAddr string
	// Bootstrap lists DHT nodes to contact on start.
	Bootstrap []string
	// NodeID is a 40-character hex identifier. Empty selects a random one.
	NodeID string
	// Namespace prefixes every key. Two deployments can share one DHT.
	Namespace string
	// TTL is how long the DHT keeps a record. Zero selects the DHT default.
	TTL time.Duration
	// AnnounceTTL is how long an announcement stays valid on a reader, as
	// written into the record by its publisher; a reader's own value does not
	// shorten what it reads.
	AnnounceTTL time.Duration
	// RepublishInterval is how often live announcements are rewritten.
	RepublishInterval time.Duration
	// LookupTimeout bounds one resolution.
	LookupTimeout time.Duration
	// Signer signs every record this node publishes, and names its public key in
	// the record. nil publishes unsigned records.
	Signer Signer
	// RequireSigned makes Lookup refuse a record that carries no signature.
	RequireSigned bool
	// TrustedKeys restricts Lookup to records signed by one of these keys, which
	// is what turns "some node claimed this address" into "the server I trust
	// claimed this address". A non-empty list also refuses unsigned records.
	TrustedKeys []ed25519.PublicKey
	// Logger receives diagnostic lines. nil disables logging.
	Logger *log.Logger
}

// Node is a DHT node with the tunnel's key namespace applied.
//
// Every method is safe for concurrent use.
type Node struct {
	table *dht.Table
	cfg   Config

	// writeMu orders the writers of one name end to end. The epoch and the
	// live map each decide part of what a publish does, but a publish's store
	// and its half of the decision are two critical sections on n.mu, and a
	// Withdraw is a third: a publish whose store has landed can be judged
	// stale, Forget the bytes a second publish has already stored, and leave
	// the live map naming a record the DHT no longer holds. Holding this
	// across each whole write path runs the writers of a name one at a time,
	// which is what lets the epoch and supersededBy checks settle. It is only
	// ever taken with n.mu released — the callers read the epoch and the live
	// record under n.mu and then enter here — because taking n.mu while
	// holding it would close a lock-order cycle.
	writeMu sync.Mutex
	// lastUpdated is the Updated of the newest record this node published.
	// The publishes of a name are ordered by writeMu, and Updated is forced
	// strictly forward from it (see publish): a wall clock whose resolution
	// does not outrun the publishers — Windows advances in milliseconds —
	// would otherwise stamp two publishes of one name with one timestamp, and
	// the signature covers Updated, so the two records come out byte-identical
	// and a record a caller read before is indistinguishable from the one a
	// later publish installed. The checks that keep the live map and the DHT
	// agreeing judge by that identity.
	lastUpdated time.Time

	mu   sync.Mutex
	live map[string]Record
	// withdrawn holds, per name, the withdrawals' epoch and when that mark
	// was last read by a publish. A republish snapshots the live records and
	// then writes to the DHT, and a Withdraw that runs while that write is in
	// flight would otherwise be undone by it: the entry was deleted and the
	// value was forgotten, and the publish that finishes afterwards puts the
	// record back in both places, so the name resolves forever after it had
	// been withdrawn. The epoch is what the publisher compares to tell the
	// two apart. The timestamp lets the republish loop prune marks that no
	// in-flight publish can still be comparing, so the map does not hold one
	// entry per name ever withdrawn for the process's whole life.
	withdrawn map[string]withdrawMark

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start binds the node's socket and begins serving and republishing.
func Start(cfg Config) (*Node, error) {
	cfg = cfg.withDefaults()

	tableCfg := dht.Config{
		ListenAddr: cfg.ListenAddr,
		Bootstrap:  cfg.Bootstrap,
		TTL:        cfg.TTL,
		Logger:     cfg.Logger,
	}
	if cfg.NodeID != "" {
		id, err := dht.IDFromString(cfg.NodeID)
		if err != nil {
			return nil, fmt.Errorf("discovery: node id: %w", err)
		}
		tableCfg.ID = id
	}

	table, err := dht.New(tableCfg)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if _, err := table.Start(); err != nil {
		_ = table.Close()
		return nil, fmt.Errorf("discovery: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		table:     table,
		cfg:       cfg,
		live:      make(map[string]Record),
		withdrawn: make(map[string]withdrawMark),
		ctx:       ctx,
		cancel:    cancel,
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.republishLoop()
	}()
	return n, nil
}

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.AnnounceTTL <= 0 {
		c.AnnounceTTL = DefaultAnnounceTTL
	}
	if c.RepublishInterval <= 0 {
		c.RepublishInterval = republishIntervalFor(c.AnnounceTTL)
	}
	if c.LookupTimeout <= 0 {
		c.LookupTimeout = DefaultLookupTimeout
	}
	return c
}

// republishIntervalFor is how often a live announcement is rewritten when the
// caller named no interval: a third of the announce TTL, so a record is refreshed
// twice before a reader stops honouring it.
//
// A third of a TTL shorter than three seconds is zero, and a zero interval would
// mean "the caller chose nothing" all over again. The floor of one second keeps
// the interval below any TTL the configuration accepts, which is the property that
// matters: an announcement that lapses before it is rewritten stops resolving
// while its server is still running.
func republishIntervalFor(announceTTL time.Duration) time.Duration {
	interval := announceTTL / 3
	if interval > time.Second {
		return interval
	}
	// The floor is a second, but never at or above the TTL the operator
	// chose: an announcement that lapses before its rewrite would stop
	// resolving while its server is still running. A TTL below three
	// nanoseconds has no third that survives the division, and a zero
	// interval panics NewTicker in the caller; such a TTL is below any
	// resolution the configuration accepts, so the floor is the answer.
	if announceTTL > 0 && interval > 0 && interval < announceTTL {
		return interval
	}
	return time.Second
}

// Key is the DHT key a proxy name is published under.
func (n *Node) Key(name string) string {
	return n.cfg.Namespace + "/proxy/" + name
}

// Publish stores an announcement for a proxy name, replacing any earlier one.
//
// The timestamp and expiry are set here, so a caller cannot publish a record that
// claims a validity it does not have. When the node has a signer, the record is
// signed here for the same reason: the stored bytes are the ones that were signed,
// and no caller can publish a record under a key it does not hold.
func (n *Node) Publish(ctx context.Context, rec Record) error {
	if rec.Name == "" {
		return errors.New("discovery: an announcement needs a name")
	}
	if rec.Server == "" {
		return errors.New("discovery: an announcement needs a server address")
	}

	// The withdrawal this publish races: read at the moment the caller decided to
	// publish. The refresh loop reads it with its snapshot instead, because a
	// withdrawal that lands between that snapshot and this call must still win.
	//
	// The live record is read with it and handed to publish as the snapshot to
	// check, for the same reason refresh does: two Publishes of one name — a
	// client reconnecting with a new remote port, or two sessions registering the
	// same name — store their values in one order and take the lock in publish in
	// another. Without the comparison the live entry could name the address whose
	// store landed first while the DHT holds the second, and every later refresh
	// would keep republishing the first. The check makes the last store win in
	// both places.
	n.mu.Lock()
	epoch := n.withdrawn[rec.Name].epoch
	// Touch the mark: the post-write check below compares against this very
	// read, and the pruning leaves a mark that was read moments ago alone.
	if mark, ok := n.withdrawn[rec.Name]; ok {
		mark.at = time.Now()
		n.withdrawn[rec.Name] = mark
	}
	current, live := n.live[rec.Name]
	n.mu.Unlock()
	expect := &current
	if !live {
		// Nothing was live when this call read the map. That is an expectation
		// too: two first-ever Publishes of one name both see no record, and
		// without a snapshot to compare they would install themselves in the map
		// whatever order the stores landed in, leaving the live entry and the DHT
		// naming different addresses. A zero Updated marks "expected nothing".
		expect = &Record{}
	}
	return n.publish(ctx, rec, epoch, expect)
}

// refresh rewrites one announcement the loop listed, unless the listing has been
// superseded. liveSnapshot copies the records and then does network I/O per record,
// so a Publish that lands in that window is newer, and the snapshot value rec passed
// here is what the writer has to be held to: publish re-checks it after the store
// below, because the check at the top of this function and the write are separated
// by that network I/O.
func (n *Node) refresh(ctx context.Context, rec Record, epoch uint64) error {
	n.mu.Lock()
	current, live := n.live[rec.Name]
	stale := !live || !sameRecord(&current, &rec)
	n.mu.Unlock()
	if stale {
		return nil
	}
	return n.publish(ctx, rec, epoch, &rec)
}

// publish is Publish with the withdrawal epoch and the snapshot record the caller
// observed. expect is nil for a fresh announcement, which replaces whatever is live;
// refresh passes the record its snapshot held, so the live entry is only replaced if
// it is still that one. It is the epoch the post-write check compares against, so any
// withdrawal that landed after that epoch was read wins over this write.
func (n *Node) publish(ctx context.Context, rec Record, epoch uint64, expect *Record) error {
	// One writer per name at a time, from the store through the decision
	// below: without it this call's Forget can land on top of a newer
	// publish's store and take that record out of the DHT while the live map
	// keeps naming it. n.mu is not held here, so the order is always
	// n.mu -> writeMu.
	n.writeMu.Lock()
	defer n.writeMu.Unlock()

	// The record's Updated is its version, and the comparisons that keep the
	// live map and the DHT agreeing judge by it. It is forced strictly forward
	// here, under the write lock that orders the publishes of a name: the wall
	// clock alone does not outrun the publishers on every platform (Windows
	// advances in milliseconds), and two publishes of one name in one tick
	// would stamp byte-identical records — identical signature included — so a
	// record a caller read before could pass for the one a later publish just
	// installed, and the stale caller would take the withdrawal-won path and
	// Forget the bytes the newer publish had stored.
	now := time.Now()
	if !now.After(n.lastUpdated) {
		now = n.lastUpdated.Add(time.Nanosecond)
	}
	n.lastUpdated = now
	rec.Updated = now
	rec.Expires = now.Add(n.cfg.AnnounceTTL)
	rec.sign(n.cfg.Signer)

	if err := n.storeRecord(ctx, rec); err != nil {
		return err
	}

	// A newer Publish replaced the live record while this value was in flight.
	// Assigning rec now would revert the name to the address the publisher already
	// moved away from — and, because every later refresh reads the live map, it
	// would keep publishing that address until the caller published again. The
	// bytes just stored are that stale address, so the newer record is written over
	// them; it is already signed and its expiry is later.
	//
	// The check and the write-back cannot be made atomic with respect to each
	// other, so another publish can land between them and make the stored bytes
	// stale again. The write-back is therefore re-judged against the live map until
	// the record the DHT holds is the one the map holds; written is what the last
	// store left there. Each round costs one DHT write, so this settles as soon as
	// the publishers stop moving the name, and maxRestoreAttempts is the valve that
	// keeps a name rewritten faster than this call can follow from holding the
	// caller.
	written := rec
	for attempt := 0; ; attempt++ {
		n.mu.Lock()
		current, stillLive := n.live[rec.Name]
		if !stillLive || expect == nil || !supersededBy(&current, expect) {
			// Nothing newer than what the caller read is live, so the record just
			// stored is the one the map should name. The decision and the
			// assignment share this critical section on purpose: in separate ones
			// two Publishes of one name can each read a live entry that neither has
			// installed yet, each find it not superseded, and each assign — the one
			// whose store landed first assigning last, so the map and the DHT name
			// different addresses and every later refresh republishes the loser.
			// The withdrawal epoch is read under the same lock as before.
			if n.withdrawn[rec.Name].epoch != epoch {
				n.mu.Unlock()
				break
			}
			n.live[rec.Name] = rec
			n.mu.Unlock()
			return nil
		}
		if sameRecord(&current, &written) || attempt >= maxRestoreAttempts {
			// Either the DHT already holds this record, or the name is being
			// replaced faster than this call can follow. What is stored is the
			// most recent value this call managed to write, and the next refresh
			// or publish rewrites it. Either way rec must not be assigned, which
			// is what returning here keeps: the superseded branch has never
			// fallen through to the epoch check below.
			n.mu.Unlock()
			return nil
		}
		n.mu.Unlock()
		if err := n.storeRecord(ctx, current); err != nil {
			return fmt.Errorf("discovery: restore the current announcement for %q: %w", rec.Name, err)
		}
		written = current
	}

	// A Withdraw for this name ran while the value was on its way to the peers,
	// so it is the withdrawal that has to win. The local record is already gone;
	// the copy just stored is forgotten again, and a failure to do so is
	// reported because until it lapses the name still resolves.
	if err := n.table.Forget(n.Key(rec.Name)); err != nil {
		return fmt.Errorf("discovery: withdraw %q while it was being published: %w", rec.Name, err)
	}
	return nil
}

// maxRestoreAttempts bounds how many times one publish rewrites a live record
// over bytes a concurrent publish replaced. Each round is one DHT write, so the
// loop ends on its own as soon as the name stops moving; the bound only keeps a
// publisher that rewrites the same name in a tight loop from holding the caller
// for as long as it keeps doing that.
const maxRestoreAttempts = 8

// supersededBy reports whether the live record is not the one the caller read
// before it stored its own value: either a different record took its place, or
// the caller read no record at all and one has appeared since, which the zero
// Updated of its "expected nothing" snapshot marks.
func supersededBy(current, expect *Record) bool {
	if expect.Updated.IsZero() {
		return true
	}
	return !sameRecord(current, expect)
}

// sameRecord reports whether two announcements are the same record. The Updated
// timestamp used to stand in for a record's identity, which holds only while the
// clock's resolution outruns the publishers: two Publishes of one name that land
// in the same tick carry equal Updated values while their values differ, and
// every comparison below then mistakes one for the other — the write-back loop
// believes the DHT already holds the live record and leaves the two naming
// different addresses. The default timer granularity on Windows is coarse enough
// for the race tests to hit that every run. The fields are compared instead;
// the signature covers all of them, so equal fields are the same record.
func sameRecord(a, b *Record) bool {
	return a.Name == b.Name &&
		a.Type == b.Type &&
		a.Server == b.Server &&
		slices.Equal(a.Domains, b.Domains) &&
		a.Updated.Equal(b.Updated) &&
		a.Expires.Equal(b.Expires) &&
		a.PublicKey == b.PublicKey &&
		a.Signature == b.Signature
}

// storeRecord encodes one announcement and puts it in the DHT. The size check is
// here so every write path refuses a record the DHT would truncate or reject.
func (n *Node) storeRecord(ctx context.Context, rec Record) error {
	value, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("discovery: encode the announcement for %q: %w", rec.Name, err)
	}
	if len(value) > dht.MaxValueSize {
		return fmt.Errorf("discovery: the announcement for %q is %d bytes, more than the %d a DHT value holds",
			rec.Name, len(value), dht.MaxValueSize)
	}
	if err := n.table.Put(ctx, n.Key(rec.Name), value); err != nil {
		return fmt.Errorf("discovery: publish %q: %w", rec.Name, err)
	}
	return nil
}

// Withdraw stops announcing a name. The local record is dropped so it is no longer
// republished, and copies already replicated to peers lapse within one TTL.
func (n *Node) Withdraw(name string) error {
	// The writers of a name run one at a time: a withdrawal that ran in the
	// middle of a publish is what used to leave the live map naming a record
	// the DHT no longer held, because the two halves of a publish are
	// separate critical sections.
	n.writeMu.Lock()
	defer n.writeMu.Unlock()

	n.mu.Lock()
	delete(n.live, name)
	mark := n.withdrawn[name]
	mark.epoch++
	mark.at = time.Now()
	n.withdrawn[name] = mark
	n.mu.Unlock()

	if err := n.table.Forget(n.Key(name)); err != nil {
		return fmt.Errorf("discovery: withdraw %q: %w", name, err)
	}
	return nil
}

// Lookup resolves a proxy name. It returns ErrStale when the record is there but
// its announcer stopped refreshing it, dht.ErrNotFound when nothing is known, and
// ErrUnsigned, ErrBadSignature or ErrUntrustedPublisher when the stored value does
// not satisfy this node's signature policy.
//
// The signature is checked before the expiry, so a forged record is reported as
// forged rather than as stale.
func (n *Node) Lookup(ctx context.Context, name string) (Record, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(n.ctx, n.cfg.LookupTimeout)
		defer cancel()
	}

	value, err := n.table.Get(ctx, n.Key(name))
	if err != nil {
		return Record{}, err
	}

	var rec Record
	if err := json.Unmarshal(value, &rec); err != nil {
		return Record{}, fmt.Errorf("discovery: %q holds a value that is not an announcement: %w", name, err)
	}
	if rec.Server == "" {
		return Record{}, fmt.Errorf("discovery: %q holds an announcement without a server address", name)
	}
	if rec.Name != name {
		// The signature covers the name, so a valid record for another name
		// proves the signer announced THAT name. The DHT is unauthenticated
		// and any peer may answer a lookup: without this check a node could
		// replay the trusted server's record for "web" in reply to a lookup
		// for "ssh" and the caller would dial the wrong service.
		return Record{}, fmt.Errorf("discovery: %q was answered with the announcement for %q", name, rec.Name)
	}
	if _, err := rec.verify(n.cfg.RequireSigned, n.cfg.TrustedKeys); err != nil {
		return Record{}, fmt.Errorf("%q: %w", name, err)
	}
	if !rec.Fresh(time.Now()) {
		return Record{}, fmt.Errorf("%q was last announced at %s: %w",
			name, rec.Updated.UTC().Format(time.RFC3339), ErrStale)
	}
	return rec, nil
}

// Resolve is Lookup with the configured timeout applied, for callers that hold no
// context of their own.
func (n *Node) Resolve(name string) (Record, error) {
	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.LookupTimeout)
	defer cancel()
	return n.Lookup(ctx, name)
}

// Announced lists the names this node is currently publishing, sorted.
func (n *Node) Announced() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.live))
	for name := range n.live {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Self is this node's DHT identifier in hex.
func (n *Node) Self() string { return n.table.Self().String() }

// Addr is the address the node bound, or an empty string before Start succeeded.
func (n *Node) Addr() string {
	if addr := n.table.Addr(); addr != nil {
		return addr.String()
	}
	return ""
}

// Namespace is the key prefix in use.
func (n *Node) Namespace() string { return n.cfg.Namespace }

// Contacts is the number of peers in the routing table.
func (n *Node) Contacts() int { return n.table.Len() }

// Bootstrap joins the configured nodes, which is how a node learns about a DHT it
// was not part of. Start does not wait for this, so a DHT that is unreachable
// delays nothing; the call reports what it could not reach.
func (n *Node) Bootstrap(ctx context.Context) []error {
	var failures []error
	for _, addr := range n.cfg.Bootstrap {
		if err := n.table.Ping(ctx, addr); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", addr, err))
		}
	}
	return failures
}

// Context is the node's lifetime context, cancelled by Close. It bounds a call that
// has no request scope of its own, such as the bootstrap a command line performs
// before its first lookup.
func (n *Node) Context() context.Context { return n.ctx }

// Close stops the node and waits for its goroutines.
func (n *Node) Close() error {
	n.cancel()
	err := n.table.Close()
	n.wg.Wait()
	return err
}

// republishLoop rewrites every live announcement before it lapses. Without it a
// long-running server would publish a name once and then be invisible to readers
// as soon as the first announcement expired.
func (n *Node) republishLoop() {
	ticker := time.NewTicker(n.cfg.RepublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.pruneWithdrawn(time.Now())
			republishEntries(n.ctx, n.liveSnapshot(), n.cfg.LookupTimeout, n.refresh, n.logf)
		}
	}
}

// republishEntries rewrites every announcement in the snapshot, a few at a
// time. The records are independent — different names in the store — and
// refresh already arbitrates against a concurrent Publish, so rewriting them
// one at a time bought no ordering: a round lasted one lookup timeout per
// name, and a server with many names pushed the later names past their expiry
// and off the directory whenever the DHT slowed down.
func republishEntries(ctx context.Context, live []liveAnnouncement, timeout time.Duration, refresh func(context.Context, Record, uint64) error, logf func(string, ...any)) {
	const workers = 4
	var wg sync.WaitGroup
	work := make(chan liveAnnouncement, len(live))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range work {
				runCtx, cancel := context.WithTimeout(ctx, timeout)
				err := refresh(runCtx, entry.rec, entry.epoch)
				cancel()
				if err != nil && ctx.Err() == nil {
					// Close cancelled the node while this republish was in
					// flight, so "context canceled" is the shutdown, not a
					// failure to refresh. The DHT package suppresses the same
					// noise for the same reason.
					logf("discovery: cannot refresh %q: %v", entry.rec.Name, err)
				}
			}
		}()
	}
	for _, entry := range live {
		work <- entry
	}
	close(work)
	wg.Wait()
}

// liveAnnouncement is one announcement the refresh loop should rewrite, with the
// withdrawal epoch it was taken at.
type liveAnnouncement struct {
	rec   Record
	epoch uint64
}

// withdrawMark is one name's withdrawal epoch, plus when the mark was last
// written or read by a publish — the freshness the pruning judges.
type withdrawMark struct {
	epoch uint64
	at    time.Time
}

// withdrawnRetention is how long an untouched, no-longer-live mark is kept.
// A publish touches the mark it reads and its store is bounded by the lookup
// timeout, so a mark this old is out of every comparison a publish still in
// flight could make.
const withdrawnRetention = 10 * time.Minute

// pruneWithdrawn drops the withdrawal marks that no live record and no
// in-flight publish can still reach. Without it the map keeps one entry per
// name ever withdrawn, for as long as the process runs.
func (n *Node) pruneWithdrawn(now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for name, mark := range n.withdrawn {
		if _, live := n.live[name]; !live && now.Sub(mark.at) > withdrawnRetention {
			delete(n.withdrawn, name)
		}
	}
}

// liveSnapshot lists the announcements to refresh and the epoch the listing was
// taken at. Reading the epoch inside the publish instead let a withdrawal that
// landed after the snapshot go unnoticed, and the name was published and
// refreshed forever although it had been withdrawn.
func (n *Node) liveSnapshot() []liveAnnouncement {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]liveAnnouncement, 0, len(n.live))
	now := time.Now()
	for name, rec := range n.live {
		mark := n.withdrawn[name]
		if mark.epoch > 0 {
			// The refresh this snapshot feeds compares against this epoch,
			// and its store takes network time; a young mark is one the
			// pruning leaves alone while that is still in flight.
			mark.at = now
			n.withdrawn[name] = mark
		}
		out = append(out, liveAnnouncement{rec: rec, epoch: mark.epoch})
	}
	return out
}

func (n *Node) logf(format string, args ...any) {
	if n.cfg.Logger != nil {
		n.cfg.Logger.Printf(format, args...)
	}
}
