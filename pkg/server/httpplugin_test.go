package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// hookServer is a webhook endpoint the tests script: it records the requests
// it saw and answers from a function.
type hookServer struct {
	*httptest.Server

	mu     sync.Mutex
	bodies []map[string]any
	answer func(op string, body map[string]any) (int, string)
}

func newHookServer(t *testing.T, answer func(op string, body map[string]any) (int, string)) *hookServer {
	t.Helper()
	hook := &hookServer{answer: answer}
	hook.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		hook.mu.Lock()
		hook.bodies = append(hook.bodies, parsed)
		hook.mu.Unlock()
		code, payload := hook.answer(r.URL.Query().Get("op"), parsed)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(hook.Server.Close)
	return hook
}

func (h *hookServer) seen(t *testing.T, op string) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, body := range h.bodies {
		if body["op"] == op {
			return body
		}
	}
	t.Fatalf("the webhook never saw op %q; it saw %d requests", op, len(h.bodies))
	return nil
}

func pluginConfig(t *testing.T, addr string, ops ...string) *config.Config {
	t.Helper()
	cfg := testConfig(t, false)
	cfg.HTTPPlugins = []config.HTTPPluginConfig{{
		Name: "gate", Addr: addr, Path: "/hook", Ops: ops,
	}}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestHTTPPluginLoginRejected(t *testing.T) {
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		if op != config.HTTPPluginOpLogin {
			t.Fatalf("the webhook was asked op %q, want login", op)
		}
		return http.StatusOK, `{"reject":true,"reject_reason":"no entry today"}`
	})
	rs := startServer(t, pluginConfig(t, hook.URL, config.HTTPPluginOpLogin))

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.close()
	response, err := client.authenticate("test-agent", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("the login was accepted though the webhook rejected it")
	}
	if !strings.Contains(response.Error, "no entry today") {
		t.Fatalf("the refusal says %q, want the webhook's reason", response.Error)
	}

	// The webhook saw who was asking.
	body := hook.seen(t, config.HTTPPluginOpLogin)
	user, ok := body["content"].(map[string]any)["user"].(map[string]any)
	if !ok {
		t.Fatalf("the login content carries no user: %v", body)
	}
	if user["client_version"] != "test-agent" {
		t.Fatalf("the webhook saw client_version %v", user["client_version"])
	}
}

func TestHTTPPluginLoginDownRejects(t *testing.T) {
	// A webhook that cannot be reached closes the door instead of holding it
	// open.
	cfg := pluginConfig(t, "http://127.0.0.1:1", config.HTTPPluginOpLogin)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.close()
	response, err := client.authenticate("test-agent", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK || !strings.Contains(response.Error, "send login request to plugin error") {
		t.Fatalf("an unreachable webhook answered ok=%v error=%q", response.OK, response.Error)
	}
}

func TestHTTPPluginNewProxyRewritesTheSpec(t *testing.T) {
	echo := echoService(t)
	rewritten := freePort(t)
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		content := body["content"].(map[string]any)
		return http.StatusOK, fmt.Sprintf(
			`{"reject":false,"unchange":false,"content":{"name":%q,"type":"tcp","remote_port":%d}}`,
			content["name"], rewritten)
	})
	cfg := pluginConfig(t, hook.URL, config.HTTPPluginOpNewProxy)
	port := freePort(t)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo": framedStreamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: port,
	})

	// The tunnel is live on the port the webhook rewrote to, and the port the
	// client asked for stays closed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoaOf(rewritten)), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rewritten port %d never opened: %v", rewritten, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestHTTPPluginNewProxyRejected(t *testing.T) {
	echo := echoService(t)
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		return http.StatusOK, `{"reject":true,"reject_reason":"that name is on the blocklist"}`
	})
	rs := startServer(t, pluginConfig(t, hook.URL, config.HTTPPluginOpNewProxy))

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(echo)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if refusal := agent.expectRefused("web"); !strings.Contains(refusal, "that name is on the blocklist") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}

func TestHTTPPluginNewUserConnRejected(t *testing.T) {
	echo := echoService(t)
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		if op == config.HTTPPluginOpNewUserConn {
			content := body["content"].(map[string]any)
			if content["proxy_name"] != "echo" {
				t.Fatalf("the webhook saw proxy_name %v", content["proxy_name"])
			}
			if content["remote_addr"] == "" {
				t.Fatal("the webhook saw no remote_addr")
			}
			return http.StatusOK, `{"reject":true,"reject_reason":"off hours"}`
		}
		return http.StatusOK, `{"reject":false}`
	})
	cfg := pluginConfig(t, hook.URL, config.HTTPPluginOpNewProxy, config.HTTPPluginOpNewUserConn)
	port := freePort(t)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"echo": framedStreamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: port,
	})

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoaOf(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if n, err := conn.Read(buf); n > 0 || err == nil {
		t.Fatalf("the visitor was served %d bytes with no error, want a refusal close", n)
	}
}

func TestProxyHeadersInjectedOnTheTerminatingPath(t *testing.T) {
	// The origin records the header the server was told to inject and stamps
	// the response header it was told to add.
	originHeader := ""
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHeader = r.Header.Get("X-Aether-Stamp")
		w.Header().Set("X-Aether-Back", "server-says")
		fmt.Fprint(w, "origin-ok")
	}))
	defer origin.Close()

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(strings.TrimPrefix(origin.URL, "http://"))})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: strings.TrimPrefix(origin.URL, "http://"),
		Domains:        []string{"stamped.example"},
		RequestHeaders: map[string]string{"X-Aether-Stamp": "from-the-server"},
	})

	request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.Server.HTTPPort))+"/", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Host = "stamped.example"
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("status %d", answer.StatusCode)
	}
	if back := answer.Header.Get("X-Aether-Back"); back != "server-says" {
		t.Fatalf("the response carries X-Aether-Back %q", back)
	}
	if originHeader != "from-the-server" {
		t.Fatalf("the origin saw X-Aether-Stamp %q", originHeader)
	}
}
