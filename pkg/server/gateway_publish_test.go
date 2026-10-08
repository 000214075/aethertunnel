package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// newPipeSession builds the session shape the SSH gateway and the dashboard code
// take as input: an internal session whose control connection is a pipe, since
// neither of them ever reads a real socket.
func newPipeSession(t *testing.T, srv *Server, clientID string, heartbeatSeconds int) *Session {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})
	return newSession(srv, conn, protocol.NewFramer(conn, nil, 0), &protocol.AuthRequest{
		ClientVersion: "test-client", Protocol: protocol.ProtocolVersion, ClientID: clientID,
	}, false, heartbeatSeconds)
}

// usageOf creates a name's counter when the name is registered, and a registration
// the client withdraws again never moves it. Billing that counter would append a
// permanent zero-byte entry for every name a client registers and drops — growth
// the client chooses, in the file an operator reads for what was carried. A name
// the session still publishes keeps its entry even when it carried nothing: the
// operator asked to bill that proxy, and the line is how the chain records it.
func TestTheLedgerSkipsAWithdrawnNameThatNeverCarriedAByte(t *testing.T) {
	cfg := ledgerConfig(t, false)
	srv, err := New(cfg, Options{Version: "test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// closeStores rather than the auditor alone: this test's config is file
	// backed, and the ledger's handle has to be down before the temporary
	// directory is removed — Windows refuses to unlink a file a live handle
	// still holds.
	t.Cleanup(func() { srv.closeStores() })

	session := newPipeSession(t, srv, "hp-box", 0)
	if _, created := session.usageOf("withdrawn"); !created {
		t.Fatal("a counter for a name that was never registered was reported as already there")
	}
	carried, _ := session.usageOf("carried")
	carried.bytesIn.Add(7)
	carried.bytesOut.Add(9)
	session.bytesFromClient.Store(7)
	session.bytesToClient.Store(9)
	// A name the session still holds: registered and never withdrawn, with a
	// counter that never moved either. group.add creates the counter when the
	// member joins, which is what usageOf does here.
	if _, created := session.usageOf("published"); !created {
		t.Fatal("a counter for a name that was never registered was reported as already there")
	}
	if err := session.RegisterProxy(&Tunnel{Name: "published", Session: session, totals: &trafficTotals{}}); err != nil {
		t.Fatalf("publish a name: %v", err)
	}

	srv.recordSessionUsage(session)

	data, err := os.ReadFile(cfg.Ledger.Path)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("the ledger holds %d entries, want one for the name that carried bytes and one for the published name:\n%s", lines, data)
	}
	if strings.Contains(string(data), `"withdrawn"`) {
		t.Fatalf("the ledger bills a name the client withdrew without carrying a byte:\n%s", data)
	}
	for _, want := range []string{`"carried"`, `"published"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the ledger is missing the entry for %s:\n%s", want, data)
		}
	}
}

// A publish the SSH gateway refuses has to reach the server's log and the audit
// trail, the way the same refusal over a client's register-proxy frame does: the
// gateway promises the client's own rules, and an operator watching for who tried
// what sees nothing in either place otherwise.
func TestARefusedSSHGatewayPublishIsAudited(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.jsonl")
	cfg.Server.AllowPorts = []string{"20000-20010"}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, Options{Version: "test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.auditor.Close() })

	session := newPipeSession(t, srv, "ssh-client", 0)
	if _, err := srv.registerSSHProxy(session, protocol.ProxySpec{
		Name: "ssh-echo", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: 30000,
	}); err == nil {
		t.Fatal("a remote port outside allow_ports was accepted over the ssh gateway")
	}

	events, err := readAuditEvents(t, cfg.Audit.Path)
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	rejected := 0
	for _, event := range events {
		if event.Event != EventProxyRejected {
			continue
		}
		rejected++
		if event.ClientID != session.ClientName() {
			t.Errorf("the refusal names client %q, want %q", event.ClientID, session.ClientName())
		}
		if event.Proxy != "ssh-echo" {
			t.Errorf("the refusal names proxy %q, want the one that was refused", event.Proxy)
		}
	}
	if rejected == 0 {
		t.Fatalf("a refused ssh publish left no %s record: %+v", EventProxyRejected, events)
	}
}

// A registration whose session closes underneath it keeps the counter of the name
// it published: bind came up first, so this proxy name was reachable — a private
// proxy reaches its visitors through the control connection — and a stream may be
// holding that counter and writing into it. Taking it back would drop those bytes
// and leave the ledger without a name the session carried. The same rollback still
// drops the counter when the endpoint never came up, which
// TestASSHGatewayRegistrationThatNeverPublishedLeavesNoCounter pins.
func TestASSHGatewayRegistrationThatLosesItsSessionKeepsItsPublishedCounter(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, Options{Version: "test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.auditor.Close() })

	session := newPipeSession(t, srv, "ssh-client", 0)
	session.Close("shutdown: the session ends during the registration")
	if _, err := srv.registerSSHProxy(session, protocol.ProxySpec{
		Name: "ssh-private", Type: protocol.ProxyTypeSTCP, LocalAddr: "127.0.0.1:1", SecretKey: "s3cret",
	}); err == nil {
		t.Fatal("a closed session published a proxy")
	}
	if usage := session.Usage(); len(usage) != 1 {
		t.Fatalf("the session dropped the counter of a name whose endpoint was live: %v", usage)
	}
}

// The audit records for one client have to be joinable by the same client_id: a
// proxy's registration names the stable client_id the client sent, so its removal
// has to name the same value rather than the session's random identifier.
func TestTheAuditTrailNamesTheSameClientWhenAProxyIsRemoved(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.jsonl")
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := client.authenticateAs("test-agent", testToken, "hp-box"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	agent := startAgentWithClient(t, client, rs.addr, map[string]dataHandler{"echo": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
	})

	// The client leaves, which is what removes its member.
	client.close()

	deadline := time.Now().Add(5 * time.Second)
	var removed []AuditEvent
	for {
		events, err := readAuditEvents(t, cfg.Audit.Path)
		if err == nil {
			removed = removed[:0]
			for _, event := range events {
				if event.Event == EventProxyRemoved {
					removed = append(removed, event)
				}
			}
		}
		if len(removed) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(removed) == 0 {
		t.Fatal("the removal of the client's proxy left no audit record")
	}
	if removed[0].ClientID != "hp-box" {
		t.Fatalf("the removal names client %q, and the registration of the same proxy named %q",
			removed[0].ClientID, "hp-box")
	}
}

// The ledger bills the members' own counters, and the share of a finished session
// is an integer division: without the remainder every finished session loses up to
// one byte per member, for as long as the deployment runs.
func TestAFinishedTransactionCreditsEveryByteToTheMembers(t *testing.T) {
	sessionA := &Session{done: make(chan struct{})}
	sessionB := &Session{done: make(chan struct{})}
	memberA := &Tunnel{Name: "web", Session: sessionA, totals: &trafficTotals{}}
	memberB := &Tunnel{Name: "web", Session: sessionB, totals: &trafficTotals{}}
	group := &ProxyGroup{Name: "web", members: []*Tunnel{memberA, memberB}, metrics: newMetrics()}

	group.recordHTTPTraffic(3, 5)

	if out := memberA.BytesOut.Load() + memberB.BytesOut.Load(); out != 3 {
		t.Errorf("the members were credited %d bytes to the client, want the 3 the request carried", out)
	}
	if in := memberA.BytesIn.Load() + memberB.BytesIn.Load(); in != 5 {
		t.Errorf("the members were credited %d bytes from the client, want the 5 the request carried", in)
	}
	if out := sessionA.bytesToClient.Load() + sessionB.bytesToClient.Load(); out != 3 {
		t.Errorf("the sessions report %d bytes to clients, want 3", out)
	}

	// A datagram session books the same counters through the same division.
	datagramA := &Session{done: make(chan struct{})}
	datagramB := &Session{done: make(chan struct{})}
	packetA := &Tunnel{Name: "dns", Session: datagramA, totals: &trafficTotals{}}
	packetB := &Tunnel{Name: "dns", Session: datagramB, totals: &trafficTotals{}}
	packets := &ProxyGroup{Name: "dns", members: []*Tunnel{packetA, packetB}, metrics: newMetrics()}

	packets.recordSession(7, 11)

	if out := packetA.BytesOut.Load() + packetB.BytesOut.Load(); out != 7 {
		t.Errorf("the members were credited %d datagram bytes to the client, want 7", out)
	}
	if in := packetA.BytesIn.Load() + packetB.BytesIn.Load(); in != 11 {
		t.Errorf("the members were credited %d datagram bytes from the client, want 11", in)
	}
}

// A session with no control loop never refreshes its heartbeat, so reporting the
// two heartbeat fields for it shows an operator a client that stopped
// heartbeating and an interval nothing keeps. The fields are what the panel
// renders; leaving them out of the payload is what makes it show nothing.
func TestTheDashboardLeavesOutHeartbeatFieldsForASessionWithoutAControlLoop(t *testing.T) {
	dashboard, base := newTestDashboard(t, nil)

	for _, heartbeat := range []int{0, 30} {
		if err := dashboard.server.sessions.Add(newPipeSession(t, dashboard.server, "test-client", heartbeat)); err != nil {
			t.Fatalf("add the session with a %d-second heartbeat: %v", heartbeat, err)
		}
	}

	resp := doDashboardRequest(t, http.MethodGet, base+"/api/clients", "dashboard-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/clients = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the client list: %v", err)
	}
	if len(payload.Clients) != 2 {
		t.Fatalf("the client list holds %d rows, want the 2 sessions", len(payload.Clients))
	}
	for _, row := range payload.Clients {
		seconds, reported := row["heartbeat_seconds"]
		_, hasLast := row["last_heartbeat"]
		if reported != hasLast {
			t.Fatalf("a row reports heartbeat_seconds=%v and last_heartbeat=%v", reported, hasLast)
		}
		if !reported {
			continue
		}
		if seconds != float64(30) {
			t.Fatalf("the row of the session that agreed an interval reports %v seconds, want 30", seconds)
		}
	}
}

// The page is the whole published surface of the panel's file server. Anything
// else in the directory it was embedded from is a file no visitor asked for, and
// the dashboard's file handler is deliberately unauthenticated.
func TestTheDashboardServesItsPageAndNothingElseFromTheDirectory(t *testing.T) {
	_, base := newTestDashboard(t, nil)

	page := doDashboardRequest(t, http.MethodGet, base+"/", "")
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	if page.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", page.StatusCode)
	}
	if !strings.Contains(string(body), "<title>AetherTunnel Dashboard</title>") {
		t.Fatalf("GET / did not serve the dashboard page (%d bytes)", len(body))
	}

	readme := doDashboardRequest(t, http.MethodGet, base+"/README.md", "")
	defer readme.Body.Close()
	if readme.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /README.md = %d, want 404: the directory's README is not the panel", readme.StatusCode)
	}
}

// A visitor that is refused after the challenge is reading the connection under
// the key the challenge agreed, so the refusal has to be written under it too.
// Sealing the refusal with the configured cipher leaves the visitor with a
// decryption failure where the server's own reason belongs, and the reason in the
// audit trail disagrees with what went out.
func TestAChallengedVisitorIsToldWhyItIsRefusedUnderTheSessionKey(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, true)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, true, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
		// The publisher's own identity may visit; this visitor names none.
		AllowUsers: []string{"the-publisher"},
	})

	static, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, testToken, "test-salt")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	framer := protocol.NewFramer(conn, static, 0)
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send visitor-connect: %v", err)
	}
	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		t.Fatalf("read the challenge: %v", err)
	}
	if len(challenge.SessionSalt) == 0 {
		t.Fatal("the server challenged without a session salt, so the connection never switches keys")
	}

	// The challenge was sealed with the configured cipher; the challenge moved both
	// ends onto the key derived from the salt, which is what the visitor answers on.
	proof, err := visitorProofFor(config.AuthMethodNIZK, "s3cret", protocol.VisitorProofContext("private", challenge.Nonce))
	if err != nil {
		t.Fatalf("build the proof: %v", err)
	}
	key, err := crypto.SessionKey(testToken, challenge.SessionSalt)
	if err != nil {
		t.Fatalf("derive the session key: %v", err)
	}
	sessionCipher, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
	if err != nil {
		t.Fatalf("cipher for the session key: %v", err)
	}
	framer = protocol.NewFramer(conn, sessionCipher, 0)
	if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
		t.Fatalf("send the proof: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("the refusal was not written under the key the challenge agreed: %v", err)
	}
	if ack.OK {
		t.Fatal("a visitor the proxy's allow_users does not name was accepted")
	}
	if !strings.Contains(ack.Error, "allows the identities") {
		t.Fatalf("the refusal says %q, want the reason the identity check gives", ack.Error)
	}
}
