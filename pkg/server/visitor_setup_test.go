package server

import (
	"encoding/json"
	"fmt"
	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- the visitor relay leg agrees its own per-session key --------------------

// Without a post-quantum exchange the whole relayed visitor stream used to stay
// under the static cipher the configuration derives — the same long-lived key
// that seals every handshake on the server, spending AES-GCM random nonces on
// every record of bulk traffic. The challenge now carries a fresh salt, both
// ends derive the visitor session's key from the presented token and that salt,
// and everything after the challenge — the proof, the ack, the relayed bytes —
// only makes sense under it.
func TestAVisitorWithoutPostQuantumStillAgreesAPerSessionKey(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, true)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, true, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	})

	staticCipher, err := crypto.NewCipher(cfg.Encryption.Algorithm, testToken, cfg.Encryption.Salt)
	if err != nil {
		t.Fatalf("static cipher: %v", err)
	}
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, staticCipher, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send visitor-connect: %v", err)
	}

	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		t.Fatalf("read the challenge: %v", err)
	}
	if len(challenge.SessionSalt) != crypto.SessionSaltLen {
		t.Fatalf("the challenge carries a %d byte session salt, want %d", len(challenge.SessionSalt), crypto.SessionSaltLen)
	}

	// Both ends switch right after the challenge: the proof that follows only
	// makes sense under the key derived from the salt, so a completed handshake
	// proves the switch happened on both sides.
	key, err := crypto.SessionKey(testToken, challenge.SessionSalt)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	sessionCipher, err := crypto.NewCipherFromKey(cfg.Encryption.Algorithm, key)
	if err != nil {
		t.Fatalf("session cipher: %v", err)
	}
	framer = protocol.NewFramer(conn, sessionCipher, 0)

	proof, err := visitorProofFor(config.AuthMethodNIZK, "s3cret", protocol.VisitorProofContext("private", challenge.Nonce))
	if err != nil {
		t.Fatalf("build the proof: %v", err)
	}
	if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
		t.Fatalf("send the proof under the derived session key: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack under the derived session key: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the visitor was refused: %s", ack.Error)
	}
	_ = conn.SetDeadline(time.Time{})

	// The relayed stream is sealed with the same derived key: both directions
	// only make sense under it, so a round trip proves the relay leg switched.
	stream := crypto.NewStream(conn, sessionCipher)
	payload := []byte("a visitor relay sealed under its own key")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("send through the relay: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read through the relay: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
}

// A server without encryption never grew a salt: the challenge of a plain
// visitor carries none, and the handshake runs unsealed end to end exactly as
// before.
func TestAVisitorWithoutEncryptionCarriesNoSessionSalt(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send visitor-connect: %v", err)
	}

	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		t.Fatalf("read the challenge: %v", err)
	}
	if len(challenge.SessionSalt) != 0 {
		t.Fatal("an unencrypted server drew a session salt")
	}

	proof, err := visitorProofFor(config.AuthMethodNIZK, "s3cret", protocol.VisitorProofContext("private", challenge.Nonce))
	if err != nil {
		t.Fatalf("build the proof: %v", err)
	}
	if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
		t.Fatalf("send the proof: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the visitor was refused: %s", ack.Error)
	}
	_ = conn.SetDeadline(time.Time{})

	payload := []byte("a plain visitor relay stays plain")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
}

// --- the newProxy webhooks see each other's rewrites -------------------------

