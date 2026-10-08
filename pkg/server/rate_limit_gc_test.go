package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/oidc"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// The GC drops a bucket once it could have refilled to full. A rate so small
// that the refill outruns a time.Duration used to wrap the conversion
// negative, which made every bucket look older than idle: the GC then wiped
// the buckets of sources that were still being limited, handing each one a
// fresh burst every period.
func TestARateLimitGcKeepsBucketsARefillCannotRepresent(t *testing.T) {
	l := newRateLimiter(1e-12, 20) // refill to full would take 2e13 seconds
	l.allow("10.0.0.1")
	if l.Size() != 1 {
		t.Fatal("the bucket was not created")
	}
	l.lastGC = time.Now().Add(-l.gcPeriod - time.Second)
	l.collect(time.Now())
	if l.Size() != 1 {
		t.Fatal("the GC wiped a bucket whose refill time cannot be represented")
	}

	// A bucket that really has been idle longer than its refill still goes.
	fast := newRateLimiter(1, 20) // refill in 20 seconds
	fast.allow("10.0.0.2")
	fast.buckets["10.0.0.2"].seen = time.Now().Add(-time.Hour)
	fast.lastGC = time.Now().Add(-fast.gcPeriod - time.Second)
	fast.collect(time.Now())
	if fast.Size() != 0 {
		t.Fatal("an idle bucket that could have refilled was kept")
	}
}

// The metrics endpoint serves scrapes on its own address, so it is the one
// HTTP surface an operator commonly rebinds away from localhost. Without
// read/write/idle timeouts a keep-alive connection that goes quiet after one
// response pinned its goroutine and descriptor forever.
func TestTheMetricsListenerBoundsItsConnections(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Metrics.Enabled = true
	cfg.Metrics.BindAddr = "127.0.0.1"
	cfg.Metrics.Port = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	listener := NewMetricsListener(rs.server, log.New(io.Discard, "", 0))
	if err := listener.Start(); err != nil {
		t.Fatalf("start the metrics listener: %v", err)
	}
	t.Cleanup(listener.Stop)

	if listener.server.ReadTimeout == 0 || listener.server.WriteTimeout == 0 || listener.server.IdleTimeout == 0 {
		t.Fatalf("the metrics server carries no read/write/idle timeouts: %+v", listener.server)
	}
}

// A handler whose control connection was already sitting in the accept
// backlog can finish authenticating after Shutdown has taken its snapshot and
// closed every session: the late session used to be registered anyway, and it
// then ran — holding Run's WaitGroup and any ports it bound — until the
// client disconnected on its own.
func TestASessionThatArrivesDuringShutdownIsRefused(t *testing.T) {
	m := newSessionManager(0)

	conn1, conn2 := net.Pipe()
	defer conn2.Close()
	live := &Session{ID: "early", conn: conn1, done: make(chan struct{}),
		tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}
	if err := m.Add(live); err != nil {
		t.Fatalf("Add before shutdown: %v", err)
	}

	m.CloseAll("test")

	late := &Session{ID: "late", conn: conn1, done: make(chan struct{}),
		tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}
	if err := m.Add(late); !errors.Is(err, errShuttingDown) {
		t.Fatalf("a session registered during shutdown was accepted: %v", err)
	}
	if !live.IsClosed() {
		t.Fatal("the session registered before the shutdown was not closed")
	}
}

// A [[http_plugins]] entry that lists one op twice used to run the plugin
// twice per event; frp's contract is one call per event, which is what
// counting and gating plugins build on.
func TestADuplicatePluginOpRunsOnce(t *testing.T) {
	// The answer must parse: an unreadable body makes runLogin give up after
	// the first plugin, which would hide a second registration entirely.
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) { return 200, "{}" })
	cfg := pluginConfig(t, hook.URL, "login", "login")
	m := newHTTPPluginManager(cfg, log.New(io.Discard, "", 0))

	m.runLogin(&Session{ID: "s1", ClientID: "c1", RemoteAddr: "127.0.0.1:1", done: make(chan struct{})},
		&protocol.AuthRequest{})

	if got := len(hook.bodies); got != 1 {
		t.Fatalf("the plugin saw %d login calls for one event, want 1", got)
	}
}

