package dht

import (
	"strings"
	"sync"
	"time"
)

// Key namespaces. The namespace byte is part of the storage slot and of the
// hashed key, so a plain value and a provider record for the same user-visible
// name can neither overwrite nor resolve to each other.
const (
	nsValue    byte = 0
	nsProvider byte = 1
)

// keyFor derives the 160-bit key a name is stored under in a namespace.
func keyFor(ns byte, name string) ID {
	buf := make([]byte, 0, idLen+len(name))
	buf = append(buf, ns)
	buf = append(buf, name...)
	return NewIDFromBytes(buf)
}

// storeKey is the map key of a slot: the namespace byte followed by the ID.
func storeKey(ns byte, id ID) string {
	buf := make([]byte, 0, 1+idLen)
	buf = append(buf, ns)
	buf = append(buf, id[:]...)
	return string(buf)
}

type entry struct {
	value   []byte
	expires time.Time
	// local marks a record this node owns (Put or Provide), which is what the
	// refresh loop republishes.
	local bool
}

// storedRecord is a record this node owns, in the form the wire protocol needs.
type storedRecord struct {
	ns    byte
	id    ID
	value []byte
}

type valueStore struct {
	mu      sync.Mutex
	entries map[string]entry
	// fullScanAt is when the last full-table sweep to make room ran, the
	// throttle on hasRoomLocked's walk of the map.
	fullScanAt time.Time
}

func newValueStore() *valueStore {
	return &valueStore{entries: make(map[string]entry)}
}

// maxStoredEntries caps how many records this node holds. Each entry is at
// most MaxValueSize bytes, so the cap bounds the store at a bounded amount of
// memory even under a flood of well-formed STORE datagrams from an
// unauthenticated peer. Local records are exempt: how many of those exist is
// the node owner's own choice, not an attacker's.
const maxStoredEntries = 16384

// put replaces the slot. The value is copied so callers cannot mutate stored
// bytes after the fact. It returns false when the caller's record is a remote
// one and the store is full of unexpired entries.
func (s *valueStore) put(key string, value []byte, expires time.Time, local bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.entries[key]; ok && prev.local && !local && time.Now().Before(prev.expires) {
		// A record this node owns is not replaced by a remote STORE. The local flag
		// is what the refresh loop republishes, so clearing it would stop this node
		// announcing its own name: one unauthenticated datagram naming the proxy,
		// which is public, would unpublish it. A provider record already keeps the
		// flag this way; a value record did not.
		//
		// Only while it is unexpired: once the entry in the slot has lapsed it is
		// not a record this node still announces, and refusing the STORE left the
		// slot occupied by nothing — add() resets an expired entry, this did not.
		return false
	}
	if !local {
		if _, ok := s.entries[key]; !ok && !s.hasRoomLocked() {
			return false
		}
	}
	s.entries[key] = entry{value: append([]byte(nil), value...), expires: expires, local: local}
	return true
}

// fullScanInterval throttles the whole-table walk hasRoomLocked makes once the
// store is full. Every new key of an unauthenticated STORE flood reaches that
// walk, and it holds s.mu, which get and put share: without the throttle a full
// store turns each datagram into a scan of 16384 entries that stalls every
// reader behind it. The periodic sweep reclaims the same expired entries, so a
// scan skipped here only delays a slot being freed until the next interval.
const fullScanInterval = time.Second

// hasRoomLocked drops expired entries to make room and reports whether the
// store is below its cap.
func (s *valueStore) hasRoomLocked() bool {
	if len(s.entries) < maxStoredEntries {
		return true
	}
	now := time.Now()
	if now.Sub(s.fullScanAt) < fullScanInterval {
		return false
	}
	s.fullScanAt = now
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
	return len(s.entries) < maxStoredEntries
}

// Sweep drops every expired entry. It runs on the periodic refresh loop: an
// expired record that nobody ever reads again would otherwise sit in the map
// forever, and a flood of distinct keys would fill the store with entries
// nothing will ever ask for.
func (s *valueStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
}

// get returns a copy of the value, dropping the slot when it has expired.
func (s *valueStore) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if !time.Now().Before(e.expires) {
		delete(s.entries, key)
		return nil, false
	}
	return append([]byte(nil), e.value...), true
}

// add merges a provider announcement into the slot. Provider records are sets
// of newline separated addresses because several nodes may serve one key, so an
// announcement adds to the list instead of replacing it. It returns false when
// the caller's record is a remote one and the store is full of unexpired
// entries. local marks a record this node owns through Provide, which is what
// the refresh loop republishes; a remote merge never clears that mark.
func (s *valueStore) add(key, addr string, expires time.Time, local bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.entries[key]
	if !time.Now().Before(prev.expires) {
		prev = entry{}
	}
	// A brand-new slot needs room; merging into an existing one does not,
	// because the value limit below bounds its size.
	if !local {
		if _, ok := s.entries[key]; !ok && !s.hasRoomLocked() {
			return false
		}
	}
	if !local && expires.Before(prev.expires) {
		// The wire accepts any expiry, so a remote announcement carrying one in
		// the past would expire the slot early. A provider slot holds the union
		// of this node's own address and the ones other peers announced, and a
		// lookup answers from the whole set, so one peer could take every
		// address in it out of service until the next republish — ten minutes by
		// default, this node's own record included. Merging keeps the later
		// expiry: a peer does not get to shorten the life of an unexpired slot.
		expires = prev.expires
	}
	list := splitAddresses(string(prev.value))
	known := false
	for _, a := range list {
		if a == addr {
			known = true
			break
		}
	}
	if !known {
		list = append(list, addr)
	}
	if prev.local && !local && len(strings.Join(list, "\n")) > MaxValueSize {
		// This node's own address is the oldest in the list, so the trim below is
		// what would drop it, and the record would no longer point at this node.
		// The local announcement wins the slot instead.
		return false
	}
	// Keep the record inside the value limit by dropping the least recently
	// announced addresses: the address being announced right now was appended
	// last, so the head of the list is the oldest one.
	for len(list) > 1 && len(strings.Join(list, "\n")) > MaxValueSize {
		list = list[1:]
	}
	s.entries[key] = entry{value: []byte(strings.Join(list, "\n")), expires: expires, local: prev.local || local}
	return true
}

// remove drops a slot, which stops this node from republishing it.
func (s *valueStore) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// localRecords lists the unexpired records this node owns.
func (s *valueStore) localRecords() []storedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []storedRecord
	for k, e := range s.entries {
		if len(k) != 1+idLen || !e.local || !now.Before(e.expires) {
			continue
		}
		var id ID
		copy(id[:], k[1:])
		out = append(out, storedRecord{ns: k[0], id: id, value: append([]byte(nil), e.value...)})
	}
	return out
}

// splitAddresses parses a provider record value.
func splitAddresses(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
