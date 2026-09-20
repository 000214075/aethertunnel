package dht

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"testing"
	"time"
)

// networkSize is the node count used by the multi-node tests. The first node is
// the bootstrap contact for the rest.
const networkSize = 30

// newTable starts one node on loopback. The explicit loopback host matters:
// it is what the node advertises in provider records, so the address the tests
// compare against is the one Start returned.
func newTable(t *testing.T, bootstrap ...string) *Table {
	t.Helper()
	cfg := Config{ListenAddr: "127.0.0.1:0", Bootstrap: bootstrap}
	if testing.Verbose() {
		cfg.Logger = log.New(os.Stderr, "dht ", log.Lmicroseconds)
	}
	tab, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tab.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = tab.Close() })
	return tab
}

func startNetwork(t *testing.T, n int) []*Table {
	t.Helper()
	tables := make([]*Table, 0, n)
	tables = append(tables, newTable(t))
	for i := 1; i < n; i++ {
		tables = append(tables, newTable(t, tables[0].Addr().String()))
	}
	return tables
}

// waitForTables waits until every node knows at least one contact.
func waitForTables(t *testing.T, tables []*Table) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		empty := 0
		for _, tab := range tables {
			if tab.Len() == 0 {
				empty++
			}
		}
		if empty == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d routing tables are still empty after 30s", empty, len(tables))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRoutingTablesFill(t *testing.T) {
	tables := startNetwork(t, networkSize)
	waitForTables(t, tables)
	for i, tab := range tables {
		if got := tab.Len(); got == 0 {
			t.Fatalf("node %d has an empty routing table", i)
		}
		if tab.Addr() == nil {
			t.Fatalf("node %d reports no bound address", i)
		}
	}
	if got := tables[0].Len(); got < 2 {
		t.Fatalf("the bootstrap node knows %d contacts, want more than one", got)
	}
	seen := map[ID]bool{}
	for _, tab := range tables {
		if seen[tab.Self()] {
			t.Fatalf("two nodes share the id %s", tab.Self())
		}
		seen[tab.Self()] = true
	}
}

