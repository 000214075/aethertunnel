package ledger

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey returns a fresh signing key and its public half.
func testKey(t *testing.T) (*SigningKey, ed25519.PublicKey) {
	t.Helper()
	key, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	return key, key.PublicKey()
}

// fill appends n entries and returns them.
func fill(t *testing.T, l *Ledger, n int) []Entry {
	t.Helper()
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		entry, err := l.Append("client-a", "example.com:443", int64(100+i), int64(200+i))
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestSigningKeySeedRoundTrip(t *testing.T) {
	key, pub := testKey(t)
	seed := key.Seed()
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("Seed length = %d, want %d", len(seed), ed25519.SeedSize)
	}

	restored, err := SigningKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("SigningKeyFromSeed: %v", err)
	}
	if string(restored.Seed()) != string(seed) {
		t.Fatal("restored seed differs from the original")
	}
	if string(restored.PublicKey()) != string(pub) {
		t.Fatal("restored public key differs from the original")
	}

	if _, err := SigningKeyFromSeed(seed[:16]); err == nil {
		t.Fatal("SigningKeyFromSeed accepted a short seed")
	}

	// Mutating the returned public key must not reach the stored key.
	mutated := key.PublicKey()
	mutated[0] ^= 0xff
	if key.PublicKey()[0] == mutated[0] {
		t.Fatal("PublicKey returned a shared buffer")
	}
}

func TestAppendAndVerify(t *testing.T) {
	key, pub := testKey(t)
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	if l.Len() != 0 || l.Head() != "" {
		t.Fatalf("empty ledger: Len = %d, Head = %q", l.Len(), l.Head())
	}

	appended := fill(t, l, 4)
	if l.Len() != 4 {
		t.Fatalf("Len = %d, want 4", l.Len())
	}
	if l.Head() != appended[3].Hash {
		t.Fatalf("Head = %q, want %q", l.Head(), appended[3].Hash)
	}

	entries := l.Entries()
	if len(entries) != len(appended) {
		t.Fatalf("Entries length = %d, want %d", len(entries), len(appended))
	}
	for i, entry := range entries {
		if entry.Index != uint64(i) {
			t.Fatalf("entry %d has index %d", i, entry.Index)
		}
		if entry.Time.IsZero() {
			t.Fatalf("entry %d has no timestamp", i)
		}
		if entry.Time.Location() != time.UTC {
			t.Fatalf("entry %d timestamp is not UTC", i)
		}
		if i == 0 && entry.PrevHash != "" {
			t.Fatalf("first entry has PrevHash %q", entry.PrevHash)
		}
		if i > 0 && entry.PrevHash != entries[i-1].Hash {
			t.Fatalf("entry %d does not link to entry %d", i, i-1)
		}
		if entry.Hash == "" || entry.Signature == "" {
			t.Fatalf("entry %d is missing hash or signature", i)
		}
	}

	count, err := Verify(entries, pub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if count != len(entries) {
		t.Fatalf("Verify count = %d, want %d", count, len(entries))
	}

	// Entries returns a copy, so a caller cannot rewrite the chain.
	entries[0].BytesIn = -1
	if l.Entries()[0].BytesIn != appended[0].BytesIn {
		t.Fatal("Entries handed out the ledger's own slice")
	}

	if n, err := Verify(nil, pub); n != 0 || err != nil {
		t.Fatalf("Verify(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestVerifyDetectsChangedByteCount(t *testing.T) {
	key, pub := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 3)

	entries := l.Entries()
	entries[1].BytesIn++

	count, err := Verify(entries, pub)
	if err == nil {
		t.Fatal("Verify accepted a chain with a modified byte count")
	}
	if count != 1 {
		t.Fatalf("Verify count = %d, want 1", count)
	}
	if !strings.Contains(err.Error(), "entry 1") {
		t.Fatalf("error does not name the entry: %v", err)
	}
}

func TestVerifyDetectsRemovedEntry(t *testing.T) {
	key, pub := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 3)

	entries := l.Entries()
	trimmed := append(entries[:1:1], entries[2:]...)

	if _, err := Verify(trimmed, pub); err == nil {
		t.Fatal("Verify accepted a chain with a removed entry")
	}
}

func TestVerifyDetectsReorderedPair(t *testing.T) {
	key, pub := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 4)

	entries := l.Entries()
	entries[1], entries[2] = entries[2], entries[1]

	if _, err := Verify(entries, pub); err == nil {
		t.Fatal("Verify accepted a chain with two entries swapped")
	}
}