// A registration can arrive without the client's own validation in front of
// it — a hand-written protocol message, an SSH command, a plugin's rewritten
// spec. The name-routing tables key on the domain strings verbatim, so a
// value with stray whitespace or an empty label used to publish successfully
// and then be unreachable.
func TestARegistrationWithAnUnusableDomainIsRefused(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	for _, domain := range []string{" example.com", ".", "*.", "ex ample.com"} {
		spec := protocol.ProxySpec{
			Name: "web", Type: protocol.ProxyTypeHTTP,
			LocalAddr: "127.0.0.1:80", Domains: []string{domain},
		}
		if err := agent.client.register(spec); err != nil {
			t.Fatalf("register with domain %q: %v", domain, err)
		}
		msg := agent.expectRefused("web")
		if !strings.Contains(msg, "not a usable hostname") {
			t.Fatalf("domain %q: the refusal does not name the domain problem: %s", domain, msg)
		}
	}
}

// A joiner whose hostname extend fails detaches itself. When the last other
// member leaves while that extend is stuck, the failure empties the group —
// and nobody used to close it: the name bindings stayed installed for a group
// the manager had dropped, so the name (and every hostname it carried) could
// not be published again until the process restarted.
func TestAJoinerThatFailsToExtendDoesNotOrphanTheName(t *testing.T) {
	echo := countingEcho(t, "orphan")
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	// Another proxy owns the domain the joiner will bring.
	owner := startAgent(t, rs.addr, false, map[string]dataHandler{"x": streamHandler(echo)})
	owner.register(protocol.ProxySpec{
		Name: "x", Type: protocol.ProxyTypeHTTP, LocalAddr: echo,
		Domains: []string{"taken.example"},
	})

	// The pool's first member binds d1.example.
	first := startAgent(t, rs.addr, false, map[string]dataHandler{"p1": streamHandler(echo)})
	first.register(protocol.ProxySpec{
		Name: "p1", Type: protocol.ProxyTypeHTTP, LocalAddr: echo, Group: "pool1",
		Domains: []string{"d1.example"},
	})

	group := rs.server.tunnels.groups["p1"]
	if group == nil {
		t.Fatal("the pool group is missing")
	}
	group.endpointMu.RLock()
	binding := group.vhost
	group.endpointMu.RUnlock()
	if binding == nil {
		t.Fatal("the pool group has no vhost binding")
	}

	// Hold the router lock so the joiner's extend blocks after it appended.
	binding.router.mu.Lock()

	joinerConn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	joiner := &testClient{conn: joinerConn, framer: protocol.NewFramer(joinerConn, nil, 0)}
	if _, err := joiner.authenticate("t16", testToken); err != nil {
		t.Fatalf("authenticate the joiner: %v", err)
	}
	joinerSpec := protocol.ProxySpec{
		Name: "p1", Type: protocol.ProxyTypeHTTP, LocalAddr: echo, Group: "pool1",
		Domains: []string{"taken.example"},
	}
	registered := make(chan error, 1)
	go func() { registered <- joiner.register(joinerSpec) }()

	// The joiner is in the pool and its extend is stuck on the router lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if group.memberCount() == 2 {
			break
		}
		if time.Now().After(deadline) {
			binding.router.mu.Unlock()
			t.Fatal("the joiner never entered the pool")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The first member's session drops while the extend is stuck: the pool
	// still holds the joiner, so the removal leaves the group open.
	first.client.close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if group.memberCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			binding.router.mu.Unlock()
			t.Fatal("the first member never left")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now the extend runs, hits the conflict, and detaches the joiner —
	// leaving the group empty, which is the case that used to be stranded.
	binding.router.mu.Unlock()
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner's registration never came back")
	}

	// The emptied group is closed, so the name — and the hostname its first
	// member bound — can be published again.
	retry := startAgent(t, rs.addr, false, map[string]dataHandler{"p1": streamHandler(echo)})
	retry.register(protocol.ProxySpec{
		Name: "p1", Type: protocol.ProxyTypeHTTP, LocalAddr: echo, Group: "pool1",
		Domains: []string{"d1.example"},
	})
}

