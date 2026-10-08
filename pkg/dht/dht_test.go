package dht

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIDhashAndHex(t *testing.T) {
	a := NewIDFromBytes([]byte("aethertunnel"))
	if a != NewIDFromBytes([]byte("aethertunnel")) {
		t.Fatal("NewIDFromBytes is not deterministic")
	}
	if a == NewIDFromBytes([]byte("aethertunne")) {
		t.Fatal("NewIDFromBytes returned the same id for different input")
	}
	if a.isZero() {
		t.Fatal("NewIDFromBytes returned the zero id")
	}
	if got := len(a.String()); got != 40 {
		t.Fatalf("String() has %d characters, want 40", got)
	}
	back, err := IDFromString(a.String())
	if err != nil {
		t.Fatalf("IDFromString(%q): %v", a.String(), err)
	}
	if back != a {
		t.Fatalf("hex round trip returned %s, want %s", back, a)
	}
	upper, err := IDFromString(strings.ToUpper(a.String()))
	if err != nil {
		t.Fatalf("IDFromString(uppercase): %v", err)
	}
	if upper != a {
		t.Fatalf("uppercase round trip returned %s, want %s", upper, a)
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("z", 40), a.String() + "00"} {
		if _, err := IDFromString(bad); err == nil {
			t.Fatalf("IDFromString(%q) accepted an invalid id", bad)
		}
	}
	if RandomID() == RandomID() {
		t.Fatal("RandomID returned the same id twice")
	}
	if RandomID().isZero() {
		t.Fatal("RandomID returned the zero id")
	}
}

func TestIDDistance(t *testing.T) {
	a := NewIDFromBytes([]byte("a"))
	b := NewIDFromBytes([]byte("b"))
	if a.Distance(a) != (ID{}) {
		t.Fatal("distance to self is not zero")
	}
	if a.Distance(b) != b.Distance(a) {
		t.Fatal("distance is not symmetric")
	}
	var want ID
	for i := range want {
		want[i] = a[i] ^ b[i]
	}
	if got := a.Distance(b); got != want {
		t.Fatalf("distance is %s, want %s", got, want)
	}
	var x ID
	x[0], x[19] = 0x81, 0xff
	if got := x.Distance(ID{}); got != x {
		t.Fatalf("distance to the zero id is %s, want %s", got, x)
	}
	// Closer to a than to b is what orders a routing table.
	if !lessDistance(a, b, a) || lessDistance(b, a, a) {
		t.Fatal("lessDistance does not follow the XOR metric")
	}
}

func TestIDCommonPrefixLen(t *testing.T) {
	var a ID
	if got := a.CommonPrefixLen(a); got != 160 {
		t.Fatalf("CommonPrefixLen of equal ids is %d, want 160", got)
	}
	cases := []struct {
		index int
		value byte
		want  int
	}{
		{0, 0x80, 0},
		{0, 0x40, 1},
		{0, 0x20, 2},
		{0, 0x01, 7},
		{1, 0x80, 8},
		{2, 0x08, 20},
		{19, 0x01, 159},
	}
	for _, c := range cases {
		var b ID
		b[c.index] = c.value
		if got := a.CommonPrefixLen(b); got != c.want {
			t.Fatalf("byte %d = %#x: CommonPrefixLen is %d, want %d", c.index, c.value, got, c.want)
		}
		if got := b.CommonPrefixLen(a); got != c.want {
			t.Fatalf("byte %d = %#x: CommonPrefixLen is not symmetric: %d", c.index, c.value, got)
		}
	}
}

