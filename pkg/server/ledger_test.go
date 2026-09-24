package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/ledger"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// ledgerConfig is a server configuration with the bandwidth ledger turned on in a
// temporary directory.
func ledgerConfig(t *testing.T, encryption bool) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(t, encryption)
	cfg.Ledger.Enabled = true
	cfg.Ledger.Path = filepath.Join(dir, "ledger.jsonl")
	cfg.Ledger.SigningKey = filepath.Join(dir, "ledger.key")
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("ledger config invalid: %v", err)
	}
	return cfg
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// runOneStream drives one tunnel stream end to end through the server's public
// port, then closes the control connection so the server writes its ledger entry.
func runOneStream(t *testing.T, rs *runningServer, encryption bool, publicPort int, echoAddr string, payload []byte) {
	t.Helper()

	client, err := newTestClient(t, rs.addr, encryption)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("test-client", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !response.OK {
		t.Fatalf("authentication rejected: %s", response.Error)
	}

	if err := client.register(protocol.ProxySpec{
		Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "echo")

	streamErr := make(chan error, 1)
	go func() { streamErr <- client.serveOneStream(rs.addr, echoAddr, 10*time.Second) }()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor connect: %v", err)
	}
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echoed %q, want %q", got, payload)
	}
	_ = visitor.Close()

	select {
	case err := <-streamErr:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not finish")
	}
}

// waitForLedgerEntries blocks until the ledger file holds at least n entries. The
// entry is written by the connection handler as it tears down, so a reader has to
// allow for that rather than reading the file the moment the client closes.
func waitForLedgerEntries(t *testing.T, path string, n int) []ledger.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := ledger.ReadFile(path)
		if err == nil && len(entries) >= n {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("ledger %s holds %d entries after 5s, want at least %d", path, len(entries), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- tests --------------------------------------------------------------------

// slowService reads exactly n bytes, signals that it has them, then answers after a delay
// and closes. The signal is what makes the test deterministic: the stream is established and
// the payload has travelled through it before the control connection goes away.
func slowService(t *testing.T, n int, delay time.Duration, received chan<- int) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				data := make([]byte, n)
				if _, err := io.ReadFull(conn, data); err != nil {
					return
				}
				select {
				case received <- len(data):
				default:
				}
				time.Sleep(delay)
				_, _ = conn.Write(data)
			}()
		}
	}()
	return listener.Addr().String()
}

