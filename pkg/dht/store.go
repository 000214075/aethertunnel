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
}

func newValueStore() *valueStore {
	return &valueStore{entries: make(map[string]entry)}
}

// put replaces the slot. The value is copied so callers cannot mutate stored
// bytes after the fact.
func (s *valueStore) put(key string, value []byte, expires time.Time, local bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = entry{value: append([]byte(nil), value...), expires: expires, local: local}
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
// announcement adds to the list instead of replacing it.
func (s *valueStore) add(key, addr string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.entries[key]
	if !time.Now().Before(prev.expires) {
		prev = entry{}
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
	// Keep the record inside the value limit by dropping the least recently
	// announced addresses.
	for len(list) > 1 && len(strings.Join(list, "\n")) > MaxValueSize {
		list = list[:len(list)-1]
	}
	s.entries[key] = entry{value: []byte(strings.Join(list, "\n")), expires: expires, local: prev.local}
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