// TestBucketEviction checks the k-bucket discipline: a full bucket reports the
// least recently seen occupant as an eviction candidate, keeps the newcomer in
// the replacement cache, and promotes it when a slot frees up.
func TestBucketEviction(t *testing.T) {
	self := RandomID()
	rt := newRoutingTable(self, 2)
	// All ids share the top 144 bits with self, so they land in one bucket.
	mk := func(i byte) ID {
		id := self
		id[18] = self[18] ^ 0x80
		id[19] = i
		return id
	}
	addr := func(i byte) string { return fmt.Sprintf("127.0.0.1:%d", 20000+int(i)) }
	a, b, c, d := mk(1), mk(2), mk(3), mk(4)
	if idx := rt.bucketIndex(a); idx < 0 || rt.bucketIndex(b) != idx || rt.bucketIndex(c) != idx {
		t.Fatalf("crafted ids do not share a bucket: %d, %d, %d", rt.bucketIndex(a), rt.bucketIndex(b), rt.bucketIndex(c))
	}
	for _, id := range []ID{a, b} {
		if cand := rt.seen(id, addr(id[19])); cand != nil {
			t.Fatalf("bucket reported a candidate while it still had room: %s", cand.id)
		}
	}
	if got := rt.len(); got != 2 {
		t.Fatalf("Len is %d, want 2", got)
	}
	cand := rt.seen(c, addr(c[19]))
	if cand == nil {
		t.Fatal("a full bucket did not report an eviction candidate")
	}
	if cand.id != a {
		t.Fatalf("eviction candidate is %s, want the least recently seen %s", cand.id, a)
	}
	if got := rt.len(); got != 2 {
		t.Fatalf("a full bucket grew to %d contacts", got)
	}
	// Refreshing a keeps it ahead of b, so b becomes the next candidate.
	rt.seen(a, addr(a[19]))
	if cand := rt.seen(d, addr(d[19])); cand == nil || cand.id != b {
		t.Fatalf("eviction candidate after refreshing %s is %v, want %s", a, cand, b)
	}
	// Dropping a silent contact promotes the most recently seen replacement.
	rt.remove(a)
	if got := rt.len(); got != 2 {
		t.Fatalf("Len after eviction is %d, want 2", got)
	}
	nodes := rt.closest(RandomID(), 10)
	ids := map[ID]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if ids[a] {
		t.Fatal("an evicted contact is still in the table")
	}
	if !ids[d] {
		t.Fatal("the replacement cache entry was not promoted after an eviction")
	}
	if node := rt.closest(RandomID(), 1); len(node) != 1 || node[0].Addr == "" || node[0].LastSeen.IsZero() {
		t.Fatalf("Closest returned an incomplete node: %+v", node)
	}
	if got := rt.closest(RandomID(), 0); len(got) != 0 {
		t.Fatalf("Closest(0) returned %d contacts", len(got))
	}
}

func TestKeyNamespaces(t *testing.T) {
	if keyFor(nsValue, "x") == keyFor(nsProvider, "x") {
		t.Fatal("the value and provider namespaces derive the same key")
	}
	if keyFor(nsValue, "x") == keyFor(nsValue, "y") {
		t.Fatal("different names derive the same key")
	}
	if keyFor(nsValue, "x") == NewIDFromBytes([]byte("x")) {
		t.Fatal("the namespace byte is not part of the hashed key")
	}
	id := keyFor(nsValue, "x")
	if storeKey(nsValue, id) == storeKey(nsProvider, id) {
		t.Fatal("the value and provider namespaces share a storage slot")
	}
	var zero ID
	if storeKey(nsValue, zero) == storeKey(nsProvider, id) {
		t.Fatal("distinct slots collide")
	}
}