func TestPutGetAcrossNodes(t *testing.T) {
	tables := startNetwork(t, networkSize)
	waitForTables(t, tables)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := "proxy.example.com"
	value := []byte("10.1.2.3:8443")
	if err := tables[0].Put(ctx, key, value); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reader := tables[len(tables)-1]
	got, err := getEventually(ctx, reader, key)
	if err != nil {
		t.Fatalf("Get on a node that never stored the value: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("Get returned %q, want %q", got, value)
	}
}

func TestPutValueTooLarge(t *testing.T) {
	tab := newTable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tab.Put(ctx, "k", make([]byte, MaxValueSize+1)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("Put of an oversized value returned %v, want ErrValueTooLarge", err)
	}
	if err := tab.Put(ctx, "k", make([]byte, MaxValueSize)); err != nil {
		t.Fatalf("Put of a value at the limit: %v", err)
	}
	got, err := tab.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != MaxValueSize {
		t.Fatalf("Get returned %d bytes, want %d", len(got), MaxValueSize)
	}
}

func TestValueExpires(t *testing.T) {
	tab, err := New(Config{ListenAddr: "127.0.0.1:0", TTL: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tab.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tab.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tab.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := tab.Get(ctx, "k"); err != nil || string(got) != "v" {
		t.Fatalf("Get before the TTL: %q, %v", got, err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := tab.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after the TTL returned %v, want ErrNotFound", err)
	}
}

func TestGetUnknownKey(t *testing.T) {
	tables := startNetwork(t, 3)
	waitForTables(t, tables)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := tables[2].Get(ctx, "never-stored"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get returned %v, want ErrNotFound", err)
	}
	if _, err := tables[2].FindProviders(ctx, "never-stored"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindProviders returned %v, want ErrNotFound", err)
	}
}

func TestProvideFindProviders(t *testing.T) {
	tables := startNetwork(t, networkSize)
	waitForTables(t, tables)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provider := tables[0]
	reader := tables[len(tables)-1]
	key := "chat.example.com"

	if err := provider.Provide(ctx, key); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	// The same name carries a plain value as well: the two namespaces must not
	// shadow each other.
	value := []byte("value-under-the-same-name")
	if err := provider.Put(ctx, key, value); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := provider.Addr().String()
	addrs := findProvidersEventually(ctx, reader, key)
	if !containsString(addrs, want) {
		t.Fatalf("FindProviders returned %v, want it to contain %s", addrs, want)
	}
	got, err := getEventually(ctx, reader, key)
	if err != nil {
		t.Fatalf("Get after Provide: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("Get returned %q, want the plain value %q", got, value)
	}
}

func TestClosestOrdering(t *testing.T) {
	tables := startNetwork(t, 10)
	waitForTables(t, tables)
	tab := tables[0]
	target := RandomID()

	nodes := tab.Closest(target, 3)
	if len(nodes) == 0 {
		t.Fatal("Closest returned no contacts after the network formed")
	}
	if len(nodes) > 3 {
		t.Fatalf("Closest(3) returned %d contacts", len(nodes))
	}
	for i := 1; i < len(nodes); i++ {
		if !lessDistance(nodes[i-1].ID, nodes[i].ID, target) {
			t.Fatalf("Closest is not ordered by ascending distance: %s then %s", nodes[i-1].ID, nodes[i].ID)
		}
	}
	all := tab.Closest(target, 1000)
	if len(all) != tab.Len() {
		t.Fatalf("Closest(1000) returned %d contacts, the table holds %d", len(all), tab.Len())
	}
	for i := range nodes {
		if nodes[i].ID != all[i].ID {
			t.Fatalf("Closest(3) is not a prefix of the full ordering at %d", i)
		}
	}
	if got := tab.Closest(target, 0); len(got) != 0 {
		t.Fatalf("Closest(0) returned %d contacts", len(got))
	}
	if got := tab.Closest(target, -1); len(got) != 0 {
		t.Fatalf("Closest(-1) returned %d contacts", len(got))
	}
}

func TestNotStarted(t *testing.T) {
	tab, err := New(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if tab.Addr() != nil {
		t.Fatalf("Addr before Start is %v, want nil", tab.Addr())
	}
	if tab.Len() != 0 {
		t.Fatalf("Len before Start is %d, want 0", tab.Len())
	}
	if got := tab.Closest(RandomID(), 5); len(got) != 0 {
		t.Fatalf("Closest before Start returned %d contacts", len(got))
	}
	if tab.Self().isZero() {
		t.Fatal("Self returned the zero id")
	}
	for _, err := range []error{
		tab.Ping(ctx, "127.0.0.1:9"),
		tab.Put(ctx, "k", []byte("v")),
		tab.Provide(ctx, "k"),
	} {
		if !errors.Is(err, ErrNotStarted) {
			t.Fatalf("an operation on an unstarted table returned %v, want ErrNotStarted", err)
		}
	}
	if _, err := tab.Get(ctx, "k"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Get before Start returned %v, want ErrNotStarted", err)
	}
	if _, err := tab.FindProviders(ctx, "k"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("FindProviders before Start returned %v, want ErrNotStarted", err)
	}
	if err := tab.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if _, err := tab.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close returned %v, want ErrClosed", err)
	}
	if _, err := New(Config{ListenAddr: "definitely not an address"}); err == nil {
		t.Fatal("New accepted an invalid listen address")
	}
	if tab.Self() != tab.Self() {
		t.Fatal("Self is not stable")
	}
}

func TestClosedNode(t *testing.T) {
	node := newTable(t)
	peer := newTable(t)
	addr := node.Addr().String()
	if err := node.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := node.Ping(ctx, addr); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping on a closed node returned %v, want ErrClosed", err)
	}
	if err := node.Put(ctx, "k", []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put on a closed node returned %v, want ErrClosed", err)
	}
	if _, err := node.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get on a closed node returned %v, want ErrClosed", err)
	}
	if err := node.Provide(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Provide on a closed node returned %v, want ErrClosed", err)
	}
	if _, err := node.FindProviders(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("FindProviders on a closed node returned %v, want ErrClosed", err)
	}
	if _, err := node.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start on a closed node returned %v, want ErrClosed", err)
	}

	start := time.Now()
	if err := peer.Ping(ctx, addr); err == nil {
		t.Fatal("a peer pinged a closed node successfully")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("pinging a closed node took %s", elapsed)
	}
}

func TestPingSilentPeer(t *testing.T) {
	tab := newTable(t)
	// A port that nothing is listening on answers nothing.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	silent := probe.LocalAddr().String()
	probe.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err = tab.Ping(ctx, silent)
	if err == nil {
		t.Fatal("Ping to a silent peer succeeded")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Ping to a silent peer returned %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Ping to a silent peer took %s", elapsed)
	}
}

func TestMalformedDatagramsAreIgnored(t *testing.T) {
	node := newTable(t)
	peer := newTable(t)
	addr, err := net.ResolveUDPAddr("udp", node.Addr().String())
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()

	id := RandomID()
	valid, err := encode(message{typ: msgPing, id: id})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	storeBody := append([]byte{}, id[:]...)
	storeBody = append(storeBody, nsValue)
	storeBody = append(storeBody, id[:]...)

	payloads := []struct {
		name string
		pkt  []byte
	}{
		{"empty", nil},
		{"short header", valid[:headerLen-1]},
		{"truncated body", valid[:len(valid)-1]},
		{"bad magic", append([]byte{0x00}, valid[1:]...)},
		{"unknown type", rawMessage(msgType(0x7f), id[:])},
		{"ping body too short", rawMessage(msgPing, []byte{1, 2, 3})},
		{"store body truncated", rawMessage(msgStore, storeBody)},
		{"value body truncated", rawMessage(msgValue, storeBody)},
		{"nodes absurd count", rawMessage(msgNodes, bytes.Repeat([]byte{0xff}, 42))},
		{"oversized value", rawMessage(msgValue, append(append([]byte{}, storeBody...), append([]byte{1}, bytes.Repeat([]byte{'v'}, MaxValueSize+1)...)...))},
		{"random bytes", bytes.Repeat([]byte{0x5a}, 4096)},
		{"bigger than the read buffer", bytes.Repeat([]byte{0xff}, maxDatagram+512)},
	}
	for _, p := range payloads {
		if _, err := conn.Write(p.pkt); err != nil {
			t.Fatalf("sending the %s datagram: %v", p.name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := peer.Ping(ctx, node.Addr().String()); err != nil {
		t.Fatalf("the node stopped answering after malformed datagrams: %v", err)
	}
	if node.Len() == 0 {
		t.Fatal("the node did not record the peer that pinged it")
	}
	if err := node.Put(ctx, "after-garbage", []byte("ok")); err != nil {
		t.Fatalf("Put after malformed datagrams: %v", err)
	}
}

// getEventually retries a lookup while the network converges.
func getEventually(ctx context.Context, tab *Table, key string) ([]byte, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := tab.Get(callCtx, key)
		cancel()
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func findProvidersEventually(ctx context.Context, tab *Table, key string) []string {
	deadline := time.Now().Add(10 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := tab.FindProviders(callCtx, key)
		cancel()
		if err == nil || !errors.Is(err, ErrNotFound) || time.Now().After(deadline) {
			return addrs
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestLookupConcurrency(t *testing.T) {
	tables := startNetwork(t, 8)
	waitForTables(t, tables)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Exercised concurrently on purpose: the package must be race free under
	// the exported API.
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		tab := tables[i+1]
		go func() {
			defer func() { done <- struct{}{} }()
			key := fmt.Sprintf("proxy-%d.example.com", i)
			if err := tab.Provide(ctx, key); err != nil {
				t.Errorf("Provide: %v", err)
				return
			}
			if err := tab.Put(ctx, key, []byte(key)); err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			if got, err := tab.Get(ctx, key); err != nil || string(got) != key {
				t.Errorf("Get returned %q, %v", got, err)
			}
			if _, err := tab.FindProviders(ctx, key); err != nil {
				t.Errorf("FindProviders: %v", err)
			}
			_ = tab.Closest(RandomID(), 5)
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("concurrent lookups did not finish")
		}
	}
}