// A wildcard owns its base domain on the tcpmux and TLS-passthrough tables;
// the vhost table used to answer only the subdomains, so the same pattern
// behaved differently per proxy type and a move between them silently lost
// the bare domain.
func TestAVhostWildcardOwnsItsBaseDomain(t *testing.T) {
	cfg := &config.Config{}
	manager := &TunnelManager{cfg: cfg, logger: log.New(io.Discard, "", 0), groups: map[string]*ProxyGroup{},
		policies: &proxyPolicies{}, metrics: newMetrics()}
	router := newVhostRouter("http", cfg, log.New(io.Discard, "", 0), manager.metrics)
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP,
		Domains: []string{"*.example.com"}}, manager)
	if _, err := router.add(group); err != nil {
		t.Fatalf("add: %v", err)
	}

	for _, host := range []string{"a.example.com", "a.b.example.com", "example.com", "EXAMPLE.com."} {
		if matched := router.lookup(host); len(matched) == 0 {
			t.Fatalf("host %q does not reach the wildcard", host)
		}
	}
	if matched := router.lookup("notexample.com"); len(matched) != 0 {
		t.Fatalf("an unrelated host reached the wildcard: %q", "notexample.com")
	}
	if matched := router.lookup("badexample.com"); len(matched) != 0 {
		t.Fatalf("a host that merely ends with the suffix reached the wildcard: %q", "badexample.com")
	}
}