// TestVerifyDetectsTruncation covers the one alteration a chain cannot report
// by itself: dropping the tail leaves a prefix that is completely valid, because
// every remaining entry still links to its predecessor and the first entry keeps
// its empty PrevHash. Nothing inside the prefix commits to the entries that used
// to follow it. The mechanism the package offers for this is the head hash:
// Verify(prefix) succeeds, but prefix[len-1].Hash (or Ledger.Head on the
// untruncated chain) no longer matches the head published out of band, and the
// entry count no longer matches the published count.
func TestVerifyDetectsTruncation(t *testing.T) {
	key, pub := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 4)

	publishedHead := l.Head()
	publishedCount := l.Len()

	truncated := l.Entries()[:3]
	if len(truncated) != 3 {
		t.Fatalf("truncated length = %d, want 3", len(truncated))
	}

	// The prefix verifies on its own; that is exactly why the published head is
	// needed as well.
	count, err := Verify(truncated, pub)
	if err != nil {
		t.Fatalf("Verify(prefix): %v", err)
	}
	if count != 3 {
		t.Fatalf("Verify(prefix) count = %d, want 3", count)
	}

	if truncated[len(truncated)-1].Hash == publishedHead {
		t.Fatal("truncated chain still ends at the published head")
	}
	if len(truncated) == publishedCount {
		t.Fatal("truncated chain still has the published length")
	}

	// The same check against the intact chain must pass.
	if l.Head() != publishedHead || l.Len() != publishedCount {
		t.Fatal("intact chain does not match the published head and count")
	}
}

func TestVerifyRejectsOtherKey(t *testing.T) {
	key, _ := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 3)

	_, otherPub := testKey(t)
	count, err := Verify(l.Entries(), otherPub)
	if err == nil {
		t.Fatal("Verify accepted a chain signed by a different key")
	}
	if count != 0 {
		t.Fatalf("Verify count = %d, want 0", count)
	}

	if _, err := Verify(l.Entries(), ed25519.PublicKey("short")); err == nil {
		t.Fatal("Verify accepted a malformed public key")
	}
}

