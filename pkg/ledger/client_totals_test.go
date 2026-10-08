package ledger

import (
	"fmt"
	"path/filepath"
	"testing"
)

// A client_id is the client's to choose and may be 128 bytes long, so the map
// /api/ledger copies for the dashboard has to be bounded the way the entry tail
// already is — without losing the accounting: the traffic of the ids past the cap
// goes into one overflow bucket, so the totals still sum to what the chain
// carried.
func TestThePerClientTotalsStayBoundedHoweverManyClientIdsAppear(t *testing.T) {
	l := memoryLedger(t)
	defer func() { _ = l.Close() }()

	const extra = 40
	for i := 0; i < ledgerMaxClientTotals+extra; i++ {
		if _, err := l.Append(fmt.Sprintf("client-%05d", i), "web", 10, 20); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	_, _, count, totals := l.Snapshot()
	if count != ledgerMaxClientTotals+extra {
		t.Fatalf("the chain holds %d entries, want %d", count, ledgerMaxClientTotals+extra)
	}
	if len(totals) > ledgerMaxClientTotals+1 {
		t.Fatalf("%d buckets for %d client ids: the totals map is not bounded", len(totals), count)
	}
	if _, ok := totals[overflowClientID]; !ok {
		t.Fatalf("no %q bucket holds the ids that arrived past the cap", overflowClientID)
	}
	if _, ok := totals["client-00000"]; !ok {
		t.Fatal("an id that arrived before the cap lost its own bucket")
	}

	var bytes int64
	var entries int
	for _, sum := range totals {
		bytes += sum.BytesIn + sum.BytesOut
		entries += sum.Entries
	}
	if entries != count {
		t.Fatalf("the buckets account for %d entries, want %d", entries, count)
	}
	if want := int64(count * 30); bytes != want {
		t.Fatalf("the buckets account for %d bytes, want %d", bytes, want)
	}
}

// The totals are rebuilt from the file at startup, so the same rule has to hold
// there: otherwise a restart grows the map again, and the dashboard's answer
// depends on how many times the server has been restarted.
func TestTheBoundedTotalsAreRebuiltTheSameWayFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	key, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	first, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const extra = 25
	for i := 0; i < ledgerMaxClientTotals+extra; i++ {
		if _, err := first.Append(fmt.Sprintf("client-%05d", i), "web", 7, 11); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := New(path, key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	_, _, count, totals := reopened.Snapshot()
	if count != ledgerMaxClientTotals+extra {
		t.Fatalf("the reloaded chain holds %d entries, want %d", count, ledgerMaxClientTotals+extra)
	}
	if len(totals) > ledgerMaxClientTotals+1 {
		t.Fatalf("the reloaded totals hold %d buckets for %d client ids", len(totals), count)
	}
	if _, ok := totals[overflowClientID]; !ok {
		t.Fatalf("the reloaded totals have no %q bucket", overflowClientID)
	}
	var bytes int64
	for _, sum := range totals {
		bytes += sum.BytesIn + sum.BytesOut
	}
	if want := int64(count * 18); bytes != want {
		t.Fatalf("the reloaded buckets account for %d bytes, want %d", bytes, want)
	}
}

// memoryLedger is a ledger that writes nowhere, for tests that only care about
// what it keeps in memory.
func memoryLedger(t *testing.T) *Ledger {
	t.Helper()
	key, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}
