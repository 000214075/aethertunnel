// Package dht implements a Kademlia distributed hash table over UDP.
//
// The tunnel server uses it to advertise which nodes can serve a named proxy and
// to let clients find each other without a central directory: a provider
// announces a key, and any node resolves that key by walking its routing table
// towards the nodes closest to it. Stored values are opaque byte strings that
// expire after Config.TTL; provider records live in a separate key namespace so
// an announcement can never shadow a value stored under the same user-visible
// name.
//
// Every exported method is safe for concurrent use, and the zero Config is a
// valid configuration.
package dht

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/bits"
	"time"
)

// ID is a 160-bit node or key identifier.
type ID [20]byte

// idLen is the size of an ID in bytes; the keyspace is idLen*8 bits wide.
const idLen = 20

// NewIDFromBytes derives an ID from arbitrary bytes via SHA-1.
func NewIDFromBytes(b []byte) ID { return sha1.Sum(b) }

// RandomID returns a fresh random ID.
func RandomID() ID {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		// crypto/rand only fails when the OS entropy source is unusable, in
		// which case no usable ID can be produced.
		panic("dht: random id: " + err.Error())
	}
	return id
}

// IDFromString parses a 40-character hex ID.
func IDFromString(s string) (ID, error) {
	var id ID
	if len(s) != idLen*2 {
		return id, fmt.Errorf("dht: id %q: want %d hex characters, got %d", s, idLen*2, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("dht: id %q: %w", s, err)
	}
	copy(id[:], b)
	return id, nil
}

// String renders the ID as hex.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Distance returns the XOR distance to other, big endian.
func (id ID) Distance(other ID) ID {
	var d ID
	for i := range d {
		d[i] = id[i] ^ other[i]
	}
	return d
}

// CommonPrefixLen returns the number of leading equal bits.
func (id ID) CommonPrefixLen(other ID) int {
	for i := range id {
		if x := id[i] ^ other[i]; x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return idLen * 8
}

// isZero reports whether the ID is the zero ID.
func (id ID) isZero() bool { return id == ID{} }

const (
	// DefaultK is the bucket size and the replication factor.
	DefaultK = 20
	// DefaultAlpha is the lookup concurrency.
	DefaultAlpha = 3
	// DefaultRefreshInterval is how often buckets are refreshed and locally
	// owned records are republished.
	DefaultRefreshInterval = 10 * time.Minute
	// DefaultTTL is how long a stored value lives.
	DefaultTTL = time.Hour
)

// MaxValueSize is the largest storable value.
const MaxValueSize = 1024

var (
	// ErrNotFound is returned when a key has no value.
	ErrNotFound = errors.New("dht: key not found")
	// ErrValueTooLarge is returned by Put when the value exceeds MaxValueSize.
	ErrValueTooLarge = fmt.Errorf("dht: value larger than %d bytes", MaxValueSize)
	// ErrNotStarted is returned by operations that need a bound socket before
	// Start succeeded.
	ErrNotStarted = errors.New("dht: table not started")
	// ErrClosed is returned by operations on a closed table.
	ErrClosed = errors.New("dht: table closed")
	// ErrTimeout is returned when a peer does not answer within the request
	// timeout.
	ErrTimeout = errors.New("dht: request timed out")
)

// Config configures a node. The zero value is usable.
type Config struct {
	// ListenAddr is the UDP address to bind. ":0" chooses a port.
	ListenAddr string
	// ID is the node's identifier. The zero ID selects a random one.
	ID ID
	// Bootstrap lists nodes to contact on Start.
	Bootstrap []string
	// K is the bucket size. Zero selects DefaultK (20).
	K int
	// Alpha is the lookup concurrency. Zero selects DefaultAlpha (3).
	Alpha int
	// RefreshInterval re-publishes and refreshes buckets. Zero selects
	// DefaultRefreshInterval (10 * time.Minute).
	RefreshInterval time.Duration
	// TTL is how long a stored value lives. Zero selects DefaultTTL (1h).
	TTL time.Duration
	// Logger receives diagnostic lines. nil disables logging.
	Logger *log.Logger
}

// Node is a remote contact.
type Node struct {
	ID       ID
	Addr     string // host:port
	LastSeen time.Time
}
