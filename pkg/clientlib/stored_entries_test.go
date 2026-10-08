package clientlib

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A stored entry carrying enabled = false is left stopped. selectByStart judges
// the configuration's own entries, but the store's are merged on afterwards and
// used never to be judged at all: a dynamic entry turned off with enabled = false
// was published while the same entry written in the file was not.
func TestADisabledStoredEntryIsLeftStoppedLikeAConfiguredOne(t *testing.T) {
	off := false
	stored, err := newStore("", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	stored.proxies["dynamic"] = []config.ProxyConfig{{
		Name: "dynamic", Type: config.ProxyTypeTCP, LocalPort: 8080, Enabled: &off,
	}}

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0), store: stored}
	c.setFileLists([]config.ProxyConfig{{Name: "configured", Type: config.ProxyTypeTCP, LocalPort: 8081}}, nil)

	proxies, _ := c.effectiveLists()
	for _, proxy := range proxies {
		if proxy.Name == "dynamic" {
			t.Fatalf("a stored entry with enabled = false is still dispatched: %+v", proxies)
		}
	}
	if len(proxies) != 1 || proxies[0].Name != "configured" {
		t.Fatalf("the enabled configured entry did not survive the merge: %+v", proxies)
	}
}

// A stored entry that is enabled still wins its name over the file's entry, so
// the filtering above must not drop live dynamic entries along with the stopped
// ones.
func TestAnEnabledStoredEntryStillOverridesTheConfiguredOne(t *testing.T) {
	stored, err := newStore("", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	stored.proxies["shared"] = []config.ProxyConfig{{
		Name: "shared", Type: config.ProxyTypeTCP, LocalPort: 9090,
	}}

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0), store: stored}
	c.setFileLists([]config.ProxyConfig{{Name: "shared", Type: config.ProxyTypeTCP, LocalPort: 8080}}, nil)

	proxies, _ := c.effectiveLists()
	if len(proxies) != 1 {
		t.Fatalf("want one merged entry, got %+v", proxies)
	}
	if proxies[0].LocalPort != 9090 {
		t.Fatalf("the stored entry did not win its name: %+v", proxies[0])
	}
}

// A visitor.fallback_to is a local service, so the fallback dial must not carry
// the server-facing source address. It is set to an address this machine does not
// hold: the fallback dial must still reach a listener on loopback.
func TestTheFallbackDialDoesNotBindTheServerSourceAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.Client.DialTimeoutSecs = 5
	c.cfg.Client.IdleTimeoutSecs = 5
	// TEST-NET-1: an address chosen for reaching a server elsewhere, which this
	// machine cannot use as the source of a connection to loopback.
	c.cfg.Client.ConnectServerLocalIP = "192.0.2.1"

	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	done := make(chan struct{})
	go func() {
		c.serveVisitorFallback(context.Background(), config.VisitorConfig{
			Name: "private", FallbackTo: listener.Addr().String(),
		}, local, errors.New("the tunnel could not be opened"))
		close(done)
	}()

	select {
	case conn := <-accepted:
		_ = conn.Close()
		_ = peer.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the fallback listener never accepted a connection")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fallback session did not end")
	}
}

// The visitor proof is written under a fresh deadline. dialVisitor set one
// absolute deadline for the whole exchange, and the proof is built after the
// challenge arrives, so a slow proof used to make this write fail with i/o
// timeout on a connection the server was still waiting on.
func TestAVisitorProofIsWrittenUnderAFreshDeadline(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	c.cfg.Client.DialTimeoutSecs = 30
	// The deadline dialVisitor sets has already passed, as it would have when the
	// challenge round trip and the proof took longer than the dial timeout.
	if err := clientSide.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set the stale write deadline: %v", err)
	}

	framer := protocol.NewFramer(clientSide, nil, 0)
	server := protocol.NewFramer(serverSide, nil, 0)
	serverErr := make(chan error, 1)
	go func() {
		if err := server.WriteJSON(protocol.TypeVisitorChallenge, protocol.VisitorChallenge{Nonce: []byte("nonce-one")}); err != nil {
			serverErr <- err
			return
		}
		var prove protocol.VisitorProve
		if err := server.ReadJSON(protocol.TypeVisitorProve, &prove); err != nil {
			serverErr <- err
			return
		}
		serverErr <- server.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true})
	}()

	cfg := config.VisitorConfig{
		Name: "private", ServerName: "private", SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	}
	if _, _, err := c.awaitVisitorReady(clientSide, framer, context.Background(), cfg, nil, "", nil); err != nil {
		t.Fatalf("the visitor handshake failed: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("the server side of the handshake failed: %v", err)
	}
}
