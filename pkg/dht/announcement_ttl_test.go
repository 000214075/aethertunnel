package dht

import (
	"testing"
	"time"
)

// A provider slot is the union of this node's address and the ones other peers
// announced, and a lookup answers from the whole set. A remote announcement
// carrying an early expiry used to shorten the slot, which took every address in
// it — including the node's own — out of service until the next republish. Only
// the node's own record was protected before.
func TestARemoteAnnouncementCannotShortenAnUnexpiredProviderSlot(t *testing.T) {
	store := newValueStore()
	slot := storeKey(nsProvider, keyFor(nsProvider, "example.com:4000"))
	now := time.Now()
	keep := now.Add(10 * time.Minute)

	if !store.add(slot, "10.0.0.1:4000", keep, false) {
		t.Fatal("the first provider announcement was refused")
	}
	if !store.add(slot, "10.0.0.2:4000", now.Add(time.Second), false) {
		t.Fatal("the second provider announcement was refused")
	}

	store.mu.Lock()
	got := store.entries[slot].expires
	store.mu.Unlock()
	if got.Before(keep) {
		t.Fatalf("a remote announcement shortened the slot to %s from %s: every address in it "+
			"goes out of service until the next republish", got.Sub(now), keep.Sub(now))
	}
}