// use_encryption beside [oidc] authentication: the per-proxy key used to be
// derived from each side's static token, which an [oidc] session never agreed
// on — the client presented an access token, so the stream arrived as garbage
// with no error anywhere. Both ends now derive from the credential the session
// authenticated with, so the round trip only completes when they match.
func TestAUseEncryptionTunnelCarriesBytesUnderOidcAuth(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the issuer key: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer jwks.Close()

	backend := echoService(t)
	cfg := testConfig(t, false)
	cfg.OIDC = &config.OIDCConfig{
		Issuer:   "https://issuer.example",
		Audience: "aethertunnel",
		JWKSURL:  jwks.URL + "/jwks",
	}
	port := freePort(t)
	rs := startServer(t, cfg)

	accessToken, err := oidc.SignToken("RS256", "k1", rsaKey, nil, map[string]any{
		"iss": "https://issuer.example",
		"aud": "aethertunnel",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign the access token: %v", err)
	}

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	response, err := client.authenticate("oidc-agent", accessToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !response.OK {
		t.Fatalf("the oidc token was refused: %s", response.Error)
	}
	agent := startAgentWithClient(t, client, rs.addr, map[string]dataHandler{"sealed": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "sealed", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
		UseEncryption: true,
	})

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := strings.Repeat("oidc-sealed-", 300)
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != payload {
		t.Fatal("the per-proxy keys disagreed: the oidc session's encrypted tunnel did not restore the payload")
	}
	if request := agent.requestFor(t, "sealed"); !request.Encrypted {
		t.Fatal("the server did not echo use_encryption in its DataRequest")
	}
}

// The signing key file is re-read for two seconds when its first read does not
// parse, which covers the O_EXCL loser reading a half-written file. The error
// that finally comes back has to describe that adoption attempt; reporting the
// stale first-read parse error instead sent the operator after a problem the
// file no longer had.
func TestAnUnadoptableSigningKeyFileNamesTheAdoption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.key")
	if err := os.WriteFile(path, []byte("this file holds no ed25519 seed\n"), 0o600); err != nil {
		t.Fatalf("write the key file: %v", err)
	}

	start := time.Now()
	_, _, err := loadLedgerKey(path)
	if err == nil {
		t.Fatal("loadLedgerKey accepted a key file that holds no seed")
	}
	if !strings.Contains(err.Error(), "adopting the signing key failed") {
		t.Fatalf("the error does not report the adoption attempt: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("the adoption retry gave up after %s, want the full window", elapsed)
	}
}

// Start hands the serve goroutine its own copy of the server pointer: Stop
// nils the field, and a goroutine that read the field on its way into Serve
// used to dereference that nil. A start/stop pair immediately after another is
// what used to hit it.
func TestStartAndStopOfTheMetricsListenerDoNotRace(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Metrics.Enabled = true
	cfg.Metrics.BindAddr = "127.0.0.1"
	cfg.Metrics.Port = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	listener := NewMetricsListener(rs.server, log.New(io.Discard, "", 0))
	for i := 0; i < 200; i++ {
		if err := listener.Start(); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		listener.Stop()
	}
}

// A pooled connection used to be counted twice on data_connections_total:
// once when it was parked and again when serveData put it to use. The counter
// says how many connections the client opened, so each one counts once.
func TestDataConnectionsAreCountedOnce(t *testing.T) {
	backend := echoService(t)
	cfg := testConfig(t, false)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": streamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
	})

	// Park one connection on the agent's session. A pooled connection is a
	// bare data connection: the session id in the DataOpen is its credential,
	// exactly as the real pool worker sends it.
	dataConn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dataConn.Close()
	parkedFramer := protocol.NewFramer(dataConn, nil, 0)
	if err := parkedFramer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: agent.client.session, Pool: true,
	}); err != nil {
		t.Fatalf("offer the pooled connection: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := parkedFramer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil || !ack.OK {
		t.Fatalf("the pooled connection was not confirmed: ack=%+v err=%v", ack, err)
	}
	before := rs.server.metrics.dataConnections.Load()
	if before != 1 {
		t.Fatalf("the parked connection was counted %d time(s), want 1", before)
	}

	// A visitor arrives; the server must put the parked connection to use,
	// and the counter must stay where it is.
	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	payload := []byte("counted-once")
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}

	// The parked leg answers the data request and echoes the payload, so the
	// round trip proves the pairing went through serveData.
	msg, err := parkedFramer.ReadFrame()
	if err != nil {
		t.Fatalf("read the data request: %v", err)
	}
	if msg.Type != protocol.TypeDataRequest {
		t.Fatalf("the parked connection got %s, want a data request", msg.Type)
	}
	var request protocol.DataRequest
	if err := json.Unmarshal(msg.Payload, &request); err != nil {
		t.Fatalf("decode the data request: %v", err)
	}
	if err := parkedFramer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: agent.client.session, Proxy: request.Proxy, StreamID: request.StreamID,
	}); err != nil {
		t.Fatalf("answer the data request: %v", err)
	}
	// The pairing is confirmed on this connection like on any data one.
	var paired protocol.DataOpenAck
	if err := parkedFramer.ReadJSON(protocol.TypeDataOpenAck, &paired); err != nil || !paired.OK {
		t.Fatalf("the pair was not confirmed: ack=%+v err=%v", paired, err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(dataConn, echoed); err != nil {
		t.Fatalf("read the relayed payload: %v", err)
	}
	// The parked leg is the far end of this stream: echo the payload back,
	// which proves bytes flow both ways before the counter is read.
	if _, err := dataConn.Write(echoed); err != nil {
		t.Fatalf("echo the payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the echoed payload is %q, want %q", got, payload)
	}

	if got := rs.server.metrics.dataConnections.Load(); got != before {
		t.Fatalf("the pooled connection moved the counter by %d, want 0", got-before)
	}
}

// The gate that caps session channels has to refuse past the limit and hand
// the slot back on release; a refused attempt holds nothing.
func TestASessionChannelGateStopsAtItsLimit(t *testing.T) {
	var gate channelGate
	for i := int64(0); i < maxSSHSessionChannels; i++ {
		if !gate.reserve(maxSSHSessionChannels) {
			t.Fatalf("reservation %d was refused", i)
		}
	}
	if gate.reserve(maxSSHSessionChannels) {
		t.Fatal("a reservation past the limit was granted")
	}
	gate.release()
	if !gate.reserve(maxSSHSessionChannels) {
		t.Fatal("a slot freed by release was not reusable")
	}
}

// CloseAll's snapshot is what Shutdown disconnects streams over, so it has to
// be exactly the sessions the call closed — and it is only complete under the
// lock that also closes the registry, because a handler removes its session
// from the registry the moment its control connection drops.
func TestCloseAllHandsBackTheSessionsItClosed(t *testing.T) {
	m := newSessionManager(0)

	conn1, conn2 := net.Pipe()
	defer conn2.Close()
	newSession := func(id string) *Session {
		return &Session{ID: id, conn: conn1, done: make(chan struct{}),
			tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}
	}
	first, second := newSession("first"), newSession("second")
	if err := m.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if err := m.Add(second); err != nil {
		t.Fatalf("Add second: %v", err)
	}

	snapped := m.CloseAll("test")
	if len(snapped) != 2 {
		t.Fatalf("CloseAll returned %d session(s), want the 2 it closed", len(snapped))
	}
	for _, session := range snapped {
		if !session.IsClosed() {
			t.Fatalf("session %s was returned but not closed", session.ID)
		}
	}
	if err := m.Add(newSession("late")); !errors.Is(err, errShuttingDown) {
		t.Fatalf("a session registered after the shutdown was accepted: %v", err)
	}
}

// A VPN refusal happens after the session was registered and counted as
// accepted; docs/SECURITY.md puts every post-acceptance failure on the
// accepted side, so counting it in control_rejected as well would make
// accepted plus rejected exceed the connections the server answered.
func TestAVpnRefusalAfterAcceptanceIsCountedAsAcceptedOnly(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.conn.Close()
	// The server has no [vpn] section, so a client asking for a tunnel is
	// refused — but only after it authenticated.
	if err := client.framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "t16", Protocol: protocol.ProtocolVersion,
		Encryption: client.cipher.Algorithm(), VPN: true,
	}); err != nil {
		t.Fatalf("send the auth request: %v", err)
	}
	var response protocol.AuthResponse
	if err := client.framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		t.Fatalf("read the auth response: %v", err)
	}
	if response.OK {
		t.Fatal("a session asking for a tunnel on a server without one was accepted")
	}
	if got := rs.server.metrics.controlAccepted.Load(); got != 1 {
		t.Fatalf("aethertunnel_control_connections_total is %d, want 1: the session passed the handshake", got)
	}
	if got := rs.server.metrics.controlRejected.Load(); got != 0 {
		t.Fatalf("aethertunnel_control_rejected_total is %d, want 0: the refusal came after acceptance", got)
	}
}