func TestValueStoreExpiry(t *testing.T) {
	s := newValueStore()
	k := storeKey(nsValue, keyFor(nsValue, "k"))
	s.put(k, []byte("v"), time.Now().Add(30*time.Millisecond), true)
	v, ok := s.get(k)
	if !ok || string(v) != "v" {
		t.Fatalf("get returned %q, %v", v, ok)
	}
	// Stored bytes are copies: a caller cannot corrupt the store.
	v[0] = 'X'
	if again, _ := s.get(k); string(again) != "v" {
		t.Fatalf("the store returned shared memory: %q", again)
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := s.get(k); ok {
		t.Fatal("an expired value was returned")
	}
	if _, ok := s.get(k); ok {
		t.Fatal("the expired value survived the read that dropped it")
	}
	if got := s.localRecords(); len(got) != 0 {
		t.Fatalf("expired records are still scheduled for republish: %d", len(got))
	}
}

func TestProviderRecordsMerge(t *testing.T) {
	s := newValueStore()
	k := storeKey(nsProvider, keyFor(nsProvider, "key"))
	s.add(k, "127.0.0.1:1", time.Now().Add(time.Minute), true)
	s.add(k, "127.0.0.1:2", time.Now().Add(time.Minute), true)
	s.add(k, "127.0.0.1:1", time.Now().Add(time.Minute), true)
	v, ok := s.get(k)
	if !ok {
		t.Fatal("the provider record disappeared")
	}
	got := splitAddresses(string(v))
	if len(got) != 2 || got[0] != "127.0.0.1:1" || got[1] != "127.0.0.1:2" {
		t.Fatalf("provider record is %v, want two distinct addresses in announcement order", got)
	}
	for i := 0; i < 500; i++ {
		s.add(k, fmt.Sprintf("10.0.0.%d:1000", i%256), time.Now().Add(time.Minute), true)
	}
	v, ok = s.get(k)
	if !ok {
		t.Fatal("the provider record disappeared while merging")
	}
	if len(v) > MaxValueSize {
		t.Fatalf("merged provider record is %d bytes, above MaxValueSize", len(v))
	}
	if len(splitAddresses(string(v))) == 0 {
		t.Fatal("merging dropped every address")
	}
}

func TestExpiredRemoteRecordIsDropped(t *testing.T) {
	s := newValueStore()
	k := storeKey(nsValue, keyFor(nsValue, "k"))
	s.put(k, []byte("v"), time.Now().Add(-time.Second), false)
	if _, ok := s.get(k); ok {
		t.Fatal("a record that arrived expired was stored")
	}
	if got := s.localRecords(); len(got) != 0 {
		t.Fatalf("a replicated record is scheduled for republish: %d", len(got))
	}
}

func TestWireRoundTrip(t *testing.T) {
	sender := RandomID()
	target := RandomID()
	msgs := []message{
		{typ: msgPing, id: sender},
		{typ: msgPong, id: sender},
		{typ: msgFindNode, id: sender, target: target},
		{typ: msgFindValue, id: sender, ns: nsProvider, key: target},
		{typ: msgStore, id: sender, ns: nsProvider, key: target, expiry: 1712345678, value: []byte("127.0.0.1:9000")},
		{typ: msgValue, id: sender, ns: nsValue, key: target, found: true, value: []byte("payload")},
		{typ: msgValue, id: sender, ns: nsValue, key: target},
		{typ: msgNodes, id: sender, target: target, nodes: []Node{
			{ID: RandomID(), Addr: "127.0.0.1:1234"},
			{ID: RandomID(), Addr: "[::1]:4321"},
		}},
		{typ: msgNodes, id: sender, target: target},
	}
	for _, m := range msgs {
		pkt, err := encode(m)
		if err != nil {
			t.Fatalf("encode %s: %v", m.typ, err)
		}
		got, err := decode(pkt)
		if err != nil {
			t.Fatalf("decode %s: %v", m.typ, err)
		}
		if got.typ != m.typ || got.tx != m.tx || got.id != m.id || got.target != m.target ||
			got.ns != m.ns || got.key != m.key || got.expiry != m.expiry || got.found != m.found {
			t.Fatalf("%s round trip changed fields:\n got %+v\nwant %+v", m.typ, got, m)
		}
		if !bytes.Equal(got.value, m.value) {
			t.Fatalf("%s round trip changed the value: %q", m.typ, got.value)
		}
		if len(got.nodes) != len(m.nodes) {
			t.Fatalf("%s round trip changed the contact count: %d, want %d", m.typ, len(got.nodes), len(m.nodes))
		}
		for i := range m.nodes {
			if got.nodes[i].ID != m.nodes[i].ID || got.nodes[i].Addr != m.nodes[i].Addr {
				t.Fatalf("%s contact %d round tripped to %+v, want %+v", m.typ, i, got.nodes[i], m.nodes[i])
			}
		}
	}
}

func TestWireRejectsMalformed(t *testing.T) {
	valid, err := encode(message{typ: msgPing, id: RandomID()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	edit := func(i int, v byte) []byte {
		pkt := append([]byte(nil), valid...)
		pkt[i] = v
		return pkt
	}
	oversizedValue := func() []byte {
		id := RandomID()
		body := make([]byte, 0, MaxValueSize+64)
		body = append(body, id[:]...)
		body = append(body, nsValue)
		body = append(body, id[:]...)
		body = append(body, 1)
		body = append(body, bytes.Repeat([]byte{'v'}, MaxValueSize+1)...)
		return rawMessage(msgValue, body)
	}
	nodesAbsurdCount := func() []byte {
		id := RandomID()
		body := make([]byte, 0, 2*idLen+2)
		body = append(body, id[:]...)
		body = append(body, id[:]...)
		var cnt [2]byte
		binary.BigEndian.PutUint16(cnt[:], 0xffff)
		body = append(body, cnt[:]...)
		return rawMessage(msgNodes, body)
	}

	cases := []struct {
		name string
		pkt  []byte
	}{
		{"empty", nil},
		{"one byte", []byte{magicA}},
		{"short header", valid[:headerLen-1]},
		{"bad magic", edit(0, 0x00)},
		{"bad version", edit(3, 0x09)},
		{"unknown type", edit(4, 0x7f)},
		{"truncated body", valid[:len(valid)-1]},
		{"padded body", append(append([]byte(nil), valid...), 0x00)},
		{"body shorter than declared", valid[:len(valid)-4]},
		{"ping body too short", rawMessage(msgPing, []byte{1, 2, 3})},
		{"store body truncated", rawMessage(msgStore, []byte{1, 2, 3})},
		{"value body truncated", rawMessage(msgValue, []byte{1, 2, 3})},
		{"nodes missing contacts", nodesAbsurdCount()},
		{"oversized value", oversizedValue()},
		{"trailing bytes after a ping", rawMessage(msgPing, bytes.Repeat([]byte{0}, idLen+1))},
	}
	for _, c := range cases {
		if _, err := decode(c.pkt); err == nil {
			t.Fatalf("decode accepted a malformed datagram: %s", c.name)
		}
	}
	if _, err := decode(valid); err != nil {
		t.Fatalf("decode rejected a well formed datagram: %v", err)
	}
}

// rawMessage builds a datagram with a valid header around an arbitrary body.
func rawMessage(kind msgType, body []byte) []byte {
	pkt := make([]byte, headerLen+len(body))
	pkt[0], pkt[1], pkt[2], pkt[3] = magicA, magicE, magicT, magicVer
	pkt[4] = byte(kind)
	binary.BigEndian.PutUint32(pkt[headerLen-4:headerLen], uint32(len(body)))
	copy(pkt[headerLen:], body)
	return pkt
}

func TestEncodeRejectsOversizedValue(t *testing.T) {
	if _, err := encode(message{typ: msgStore, id: RandomID(), key: RandomID(), value: make([]byte, MaxValueSize+1)}); err == nil {
		t.Fatal("encode accepted a value above MaxValueSize")
	}
	if _, err := encode(message{typ: msgStore, id: RandomID(), key: RandomID(), value: make([]byte, MaxValueSize)}); err != nil {
		t.Fatalf("encode rejected a value at MaxValueSize: %v", err)
	}
	if _, err := encode(message{typ: msgType(0)}); err == nil {
		t.Fatal("encode accepted an unknown message type")
	}
}

// A contact that names port 0 or 70000 is not an address this node could ever
// reach, and the routing table fills up with whatever a peer sends.
func TestValidContactNeedsAUsablePort(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7000", "[2001:db8::1]:7001"} {
		if !validContact(addr) {
			t.Errorf("validContact(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:99999", "127.0.0.1:",
		"127.0.0.1", "example.com:7000", "127.0.0.1:port",
	} {
		if validContact(addr) {
			t.Errorf("validContact(%q) = true, want false", addr)
		}
	}
}

// A record this node owns must not be replaced by a remote STORE. The local flag is
// what the refresh loop republishes, so clearing it stops this node announcing its
// own name: one unauthenticated STORE datagram naming the proxy — the name is
// public — unpublishes it network-wide after one TTL.
func TestARemoteStoreDoesNotUnpublishALocalRecord(t *testing.T) {
	s := newValueStore()
	k := storeKey(nsValue, keyFor(nsValue, "web"))
	s.put(k, []byte("mine"), time.Now().Add(time.Minute), true)

	if s.put(k, []byte("theirs"), time.Now().Add(time.Minute), false) {
		t.Fatal("a remote STORE replaced a record this node owns")
	}
	v, ok := s.get(k)
	if !ok || string(v) != "mine" {
		t.Fatalf("the stored value is %q (%v), want the local one", v, ok)
	}
	if got := s.localRecords(); len(got) != 1 {
		t.Fatalf("%d records are scheduled for republish, want the local one", len(got))
	}

	// The guard is only about ownership: a peer's record is still replaced by a
	// later one, and a local Put still takes a slot a peer filled first.
	k2 := storeKey(nsValue, keyFor(nsValue, "other"))
	s.put(k2, []byte("first"), time.Now().Add(time.Minute), false)
	if !s.put(k2, []byte("second"), time.Now().Add(time.Minute), false) {
		t.Fatal("a remote record was not replaced by a later remote one")
	}
	if v, _ := s.get(k2); string(v) != "second" {
		t.Fatalf("the remote record reads %q, want second", v)
	}
	if !s.put(k2, []byte("local"), time.Now().Add(time.Minute), true) {
		t.Fatal("a local Put was refused over a remote record")
	}
	if v, _ := s.get(k2); string(v) != "local" {
		t.Fatalf("the local Put did not take the slot: %q", v)
	}
}

// A remote provider announcement must not expire a record this node owns, nor
// evict this node's own address from a full list. The wire accepts any expiry, so
// one datagram carrying expiry 0 would stop the refresh loop re-announcing the
// node, and the trim drops the oldest address, which is the node's own.
func TestARemoteProviderAnnouncementCannotExpireALocalRecord(t *testing.T) {
	s := newValueStore()
	k := storeKey(nsProvider, keyFor(nsProvider, "svc"))
	mine := time.Now().Add(time.Hour)
	s.add(k, "203.0.113.9:7000", mine, true)

	if !s.add(k, "198.51.100.4:7000", time.Unix(0, 0), false) {
		t.Fatal("a remote announcement was refused room")
	}
	if got := s.localRecords(); len(got) != 1 {
		t.Fatalf("%d records are scheduled for republish, want the local one: the remote expiry cut it short", len(got))
	}
	value, ok := s.get(k)
	if !ok || !strings.Contains(string(value), "203.0.113.9:7000") {
		t.Fatalf("the merged record is %q (%v), want this node's own address in it", value, ok)
	}
}

// The per-entry size check for a NODES reply left out one of the two identifiers
// the message carries, so it allowed a list that the body check in encode then
// refused whole: a large-but-trimmable reply was dropped instead of being cut to
// fit the datagram.
func TestANodesReplyIsTrimmedInsteadOfRefused(t *testing.T) {
	// 251 bytes is within the length a contact's address may have; whether it is
	// a literal host:port is the decoder's business, not the encoder's.
	addr := strings.Repeat("a", 251)
	nodes := make([]Node, 0, 40)
	for i := 0; i < 40; i++ {
		nodes = append(nodes, Node{ID: RandomID(), Addr: addr})
	}
	pkt, err := encode(message{typ: msgNodes, id: RandomID(), target: RandomID(), nodes: nodes})
	if err != nil {
		t.Fatalf("encode a reply with large contacts: %v", err)
	}
	if len(pkt) > maxDatagram {
		t.Fatalf("the datagram is %d bytes, past the %d byte limit", len(pkt), maxDatagram)
	}
}

// A provider record is a set of addresses, one per line, and store.add merges an
// announcement into that list. storeRemote checked the whole value as a single
// address, so a record that already held two — which is what a node republishes —
// was dropped by every peer that received it.
func TestARemoteProviderRecordWithSeveralAddressesIsStored(t *testing.T) {
	table := &Table{store: newValueStore(), ttl: time.Minute}
	key := RandomID()
	table.storeRemote(message{
		typ: msgStore, ns: nsProvider, key: key,
		expiry: time.Now().Add(time.Minute).Unix(),
		value:  []byte("10.0.0.1:7000\n10.0.0.2:7000"),
	})

	v, ok := table.store.get(storeKey(nsProvider, key))
	if !ok {
		t.Fatal("a provider record holding two addresses was dropped")
	}
	addresses := splitAddresses(string(v))
	if len(addresses) != 2 || addresses[0] != "10.0.0.1:7000" || addresses[1] != "10.0.0.2:7000" {
		t.Fatalf("the stored provider record is %q, want both addresses", v)
	}

	// One bad line does not take the good ones with it, and a value that is not an
	// address at all still stores nothing.
	other := RandomID()
	table.storeRemote(message{
		typ: msgStore, ns: nsProvider, key: other,
		expiry: time.Now().Add(time.Minute).Unix(),
		value:  []byte("not-an-address\n10.0.0.3:7000"),
	})
	v, ok = table.store.get(storeKey(nsProvider, other))
	if !ok || string(v) != "10.0.0.3:7000" {
		t.Fatalf("the mixed provider record is %q (%v), want only the literal address", v, ok)
	}
}
