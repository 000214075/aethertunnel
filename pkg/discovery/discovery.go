// Package discovery publishes and resolves AetherTunnel proxy names through the
// Kademlia DHT in pkg/dht, so a client can find the server that publishes a proxy
// without being configured with that server's address.
//
// A server stores a small JSON record under "<namespace>/proxy/<name>"; any node
// that can reach the DHT resolves the same key to read it. The record carries its
// own expiry because the DHT has no remote delete: a server that stops announcing
// a name leaves a copy behind on its peers until the TTL runs out, and readers
// treat a record whose expiry has passed as absent rather than as a live address.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/dht"
)

const (
	// DefaultNamespace prefixes every key this package writes.
	DefaultNamespace = "aethertunnel"
	// DefaultAnnounceTTL is how long a reader honours an announcement.
	DefaultAnnounceTTL = 90 * time.Second
	// DefaultRepublishInterval is how often live announcements are rewritten. It
	// has to be shorter than the announce TTL so a running server never lets one
	// lapse.
	DefaultRepublishInterval = 30 * time.Second
	// DefaultLookupTimeout bounds a single resolution from start to finish.
	DefaultLookupTimeout = 5 * time.Second
)

// ErrStale reports a record that exists but whose announcing server has stopped
// refreshing it.
var ErrStale = errors.New("discovery: the announcement is stale")

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
}

// Fresh reports whether the record was still current at now.
func (r Record) Fresh(now time.Time) bool { return now.Before(r.Expires) }

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
	// AnnounceTTL is how long a reader honours an announcement.
	AnnounceTTL time.Duration
	// RepublishInterval is how often live announcements are rewritten.
	RepublishInterval time.Duration
	// LookupTimeout bounds one resolution.
	LookupTimeout time.Duration
	// Logger receives diagnostic lines. nil disables logging.
	Logger *log.Logger
}

// Node is a DHT node with the tunnel's key namespace applied.
//
// Every method is safe for concurrent use.
type Node struct {
	table *dht.Table
	cfg   Config

	mu   sync.Mutex
	live map[string]Record

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
		table:  table,
		cfg:    cfg,
		live:   make(map[string]Record),
		ctx:    ctx,
		cancel: cancel,
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
		c.RepublishInterval = DefaultRepublishInterval
	}
	if c.LookupTimeout <= 0 {
		c.LookupTimeout = DefaultLookupTimeout
	}
	return c
}

// Key is the DHT key a proxy name is published under.
func (n *Node) Key(name string) string {
	return n.cfg.Namespace + "/proxy/" + name
}

// Publish stores an announcement for a proxy name, replacing any earlier one.
//
// The timestamp and expiry are set here, so a caller cannot publish a record that
// claims a validity it does not have.
func (n *Node) Publish(ctx context.Context, rec Record) error {
	if rec.Name == "" {
		return errors.New("discovery: an announcement needs a name")
	}
	if rec.Server == "" {
		return errors.New("discovery: an announcement needs a server address")
	}

	now := time.Now()
	rec.Updated = now
	rec.Expires = now.Add(n.cfg.AnnounceTTL)

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

	n.mu.Lock()
	n.live[rec.Name] = rec
	n.mu.Unlock()
	return nil
}

// Withdraw stops announcing a name. The local record is dropped so it is no longer
// republished, and copies already replicated to peers lapse within one TTL.
func (n *Node) Withdraw(name string) error {
	n.mu.Lock()
	delete(n.live, name)
	n.mu.Unlock()

	if err := n.table.Forget(n.Key(name)); err != nil {
		return fmt.Errorf("discovery: withdraw %q: %w", name, err)
	}
	return nil
}

// Lookup resolves a proxy name. It returns ErrStale when the record is there but
// its announcer stopped refreshing it, and dht.ErrNotFound when nothing is known.
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
			for _, rec := range n.liveRecords() {
				ctx, cancel := context.WithTimeout(n.ctx, n.cfg.LookupTimeout)
				err := n.Publish(ctx, rec)
				cancel()
				if err != nil {
					n.logf("discovery: cannot refresh %q: %v", rec.Name, err)
				}
			}
		}
	}
}

func (n *Node) liveRecords() []Record {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Record, 0, len(n.live))
	for _, rec := range n.live {
		out = append(out, rec)
	}
	return out
}

func (n *Node) logf(format string, args ...any) {
	if n.cfg.Logger != nil {
		n.cfg.Logger.Printf(format, args...)
	}
}