// The server refuses a client_id past 128 bytes before authentication: the
// bandwidth ledger keys its per-client totals by the name and writes it into
// every JSONL line, so an unbounded one let one authenticated connection grow
// permanent storage once per closed stream. pkg/config pins the same number in
// its client.client_id check, which is why it is asserted here.
func TestAnOverlongClientIDIsRefusedBeforeAuthentication(t *testing.T) {
	if maxClientIDLen != 128 {
		t.Fatalf("maxClientIDLen is %d; pkg/config's client.client_id check pins 128", maxClientIDLen)
	}
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.conn.Close()
	response, err := client.authenticateAs("t16", testToken, strings.Repeat("long-", 40))
	if err != nil {
		t.Fatalf("read the auth response: %v", err)
	}
	if response.OK {
		t.Fatal("an auth request carrying a 200-byte client_id was accepted")
	}
	if !strings.Contains(response.Error, "client_id") {
		t.Fatalf("the refusal does not name client_id: %s", response.Error)
	}
	if got := rs.server.metrics.controlRejected.Load(); got != 1 {
		t.Fatalf("aethertunnel_control_rejected_total is %d, want 1 for the unusable auth request", got)
	}

	// A name of a sane length still authenticates.
	okClient, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer okClient.conn.Close()
	response, err = okClient.authenticateAs("t16", testToken, "reasonable-name")
	if err != nil {
		t.Fatalf("read the auth response: %v", err)
	}
	if !response.OK {
		t.Fatalf("a normal client_id was refused: %s", response.Error)
	}
}

