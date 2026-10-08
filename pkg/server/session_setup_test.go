package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- per-session keys without a post-quantum exchange -------------------------

// Without a key agreement the session used to keep sealing every record under
// the static cipher the configuration derives — one long-lived key sealing all
// sessions' bulk traffic with random nonces, which AES-GCM's random-IV bound
// puts at risk on a server that relays for months. The auth response now
// carries a fresh salt, and both ends switch to the key derived from it.
func TestASessionWithoutPostQuantumStillAgreesAPerSessionKey(t *testing.T) {
	rs := startServer(t, testConfig(t, true))

	first, err := newTestClient(t, rs.addr, true)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer first.close()
	response, err := first.authenticate("r18", testToken)
	if err != nil || !response.OK {
		t.Fatalf("authenticate: %v (ok=%v, error=%q)", err, response.OK, response.Error)
	}
	if len(response.SessionSalt) != crypto.SessionSaltLen {
		t.Fatalf("the auth response carries a %d byte session salt, want %d", len(response.SessionSalt), crypto.SessionSaltLen)
	}

	// Both ends switch right after the response: the frames that follow only
	// make sense under the key derived from the salt, so a successful
	// registration proves the switch happened on both sides.
	key, err := crypto.SessionKey(testToken, response.SessionSalt)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	sessionCipher, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
	if err != nil {
		t.Fatalf("session cipher: %v", err)
	}
	first.framer = protocol.NewFramer(first.conn, sessionCipher, 0)
	if err := first.framer.WriteJSON(protocol.TypeRegisterProxy, protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:80", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register under the derived session key: %v", err)
	}
	_ = first.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		msg, err := first.framer.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for the registration answer under the derived session key: %v", err)
		}
		if msg.Type == protocol.TypeProxyList {
			break
		}
		if msg.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			t.Fatalf("the server refused the proxy: %s", payload.Error)
		}
	}

	// A second session draws its own salt: no two sessions share a key.
	second, err := newTestClient(t, rs.addr, true)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer second.close()
	secondResponse, err := second.authenticate("r18", testToken)
	if err != nil || !secondResponse.OK {
		t.Fatalf("second authenticate: %v (ok=%v)", err, secondResponse.OK)
	}
	if string(secondResponse.SessionSalt) == string(response.SessionSalt) {
		t.Fatal("two sessions were handed the same session salt")
	}
}

// --- parked connections and their confirmation -------------------------------

// A pooled connection must not be handed out before its confirmation crossed
// the wire: the client's first read on it is the DataOpenAck, and a DataRequest
// that got there first would be refused as a type mismatch, burning the pooled
// connection and a round trip.
func TestAParkedConnectionIsNotHandedOutBeforeItsConfirmationCrossed(t *testing.T) {
	selfConn, selfPeer := net.Pipe()
	defer selfConn.Close()
	defer selfPeer.Close()
	session := &Session{conn: selfConn, done: make(chan struct{}), tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}

	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	if err := session.Park(conn, protocol.NewFramer(conn, nil, 0)); err != nil {
		t.Fatalf("Park: %v", err)
	}
	other, otherPeer := net.Pipe()
	defer other.Close()
	defer otherPeer.Close()
	if err := session.Park(other, protocol.NewFramer(other, nil, 0)); err != nil {
		t.Fatalf("Park the second connection: %v", err)
	}
	if _, _, ok := session.TakeParked(); ok {
		t.Fatal("TakeParked handed out a connection whose ack is still in flight")
	}
	// Confirmation is what makes a connection visible, in the order confirmed.
	if !session.ConfirmParked(other) {
		t.Fatal("ConfirmParked did not find the parked connection")
	}
	got, _, ok := session.TakeParked()
	if !ok || got != other {
		t.Fatalf("TakeParked returned (%v, %v) after the confirmation crossed", got, ok)
	}
	if !session.ConfirmParked(conn) {
		t.Fatal("ConfirmParked did not find the connection parked first")
	}
	if got, _, ok := session.TakeParked(); !ok || got != conn {
		t.Fatalf("TakeParked returned (%v, %v), want the connection that confirmed next", got, ok)
	}
	if session.ConfirmParked(conn) {
		t.Fatal("ConfirmParked confirmed a connection that is no longer parked")
	}
	// An unconfirmed connection that fails its ack stops occupying a slot.
	third, thirdPeer := net.Pipe()
	defer third.Close()
	defer thirdPeer.Close()
	if err := session.Park(third, protocol.NewFramer(third, nil, 0)); err != nil {
		t.Fatalf("Park the third connection: %v", err)
	}
	if !session.DropParked(third) {
		t.Fatal("DropParked did not find the unconfirmed connection")
	}
	if _, _, ok := session.TakeParked(); ok {
		t.Fatal("TakeParked handed out a dropped connection")
	}
}

// --- Run's background goroutines ---------------------------------------------

// Run also ends when the listener is closed or Shutdown was called — paths on
// which nobody cancels the caller's context. The context watcher and the ban
// collector used to park on that context forever, one leak per start/stop.
func TestARunThatReturnsLeavesNoContextWatcherOrBanCollectorBehind(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 3
	cfg.Server.BanSeconds = 60
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	srv, err := New(cfg, Options{Version: "test-version", BuildTime: "now", GitCommit: "test", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(context.Background()) }()

	// The context stays alive on purpose: cancelling it would stop the
	// goroutines themselves and hide the gap.
	deadline := time.Now().Add(5 * time.Second)
	for srv.Listener() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}

	srv.Shutdown("test over")
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of the shutdown")
	}

	for _, name := range []string{"collectBans", "watchCallerContext"} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			if !strings.Contains(string(buf[:n]), name) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the %s goroutine is still running after Run returned", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// --- stop paths that close only the http.Server -------------------------------

// http.Server.Close only closes listeners its Serve has registered, and Serve
// registers late: a stop that lands between start and the Serve goroutine used
// to leave the port bound with nobody left to close it. The metrics listener
// and the tcpmux/sni sets already close the listener itself; vhost and the
// dashboard are held to the same contract, whether or not Serve ever ran.
func TestAVhostStopClosesTheListenerServeNeverRegistered(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	v := &vhostRouter{
		kind:     "test",
		logger:   log.New(io.Discard, "", 0),
		done:     make(chan struct{}),
		listener: listener,
		server:   &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}
	v.stop()
	assertRefused(t, listener.Addr().String())
}

func TestTheDashboardStopClosesTheListenerServeNeverRegistered(t *testing.T) {
	d := &Dashboard{
		cfg:    &config.Config{},
		logger: log.New(io.Discard, "", 0),
		mux:    http.NewServeMux(),
	}
	d.httpServer = &http.Server{Handler: d.mux}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d.setListener(listener)
	d.Stop()
	if d.Listener() != nil {
		t.Fatal("Stop left the listener recorded")
	}
	assertRefused(t, listener.Addr().String())
}

// assertRefused checks that nothing accepts connections on addr any more: a
// listener that stayed bound accepts the dial and only then hangs or resets.
func assertRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("something still accepts connections on %s", addr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
