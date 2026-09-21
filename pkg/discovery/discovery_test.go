package discovery

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/dht"
)

// freeUDPAddr reserves a loopback UDP port and releases it, which is the address
// pattern the tests hand to Start.
func freeUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve UDP port: %v", err)
	}
	addr := conn.LocalAddr().String()
	_ = conn.Close()
	return addr
}

func startNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	node, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return node
}

// waitForContacts blocks until a node knows at least n peers.
func waitForContacts(t *testing.T, node *Node, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if node.Contacts() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node knows %d contacts after 5s, want at least %d", node.Contacts(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sampleRecord(name, server string) Record {
	return Record{Name: name, Type: "tcp", Server: server, Domains: []string{"a.example"}}
}

// --- tests --------------------------------------------------------------------

func TestPublishAndResolveOnOneNode(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	rec, err := node.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.Name != "ssh" || rec.Server != "203.0.113.5:7000" || rec.Type != "tcp" {
		t.Fatalf("resolved %+v, want the record that was published", rec)
	}
	if len(rec.Domains) != 1 || rec.Domains[0] != "a.example" {
		t.Errorf("resolved domains %v, want [a.example]", rec.Domains)
	}
	if !rec.Fresh(time.Now()) {
		t.Errorf("a record published just now is stale: %+v", rec)
	}

	if got := node.Announced(); len(got) != 1 || got[0] != "ssh" {
		t.Errorf("Announced() = %v, want [ssh]", got)
	}
}