// A client can leave while one of its streams is still finishing: the stream records its
// bytes on the tunnel when its pipe returns, and the ledger entry is written from those
// counters. Without a wait for the streams, the entry of such a session reports 0 bytes —
// which is what macOS CI showed for the test below ("bytes_in is 0, want 29") once the
// half-close work widened the window between the control connection ending and the stream
// finishing.
func TestTheLedgerEntryIncludesAStreamThatIsStillFinishing(t *testing.T) {
	payload := []byte("twenty-eight bytes of traffic")
	received := make(chan int, 1)
	echoAddr := slowService(t, len(payload), 300*time.Millisecond, received)

	cfg := ledgerConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := client.authenticate("test-client", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	publicPort := freePort(t)
	if err := client.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echoAddr, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read proxy list: %v", err)
	}
	waitForListener(t, rs.server, "echo")

	streamErr := make(chan error, 1)
	go func() { streamErr <- client.serveOneStream(rs.addr, echoAddr, 10*time.Second) }()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor connect: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	if err := visitor.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("visitor half-close: %v", err)
	}

	// The stream is up and the payload has arrived at the service, which is about to answer
	// in a moment. The control connection leaves right now, while the answer is on its way.
	select {
	case n := <-received:
		if n != len(payload) {
			t.Fatalf("the service read %d bytes, want %d", n, len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the local service")
	}
	client.close()

	entries := waitForLedgerEntries(t, cfg.Ledger.Path, 1)
	if got := entries[0].BytesIn; got != int64(len(payload)) {
		t.Errorf("bytes_in is %d, want %d: the stream that was still finishing was left out",
			got, len(payload))
	}

	select {
	case err := <-streamErr:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not finish")
	}
}

func TestLedgerRecordsSessionUsageWhenTheClientLeaves(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := ledgerConfig(t, false)
	rs := startServer(t, cfg)

	payload := []byte("twenty-eight bytes of traffic")
	runOneStream(t, rs, false, freePort(t), echoAddr, payload)

	entries := waitForLedgerEntries(t, cfg.Ledger.Path, 1)
	entry := entries[0]

	if entry.Index != 0 {
		t.Errorf("first entry has index %d, want 0", entry.Index)
	}
	if entry.Proxy != "echo" {
		t.Errorf("entry names proxy %q, want %q", entry.Proxy, "echo")
	}
	if entry.ClientID == "" {
		t.Error("entry has no client id")
	}
	// The echo service returns every byte, so both directions carry the payload.
	if entry.BytesIn != int64(len(payload)) {
		t.Errorf("bytes_in is %d, want %d", entry.BytesIn, len(payload))
	}
	if entry.BytesOut != int64(len(payload)) {
		t.Errorf("bytes_out is %d, want %d", entry.BytesOut, len(payload))
	}
	if entry.PrevHash != "" {
		t.Errorf("first entry has prev_hash %q, want empty", entry.PrevHash)
	}
	if entry.Hash == "" || entry.Signature == "" {
		t.Error("entry is missing its hash or signature")
	}

	if n, err := ledger.Verify(entries, mustPublicKey(t, cfg.Ledger.SigningKey)); err != nil {
		t.Fatalf("the recorded chain does not verify at entry %d: %v", n, err)
	}
}

// TestLedgerRecordsHTTPProxyTraffic covers the billing of an http proxy. Its
// requests are served by the group's reverse proxy rather than by one member's
// stream handler, so the bytes have to reach the member counters the ledger reads;
// without that an http proxy is billed as zero however much it carried.
func TestLedgerRecordsHTTPProxyTraffic(t *testing.T) {
	service := startHTTPService(t, "billed")

	cfg := ledgerConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"billed.example.com"},
	})

	sent := "ask=1"
	request, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.Server.HTTPPort), strings.NewReader(sent))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Host = "billed.example.com"

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%s)", response.StatusCode, body)
	}

	// The entry is appended when the session ends.
	agent.client.close()

	entries := waitForLedgerEntries(t, cfg.Ledger.Path, 1)
	entry := entries[0]
	if entry.Proxy != "web" {
		t.Fatalf("entry names proxy %q, want %q", entry.Proxy, "web")
	}
	if entry.BytesIn != int64(len(body)) {
		t.Errorf("bytes_in is %d, want %d", entry.BytesIn, len(body))
	}
	if entry.BytesOut != int64(len(sent)) {
		t.Errorf("bytes_out is %d, want %d", entry.BytesOut, len(sent))
	}

	if n, err := ledger.Verify(entries, mustPublicKey(t, cfg.Ledger.SigningKey)); err != nil {
		t.Fatalf("the recorded chain does not verify at entry %d: %v", n, err)
	}
}

func TestLedgerAppendsOneEntryPerSession(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := ledgerConfig(t, false)
	rs := startServer(t, cfg)

	for i := 0; i < 2; i++ {
		runOneStream(t, rs, false, freePort(t), echoAddr, []byte("payload"))
	}

	entries := waitForLedgerEntries(t, cfg.Ledger.Path, 2)
	if len(entries) != 2 {
		t.Fatalf("recorded %d entries, want 2", len(entries))
	}
	if entries[1].Index != 1 {
		t.Errorf("second entry has index %d, want 1", entries[1].Index)
	}
	if entries[1].PrevHash != entries[0].Hash {
		t.Error("second entry does not commit to the first")
	}
	if entries[0].ClientID == entries[1].ClientID {
		t.Error("two separate sessions were recorded under the same client id")
	}
	if _, err := ledger.Verify(entries, mustPublicKey(t, cfg.Ledger.SigningKey)); err != nil {
		t.Fatalf("chain does not verify: %v", err)
	}
}

func TestLedgerSurvivesARestartAndKeepsOneKey(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Ledger.Enabled = true
	cfg.Ledger.Path = filepath.Join(dir, "chain.jsonl")
	cfg.Ledger.SigningKey = filepath.Join(dir, "chain.key")

	first, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := first.Append("client-a", "ssh", 10, 20); err != nil {
		t.Fatalf("append: %v", err)
	}
	firstKey := first.PublicKeyHex()
	firstHead := first.Head()
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()

	if second.PublicKeyHex() != firstKey {
		t.Fatalf("the restarted server uses public key %s, want %s", second.PublicKeyHex(), firstKey)
	}
	if second.Len() != 1 {
		t.Fatalf("restarted ledger holds %d entries, want 1", second.Len())
	}
	entry, err := second.Append("client-b", "web", 30, 40)
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if entry.Index != 1 {
		t.Errorf("the first entry appended after a restart has index %d, want 1", entry.Index)
	}
	if entry.PrevHash != firstHead {
		t.Error("the chain does not continue from the pre-restart head")
	}
	if _, err := ledger.Verify(second.Entries(), mustPublicKey(t, cfg.Ledger.SigningKey)); err != nil {
		t.Fatalf("chain does not verify after restart: %v", err)
	}
}