func TestVerifyRejectsMalformedEntries(t *testing.T) {
	key, pub := testKey(t)
	l, _ := New("", key)
	defer l.Close()
	fill(t, l, 2)
	good := l.Entries()

	cases := []struct {
		name   string
		mutate func([]Entry) []Entry
	}{
		{"wrong index", func(e []Entry) []Entry {
			e[0].Index = 7
			return e
		}},
		{"bad hex hash", func(e []Entry) []Entry {
			e[0].Hash = "not-hex"
			return e
		}},
		{"bad hex signature", func(e []Entry) []Entry {
			e[0].Signature = "zz"
			return e
		}},
		{"short signature", func(e []Entry) []Entry {
			e[0].Signature = "aabb"
			return e
		}},
		{"empty entry fields", func(e []Entry) []Entry {
			return []Entry{{Index: 0}}
		}},
		{"index far out of range", func(e []Entry) []Entry {
			return []Entry{{Index: ^uint64(0), BytesIn: -1}}
		}},
		{"nil chain of one", func(e []Entry) []Entry {
			return append(e, Entry{})
		}},
	}
	for _, tc := range cases {
		entries := append([]Entry(nil), good...)
		if _, err := Verify(tc.mutate(entries), pub); err == nil {
			t.Fatalf("%s: Verify accepted a malformed chain", tc.name)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	key, pub := testKey(t)

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := fill(t, l, 3)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := l.Append("client-a", "example.com:443", 1, 1); err == nil {
		t.Fatal("Append on a closed ledger succeeded")
	}

	if info, err := os.Stat(path); err != nil {
		t.Fatalf("Stat: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("file is empty after appending")
	}

	reopened, err := New(path, key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if reopened.Len() != 3 {
		t.Fatalf("reopened Len = %d, want 3", reopened.Len())
	}
	if reopened.Head() != first[2].Hash {
		t.Fatalf("reopened Head = %q, want %q", reopened.Head(), first[2].Hash)
	}
	if count, err := Verify(reopened.Entries(), pub); err != nil || count != 3 {
		t.Fatalf("Verify after reopen = (%d, %v), want (3, nil)", count, err)
	}

	next, err := reopened.Append("client-b", "example.org:80", 7, 8)
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if next.Index != 3 {
		t.Fatalf("next index = %d, want 3", next.Index)
	}
	if next.PrevHash != first[2].Hash {
		t.Fatalf("next PrevHash = %q, want %q", next.PrevHash, first[2].Hash)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	final, err := New(path, key)
	if err != nil {
		t.Fatalf("reopen after continue: %v", err)
	}
	defer final.Close()
	if final.Len() != 4 {
		t.Fatalf("final Len = %d, want 4", final.Len())
	}
	entries := final.Entries()
	if count, err := Verify(entries, pub); err != nil || count != 4 {
		t.Fatalf("final Verify = (%d, %v), want (4, nil)", count, err)
	}
	if got := Summarise(entries)["client-b"]; got.BytesIn != 7 || got.BytesOut != 8 || got.Entries != 1 {
		t.Fatalf("client-b totals = %+v", got)
	}
}

func TestReopenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	key, _ := testKey(t)

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fill(t, l, 3)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	cases := map[string]struct {
		rewrite func([]string) []string
		want    string
	}{
		"changed byte count": {
			rewrite: func(lines []string) []string {
				var entry Entry
				if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
					t.Fatalf("unmarshal line 2: %v", err)
				}
				entry.BytesIn += 1024
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatalf("marshal line 2: %v", err)
				}
				lines[1] = string(encoded)
				return lines
			},
			want: "line 2",
		},
		"broken link": {
			rewrite: func(lines []string) []string {
				var entry Entry
				if err := json.Unmarshal([]byte(lines[2]), &entry); err != nil {
					t.Fatalf("unmarshal line 3: %v", err)
				}
				entry.PrevHash = strings.Repeat("0", 64)
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatalf("marshal line 3: %v", err)
				}
				lines[2] = string(encoded)
				return lines
			},
			want: "line 3",
		},
		"bad signature": {
			rewrite: func(lines []string) []string {
				var entry Entry
				if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
					t.Fatalf("unmarshal line 1: %v", err)
				}
				entry.Signature = strings.Repeat("ab", ed25519.SignatureSize)
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatalf("marshal line 1: %v", err)
				}
				lines[0] = string(encoded)
				return lines
			},
			want: "line 1",
		},
		"garbage line": {
			rewrite: func(lines []string) []string {
				lines[1] = "{not json"
				return lines
			},
			want: "line 2",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.TrimSuffix(string(original), "\n")
			lines := tc.rewrite(strings.Split(body, "\n"))
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			l, err := New(path, key)
			if err == nil {
				l.Close()
				t.Fatalf("%s: reopen accepted a corrupt file", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error %q does not name %q", name, err, tc.want)
			}
		})
	}
}

func TestReopenWithOtherKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	key, _ := testKey(t)

	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fill(t, l, 2)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	other, _ := testKey(t)
	if l, err := New(path, other); err == nil {
		l.Close()
		t.Fatal("reopen with a different key succeeded")
	}
}

func TestProof(t *testing.T) {
	key, pub := testKey(t)
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	appended := fill(t, l, 5)
	publishedHead := l.Head()

	for i := range appended {
		prefix, err := Proof(l, i)
		if err != nil {
			t.Fatalf("Proof(%d): %v", i, err)
		}
		if len(prefix) != i+1 {
			t.Fatalf("Proof(%d) length = %d, want %d", i, len(prefix), i+1)
		}
		count, err := Verify(prefix, pub)
		if err != nil || count != i+1 {
			t.Fatalf("Verify(Proof(%d)) = (%d, %v), want (%d, nil)", i, count, err, i+1)
		}
		if prefix[i].Hash != appended[i].Hash {
			t.Fatalf("Proof(%d) ends at %q, want %q", i, prefix[i].Hash, appended[i].Hash)
		}
		if prefix[i].ClientID != appended[i].ClientID || prefix[i].BytesIn != appended[i].BytesIn {
			t.Fatalf("Proof(%d) returned a different payload", i)
		}
	}

	// The last covered entry must be the published head, which is how a verifier
	// knows the proof was not taken from a shorter chain.
	last, err := Proof(l, l.Len()-1)
	if err != nil {
		t.Fatalf("Proof(last): %v", err)
	}
	if last[len(last)-1].Hash != publishedHead {
		t.Fatal("proof of the last entry does not reach the published head")
	}

	for _, index := range []int{-1, l.Len(), l.Len() + 100} {
		if prefix, err := Proof(l, index); err == nil {
			t.Fatalf("Proof(%d) = %d entries, want an error", index, len(prefix))
		}
	}

	if _, err := Proof(nil, 0); err == nil {
		t.Fatal("Proof on a nil ledger succeeded")
	}
}

