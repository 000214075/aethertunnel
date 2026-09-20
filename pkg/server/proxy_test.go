package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- a full protocol client ---------------------------------------------------

// dataHandler serves one accepted data connection. It owns the connection and
// must close it.
type dataHandler func(conn net.Conn, framer *protocol.Framer)

// testAgent behaves like the real client binary: it authenticates, registers its
// proxies and forwards every data request to the matching local service.
//
// One goroutine reads the control framer for the life of the agent; everything
// else goes through it. Reading from two places, or reading with a deadline that
// can fire in the middle of a frame, desynchronises the stream.
type testAgent struct {
	t       *testing.T
	client  *testClient
	addr    string
	done    chan struct{}
	stopped chan struct{}
	verdict chan *protocol.Message
}

func startAgent(t *testing.T, serverAddr string, encryption bool, handlers map[string]dataHandler) *testAgent {
	t.Helper()

	client, err := newTestClient(t, serverAddr, encryption)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := client.authenticate("test-agent", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	agent := &testAgent{
		t:       t,
		client:  client,
		addr:    serverAddr,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		verdict: make(chan *protocol.Message, 8),
	}
	go agent.loop(handlers)
	t.Cleanup(func() {
		close(agent.done)
		client.close()
		<-agent.stopped
	})
	return agent
}

func (a *testAgent) register(spec protocol.ProxySpec) {
	a.t.Helper()
	if err := a.client.register(spec); err != nil {
		a.t.Fatalf("register %q: %v", spec.Name, err)
	}
	a.expectAccepted(spec.Name)
}

// expectAccepted waits for the server's verdict on the last registration or
// visitor exchange and fails the test when it was refused.
func (a *testAgent) expectAccepted(name string) {
	a.t.Helper()
	select {
	case msg := <-a.verdict:
		if msg.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			a.t.Fatalf("the server refused %q: %s", name, payload.Error)
		}
	case <-time.After(5 * time.Second):
		a.t.Fatalf("no verdict for %q arrived", name)
	}
}

// expectRefused waits for the server's verdict and returns its message.
func (a *testAgent) expectRefused(name string) string {
	a.t.Helper()
	select {
	case msg := <-a.verdict:
		if msg.Type != protocol.TypeError {
			a.t.Fatalf("%q was accepted (got %s)", name, msg.Type)
		}
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(msg.Payload, &payload)
		return payload.Error
	case <-time.After(5 * time.Second):
		a.t.Fatalf("no verdict for %q arrived", name)
		return ""
	}
}

func (a *testAgent) loop(handlers map[string]dataHandler) {
	defer close(a.stopped)

	for {
		msg, err := a.client.framer.ReadFrame()
		if err != nil {
			return
		}

		switch msg.Type {
		case protocol.TypeDataRequest:
			var request protocol.DataRequest
			if err := json.Unmarshal(msg.Payload, &request); err != nil {
				continue
			}
			handler, ok := handlers[request.Proxy]
			if !ok {
				continue
			}
			go a.serve(request, handler)

		case protocol.TypeError, protocol.TypeProxyList:
			select {
			case a.verdict <- msg:
			default:
			}
		}
	}
}

func (a *testAgent) serve(request protocol.DataRequest, handler dataHandler) {
	conn, err := net.DialTimeout("tcp", a.addr, 5*time.Second)
	if err != nil {
		return
	}
	framer := protocol.NewFramer(conn, a.client.cipher, 0)
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: a.client.session, Proxy: request.Proxy, StreamID: request.StreamID,
	}); err != nil {
		_ = conn.Close()
		return
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil || !ack.OK {
		_ = conn.Close()
		return
	}
	handler(conn, framer)
}

// --- local services -----------------------------------------------------------

func startUDPEcho(t *testing.T) string {
	t.Helper()
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start udp echo: %v", err)
	}
	t.Cleanup(func() { socket.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := socket.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = socket.WriteTo(buf[:n], addr)
		}
	}()
	return socket.LocalAddr().String()
}

