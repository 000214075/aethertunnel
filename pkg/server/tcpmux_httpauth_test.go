package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// echoService listens and copies every byte back, so a raw tunnel (tcpmux
// carries raw bytes after the CONNECT answer) has something to talk to.
func echoService(t *testing.T) string {
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
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// connectThrough sends one CONNECT request and returns the connection positioned
// right after the status line: the caller reads the answer it expects.
func connectThrough(t *testing.T, addr, authority string) (net.Conn, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the mux: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	request := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	return conn, response
}

// TestBasicAuthGuardsAnHTTPHostname covers http_user/http_password: a request
// without credentials and one with the wrong password are answered 401 with the
// challenge header, and the right credentials reach the tunnel.
func TestBasicAuthGuardsAnHTTPHostname(t *testing.T) {
	service := startHTTPService(t, "guarded-service")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: service,
		Domains:  []string{"guarded.smoke.test"},
		HTTPUser: "ops", HTTPPassword: "s3cret",
	})

	request := func(user, password string) *http.Response {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://guarded.smoke.test/", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		if user != "" || password != "" {
			req.SetBasicAuth(user, password)
		}
		// serveHTTP is the group's handler; the vhost router reaches it the
		// same way, so calling it directly pins the contract.
		group, err := rs.server.tunnels.Get("web")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		group.serveHTTP(recorder, req)
		return recorder.Result()
	}

	response := request("", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: status %d, want 401", response.StatusCode)
	}
	if challenge := response.Header.Get("WWW-Authenticate"); !strings.Contains(challenge, "Basic") {
		t.Fatalf("the 401 carries %q, want a Basic challenge", challenge)
	}

	response = request("ops", "wrong")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: status %d, want 401", response.StatusCode)
	}

	response = request("ops", "s3cret")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("right credentials: status %d, want 200", response.StatusCode)
	}
}

// TestTCPMuxRoutesByCONNECTAuthority covers the multiplexer end to end: a
// CONNECT naming the hostname opens the tunnel, an unknown hostname is answered
// 404, and a request that is not CONNECT is refused without leaking anything.
func TestTCPMuxRoutesByCONNECTAuthority(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.TCPMuxPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeTCPMux, LocalAddr: backend,
		Multiplexer: config.MultiplexerHTTPConnect,
		Domains:     []string{"tunnel1"},
	})

	muxAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Server.TCPMuxPort))
	conn, response := connectThrough(t, muxAddr, "tunnel1:9999")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT tunnel1: status %d, want 200", response.StatusCode)
	}
	payload := "ping-through-the-mux"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(reply) != payload {
		t.Fatalf("the tunnel carried %q, want %q", reply, payload)
	}
	_ = conn.SetDeadline(time.Time{})

	// A hostname nobody publishes is answered 404, not held open.
	_, response = connectThrough(t, muxAddr, "nobody:1")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("CONNECT nobody: status %d, want 404", response.StatusCode)
	}

	// A plain GET on the multiplexer gets a refusal instead of an answer that
	// would reveal which hostnames exist.
	plain, err := net.DialTimeout("tcp", muxAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer plain.Close()
	_ = plain.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := plain.Write([]byte("GET / HTTP/1.1\r\nHost: tunnel1\r\n\r\n")); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(plain), nil)
	if err != nil {
		t.Fatalf("read the GET answer: %v", err)
	}
	if answer.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the mux: status %d, want 405", answer.StatusCode)
	}
}

// TestTCPMuxRefusesARegistrationWithoutDomains pins the refusal a client gets
// for a tcpmux proxy that names no hostname: the multiplexer has nothing to
// route by.
func TestTCPMuxRefusesARegistrationWithoutDomains(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.TCPMuxPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backend)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeTCPMux, LocalAddr: backend,
		Multiplexer: config.MultiplexerHTTPConnect,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	refusal := agent.expectRefused("app")
	if !strings.Contains(refusal, "domains") {
		t.Fatalf("the refusal does not name domains: %s", refusal)
	}
}

// A location is a path prefix, and the route table compares it against a request
// path that always starts with a slash. A relative prefix was accepted and then
// never matched, so the proxy was published and unreachable at the same time.
func TestARelativeLocationIsRefused(t *testing.T) {
	backing := startHTTPService(t, "located")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(backing)})

	refused := protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeHTTP, LocalAddr: backing,
		Domains: []string{"located"}, Locations: []string{"api"},
	}
	if err := agent.client.register(refused); err != nil {
		t.Fatalf("register: %v", err)
	}
	refusal := agent.expectRefused("app")
	if !strings.Contains(refusal, "slash") {
		t.Fatalf("the refusal does not name the slash rule: %s", refusal)
	}

	// The same registration with a real prefix is accepted, so the check is not
	// refusing locations as such.
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeHTTP, LocalAddr: backing,
		Domains: []string{"located"}, Locations: []string{"/api"},
	})
}
