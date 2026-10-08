package ledger

import (
	"testing"
)

// A proof for an entry that the trimmed memory window still covers must be the
// chain's real prefix. Memory holds the most recent entries once the chain
// outgrew the retention window, so slicing it handed back the tail's first
// entries under indices they do not have: Verify refused every such proof for
// an old entry even though the file could have answered it.
func TestAProofUnderTheTrimmedWindowIsTheChainsRealPrefix(t *testing.T) {
	dir := t.TempDir()
	key, pub := testKey(t)
	path := dir + "/ledger.jsonl"

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const total = 6000 // more than ledgerRetainEntries, so memory holds a tail
	for i := 0; i < total; i++ {
		if _, err := l.Append("client", "proxy", int64(i), 0); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	for _, index := range []int{0, 10, 5000, total - 1} {
		prefix, err := Proof(l, index)
		if err != nil {
			t.Fatalf("Proof(%d): %v", index, err)
		}
		if len(prefix) != index+1 {
			t.Fatalf("Proof(%d) returned %d entries, want %d", index, len(prefix), index+1)
		}
		if prefix[0].Index != 0 || prefix[index].Index != uint64(index) {
			t.Fatalf("Proof(%d) covers indices %d..%d, want 0..%d",
				index, prefix[0].Index, prefix[index].Index, index)
		}
		if n, err := Verify(prefix, pub); err != nil || n != len(prefix) {
			t.Fatalf("Proof(%d) does not verify: %d entries checked: %v", index, n, err)
		}
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