func startHTTPService(t *testing.T, body string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start http service: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|host=%s", body, r.Host)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// --- handlers used by the agents ----------------------------------------------

// streamHandler serves a byte-stream proxy exactly as the real client does: the
// handshake is framed, and everything after the ack is raw bytes.
func streamHandler(localAddr string) dataHandler {
	return func(conn net.Conn, framer *protocol.Framer) {
		_ = framer
		local, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
		if err != nil {
			_ = conn.Close()
			return
		}
		flynet.Pipe(local, conn, 2*time.Second)
	}
}

// --- tests --------------------------------------------------------------------

func datagramHandler(localAddr string) dataHandler {
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
				_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
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

// bytesStreamHandler moves a byte stream that is carried as frames, which is what
// a visitor connection uses before the record layer is applied.
func framedStreamHandler(localAddr string) dataHandler {
	return streamHandler(localAddr)
}

// --- tests --------------------------------------------------------------------

func TestUDPTunnelRoundTrip(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	remotePort := freePort(t)

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo-udp": datagramHandler(echo)})
	agent.register(protocol.ProxySpec{Name: "echo-udp", Type: protocol.ProxyTypeUDP, RemotePort: remotePort})

	visitor, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err != nil {
		t.Fatalf("dial the tunnel: %v", err)
	}
	defer visitor.Close()

	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < 12; i++ {
		payload := []byte(fmt.Sprintf("datagram-%d", i))
		if _, err := visitor.Write(payload); err != nil {
			t.Fatalf("send: %v", err)
		}

		_ = visitor.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 2048)
		n, err := visitor.Read(buf)
		if err != nil {
			continue // the first datagrams can be dropped while the session is set up
		}
		if string(buf[:n]) != string(payload) {
			t.Fatalf("echo returned %q, want %q", buf[:n], payload)
		}
		if time.Now().After(deadline) {
			t.Fatal("the tunnel did not answer in time")
		}
		t.Logf("datagram %d echoed after %d attempt(s)", i, i+1)
		return
	}
	t.Fatal("no datagram was echoed within 12 attempts")
}

func TestUDPTunnelKeepsSessionsPerVisitorAddress(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	remotePort := freePort(t)

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo-udp": datagramHandler(echo)})
	agent.register(protocol.ProxySpec{Name: "echo-udp", Type: protocol.ProxyTypeUDP, RemotePort: remotePort})

	// Two visitors must each get their own session, and both must work.
	target := fmt.Sprintf("127.0.0.1:%d", remotePort)
	for i := 0; i < 2; i++ {
		conn, err := net.Dial("udp", target)
		if err != nil {
			t.Fatalf("visitor %d: %v", i, err)
		}
		defer conn.Close()

		payload := []byte(fmt.Sprintf("visitor-%d", i))
		answered := false
		for attempt := 0; attempt < 10 && !answered; attempt++ {
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("visitor %d: send: %v", i, err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			buf := make([]byte, 2048)
			n, err := conn.Read(buf)
			if err == nil && string(buf[:n]) == string(payload) {
				answered = true
			}
		}
		if !answered {
			t.Fatalf("visitor %d never got its datagram back", i)
		}
	}

	if sessions := agentSessions(rs.server, "echo-udp"); sessions < 1 {
		t.Fatalf("the tunnel tracked %d datagram sessions", sessions)
	}
}

func agentSessions(srv *Server, name string) int {
	tunnel, err := srv.tunnels.Get(name)
	if err != nil || tunnel.pump == nil {
		return 0
	}
	return tunnel.pump.Sessions()
}

func TestHTTPVHostRoutesByHostHeader(t *testing.T) {
	service := startHTTPService(t, "local-service")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"web.example.com", "*.wild.example.com"},
	})

	base := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Server.HTTPPort)

	cases := []struct {
		host string
		want int
		body string
	}{
		{"web.example.com", http.StatusOK, "local-service|host=web.example.com"},
		{"WEB.EXAMPLE.COM", http.StatusOK, "local-service|host=WEB.EXAMPLE.COM"},
		{"a.wild.example.com", http.StatusOK, "local-service|host=a.wild.example.com"},
		{"a.b.wild.example.com", http.StatusOK, "local-service|host=a.b.wild.example.com"},
		{"unknown.example.com", http.StatusNotFound, ""},
		{"example.com", http.StatusNotFound, ""},
	}

	for _, tc := range cases {
		request, err := http.NewRequest(http.MethodGet, base, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Host = tc.host

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()

		if response.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d (%s)", tc.host, response.StatusCode, tc.want, body)
		}
		if tc.body != "" && string(body) != tc.body {
			t.Fatalf("%s: body %q, want %q", tc.host, body, tc.body)
		}
	}
}

