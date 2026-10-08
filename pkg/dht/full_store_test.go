package dht

import (
	"fmt"
	"testing"
	"time"
)

// A full store used to walk its whole table inside hasRoomLocked on every
// remote STORE for a key it did not already hold, and that walk holds the lock
// get and put share: the DHT's UDP port takes STOREs from anyone, so a flood of
// distinct keys turned each datagram into a scan of 16384 entries that every
// reader queued behind. The scan is throttled now; the periodic sweep reclaims
// the same expired entries, so a skipped scan only delays a slot being freed.
func TestAFullStoreDoesNotScanItsWholeTableForEveryRemoteKey(t *testing.T) {
	store := newValueStore()
	expires := time.Now().Add(time.Hour)
	for i := 0; i < maxStoredEntries; i++ {
		if !store.put(fmt.Sprintf("key-%d", i), []byte{1}, expires, false) {
			t.Fatalf("the store refused key %d before it was full", i)
		}
	}

	// The first remote key of a full store may scan: nothing has been scanned
	// yet, so the throttle lets it through.
	if store.put("first-new-key", []byte{1}, expires, false) {
		t.Fatal("a full store of unexpired entries accepted a new key")
	}
	firstScan := store.fullScanAt
	if firstScan.IsZero() {
		t.Fatal("a full store did not scan the table once to look for expired entries")
	}

	time.Sleep(20 * time.Millisecond)
	if store.put("second-new-key", []byte{1}, expires, false) {
		t.Fatal("a full store of unexpired entries accepted a new key")
	}
	if !store.fullScanAt.Equal(firstScan) {
		t.Errorf("the second remote key scanned the whole table again (%v became %v)", firstScan, store.fullScanAt)
	}

	// The throttle must not remove the reclaiming the scan does: once the
	// entries have expired and the interval has passed, a new key is accepted
	// again.
	store.mu.Lock()
	for k, e := range store.entries {
		e.expires = time.Now().Add(-time.Minute)
		store.entries[k] = e
	}
	store.mu.Unlock()
	store.fullScanAt = time.Now().Add(-2 * fullScanInterval)
	if !store.put("third-new-key", []byte{1}, expires, false) {
		t.Fatal("the store refused a new key after every entry in it had expired")
	}
}
