package discovery

import (
	"testing"
	"time"
)

// The withdrawal marks used to live forever: one entry per name ever
// withdrawn, for the process's whole life. The republish loop now prunes the
// marks no live record and no in-flight publish can still reach.
func TestWithdrawnMarksArePrunedOnceNoPublishCanReachThem(t *testing.T) {
	now := time.Now()
	n := &Node{
		live:      map[string]Record{"kept": {}},
		withdrawn: map[string]withdrawMark{},
	}
	n.withdrawn["gone"] = withdrawMark{epoch: 3, at: now.Add(-withdrawnRetention - time.Minute)}
	n.withdrawn["fresh"] = withdrawMark{epoch: 1, at: now}
	n.withdrawn["kept"] = withdrawMark{epoch: 2, at: now.Add(-withdrawnRetention - time.Minute)}

	n.pruneWithdrawn(now)

	if _, ok := n.withdrawn["gone"]; ok {
		t.Fatal("the stale mark of a name that is gone was kept")
	}
	if _, ok := n.withdrawn["fresh"]; !ok {
		t.Fatal("a mark a publish could still be comparing was pruned")
	}
	if _, ok := n.withdrawn["kept"]; !ok {
		t.Fatal("the mark of a live name was pruned")
	}
}

// A publish reads the mark under the lock and compares against it after its
// store; the read refreshes the mark's timestamp, so the pruning leaves that
// very mark alone while the store is in flight.
func TestALiveSnapshotTouchesTheMarksItReads(t *testing.T) {
	before := time.Now().Add(-withdrawnRetention - time.Minute)
	n := &Node{
		live:      map[string]Record{"web": {}},
		withdrawn: map[string]withdrawMark{"web": {epoch: 5, at: before}},
	}

	snapshot := n.liveSnapshot()
	if len(snapshot) != 1 || snapshot[0].epoch != 5 {
		t.Fatalf("the snapshot lost the epoch: %+v", snapshot)
	}
	if mark := n.withdrawn["web"]; !mark.at.After(before) {
		t.Fatal("reading the mark left its timestamp untouched, so a prune could hit it mid-publish")
	}
}
