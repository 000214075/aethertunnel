package ledger

import (
	"testing"
)

// The ledger's file holds the whole chain and its memory holds only the most
// recent entries plus the running totals. A chain that outgrew the retention
// window used to be held in memory in full and re-summarised on every
// dashboard poll, which grew without bound on a server that runs for months.
// The visible contract stays the same: the full count, the head, the whole
// chain's per-client totals and a tail long enough for every request.
func TestALedgerReportsTheWholeChainWhileMemoryHoldsTheTail(t *testing.T) {
	key, _ := testKey(t)
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = l.Close() }()

	const total = 5200 // more than ledgerRetainEntries
	all := make([]Entry, 0, total)
	for i := 0; i < total; i++ {
		entry, err := l.Append("client", "proxy", int64(i), int64(total-i))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		all = append(all, entry)
	}

	if got := l.Len(); got != total {
		t.Fatalf("Len() = %d, want the whole chain of %d", got, total)
	}
	tail := l.Entries()
	if len(tail) != ledgerRetainEntries {
		t.Fatalf("memory holds %d entries, want the retention window of %d", len(tail), ledgerRetainEntries)
	}
	if tail[0].Index != total-ledgerRetainEntries {
		t.Fatalf("the tail starts at index %d, want %d", tail[0].Index, total-ledgerRetainEntries)
	}
	if tail[len(tail)-1].Hash != all[total-1].Hash {
		t.Fatal("the tail does not end at the chain's head")
	}

	// The next append continues the whole chain, not the tail.
	next, err := l.Append("client", "proxy", 1, 2)
	if err != nil {
		t.Fatalf("append after the trim: %v", err)
	}
	if next.Index != total {
		t.Fatalf("the entry after the trim has index %d, want %d", next.Index, total)
	}
	if next.PrevHash != all[total-1].Hash {
		t.Fatal("the entry after the trim does not link to the chain's head")
	}

	// The running totals equal a fresh pass over the whole chain. Snapshot
	// reports them beside the tail, the count and the head.
	all = append(all, next)
	want := Summarise(all)
	_, _, _, full := l.Snapshot()
	if len(full) != len(want) {
		t.Fatalf("the running totals hold %d client(s), the whole chain has %d", len(full), len(want))
	}
	for id, sum := range want {
		got := full[id]
		if got != sum {
			t.Fatalf("client %q: running totals %+v, want %+v", id, got, sum)
		}
	}
}

// A proof for an entry that memory no longer holds is rebuilt from the ledger
// file, so a holder of the public key can still verify any entry of the chain.
func TestAProofForAnEntryPastTheTailComesFromTheFile(t *testing.T) {
	dir := t.TempDir()
	key, _ := testKey(t)
	path := dir + "/ledger.jsonl"

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const total = 6000
	for i := 0; i < total; i++ {
		if _, err := l.Append("client", "proxy", int64(i), 0); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	old, err := Proof(l, 10)
	if err != nil {
		t.Fatalf("Proof(10): %v", err)
	}
	if len(old) != 11 {
		t.Fatalf("Proof(10) returned %d entries, want 11", len(old))
	}
	recent, err := Proof(l, total-1)
	if err != nil {
		t.Fatalf("Proof(%d): %v", total-1, err)
	}
	if len(recent) != total {
		t.Fatalf("Proof(%d) returned %d entries, want %d", total-1, len(recent), total)
	}

	// And the file keeps verifying as the whole chain after a reload.
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := New(path, key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Len(); got != total {
		t.Fatalf("the reopened ledger reports %d entries, want %d", got, total)
	}
	if tail := reopened.Entries(); len(tail) != ledgerRetainEntries {
		t.Fatalf("the reopened ledger holds %d entries in memory, want %d", len(tail), ledgerRetainEntries)
	}
}