func TestLookupFindsARecordPublishedOnAnotherNode(t *testing.T) {
	first := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	second := startNode(t, Config{
		ListenAddr: freeUDPAddr(t),
		Bootstrap:  []string{first.Addr()},
	})
	waitForContacts(t, second, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := first.Publish(ctx, sampleRecord("web", "198.51.100.9:8080")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	rec, err := second.Resolve("web")
	if err != nil {
		t.Fatalf("Resolve from the other node: %v", err)
	}
	if rec.Server != "198.51.100.9:8080" {
		t.Fatalf("resolved server %q, want 198.51.100.9:8080", rec.Server)
	}
}

func TestNamespacesDoNotOverlap(t *testing.T) {
	first := startNode(t, Config{ListenAddr: freeUDPAddr(t), Namespace: "alpha"})
	second := startNode(t, Config{ListenAddr: freeUDPAddr(t), Namespace: "beta"})

	if first.Key("ssh") == second.Key("ssh") {
		t.Fatalf("both namespaces map ssh to the same key %q", first.Key("ssh"))
	}
	if first.Key("ssh") != "alpha/proxy/ssh" {
		t.Errorf("Key(ssh) = %q, want alpha/proxy/ssh", first.Key("ssh"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := second.Resolve("ssh"); !errors.Is(err, dht.ErrNotFound) {
		t.Fatalf("the beta namespace resolved a record published under alpha: %v", err)
	}
}

func TestRepublishingKeepsAnAnnouncementFresh(t *testing.T) {
	node := startNode(t, Config{
		ListenAddr:        freeUDPAddr(t),
		AnnounceTTL:       300 * time.Millisecond,
		RepublishInterval: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := node.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Well past the first announcement's expiry: only the republish loop can have
	// kept the record alive.
	time.Sleep(900 * time.Millisecond)

	rec, err := node.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve after the republish window: %v", err)
	}
	if !rec.Fresh(time.Now()) {
		t.Fatalf("the announcement lapsed despite republishing: %+v", rec)
	}
}

func TestStaleAndMalformedRecordsAreRefused(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A record whose validity has passed is reported as stale, not as an address.
	stale := Record{Name: "ssh", Server: "203.0.113.5:7000",
		Updated: time.Now().Add(-time.Hour), Expires: time.Now().Add(-time.Minute)}
	value, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := node.table.Put(ctx, node.Key("ssh"), value); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := node.Resolve("ssh"); !errors.Is(err, ErrStale) {
		t.Fatalf("Resolve of a stale record returned %v, want ErrStale", err)
	}

	// A value that is not an announcement at all is reported as such.
	if err := node.table.Put(ctx, node.Key("garbage"), []byte("not json")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := node.Resolve("garbage"); err == nil || errors.Is(err, ErrStale) {
		t.Fatalf("Resolve of a non-announcement returned %v, want an encoding error", err)
	}

	// An announcement with no address cannot be dialled.
	empty, err := json.Marshal(Record{Name: "empty", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := node.table.Put(ctx, node.Key("empty"), empty); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := node.Resolve("empty"); err == nil {
		t.Fatal("an announcement without a server address was accepted")
	}

	if _, err := node.Resolve("never-published"); !errors.Is(err, dht.ErrNotFound) {
		t.Fatalf("Resolve of an unknown name returned %v, want dht.ErrNotFound", err)
	}
}

func TestWithdrawStopsAnnouncing(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := node.Withdraw("ssh"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if got := node.Announced(); len(got) != 0 {
		t.Errorf("Announced() = %v after Withdraw, want nothing", got)
	}
	if _, err := node.Resolve("ssh"); !errors.Is(err, dht.ErrNotFound) {
		t.Fatalf("Resolve after Withdraw returned %v, want dht.ErrNotFound", err)
	}

	// The name can be published again after being withdrawn.
	if err := node.Publish(ctx, sampleRecord("ssh", "203.0.113.6:7001")); err != nil {
		t.Fatalf("Publish after Withdraw: %v", err)
	}
	rec, err := node.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.Server != "203.0.113.6:7001" {
		t.Errorf("resolved server %q, want the re-published 203.0.113.6:7001", rec.Server)
	}
}

func TestPublishRejectsIncompleteAndOversizedRecords(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Publish(ctx, Record{Server: "203.0.113.5:7000"}); err == nil {
		t.Error("an announcement with no name was accepted")
	}
	if err := node.Publish(ctx, Record{Name: "ssh"}); err == nil {
		t.Error("an announcement with no server address was accepted")
	}

	// Each domain is 40 bytes of JSON, so this is far past the 1024-byte value cap.
	domains := make([]string, 60)
	for i := range domains {
		domains[i] = strings.Repeat("d", 30) + ".example"
	}
	err := node.Publish(ctx, Record{Name: "huge", Server: "203.0.113.5:7000", Domains: domains})
	if err == nil {
		t.Fatalf("an announcement of %d domains was accepted", len(domains))
	}
	if !strings.Contains(err.Error(), "DHT value") {
		t.Errorf("the oversized announcement failed with %v, which does not name the value limit", err)
	}
}

func TestBootstrapReportsUnreachableNodes(t *testing.T) {
	// A bound but never-started address: the UDP port is closed, so nothing answers.
	dead := freeUDPAddr(t)
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), Bootstrap: []string{dead}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	failures := node.Bootstrap(ctx)
	if len(failures) != 1 {
		t.Fatalf("Bootstrap reported %d failures, want 1: %v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error(), dead) {
		t.Errorf("the failure %v does not name %s", failures[0], dead)
	}
	if node.Contacts() != 0 {
		t.Errorf("the node has %d contacts after failing to reach its only bootstrap peer", node.Contacts())
	}
}

func TestStartRejectsAMalformedNodeID(t *testing.T) {
	node, err := Start(Config{ListenAddr: freeUDPAddr(t), NodeID: "not-hex"})
	if err == nil {
		_ = node.Close()
		t.Fatal("a node id that is not hex was accepted")
	}
	if !strings.Contains(err.Error(), "node id") {
		t.Errorf("the error is %v, which does not name the node id", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Namespace != DefaultNamespace {
		t.Errorf("Namespace defaults to %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.AnnounceTTL != DefaultAnnounceTTL {
		t.Errorf("AnnounceTTL defaults to %s, want %s", cfg.AnnounceTTL, DefaultAnnounceTTL)
	}
	if cfg.RepublishInterval != DefaultRepublishInterval {
		t.Errorf("RepublishInterval defaults to %s, want %s", cfg.RepublishInterval, DefaultRepublishInterval)
	}
	if cfg.RepublishInterval >= cfg.AnnounceTTL {
		t.Errorf("the default republish interval %s does not fit inside the announce TTL %s",
			cfg.RepublishInterval, cfg.AnnounceTTL)
	}
	if cfg.LookupTimeout != DefaultLookupTimeout {
		t.Errorf("LookupTimeout defaults to %s, want %s", cfg.LookupTimeout, DefaultLookupTimeout)
	}
}

func TestZeroValueContextFallsBackToTheLookupTimeout(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), LookupTimeout: time.Second})

	if err := node.Publish(context.Background(), sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A nil context is what a caller with no request scope passes.
	rec, err := node.Lookup(nil, "ssh")
	if err != nil {
		t.Fatalf("Lookup(nil): %v", err)
	}
	if rec.Server != "203.0.113.5:7000" {
		t.Errorf("resolved server %q, want 203.0.113.5:7000", rec.Server)
	}
}

func TestAddrAndSelfAreReported(t *testing.T) {
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	if node.Addr() == "" {
		t.Error("a started node reports no address")
	}
	if len(node.Self()) != 40 {
		t.Errorf("Self() = %q, want 40 hex characters", node.Self())
	}
	if node.Namespace() != DefaultNamespace {
		t.Errorf("Namespace() = %q, want %q", node.Namespace(), DefaultNamespace)
	}
}

// TestConcurrentPublishAndResolve checks that the node is usable from several
// goroutines at once, since the server publishes from the control-connection path
// while the republish loop rewrites the same records.
func TestConcurrentPublishAndResolve(t *testing.T) {
	node := startNode(t, Config{
		ListenAddr:        freeUDPAddr(t),
		AnnounceTTL:       200 * time.Millisecond,
		RepublishInterval: 20 * time.Millisecond,
	})

	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		go func(i int) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			name := fmt.Sprintf("proxy-%d", i)
			if err := node.Publish(ctx, sampleRecord(name, "203.0.113.5:7000")); err != nil {
				errs <- err
				return
			}
			if _, err := node.Resolve(name); err != nil {
				errs <- err
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent publish and resolve: %v", err)
		}
	}
	if got := len(node.Announced()); got != 8 {
		t.Errorf("%d names are announced, want 8", got)
	}
}

// --- signed announcements ------------------------------------------------------

// publisher is the signer the tests publish with. It is the same identity type the
// server loads from [identity] and [dht].signing_key_file.
func publisher(t *testing.T) *crypto.Identity {
	t.Helper()
	identity, err := crypto.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity
}

func TestARecordSignedByThePublisherVerifiesOnTheReader(t *testing.T) {
	key := publisher(t)
	writer := startNode(t, Config{ListenAddr: freeUDPAddr(t), Signer: key})
	reader := startNode(t, Config{
		ListenAddr:  freeUDPAddr(t),
		Bootstrap:   []string{writer.Addr()},
		TrustedKeys: []ed25519.PublicKey{key.PublicKey()},
	})
	waitForContacts(t, reader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	rec, err := reader.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve from a reader that trusts the key: %v", err)
	}
	if !rec.Verified {
		t.Error("the resolved record is not reported as verified")
	}
	if rec.PublicKey != key.PublicKeyHex() {
		t.Errorf("the record names key %q, want %q", rec.PublicKey, key.PublicKeyHex())
	}
	if rec.Server != "203.0.113.5:7000" {
		t.Errorf("resolved server %q, want 203.0.113.5:7000", rec.Server)
	}
}

func TestAnUnsignedRecordIsRefusedWhenSignaturesAreRequired(t *testing.T) {
	writer := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	reader := startNode(t, Config{
		ListenAddr:    freeUDPAddr(t),
		Bootstrap:     []string{writer.Addr()},
		RequireSigned: true,
	})
	waitForContacts(t, reader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := reader.Resolve("ssh"); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("an unsigned record resolved to %v, want ErrUnsigned", err)
	}
}

func TestAnUnsignedRecordIsRefusedWhenKeysAreNamed(t *testing.T) {
	writer := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	reader := startNode(t, Config{
		ListenAddr:  freeUDPAddr(t),
		Bootstrap:   []string{writer.Addr()},
		TrustedKeys: []ed25519.PublicKey{publisher(t).PublicKey()},
	})
	waitForContacts(t, reader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// A named key is a statement that only that key is believed, so a record that
	// carries no key at all cannot satisfy it.
	if _, err := reader.Resolve("ssh"); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("an unsigned record resolved to %v, want ErrUnsigned", err)
	}
}

func TestARecordSignedByAKeyTheReaderWasNotGivenIsRefused(t *testing.T) {
	stranger := publisher(t)
	trusted := publisher(t)
	writer := startNode(t, Config{ListenAddr: freeUDPAddr(t), Signer: stranger})
	reader := startNode(t, Config{
		ListenAddr:  freeUDPAddr(t),
		Bootstrap:   []string{writer.Addr()},
		TrustedKeys: []ed25519.PublicKey{trusted.PublicKey()},
	})
	waitForContacts(t, reader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := reader.Resolve("ssh"); !errors.Is(err, ErrUntrustedPublisher) {
		t.Fatalf("a record signed by a stranger resolved to %v, want ErrUntrustedPublisher", err)
	}
}

func TestARewrittenRecordFailsItsSignature(t *testing.T) {
	key := publisher(t)
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), Signer: key})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// What a node on the DHT can do: read the stored value, point it at an address
	// of its own, and write it back under the same key without the signing key.
	signed := Record{Name: "ssh", Type: "tcp", Server: "203.0.113.5:7000", Domains: []string{"a.example"}}
	signed.Updated = time.Now()
	signed.Expires = time.Now().Add(time.Minute)
	signed.sign(key)
	forged := signed
	forged.Server = "198.51.100.66:7000"
	value, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := node.table.Put(ctx, node.Key("ssh"), value); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := node.Resolve("ssh"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a rewritten record resolved to %v, want ErrBadSignature", err)
	}
}

func TestTheSignatureCoversEveryFieldAReaderActsOn(t *testing.T) {
	key := publisher(t)

	base := Record{Name: "ssh", Type: "tcp", Server: "203.0.113.5:7000",
		Domains: []string{"a.example", "b.example"},
		Updated: time.Now(), Expires: time.Now().Add(time.Minute)}
	base.sign(key)

	if verified, err := (&base).verify(true, nil); err != nil || !verified {
		t.Fatalf("the signed record did not verify: verified=%v err=%v", verified, err)
	}

	changes := map[string]func(*Record){
		"name":    func(r *Record) { r.Name = "other" },
		"type":    func(r *Record) { r.Type = "udp" },
		"server":  func(r *Record) { r.Server = "198.51.100.66:7000" },
		"domains": func(r *Record) { r.Domains = []string{"c.example"} },
		"updated": func(r *Record) { r.Updated = r.Updated.Add(time.Second) },
		"expires": func(r *Record) { r.Expires = r.Expires.Add(time.Hour) },
		"key":     func(r *Record) { r.PublicKey = publisher(t).PublicKeyHex() },
	}
	for field, change := range changes {
		tampered := base
		change(&tampered)
		if verified, err := (&tampered).verify(false, nil); err == nil {
			t.Errorf("changing the %s left the record verifying (verified=%v)", field, verified)
		}
	}
}

func TestAnUnsignedRecordIsAcceptedWithoutAReaderPolicy(t *testing.T) {
	key := publisher(t)
	writer := startNode(t, Config{ListenAddr: freeUDPAddr(t)})
	reader := startNode(t, Config{ListenAddr: freeUDPAddr(t), Bootstrap: []string{writer.Addr()}})
	waitForContacts(t, reader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	rec, err := reader.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve with no signature policy: %v", err)
	}
	if rec.Verified {
		t.Error("an unsigned record is reported as verified")
	}
	if rec.PublicKey != "" || rec.Signature != "" {
		t.Errorf("an unsigned record carries key %q and signature %q",
			rec.PublicKey, rec.Signature)
	}

	// The same reader still refuses a forged record when it is told which key to
	// believe, even though it accepted the unsigned one.
	strict := startNode(t, Config{
		ListenAddr:  freeUDPAddr(t),
		Bootstrap:   []string{writer.Addr()},
		TrustedKeys: []ed25519.PublicKey{key.PublicKey()},
	})
	waitForContacts(t, strict, 1)
	if _, err := strict.Resolve("ssh"); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a reader naming a key resolved an unsigned record to %v, want ErrUnsigned", err)
	}
}

func TestRepublishingKeepsASignatureValid(t *testing.T) {
	key := publisher(t)
	writer := startNode(t, Config{
		ListenAddr:        freeUDPAddr(t),
		Signer:            key,
		AnnounceTTL:       900 * time.Millisecond,
		RepublishInterval: 60 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Publish(ctx, sampleRecord("ssh", "203.0.113.5:7000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Several republications later the record has a new timestamp and a new
	// signature; the reader's policy has to hold for both.
	time.Sleep(400 * time.Millisecond)
	rec, err := writer.Resolve("ssh")
	if err != nil {
		t.Fatalf("Resolve after republishing: %v", err)
	}
	if !rec.Verified {
		t.Error("a republished record is not reported as verified")
	}
	if !rec.Fresh(time.Now()) {
		t.Errorf("the republished record is stale: %+v", rec)
	}
}

func TestPublishRefusesAnOversizedSignedRecord(t *testing.T) {
	key := publisher(t)
	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), Signer: key})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The key and signature add bytes to the stored value, so the limit still has
	// to be checked after signing rather than before.
	domains := make([]string, 60)
	for i := range domains {
		domains[i] = strings.Repeat("d", 30) + ".example"
	}
	err := node.Publish(ctx, Record{Name: "huge", Server: "203.0.113.5:7000", Domains: domains})
	if err == nil {
		t.Fatalf("an announcement of %d domains was accepted", len(domains))
	}
	if !strings.Contains(err.Error(), "DHT value") {
		t.Errorf("the oversized announcement failed with %v, which does not name the value limit", err)
	}
}
