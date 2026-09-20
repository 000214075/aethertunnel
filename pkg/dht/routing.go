package dht

import (
	"bytes"
	"sort"
	"sync"
	"time"
)

// contact is a routing table entry. lastSeen orders a bucket: the entry at the
// head answered most recently and is evicted last.
type contact struct {
	id       ID
	addr     string
	lastSeen time.Time
}

func (c contact) node() Node { return Node{ID: c.id, Addr: c.addr, LastSeen: c.lastSeen} }

// bucket holds the contacts whose ID shares exactly the bucket index in common
// prefix bits with the local ID, together with a cache of replacement contacts
// that did not fit. Both slices are ordered most recently seen first.
type bucket struct {
	entries      []*contact
	replacements []*contact
	touched      time.Time
}

// routingTable is the 160 k-bucket table of one node.
type routingTable struct {
	mu      sync.RWMutex
	self    ID
	k       int
	buckets [idLen * 8]bucket
}

func newRoutingTable(self ID, k int) *routingTable {
	t := &routingTable{self: self, k: k}
	now := time.Now()
	for i := range t.buckets {
		t.buckets[i].touched = now
	}
	return t
}

// bucketIndex maps a contact to its bucket, or -1 for the local ID.
func (t *routingTable) bucketIndex(id ID) int {
	if n := t.self.CommonPrefixLen(id); n < idLen*8 {
		return n
	}
	return -1
}

// seen records that id answered from addr and returns the contact the caller
// must probe before it can be replaced, when the target bucket was already
// full. A nil result means no eviction is pending.
func (t *routingTable) seen(id ID, addr string) *contact {
	idx := t.bucketIndex(id)
	if idx < 0 {
		return nil
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	b := &t.buckets[idx]
	b.touched = now
	for i, e := range b.entries {
		if e.id != id {
			continue
		}
		e.addr = addr
		e.lastSeen = now
		if i > 0 {
			copy(b.entries[1:i+1], b.entries[:i])
			b.entries[0] = e
		}
		return nil
	}
	fresh := &contact{id: id, addr: addr, lastSeen: now}
	if len(b.entries) < t.k {
		b.entries = append(b.entries, nil)
		copy(b.entries[1:], b.entries[:len(b.entries)-1])
		b.entries[0] = fresh
		return nil
	}
	// The bucket is full: the newcomer waits in the replacement cache while the
	// caller decides the fate of the least recently seen occupant.
	b.dropReplacement(fresh.id)
	b.replacements = append(b.replacements, nil)
	copy(b.replacements[1:], b.replacements[:len(b.replacements)-1])
	b.replacements[0] = fresh
	if len(b.replacements) > t.k {
		b.replacements = b.replacements[:t.k]
	}
	return b.entries[len(b.entries)-1]
}

// remove drops a contact that stopped answering and promotes the most recently
// seen replacement into the freed slot.
func (t *routingTable) remove(id ID) {
	idx := t.bucketIndex(id)
	if idx < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := &t.buckets[idx]
	for i, e := range b.entries {
		if e.id != id {
			continue
		}
		b.entries = append(b.entries[:i], b.entries[i+1:]...)
		if len(b.replacements) > 0 {
			promoted := b.replacements[0]
			b.replacements = b.replacements[1:]
			b.entries = append(b.entries, nil)
			copy(b.entries[1:], b.entries[:len(b.entries)-1])
			b.entries[0] = promoted
		}
		return
	}
}

// dropReplacement removes a pending replacement with the same ID.
func (b *bucket) dropReplacement(id ID) {
	for i, e := range b.replacements {
		if e.id == id {
			b.replacements = append(b.replacements[:i], b.replacements[i+1:]...)
			return
		}
	}
}

// closest returns up to n contacts ordered by XOR distance to target, nearest
// first, with the ID as tie breaker so the order is total.
func (t *routingTable) closest(target ID, n int) []Node {
	if n <= 0 {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Node, 0, t.countLocked())
	for i := range t.buckets {
		for _, e := range t.buckets[i].entries {
			out = append(out, e.node())
		}
	}
	sortNodes(out, target)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (t *routingTable) len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.countLocked()
}

func (t *routingTable) countLocked() int {
	n := 0
	for i := range t.buckets {
		n += len(t.buckets[i].entries)
	}
	return n
}

// takeStaleBuckets returns the indices of non-empty buckets that have not been
// touched for at least olderThan, marking them as refreshed so a slow bucket
// cannot starve the others.
func (t *routingTable) takeStaleBuckets(olderThan time.Duration) []int {
	cutoff := time.Now().Add(-olderThan)
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []int
	for i := range t.buckets {
		if len(t.buckets[i].entries) == 0 {
			continue
		}
		if t.buckets[i].touched.Before(cutoff) {
			t.buckets[i].touched = time.Now()
			out = append(out, i)
		}
	}
	return out
}

// randomIDInBucket returns a random ID that falls in bucket idx.
func (t *routingTable) randomIDInBucket(idx int) ID {
	id := RandomID()
	for i := 0; i < idx; i++ {
		if bitAt(t.self[:], i) {
			setBit(id[:], i)
		} else {
			clearBit(id[:], i)
		}
	}
	if bitAt(t.self[:], idx) {
		clearBit(id[:], idx)
	} else {
		setBit(id[:], idx)
	}
	return id
}

// bitAt reports the i-th most significant bit of b, the same order
// CommonPrefixLen counts in.
func bitAt(b []byte, i int) bool { return b[i/8]&(1<<uint(7-i%8)) != 0 }

func setBit(b []byte, i int)   { b[i/8] |= 1 << uint(7-i%8) }
func clearBit(b []byte, i int) { b[i/8] &^= 1 << uint(7-i%8) }

// sortNodes orders nodes by XOR distance to target.
func sortNodes(nodes []Node, target ID) {
	sort.Slice(nodes, func(i, j int) bool {
		return lessDistance(nodes[i].ID, nodes[j].ID, target)
	})
}

// lessDistance reports whether a is closer to target than b.
func lessDistance(a, b, target ID) bool {
	da, db := a.Distance(target), b.Distance(target)
	if c := bytes.Compare(da[:], db[:]); c != 0 {
		return c < 0
	}
	return bytes.Compare(a[:], b[:]) < 0
}
