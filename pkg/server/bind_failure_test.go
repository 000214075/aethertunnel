package server

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// tunnelTestManager builds the smallest TunnelManager newProxyGroup needs, with
// no virtual-host listener configured so an http bind fails on purpose.
func tunnelTestManager() *TunnelManager {
	return &TunnelManager{
		cfg:      &config.Config{},
		logger:   discardLogger(),
		groups:   make(map[string]*ProxyGroup),
		policies: &proxyPolicies{},
		metrics:  newMetrics(),
	}
}

// A first member's bind can fail while another session is joining the same pool.
// The joiner waits for g.bound and then re-checks the group in Register, which is
// what rolls it back; that only works if the failure closes the group before the
// wait is released. bind used to release g.bound in a deferred close and let its
// caller close the group afterwards, so a joiner could wake in between and stay in
// a group with no endpoint while its client believed the proxy was published.
func TestABindFailureClosesTheGroupBeforeItReleasesAJoiner(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)
	manager.groups["web"] = group

	foundClosed := make(chan bool, 1)
	go func() {
		<-group.bound
		foundClosed <- group.isClosed()
	}()

	if err := group.bind(); err == nil {
		t.Fatal("a bind with no http_port configured succeeded")
	}
	if !<-foundClosed {
		t.Fatal("a joiner woken by the failed bind found the group still open")
	}
}

// A joining member that brings no hostnames of its own used to return from
// extendHostnames immediately, without waiting for the first bind. When that bind
// failed, the joiner never woke on g.done and never reached Register's re-check, so
// it stayed in a group whose endpoint never came up. The wait now applies to every
// joiner.
func TestAMemberWithoutHostnamesStillWaitsForTheFirstBind(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP}, manager)

	returned := make(chan struct{})
	go func() {
		_ = group.extendHostnames(nil)
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("a joiner with no hostnames returned before the first bind settled")
	case <-time.After(50 * time.Millisecond):
	}

	// The first bind failed and closed the group; the joiner has to wake so
	// Register can roll it back.
	group.close("the first bind failed")
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the joiner did not wake when the group closed")
	}
}

// clientlib fills VisitorConnect.Secret only when the visitor's own auth_method is
// "secret". Against a proxy registered for a proof method that visitor was sent a
// challenge and answered with a Schnorr proof, which a nizk proxy accepts — the
// documented "a mismatched method is refused" did not hold in that direction. The
// server now refuses the mismatch before the challenge, for both proof methods.
func TestASecretVisitorAgainstAProofProxyIsRefused(t *testing.T) {
	for _, method := range []string{config.AuthMethodNIZK, config.AuthMethodSNARK} {
		t.Run(method, func(t *testing.T) {
			echo := startEcho(t)
			cfg := testConfig(t, false)
			rs := startServer(t, cfg)

			agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
			agent.register(protocol.ProxySpec{
				Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
				SecretKey: "s3cret", AuthMethod: method,
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
				Secret: "s3cret",
			}); err != nil {
				t.Fatalf("send the visitor connect: %v", err)
			}

			var ack protocol.DataOpenAck
			if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
				t.Fatalf("read the refusal: %v", err)
			}
			if ack.OK {
				t.Fatalf("a visitor that sent the secret was let into a %s proxy", method)
			}
			if !strings.Contains(ack.Error, "auth_method") {
				t.Fatalf("the refusal does not name the auth_method mismatch: %q", ack.Error)
			}
		})
	}
}

// A parked offer can lose its session between the frame being read and Park taking
// the lock: Park then refuses with errSessionClosed, which is an ordinary
// disconnect rather than a client that parks more than it may. pool_refused is the
// series an operator reads for pool pressure, so only a full pool may feed it.
func TestAClosedSessionIsNotCountedAsAPoolRefusal(t *testing.T) {
	s := &Server{logger: discardLogger(), metrics: newMetrics(), sessions: newSessionManager(8)}
	session := &Session{ID: "s1", done: make(chan struct{}), tunnels: map[string]*Tunnel{}}
	session.mu.Lock()
	session.closed = true
	session.mu.Unlock()
	if err := s.sessions.Add(session); err != nil {
		t.Fatalf("register the session: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()
	serverConn := <-accepted
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()

	s.parkDataConn(serverConn, protocol.NewFramer(serverConn, nil, 0), protocol.DataOpen{Session: "s1", Pool: true})

	if refused := s.metrics.poolRefused.Load(); refused != 0 {
		t.Fatalf("a closed session counted %d pool refusals, want 0", refused)
	}
	if parked := s.metrics.poolParked.Load(); parked != 0 {
		t.Fatalf("a closed session parked %d connections, want 0", parked)
	}
}
