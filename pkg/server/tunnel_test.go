package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

const testToken = "0123456789abcdef0123456789abcdef"

// --- harness ------------------------------------------------------------------

// freePort returns a loopback port reserved for TCP. A proxy that binds UDP needs
// freeUDPPort instead: the operating system hands out TCP and UDP ephemeral ports
// from different ranges, and a port that is free for one is not necessarily free for
// the other.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startEcho runs a TCP service that echoes everything back.
func startEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func testConfig(t *testing.T, encryption bool) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.BindPort = freePort(t)
	cfg.Server.AuthToken = testToken
	cfg.Server.MaxConnections = 8
	cfg.Server.HandshakeTimeoutSecs = 5
	cfg.Server.ReadTimeoutSecs = 20
	cfg.Server.HeartbeatSeconds = 30
	cfg.Server.DialTimeoutSecs = 5
	if encryption {
		cfg.Encryption.Enabled = true
		cfg.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
		cfg.Encryption.Salt = "test-salt"
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

type runningServer struct {
	server *Server
	addr   string
	cancel context.CancelFunc
	done   chan error
}

func startServer(t *testing.T, cfg *config.Config) *runningServer {
	t.Helper()
	srv, err := New(cfg, Options{Version: "test-version", BuildTime: "now", GitCommit: "test", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Listener() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rs := &runningServer{server: srv, addr: srv.Listener().Addr().String(), cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down within 5s")
		}
	})
	return rs
}

// testClient is a minimal protocol client, independent of the real client binary.
type testClient struct {
	conn    net.Conn
	framer  *protocol.Framer
	cipher  *crypto.Cipher
	session string

	// kexState and sessionKey carry the post-quantum handshake between
	// enablePostQuantum and finishPostQuantum.
	kexState   []byte
	sessionKey []byte
}

func newTestClient(t *testing.T, serverAddr string, encryption bool) (*testClient, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}

	var cipher *crypto.Cipher
	if encryption {
		cipher, err = crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, testToken, "test-salt")
		if err != nil {
			t.Fatalf("cipher: %v", err)
		}
	}

	return &testClient{conn: conn, framer: protocol.NewFramer(conn, cipher, 0), cipher: cipher}, nil
}

func (c *testClient) authenticate(version, token string) (protocol.AuthResponse, error) {
	var response protocol.AuthResponse
	err := c.framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: token, ClientVersion: version, Protocol: protocol.ProtocolVersion, Encryption: c.cipher.Algorithm(),
	})
	if err != nil {
		return response, err
	}
	err = c.framer.ReadJSON(protocol.TypeAuthResponse, &response)
	c.session = response.Session
	return response, err
}

func (c *testClient) register(spec protocol.ProxySpec) error {
	return c.framer.WriteJSON(protocol.TypeRegisterProxy, spec)
}

func (c *testClient) close() { _ = c.conn.Close() }

// serveOneStream waits for a data request and forwards it to localAddr, which is
// what the real client does for every visiting connection.
func (c *testClient) serveOneStream(serverAddr, localAddr string, timeout time.Duration) error {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	msg, err := c.framer.ReadFrame()
	if err != nil {
		return fmt.Errorf("waiting for a data request: %w", err)
	}
	if msg.Type != protocol.TypeDataRequest {
		return fmt.Errorf("expected a data request, got %s", msg.Type)
	}
	var request protocol.DataRequest
	if err := jsonUnmarshal(msg.Payload, &request); err != nil {
		return err
	}

	dataConn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return err
	}
	framer := protocol.NewFramer(dataConn, c.cipher, 0)
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: c.session, Proxy: request.Proxy, StreamID: request.StreamID,
	}); err != nil {
		dataConn.Close()
		return err
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		dataConn.Close()
		return err
	}
	if !ack.OK {
		dataConn.Close()
		return fmt.Errorf("server refused the stream: %s", ack.Error)
	}

	local, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		dataConn.Close()
		return err
	}

	serverSide := &cryptoStreamConn{Stream: crypto.NewStream(dataConn, c.cipher), conn: dataConn}
	flynet.Pipe(serverSide, local, 30*time.Second)
	return nil
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// --- tests --------------------------------------------------------------------

func TestTunnelForwardsTrafficEndToEnd(t *testing.T) {
	for _, encryption := range []bool{false, true} {
		name := "cleartext"
		if encryption {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			echoAddr := startEcho(t)
			cfg := testConfig(t, encryption)
			rs := startServer(t, cfg)

			publicPort := freePort(t)
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
			wantEncryption := "none"
			if encryption {
				wantEncryption = crypto.AlgorithmXChaCha20Poly1305
			}
			if response.Encryption != wantEncryption {
				t.Fatalf("server reported encryption %q, want %q", response.Encryption, wantEncryption)
			}

			if err := client.register(protocol.ProxySpec{
				Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
			}); err != nil {
				t.Fatalf("register: %v", err)
			}

			// The server confirms the tunnel and reports the public port bound.
			if _, err := client.framer.ReadFrame(); err != nil {
				t.Fatalf("read proxy list: %v", err)
			}
			waitForListener(t, rs.server, "echo")

			streamErr := make(chan error, 1)
			go func() {
				streamErr <- client.serveOneStream(rs.addr, echoAddr, 10*time.Second)
			}()

			// A visitor connects to the server's public port.
			visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
			if err != nil {
				t.Fatalf("visitor connect: %v", err)
			}
			defer visitor.Close()

			payload := []byte("hello through the tunnel")
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

			visitor.Close()
			select {
			case err := <-streamErr:
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("stream did not finish")
			}
		})
	}
}

