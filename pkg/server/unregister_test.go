package server

import (
	"bytes"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/ledger"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The dashboard's DELETE /api/clients/{id} closes the session, and Session.Close
// detaches the session's members before the control loop's own teardown runs. That
// teardown calls Unregister for each name, which then finds a group with no
// members: it used to return without deleting the entry, so /api/proxies kept
// reporting a phantom proxy and its DHT record stayed published until it lapsed.
func TestUnregisterDropsAGroupWhoseMembersAreAlreadyGone(t *testing.T) {
	cfg := &config.Config{}
	manager := &TunnelManager{
		cfg:      cfg,
		logger:   discardLogger(),
		groups:   make(map[string]*ProxyGroup),
		policies: &proxyPolicies{},
		metrics:  newMetrics(),
	}
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)
	manager.groups["web"] = group

	session := &Session{ID: "session-1", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	manager.Unregister("web", session, "disconnected from the dashboard")

	if _, still := manager.groups["web"]; still {
		t.Fatal("the empty group stayed in the manager")
	}
	if groups := manager.Groups(); len(groups) != 0 {
		t.Fatalf("the manager still reports %d proxies", len(groups))
	}

	// A group that another session still holds is untouched: the name stays
	// published, which is what makes a pool survive one member leaving.
	manager.groups["pooled"] = newProxyGroup(protocol.ProxySpec{Name: "pooled", Type: protocol.ProxyTypeHTTP}, manager)
	other := &Session{ID: "session-2", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	manager.groups["pooled"].members = []*Tunnel{{Name: "pooled", Session: other, group: manager.groups["pooled"]}}
	manager.Unregister("pooled", session, "disconnected from the dashboard")
	if _, still := manager.groups["pooled"]; !still {
		t.Fatal("a group another session still holds was dropped")
	}
}

// Two servers started from the same configuration race on the ledger's signing-key
// file. O_EXCL publishes the name before the winner has written the seed, so the
// loser's read can land on an empty or partial file: treating that as a corrupt key
// file made it refuse to start, and the retry that exists for exactly this case was
// unreachable from that path.
func TestASigningKeyFileThatIsStillBeingWrittenIsAdopted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.key")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create the empty key file: %v", err)
	}

	want, err := ledger.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	go func() {
		// What the process that won the O_EXCL race does a moment later.
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(path, []byte(hex.EncodeToString(want.Seed())+"\n"), 0o600)
	}()

	key, created, err := loadLedgerKey(path)
	if err != nil {
		t.Fatalf("a key file that was still being written was refused: %v", err)
	}
	if created {
		t.Error("the file already existed, but the key was reported as created")
	}
	if !bytes.Equal(key.Seed(), want.Seed()) {
		t.Error("the adopted key is not the one in the file")
	}
}

// The retry must not turn a genuinely unusable file into a working key.
func TestACorruptSigningKeyFileIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.key")
	if err := os.WriteFile(path, []byte("this is not a hex seed\n"), 0o600); err != nil {
		t.Fatalf("write the corrupt key file: %v", err)
	}
	if _, _, err := loadLedgerKey(path); err == nil {
		t.Fatal("a corrupt signing key file was accepted")
	}
}

// A data connection that arrives after the visitor gave up on its stream has to be
// refused or drained, never queued for nobody. Both sides run on different
// goroutines and the connection may be claimed (TakePending) before or after the
// give-up, so the session's lock is what decides who owns it. A single non-blocking
// drain could not cover the window in which the claim had happened and the hand-over
// had not: the connection then sat in the buffered channel holding its socket and the
// client's stream slot until the session ended.
func TestAStreamGivenUpOnNeverQueuesADataConnection(t *testing.T) {
	newSessionFor := func() *Session {
		return &Session{done: make(chan struct{}), pending: map[string]*pendingStream{}}
	}

	t.Run("given up before the connection is handed over", func(t *testing.T) {
		session := newSessionFor()
		pending, err := session.AddPending("s1")
		if err != nil {
			t.Fatalf("AddPending: %v", err)
		}
		// The deliverer claimed it, then the visitor's timer fired.
		taken, ok := session.TakePending("s1")
		if !ok || taken != pending {
			t.Fatal("TakePending did not hand back the waiting stream")
		}
		session.abandonPending("s1", pending)

		serverSide, clientSide := net.Pipe()
		defer clientSide.Close()
		if session.resolveStream(pending, streamResult{conn: &dataConn{conn: serverSide}}) {
			t.Fatal("a connection was handed to a visitor that had given up")
		}
		// The deliverer closes what it could not hand over; here that is the test,
		// so the assertion is the refusal above rather than the close.
		_ = serverSide.Close()
	})

	t.Run("handed over in the same instant", func(t *testing.T) {
		session := newSessionFor()
		pending, err := session.AddPending("s2")
		if err != nil {
			t.Fatalf("AddPending: %v", err)
		}
		taken, _ := session.TakePending("s2")

		serverSide, clientSide := net.Pipe()
		defer clientSide.Close()
		if !session.resolveStream(taken, streamResult{conn: &dataConn{conn: serverSide}}) {
			t.Fatal("the connection was refused although the visitor was still waiting")
		}

		// The visitor gives up a moment later: it drained the queued connection, so
		// the peer sees the socket close instead of the connection hanging on.
		session.abandonPending("s2", pending)
		closeLateStream(pending)

		if _, err := clientSide.Write([]byte("x")); err == nil {
			_ = clientSide.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := clientSide.Read(make([]byte, 1)); err == nil {
				t.Fatal("the connection queued for a visitor that gave up was left open")
			}
		}
	})
}