// A sudp visitor association that goes silent has to be torn down by the
// tunnel's idle timeout: without it, a visitor that stops reading while
// keeping the connection open pins the client data connection and both relay
// goroutines until the process ends. Traffic has to keep it alive, so the
// teardown must land only after the last datagram.
func TestAnIdleSudpVisitorRelayIsTornDown(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.ReadTimeoutSecs = 2
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private-udp": datagramHandlerPatient(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private-udp", Type: protocol.ProxyTypeSUDP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, framer := visitorHandshake(t, rs.addr, "private-udp", "s3cret", protocol.ProxyTypeSUDP)
	defer conn.Close()

	roundTrip := func(payload string) {
		t.Helper()
		if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: []byte(payload)}); err != nil {
			t.Fatalf("send: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		msg, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("read the echo of %q: %v", payload, err)
		}
		if string(msg.Payload) != payload {
			t.Fatalf("echo returned %q, want %q", msg.Payload, payload)
		}
	}
	roundTrip("first datagram")

	// Halfway into what would be the idle window, traffic proves the relay is
	// still there — and refreshes the deadline, which is what lets the second
	// leg through on a relay whose timeout is being honoured.
	time.Sleep(1200 * time.Millisecond)
	roundTrip("second datagram")

	// Now go silent. The teardown lands within the idle window of the last
	// datagram; a relay without the bound never closes at all and the read
	// below runs into its own deadline instead.
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	start := time.Now()
	if _, err := framer.ReadFrame(); err == nil {
		t.Fatal("the idle association was not torn down; the visitor connection is still open")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the teardown took %s after the last datagram, want the %s idle window",
			elapsed.Round(time.Millisecond), time.Duration(cfg.Server.ReadTimeoutSecs)*time.Second)
	}
}

// datagramHandlerPatient is datagramHandler without the 3-second local read
// deadline. The idle-teardown test needs both ends to sit still, so the only
// thing that can end the association is the server's own idle bound; the
// plain handler's deadline would tear the data connection down itself and
// mask whatever the server does.
func datagramHandlerPatient(localAddr string) dataHandler {
	return func(conn net.Conn, framer *protocol.Framer) {
		local, err := net.Dial("udp", localAddr)
		if err != nil {
			_ = conn.Close()
			return
		}
		defer local.Close()
		defer conn.Close()

		done := make(chan struct{}, 2)
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				msg, err := framer.ReadFrame()
				if err != nil {
					return
				}
				if msg.Type != protocol.TypeUDPPacket {
					continue
				}
				if _, err := local.Write(msg.Payload); err != nil {
					return
				}
			}
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			buf := make([]byte, 65535)
			for {
				n, err := local.Read(buf)
				if err != nil {
					return
				}
				if err := framer.WriteFrame(&protocol.Message{
					Type:    protocol.TypeUDPPacket,
					Payload: buf[:n],
				}); err != nil {
					return
				}
			}
		}()

		<-done
		_ = conn.Close()
		_ = local.Close()
		<-done
	}
}