func TestLedgerDisabledCostsNothing(t *testing.T) {
	cfg := testConfig(t, false)
	store, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("openLedger with ledger disabled: %v", err)
	}
	if store != nil {
		t.Fatal("a ledger was opened even though ledger.enabled is false")
	}
	if err := store.Close(); err != nil {
		t.Errorf("closing a disabled ledger: %v", err)
	}
	if got := store.PublicKeyHex(); got != "" {
		t.Errorf("a disabled ledger reports public key %q, want an empty string", got)
	}

	srv := &Server{cfg: cfg, logger: discardLogger()}
	rendered, err := json.Marshal(srv.renderLedger(0))
	if err != nil {
		t.Fatalf("renderLedger: %v", err)
	}
	if string(rendered) != `{"enabled":false}` {
		t.Errorf("a disabled ledger renders as %s, want {\"enabled\":false}", rendered)
	}

	// Accounting from a server with no ledger must be a no-op rather than a panic.
	srv.recordSessionUsage(&Session{})
	srv.closeStores()
}

func TestRenderLedgerReportsRecentEntriesAndTotals(t *testing.T) {
	cfg := ledgerConfig(t, false)
	store, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		client := "client-a"
		if i%2 == 1 {
			client = "client-b"
		}
		if _, err := store.Append(client, "ssh", 100, 200); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	srv := &Server{cfg: cfg, logger: discardLogger(), ledger: store}
	srv.tunnels = newTunnelManager(cfg, srv.logger, nil, newSessionManager(0), newMetrics(), nil)
	srv.metrics = newMetrics()

	body, err := json.Marshal(srv.renderLedger(2))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var payload struct {
		Enabled   bool           `json:"enabled"`
		PublicKey string         `json:"public_key"`
		Count     int            `json:"count"`
		Returned  int            `json:"returned"`
		Head      string         `json:"head"`
		Entries   []ledger.Entry `json:"entries"`
		Totals    map[string]struct {
			BytesIn  int64 `json:"bytes_in"`
			BytesOut int64 `json:"bytes_out"`
			Entries  int   `json:"entries"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !payload.Enabled {
		t.Error("the ledger is enabled but the payload says otherwise")
	}
	if payload.Count != 5 {
		t.Errorf("count is %d, want 5", payload.Count)
	}
	if payload.Returned != 2 || len(payload.Entries) != 2 {
		t.Fatalf("returned %d entries, want the 2 most recent", payload.Returned)
	}
	if payload.Entries[1].Index != 4 {
		t.Errorf("the last returned entry has index %d, want 4", payload.Entries[1].Index)
	}
	if payload.Head != payload.Entries[1].Hash {
		t.Error("the reported head is not the last entry's hash")
	}
	if len(payload.PublicKey) != 64 {
		t.Errorf("the public key is %d hex characters, want 64", len(payload.PublicKey))
	}
	if payload.Totals["client-a"].Entries != 3 || payload.Totals["client-b"].Entries != 2 {
		t.Errorf("totals split as %d/%d entries, want 3/2",
			payload.Totals["client-a"].Entries, payload.Totals["client-b"].Entries)
	}
	if payload.Totals["client-a"].BytesIn != 300 || payload.Totals["client-b"].BytesOut != 400 {
		t.Errorf("totals are %+v and %+v, want 300 bytes in for a and 400 out for b",
			payload.Totals["client-a"], payload.Totals["client-b"])
	}
}

func TestVerifyLedgerFileAcceptsAKeyFileAndAPublicKey(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Ledger.Enabled = true
	cfg.Ledger.Path = filepath.Join(dir, "chain.jsonl")
	cfg.Ledger.SigningKey = filepath.Join(dir, "chain.key")

	store, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	if _, err := store.Append("client-a", "ssh", 11, 22); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.Append("client-a", "ssh", 1, 2); err != nil {
		t.Fatalf("append: %v", err)
	}
	publicHex := store.PublicKeyHex()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, key := range []string{publicHex, cfg.Ledger.SigningKey} {
		count, totals, head, err := VerifyLedgerFile(cfg.Ledger.Path, key)
		if err != nil {
			t.Fatalf("VerifyLedgerFile with key %q: %v", key, err)
		}
		if count != 2 {
			t.Errorf("verified %d entries, want 2", count)
		}
		if len(head) != 64 {
			t.Errorf("chain head is %q, want 64 hex characters", head)
		}
		if totals["client-a"].BytesIn != 12 || totals["client-a"].BytesOut != 24 {
			t.Errorf("totals are %+v, want 12 in and 24 out", totals["client-a"])
		}
	}
}

func TestVerifyLedgerFileRejectsTamperedRecordsAndWrongKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.jsonl")
	cfg := &config.Config{}
	cfg.Ledger.Enabled = true
	cfg.Ledger.Path = path
	cfg.Ledger.SigningKey = filepath.Join(dir, "chain.key")

	store, err := openLedger(cfg, discardLogger())
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	if _, err := store.Append("client-a", "ssh", 1, 2); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.Append("client-b", "ssh", 3, 4); err != nil {
		t.Fatalf("append: %v", err)
	}
	original := store.Entries()
	publishedHead := store.Head()
	publicHex := store.PublicKeyHex()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if stored, err := ledger.ReadFile(path); err != nil {
		t.Fatalf("read back the written ledger: %v", err)
	} else if len(stored) != 2 {
		t.Fatalf("the ledger file holds %d entries, want 2", len(stored))
	}

	// One altered byte count is a different record, so the stored signature no
	// longer covers it.
	tampered := append([]ledger.Entry(nil), original...)
	tampered[1].BytesIn = 999
	if err := writeEntries(path, tampered); err != nil {
		t.Fatalf("write tampered chain: %v", err)
	}
	if _, _, _, err := VerifyLedgerFile(path, publicHex); err == nil {
		t.Fatal("a tampered ledger verified")
	}

	// A truncated chain verifies against its own head, but that head is not the one
	// the operator published, which is how truncation is detected.
	if err := os.WriteFile(path, append(mustMarshal(t, original[0]), '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	count, _, head, err := VerifyLedgerFile(path, publicHex)
	if err != nil {
		t.Fatalf("a truncated chain should still verify against its own head: %v", err)
	}
	if count != 1 {
		t.Errorf("truncated chain verified %d entries, want 1", count)
	}
	if head == publishedHead {
		t.Error("the truncated chain reports the same head as the full chain")
	}

	// A key that did not sign the chain is refused.
	other, err := ledger.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	if err := writeEntries(path, original); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, _, err := VerifyLedgerFile(path, hex.EncodeToString(other.PublicKey())); err == nil {
		t.Fatal("a chain verified under a key that did not sign it")
	}
}

func TestLedgerKeyFileRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.key")
	if err := os.WriteFile(path, []byte("not hex at all\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := loadLedgerKey(path); err == nil {
		t.Fatal("a key file that is not hex was accepted")
	}

	short := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(short, []byte("00112233\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := loadLedgerKey(short); err == nil {
		t.Fatal("a key file holding the wrong number of bytes was accepted")
	}

	if _, err := parseVerificationKey(""); err == nil {
		t.Fatal("an empty verification key was accepted")
	}
	if _, err := parseVerificationKey(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a verification key naming a missing file was accepted")
	}

	// A key file with the right shape produces exactly its own public key.
	store, err := ledger.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	seedFile := filepath.Join(t.TempDir(), "seed.key")
	if err := os.WriteFile(seedFile, []byte(hex.EncodeToString(store.Seed())+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pub, err := parseVerificationKey(seedFile)
	if err != nil {
		t.Fatalf("parseVerificationKey(seed file): %v", err)
	}
	if !ed25519.PublicKey(pub).Equal(store.PublicKey()) {
		t.Error("the public key derived from a seed file does not match the key that owns it")
	}
}

func mustPublicKey(t *testing.T, keyFile string) []byte {
	t.Helper()
	key, _, err := loadLedgerKey(keyFile)
	if err != nil {
		t.Fatalf("loadLedgerKey: %v", err)
	}
	return key.PublicKey()
}

func mustMarshal(t *testing.T, entry ledger.Entry) []byte {
	t.Helper()
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return line
}

// writeEntries rewrites a ledger file from entries, which is how the tamper test
// produces a chain whose signatures no longer match its contents.
func writeEntries(path string, entries []ledger.Entry) error {
	data := make([]byte, 0, len(entries)*256)
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return os.WriteFile(path, data, 0o600)
}
