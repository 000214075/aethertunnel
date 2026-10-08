package dht

import (
	"testing"
	"time"
)

// A remote STORE may replace a local record once that record has expired: the
// slot no longer holds something this node announces, and refusing the STORE left
// it occupied by nothing — add() resets an expired entry, put() did not.
func TestARemoteStoreReplacesAnExpiredLocalRecord(t *testing.T) {
	store := newValueStore()
	expired := time.Now().Add(-time.Minute)
	if !store.put("key", []byte("local"), expired, true) {
		t.Fatal("the local record was refused a slot")
	}
	if !store.put("key", []byte("remote"), time.Now().Add(time.Minute), false) {
		t.Fatal("a remote STORE was refused while the local record had expired")
	}
	value, ok := store.get("key")
	if !ok || string(value) != "remote" {
		t.Fatalf("the slot holds %q (present %v), want the remote value", value, ok)
	}
}

// While the local record is unexpired it is still what this node announces, so a
// remote STORE must not take the slot from it.
func TestAnUnexpiredLocalRecordStillRefusesARemoteStore(t *testing.T) {
	store := newValueStore()
	if !store.put("key", []byte("local"), time.Now().Add(time.Minute), true) {
		t.Fatal("the local record was refused a slot")
	}
	if store.put("key", []byte("remote"), time.Now().Add(time.Minute), false) {
		t.Fatal("a remote STORE replaced a local record that had not expired")
	}
	value, ok := store.get("key")
	if !ok || string(value) != "local" {
		t.Fatalf("the slot holds %q (present %v), want the local value", value, ok)
	}
}

// A probe or a lookup that fails says nothing about a contact that re-registered
// from a new address while it was in flight: removing by ID alone evicted a node
// that was answering.
func TestAFailedProbeKeepsAContactThatMoved(t *testing.T) {
	self := NewIDFromBytes([]byte("self"))
	rt := newRoutingTable(self, 2)
	peer := NewIDFromBytes([]byte("peer"))

	rt.seen(peer, "192.0.2.1:1")
	// The same node speaks from somewhere new: seen() rewrites the entry in place.
	rt.seen(peer, "192.0.2.2:2")

	if rt.removeIf(peer, "192.0.2.1:1") {
		t.Fatal("the contact was removed although the failure was for its old address")
	}
	if rt.len() != 1 {
		t.Fatalf("the routing table holds %d contacts, want the one that moved", rt.len())
	}
	if !rt.removeIf(peer, "192.0.2.2:2") {
		t.Fatal("the contact was kept although the failure was for the address it answers from")
	}
	if rt.len() != 0 {
		t.Fatalf("the routing table still holds %d contacts", rt.len())
	}
}