// A vpn router that dies on its own — a tun device that goes away, a read
// that keeps failing — used to leave its error queued in the done channel
// until Close consumed it, while the service went on assigning addresses to
// sessions whose packets had nowhere to go. The death is now recorded when it
// happens and acquire refuses sessions a dead router cannot serve.
func TestAVpnRouterThatDiesOnItsOwnStopsTheAddresses(t *testing.T) {
	pool, err := vpn.NewPool("10.7.0.0/24")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	router, err := vpn.NewRouter(&failingVPNDevice{}, vpn.Options{MTU: 1400})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &vpnService{
		router: router,
		pool:   pool,
		logger: log.New(io.Discard, "", 0),
		leases: make(map[string]net.IP),
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go service.watchRouter(ctx)

	select {
	case <-service.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the router goroutine did not report its death")
	}

	transport := vpn.NewChannelTransport(func(packet []byte) error { return nil })
	address, peer, err := service.acquire("stalled-session", transport)
	if err == nil {
		_ = peer
		t.Fatalf("acquire handed out %s with the router down", address)
	}
	if !strings.Contains(err.Error(), "router has stopped") {
		t.Fatalf("the refusal does not name the stopped router: %v", err)
	}
}

// failingVPNDevice is a tun device whose read side has already failed, which
// is what an interface that was removed looks like to the router.
type failingVPNDevice struct{}

func (d *failingVPNDevice) Read([]byte) (int, error)  { return 0, errors.New("device gone") }
func (d *failingVPNDevice) Write([]byte) (int, error) { return 0, errors.New("device gone") }
func (d *failingVPNDevice) Close() error              { return nil }
func (d *failingVPNDevice) Name() string              { return "failing0" }
func (d *failingVPNDevice) MTU() int                  { return 1400 }

// The name-routing entry points sit behind no access list, so any scanner can
// produce one refusal line per packet. The first burst is written one by one;
// after that the refusals are summarised once a minute, the way the datagram
// pump throttles its read errors.
func TestARefusalFloodIsSummarisedNotLoggedLineByLine(t *testing.T) {
	buf := &strings.Builder{}
	clock := time.Unix(0, 0)
	throttle := &refusalLog{now: func() time.Time { return clock }}
	logger := log.New(writerFunc(buf.WriteString), "", 0)

	for i := 0; i < 100; i++ {
		throttle.logf(logger, "refusal %d", i)
	}
	if lines := strings.Count(buf.String(), "\n"); lines != refusalLogBurst {
		t.Fatalf("100 refusals produced %d lines within the minute, want %d one-by-one and no more", lines, refusalLogBurst)
	}

	clock = clock.Add(61 * time.Second)
	throttle.logf(logger, "refusal 100")
	summary := buf.String()
	if lines := strings.Count(summary, "\n"); lines != refusalLogBurst+1 {
		t.Fatalf("the first refusal after the window produced %d lines in total, want %d", lines, refusalLogBurst+1)
	}
	if !strings.Contains(summary, "suppressed 36 more refusals") {
		t.Fatalf("the summary does not account for the suppressed refusals: %s", summary)
	}
}

// writerFunc adapts a write function to io.Writer for the test loggers.
type writerFunc func(string) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(string(p)) }