func TestSummarise(t *testing.T) {
	key, _ := testKey(t)
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	type record struct {
		client  string
		in, out int64
	}
	records := []record{
		{"client-a", 10, 20},
		{"client-b", 1, 2},
		{"client-a", 30, 40},
		{"client-c", 0, 0},
	}
	for _, r := range records {
		if _, err := l.Append(r.client, "example.com:443", r.in, r.out); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	entries := l.Entries()
	totals := Summarise(entries)
	if len(totals) != 3 {
		t.Fatalf("Summarise returned %d clients, want 3", len(totals))
	}
	if got := totals["client-a"]; got.BytesIn != 40 || got.BytesOut != 60 || got.Entries != 2 {
		t.Fatalf("client-a totals = %+v, want {40 60 2}", got)
	}
	if got := totals["client-b"]; got.BytesIn != 1 || got.BytesOut != 2 || got.Entries != 1 {
		t.Fatalf("client-b totals = %+v, want {1 2 1}", got)
	}
	if got := totals["client-c"]; got.BytesIn != 0 || got.BytesOut != 0 || got.Entries != 1 {
		t.Fatalf("client-c totals = %+v, want {0 0 1}", got)
	}

	// The totals must describe the same records the chain holds.
	var wantIn, wantOut int64
	for _, r := range records {
		wantIn += r.in
		wantOut += r.out
	}
	var gotIn, gotOut int64
	var gotEntries int
	for _, got := range totals {
		gotIn += got.BytesIn
		gotOut += got.BytesOut
		gotEntries += got.Entries
	}
	if gotIn != wantIn || gotOut != wantOut || gotEntries != len(records) {
		t.Fatalf("totals sum = {%d %d %d}, want {%d %d %d}", gotIn, gotOut, gotEntries, wantIn, wantOut, len(records))
	}

	if len(Summarise(nil)) != 0 {
		t.Fatal("Summarise(nil) is not empty")
	}
}

func TestConcurrentAppend(t *testing.T) {
	key, pub := testKey(t)
	l, err := New("", key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	const perGoroutine = 50
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := "client-a"
			if id%2 == 1 {
				client = "client-b"
			}
			for i := 0; i < perGoroutine; i++ {
				if _, err := l.Append(client, "example.com:443", int64(i), int64(i*2)); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				_ = l.Head()
				_ = l.Len()
			}
		}(g)
	}
	wg.Wait()

	entries := l.Entries()
	if len(entries) != 2*perGoroutine {
		t.Fatalf("Len = %d, want %d", len(entries), 2*perGoroutine)
	}
	for i, entry := range entries {
		if entry.Index != uint64(i) {
			t.Fatalf("entry %d has index %d", i, entry.Index)
		}
	}
	if count, err := Verify(entries, pub); err != nil || count != len(entries) {
		t.Fatalf("Verify = (%d, %v), want (%d, nil)", count, err, len(entries))
	}

	totals := Summarise(entries)
	if totals["client-a"].Entries != perGoroutine || totals["client-b"].Entries != perGoroutine {
		t.Fatalf("per-client entries = %+v", totals)
	}
}

func TestConcurrentAppendToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	key, pub := testKey(t)
	l, err := New(path, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	provenance := make([]Entry, 0, 2)
	var mu sync.Mutex
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				entry, err := l.Append("client-a", "example.com:443", 1, 2)
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				mu.Lock()
				provenance = append(provenance, entry)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(provenance) != 50 {
		t.Fatalf("appended %d entries, want 50", len(provenance))
	}

	reopened, err := New(path, key)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	entries := reopened.Entries()
	if len(entries) != 50 {
		t.Fatalf("reopened Len = %d, want 50", len(entries))
	}
	if count, err := Verify(entries, pub); err != nil || count != 50 {
		t.Fatalf("Verify = (%d, %v), want (50, nil)", count, err)
	}
	if got := Summarise(entries)["client-a"]; got.Entries != 50 || got.BytesIn != 50 || got.BytesOut != 100 {
		t.Fatalf("totals = %+v, want {50 100 50}", got)
	}
}