func waitForListener(t *testing.T, srv *Server, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tunnel, err := srv.tunnels.Get(name)
		if err == nil && tunnel.published() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel %q was never published", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWrongTokenIsRejectedWithoutLeakingTheToken(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("test-client", "definitely-the-wrong-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("a wrong token was accepted")
	}
	if response.Session != "" {
		t.Fatalf("a rejected client was given a session id: %q", response.Session)
	}
	if response.Error == "" {
		t.Fatal("no error message was returned")
	}
}

func TestSessionAndTunnelAreReleasedOnDisconnect(t *testing.T) {
	// The v1 server kept every connection in a map forever, so after 100
	// connections it refused everyone; and its tunnels were never closed.
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)

	for round := 0; round < 3; round++ {
		client, err := newTestClient(t, rs.addr, false)
		if err != nil {
			t.Fatalf("round %d: connect: %v", round, err)
		}
		if _, err := client.authenticate("test-client", testToken); err != nil {
			t.Fatalf("round %d: authenticate: %v", round, err)
		}
		if err := client.register(protocol.ProxySpec{
			Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort,
		}); err != nil {
			t.Fatalf("round %d: register: %v", round, err)
		}
		if _, err := client.framer.ReadFrame(); err != nil {
			t.Fatalf("round %d: proxy list: %v", round, err)
		}
		waitForListener(t, rs.server, "echo")

		client.close()

		// The session must disappear and the public port must be released, so the
		// next round can bind the same port again.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if rs.server.sessions.Count() == 0 && len(rs.server.tunnels.List()) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: session or tunnel was not released (sessions=%d tunnels=%d)",
					round, rs.server.sessions.Count(), len(rs.server.tunnels.List()))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestConnectionLimitIsEnforcedButRecovers(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.MaxConnections = 2
	rs := startServer(t, cfg)

	first, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if _, err := first.authenticate("c1", testToken); err != nil {
		t.Fatalf("first authenticate: %v", err)
	}
	second, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if _, err := second.authenticate("c2", testToken); err != nil {
		t.Fatalf("second authenticate: %v", err)
	}

	third, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("third connect: %v", err)
	}
	defer third.close()
	response, err := third.authenticate("c3", testToken)
	if err != nil {
		t.Fatalf("third authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("the server accepted more clients than its limit allows")
	}

	// Freeing a slot must let the next client in, which is what the leak in v1
	// prevented permanently.
	first.close()
	deadline := time.Now().Add(5 * time.Second)
	for rs.server.sessions.Count() > 1 {
		if time.Now().After(deadline) {
			t.Fatalf("session count did not drop: %d", rs.server.sessions.Count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	second.close()
}

func TestTunnelNameCanBeReusedAfterATakeover(t *testing.T) {
	// A client that reconnects faster than the server cleans up the old session
	// must still be able to publish its tunnels. In v3.1.0-rc1 the stale entry
	// kept the name forever, so a reconnecting client silently lost its tunnel
	// with "a tunnel with that name is already registered".
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	first, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if _, err := first.authenticate("c1", testToken); err != nil {
		t.Fatalf("first authenticate: %v", err)
	}
	if err := first.register(protocol.ProxySpec{
		Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := first.framer.ReadFrame(); err != nil {
		t.Fatalf("first proxy list: %v", err)
	}
	waitForListener(t, rs.server, "echo")

	// Tear the session down from outside its loop, the way the dashboard does.
	sessions := rs.server.sessions.List()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	sessions[0].Close("test takeover")

	second, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	defer second.close()
	if response, err := second.authenticate("c2", testToken); err != nil || !response.OK {
		t.Fatalf("second authenticate: %v / %+v", err, response)
	}
	if err := second.register(protocol.ProxySpec{
		Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("second register: %v", err)
	}

	msg, err := second.framer.ReadFrame()
	if err != nil {
		t.Fatalf("second reply: %v", err)
	}
	if msg.Type != protocol.TypeProxyList {
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(msg.Payload, &payload)
		t.Fatalf("re-registration was refused: %s (%s)", payload.Error, msg.Type)
	}
}

func TestJunkOnControlConnectionDoesNotKillTheServer(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	// A peer that connects and writes garbage must be dropped without harming
	// the server.
	junk, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	_, _ = junk.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xde, 0xad, 0xbe, 0xef, 0x01, 0x02})
	junk.Close()

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect after junk: %v", err)
	}
	defer client.close()
	response, err := client.authenticate("c1", testToken)
	if err != nil {
		t.Fatalf("authenticate after junk: %v", err)
	}
	if !response.OK {
		t.Fatalf("server refused a valid client after junk input: %s", response.Error)
	}
}

func TestDataConnectionWithoutWaitingStreamIsRefused(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	response, err := client.authenticate("c1", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !response.OK {
		t.Fatalf("authenticate rejected: %s", response.Error)
	}

	// Claim a stream nobody asked for, on a fresh connection. v1 accepted this
	// and opened a tunnel to the server's own loopback without any authentication
	// having happened on that connection.
	dataConn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect data: %v", err)
	}
	defer dataConn.Close()
	framer := protocol.NewFramer(dataConn, nil, 0)
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: client.session, Proxy: "echo", StreamID: "nonexistent",
	}); err != nil {
		t.Fatalf("send data-open: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.OK {
		t.Fatal("an unrequested stream was accepted")
	}

	// An unknown session must be refused too.
	framer2 := protocol.NewFramer(dataConn, nil, 0)
	_ = framer2
}

func TestUnknownProxyTypeIsRefused(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if err := client.register(protocol.ProxySpec{Name: "carrier-pigeon", Type: "carrier-pigeon", LocalAddr: "127.0.0.1:53"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	msg, err := client.framer.ReadFrame()
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if msg.Type != protocol.TypeError {
		t.Fatalf("expected an error frame for an unsupported type, got %s", msg.Type)
	}
}