// A peer that stops answering the forwarded-tcpip channels the gateway opens
// used to leave one goroutine per abandoned visitor stream parked in the
// channel open until the whole connection ended: the ssh mux has no deadline
// for the answer, and nothing else reaped the serve goroutine. The
// pending-forward gate bounds those waits at the same cap the session
// channels use, so past it the streams fail fast and the process stops
// growing.
func TestAnSSHClientThatNeverAnswersForwardedChannelsCannotGrowTheProcessUnboundedly(t *testing.T) {
	echoAddr := startEcho(t)
	echoPort := portOf(t, echoAddr)
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")

	cfg, gwPort := sshGatewayConfig(t, keyPath)
	rs := startServerLoggingTo(t, cfg, io.Discard)
	_ = rs
	// startServerLoggingTo waits for the control listener; the gateway binds on
	// its own, and on a loaded runner the dial below raced that bind and read
	// "connection refused" from a server that was starting fine. Every other
	// gateway test waits for the port; this one now does too.
	waitForTCPPort(t, gwPort)

	gwAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(gwPort))
	raw, err := net.DialTimeout("tcp", gwAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial ssh gateway: %v", err)
	}
	defer raw.Close()
	// chans is deliberately not serviced; see the goroutine comment below.
	conn, _, reqs, err := ssh.NewClientConn(raw, gwAddr, &ssh.ClientConfig{
		User:            "v0",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh handshake: %v", err)
	}
	// Global requests are answered; the NewChannel stream is deliberately left
	// unserviced. The mux queues the server's channel opens there and answers
	// nothing — that is the stalled peer this test is about.
	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	publicPort := freePort(t)
	ok, _, err := conn.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", uint32(publicPort)}))
	if err != nil || !ok {
		t.Fatalf("tcpip-forward: ok=%v err=%v", ok, err)
	}

	channel, _, err := conn.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open a session channel: %v", err)
	}
	command := fmt.Sprintf("tcp --proxy_name web --remote_port %d --local_ip 127.0.0.1 --local_port %d --token %s",
		publicPort, echoPort, testToken)
	if _, err := channel.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{command})); err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}
	banner := readSSHUntil(t, channel, "RemoteAddress:", 5*time.Second)
	if !strings.Contains(banner, "ProxyName: web") {
		t.Fatalf("banner %q does not name the proxy", banner)
	}

	before := runtime.NumGoroutine()
	visitors := make([]net.Conn, 0, 400)
	defer func() {
		for _, v := range visitors {
			_ = v.Close()
		}
	}()
	// The banner names the published address before the listener is opened, so
	// the first dials could race it on a loaded runner; wait for the port the
	// way the other gateway tests wait for theirs.
	waitForTCPPort(t, publicPort)
	for i := 0; i < 400; i++ {
		v, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
		if err != nil {
			t.Fatalf("visitor %d: %v", i, err)
		}
		visitors = append(visitors, v)
	}
	// The visitor's own wait resolves every stream the peer never answers;
	// the serve goroutines parked in the channel open are the ones that
	// counted. Wait out that budget, then count what is left.
	time.Sleep(6 * time.Second)
	if grew := runtime.NumGoroutine() - before; grew > 300 {
		t.Fatalf("400 unanswered visitor streams grew the process by %d goroutines; the pending-forward gate is not bounding them", grew)
	}
}

// The server's stream idle cap is read_timeout_seconds. /api/config used to
// echo it under idle_timeout_seconds — a key that exists under [client] and
// [server.ssh_tunnel_gateway] but not [server] — so an operator who copied the
// panel's key into the server section changed nothing.
func TestTheConfigViewNamesTheStreamIdleCapByItsRealKey(t *testing.T) {
	_, base := newTestDashboard(t, func(cfg *config.Config) {
		cfg.Server.ReadTimeoutSecs = 77
	})
	resp := doDashboardRequest(t, http.MethodGet, base+"/api/config", "dashboard-secret")
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read /api/config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	server, _ := parsed["server"].(map[string]any)
	if server == nil {
		t.Fatalf("/api/config has no server section: %v", parsed)
	}
	if _, present := server["idle_timeout_seconds"]; present {
		t.Fatal("/api/config echoes the stream idle cap under idle_timeout_seconds, which no [server] key carries")
	}
	if got, _ := server["read_timeout_seconds"].(float64); got != 77 {
		t.Fatalf("/api/config reports read_timeout_seconds %v, want 77", server["read_timeout_seconds"])
	}
}