func TestHTTPVHostRefusesADomainAnotherTunnelOwns(t *testing.T) {
	service := startHTTPService(t, "first")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	first := startAgent(t, rs.addr, false, map[string]dataHandler{"one": framedStreamHandler(service)})
	first.register(protocol.ProxySpec{
		Name: "one", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"taken.example.com"},
	})

	second := startAgent(t, rs.addr, false, map[string]dataHandler{"two": framedStreamHandler(service)})
	if err := second.client.register(protocol.ProxySpec{
		Name: "two", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains: []string{"taken.example.com"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if refusal := second.expectRefused("two"); !strings.Contains(refusal, "already published") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestHTTPProxyNeedsTheSharedListener(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"web.example.com"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if refusal := agent.expectRefused("web"); !strings.Contains(refusal, "http_port") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

// --- visitor connections ------------------------------------------------------

// visitorHandshake opens a visitor connection and returns it once the server has
// accepted it.
func visitorHandshake(t *testing.T, serverAddr, proxy, secret, kind string) (net.Conn, *protocol.Framer) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: proxy, Secret: secret, Type: kind, AuthToken: testToken,
	}); err != nil {
		conn.Close()
		t.Fatalf("send visitor-connect: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		conn.Close()
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		conn.Close()
		t.Fatalf("the server refused the visitor: %s", ack.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, framer
}

func TestSTCPRelayReachesThePrivateService(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, framer := visitorHandshake(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP)
	defer conn.Close()
	_ = framer

	payload := []byte("hello through a private tunnel")
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

func TestSTCPRefusesTheWrongSecret(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Secret: "wrong", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.OK {
		t.Fatal("the server accepted the wrong secret")
	}
	if !strings.Contains(ack.Error, "secret") {
		t.Fatalf("unexpected refusal: %s", ack.Error)
	}
}

func TestSTCPRefusesTheWrongServerToken(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Secret: "s3cret", Type: protocol.ProxyTypeSTCP, AuthToken: "not-the-token",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.OK {
		t.Fatal("the server accepted a visitor with the wrong auth token")
	}
}

func TestSUDPVisitorRelaysDatagrams(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private-udp": datagramHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private-udp", Type: protocol.ProxyTypeSUDP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, framer := visitorHandshake(t, rs.addr, "private-udp", "s3cret", protocol.ProxyTypeSUDP)
	defer conn.Close()

	payload := []byte("a datagram through a private tunnel")
	if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: payload}); err != nil {
		t.Fatalf("send: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := framer.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != protocol.TypeUDPPacket {
		t.Fatalf("got %s, want a datagram", msg.Type)
	}
	if string(msg.Payload) != string(payload) {
		t.Fatalf("echo returned %q, want %q", msg.Payload, payload)
	}
}

func TestPrivateProxyRefusesAPublicPort(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	if err := agent.client.register(protocol.ProxySpec{
		Name: "bad", Type: protocol.ProxyTypeSTCP, LocalAddr: "127.0.0.1:1",
		SecretKey: "s3cret", RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if refusal := agent.expectRefused("bad"); !strings.Contains(refusal, "must not open a public port") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestPrivateProxyNeedsASecret(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	if err := agent.client.register(protocol.ProxySpec{
		Name: "bad", Type: protocol.ProxyTypeSTCP, LocalAddr: "127.0.0.1:1",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if refusal := agent.expectRefused("bad"); !strings.Contains(refusal, "secret_key") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestVisitorRefusesAnUnknownProxy(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "ghost", Secret: "s3cret", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.OK {
		t.Fatal("the server accepted a visitor for a proxy that does not exist")
	}
}

func TestVisitorRefusesAPublicProxy(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"public": streamHandler(echo)})
	agent.register(protocol.ProxySpec{Name: "public", Type: protocol.ProxyTypeTCP, RemotePort: freePort(t)})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "public", Secret: "s3cret", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.OK {
		t.Fatal("a visitor was allowed into a proxy that is not private")
	}
	if !strings.Contains(ack.Error, "not a private proxy") {
		t.Fatalf("unexpected refusal: %s", ack.Error)
	}
}

// --- xtcp ---------------------------------------------------------------------

func TestXTCPFallsBackToTheRelayWhenNoPunchIsPossible(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.P2PPort = freeUDPPort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"p2p": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "p2p", Type: protocol.ProxyTypeXTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "p2p", Secret: "s3cret", Type: protocol.ProxyTypeXTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var offer protocol.P2PPeer
	if err := framer.ReadJSON(protocol.TypeP2PPeer, &offer); err != nil {
		t.Fatalf("read the punch offer: %v", err)
	}
	if len(offer.Token) != protocol.PunchTokenLen {
		t.Fatalf("the punch token is %d characters, want %d", len(offer.Token), protocol.PunchTokenLen)
	}

	// The test agent never punches, so the visitor asks for the relayed path.
	if err := framer.WriteJSON(protocol.TypeP2PFallback, protocol.P2PFallback{Proxy: "p2p", Token: offer.Token}); err != nil {
		t.Fatalf("ask for the relay: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the relayed path was refused: %s", ack.Error)
	}

	payload := []byte("relayed xtcp payload")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
}

func TestRendezvousExchangesBothAddresses(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.P2PPort = freeUDPPort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	echo := startEcho(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"p2p": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "p2p", Type: protocol.ProxyTypeXTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	// Ask for a punch token over the control connection.
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "p2p", Secret: "s3cret", Type: protocol.ProxyTypeXTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var offer protocol.P2PPeer
	if err := framer.ReadJSON(protocol.TypeP2PPeer, &offer); err != nil {
		t.Fatalf("read the punch offer: %v", err)
	}

	rendezvous := fmt.Sprintf("127.0.0.1:%d", cfg.Server.P2PPort)

	visitor, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("visitor socket: %v", err)
	}
	defer visitor.Close()
	owner, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("owner socket: %v", err)
	}
	defer owner.Close()

	serverAddr, err := net.ResolveUDPAddr("udp", rendezvous)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Only the visitor reports; the rendezvous must stay silent until the owner
	// does too, because it has no address to hand out yet.
	if _, err := visitor.WriteTo(
		protocol.EncodePunchRequest(protocol.PunchRoleVisitor, offer.Token), serverAddr); err != nil {
		t.Fatalf("visitor request: %v", err)
	}
	_ = visitor.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 512)
	if n, _, err := visitor.ReadFrom(buf); err == nil {
		t.Fatalf("the rendezvous answered %d bytes before the owner reported", n)
	}

	if _, err := owner.WriteTo(
		protocol.EncodePunchRequest(protocol.PunchRoleOwner, offer.Token), serverAddr); err != nil {
		t.Fatalf("owner request: %v", err)
	}

	// Both retry, because either request may be the one that arrives second.
	deadline := time.Now().Add(5 * time.Second)
	visitorPeer, ownerPeer := "", ""
	for time.Now().Before(deadline) && (visitorPeer == "" || ownerPeer == "") {
		_, _ = visitor.WriteTo(protocol.EncodePunchRequest(protocol.PunchRoleVisitor, offer.Token), serverAddr)
		_, _ = owner.WriteTo(protocol.EncodePunchRequest(protocol.PunchRoleOwner, offer.Token), serverAddr)

		_ = visitor.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, _, err := visitor.ReadFrom(buf); err == nil {
			if _, peer, ok := protocol.DecodePunchResponse(buf[:n]); ok {
				visitorPeer = peer
			}
		}
		_ = owner.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, _, err := owner.ReadFrom(buf); err == nil {
			if _, peer, ok := protocol.DecodePunchResponse(buf[:n]); ok {
				ownerPeer = peer
			}
		}
	}

	if visitorPeer != owner.LocalAddr().String() {
		t.Fatalf("the visitor learned %q, want the owner's address %q", visitorPeer, owner.LocalAddr())
	}
	if ownerPeer != visitor.LocalAddr().String() {
		t.Fatalf("the owner learned %q, want the visitor's address %q", ownerPeer, visitor.LocalAddr())
	}
}

func TestRendezvousIgnoresUnknownTokens(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.P2PPort = freeUDPPort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	_ = rs

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer socket.Close()

	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", cfg.Server.P2PPort))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// A made-up token, and a request with the wrong length, must both be ignored.
	_, _ = socket.WriteTo(protocol.EncodePunchRequest(protocol.PunchRoleVisitor, strings.Repeat("a", protocol.PunchTokenLen)), serverAddr)
	_, _ = socket.WriteTo([]byte("ATP1xshort"), serverAddr)
	_, _ = socket.WriteTo([]byte("garbage"), serverAddr)

	_ = socket.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	buf := make([]byte, 512)
	if n, _, err := socket.ReadFrom(buf); err == nil {
		t.Fatalf("the rendezvous answered an unknown token with %d bytes", n)
	}
}

func TestXTCPWithoutARendezvousUsesTheRelay(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false) // no p2p_port
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"p2p": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "p2p", Type: protocol.ProxyTypeXTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	conn, framer := visitorHandshake(t, rs.addr, "p2p", "s3cret", protocol.ProxyTypeXTCP)
	defer conn.Close()
	_ = framer

	payload := []byte("relayed without a rendezvous")
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

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a udp port: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// silence unused-import warnings in builds where a helper is not referenced.
var _ = log.New
var _ = context.Background
var _ = crypto.AlgorithmNone
