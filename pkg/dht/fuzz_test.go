package dht

import (
	"bytes"
	"testing"
)

// FuzzDecode parses a datagram any host can send to the node's UDP port. A node
// answers a message it decoded, stores what it was told, and hands the contacts
// it read to the table, so a decoder that trusts a length is exploitable without
// knowing anything about the node.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{magicA, magicE, magicT, magicVer, byte(msgPing)})

	// One well-formed message of every type, so the corpus starts from each branch.
	for _, m := range []message{
		{typ: msgPing, id: testID(1)},
		{typ: msgFindNode, id: testID(2), target: testID(3)},
		{typ: msgFindValue, id: testID(4), ns: 1, key: testID(5)},
		{typ: msgStore, id: testID(6), ns: 1, key: testID(7), expiry: 1700000000, value: []byte("v")},
		{typ: msgValue, id: testID(8), ns: 1, key: testID(9), found: true, value: []byte("v")},
		{typ: msgNodes, id: testID(10), target: testID(11), nodes: []Node{
			{ID: testID(12), Addr: "127.0.0.1:1"},
			{ID: testID(13), Addr: "[::1]:2"},
		}},
	} {
		encoded, err := encode(m)
		if err != nil {
			f.Fatalf("encode %s: %v", m.typ, err)
		}
		f.Add(encoded)
		// The same message with its body truncated one byte at a time is what a
		// network delivers, and every branch has to reject it rather than panic.
		for i := headerLen; i < len(encoded); i++ {
			f.Add(encoded[:i])
		}
	}

	f.Fuzz(func(t *testing.T, pkt []byte) {
		m, err := decode(pkt)
		if err != nil {
			return
		}
		// A decode that succeeded read a whole, self-consistent datagram: the body
		// is the size the header declares, and every contact names an address this
		// node could send to without resolving anything.
		if want := len(pkt) - headerLen; len(m.value) > want {
			t.Fatalf("decoded %d bytes of value out of a %d byte body", len(m.value), want)
		}
		for _, n := range m.nodes {
			if !validContact(n.Addr) {
				t.Fatalf("decoded contact %q is not an address", n.Addr)
			}
		}
		// Re-encoding what was decoded has to produce something the decoder accepts
		// again, which is what keeps the two directions of this format in step.
		again, err := encode(m)
		if err != nil {
			return // a decoded message may carry no valid reply (an oversized value)
		}
		round, err := decode(again)
		if err != nil {
			t.Fatalf("a re-encoded %s message did not decode: %v", m.typ, err)
		}
		if round.typ != m.typ || !bytes.Equal(round.tx[:], m.tx[:]) {
			t.Fatalf("round trip changed the message: %s/%x became %s/%x", m.typ, m.tx, round.typ, round.tx)
		}
	})
}

// testID builds a deterministic ID from a seed byte.
func testID(seed byte) (id ID) {
	for i := range id {
		id[i] = seed + byte(i)
	}
	return id
}