// runNewProxy built the content once, before its loop, so every plugin judged
// and rewrote the registration as it arrived: a second plugin that answered
// unchange=false with the content it was shown — the shape a plugin that only
// annotates or logs has — put the first plugin's accepted rewrite back the way
// it came in. Each plugin is now handed the spec as it stands.
func TestASecondNewProxyPluginSeesTheFirstPluginsRewrite(t *testing.T) {
	const rewrittenPort = 34567
	first := newHookServer(t, func(op string, body map[string]any) (int, string) {
		return http.StatusOK, fmt.Sprintf(
			`{"reject":false,"unchange":false,"content":{"name":"renamed","type":"tcp","remote_port":%d,"domains":["renamed.example"]}}`,
			rewrittenPort)
	})
	var secondSaw string
	second := newHookServer(t, func(op string, body map[string]any) (int, string) {
		content := body["content"].(map[string]any)
		secondSaw, _ = content["name"].(string)
		echoed, err := json.Marshal(content)
		if err != nil {
			t.Fatalf("echo the content: %v", err)
		}
		return http.StatusOK, `{"reject":false,"unchange":false,"content":` + string(echoed) + `}`
	})

	cfg := testConfig(t, false)
	cfg.HTTPPlugins = []config.HTTPPluginConfig{
		{Name: "first", Addr: first.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
		{Name: "second", Addr: second.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	manager := newHTTPPluginManager(cfg, log.New(io.Discard, "", 0))
	spec := &protocol.ProxySpec{Name: "echo", Type: protocol.ProxyTypeTCP, RemotePort: freePort(t)}
	if err := manager.runNewProxy(&Session{}, spec); err != nil {
		t.Fatalf("runNewProxy: %v", err)
	}

	if secondSaw != "renamed" {
		t.Fatalf("the second plugin was shown the registration as %q, want the first plugin's rewrite", secondSaw)
	}
	if spec.Name != "renamed" {
		t.Fatalf("the registration is named %q; the second plugin's echo undid the first plugin's rewrite", spec.Name)
	}
	if spec.RemotePort != rewrittenPort {
		t.Fatalf("the registration asks for port %d, want the rewritten %d", spec.RemotePort, rewrittenPort)
	}
}

// --- a datagram session that never opened is not a served connection ---------

// The pump reports every session's ending through Close, including one whose
// paths never opened — a refused visitor on a public datagram port, or a first
// send the session lost. Booking that as a connection every member served put a
// session in the per-member counters and in the ledger that carried no bytes.
func TestADatagramSessionThatNeverOpenedIsNotBooked(t *testing.T) {
	session := &Session{done: make(chan struct{})}
	member := &Tunnel{Name: "dns", Session: session, totals: &trafficTotals{}}
	group := &ProxyGroup{Name: "dns", lastMember: member, metrics: newMetrics()}

	group.recordSession(0, 0)

	if got := member.Total.Load(); got != 0 {
		t.Fatalf("a session that carried nothing was booked as %d connection(s)", got)
	}
	if got := member.BytesOut.Load() + member.BytesIn.Load(); got != 0 {
		t.Fatalf("a session that carried nothing was billed %d byte(s)", got)
	}

	// A session that did carry bytes is still booked, so the guard above is
	// what kept the counters at zero.
	group.recordSession(11, 22)
	if got := member.Total.Load(); got != 1 {
		t.Fatalf("a session that carried bytes was booked as %d connection(s), want 1", got)
	}
}

// --- an in-flight request does not revive a withdrawn tunnel series ----------

// serveHTTP counted its request on g.metrics.tunnel(g.Name), which creates the
// series when it is missing: a request dispatched just before the last member
// left put the withdrawn name back into every scrape for the life of the
// process. The count now goes on the series only while it exists.
func TestAnHTTPRequestDoesNotReviveAWithdrawnTunnelSeries(t *testing.T) {
	metrics := newMetrics()
	group := &ProxyGroup{
		Name: "web", Type: protocol.ProxyTypeHTTP, metrics: metrics,
		logger: log.New(io.Discard, "", 0),
	}
	// The last member left and the name's series went with it.
	metrics.forgetTunnel("web")

	group.serveHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if entry, ok := metrics.lookupTunnel("web"); ok {
		t.Fatalf("the request put the withdrawn series back with %d request(s) counted", entry.httpRequests.Load())
	}
	if got := metrics.httpRequests.Load(); got != 1 {
		t.Fatalf("the global request counter shows %d, want the request counted once", got)
	}
}
