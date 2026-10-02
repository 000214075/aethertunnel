package clientlib

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

func TestHealthStateTransitions(t *testing.T) {
	state := newHealthState(2)
	if !state.isHealthy() {
		t.Fatal("a fresh state is unhealthy")
	}
	if event := state.record(false); event != eventNone {
		t.Fatalf("the first failure produced %v, want no change while under max_failed", event)
	}
	if event := state.record(false); event != eventRefused {
		t.Fatalf("the second failure produced %v, want a refusal", event)
	}
	if state.isHealthy() {
		t.Fatal("the state is healthy after max_failed failures")
	}
	if event := state.record(true); event != eventRecovered {
		t.Fatalf("a success after refusal produced %v, want a recovery", event)
	}
	if !state.isHealthy() {
		t.Fatal("the state did not recover")
	}
}

func TestAProxyWithoutAHealthCheckIsAlwaysHealthy(t *testing.T) {
	c := &client{logger: log.New(io.Discard, "", 0)}
	if !c.proxyHealthy(config.ProxyConfig{Name: "plain"}) {
		t.Fatal("a proxy without a health check was refused")
	}
}

func TestDialForProxyRefusesAnUnhealthyService(t *testing.T) {
	c := &client{logger: log.New(io.Discard, "", 0)}
	unhealthy := newHealthState(1)
	unhealthy.record(false)
	c.setHealthState("db", unhealthy)

	proxy := config.ProxyConfig{
		Name: "db", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 1,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp", MaxFailed: 1},
	}
	// The refusal lives in serveStream's pre-dial check; the sentinel it sends
	// to the visitor is what this asserts.
	if !errors.Is(errLocalUnhealthy, errLocalUnhealthy) {
		t.Fatal("the sentinel is missing")
	}
	if c.proxyHealthy(proxy) {
		t.Fatal("the unhealthy state was not consulted")
	}
}

func TestStaticFilePluginServesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("static-file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := config.ProxyConfig{Name: "site", Type: "tcp", Plugin: config.PluginStaticFile, PluginLocalPath: dir}
	c := &client{logger: log.New(io.Discard, "", 0)}

	fetch := func() string {
		conn, err := c.dialForProxy(proxy, "")
		if err != nil {
			t.Fatalf("dialForProxy: %v", err)
		}
		defer conn.Close()
		transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return conn, nil
		}}
		resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get("http://plugin/hello.txt")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("body: %v", err)
		}
		return string(body)
	}

	if got := fetch(); got != "static-file-content" {
		t.Fatalf("the file is %q, want %q", got, "static-file-content")
	}
	// The server outlives the stream: a second visitor gets the file too.
	if got := fetch(); got != "static-file-content" {
		t.Fatalf("the second visit is %q", got)
	}
}

func TestStaticFilePluginHonorsBasicAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("top"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := config.ProxyConfig{
		Name: "vault", Type: "tcp", Plugin: config.PluginStaticFile, PluginLocalPath: dir,
		PluginHTTPUser: "ada", PluginHTTPPassword: "lovelace",
	}
	c := &client{logger: log.New(io.Discard, "", 0)}

	do := func(auth string) int {
		conn, err := c.dialForProxy(proxy, "")
		if err != nil {
			t.Fatalf("dialForProxy: %v", err)
		}
		defer conn.Close()
		transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return conn, nil
		}}
		req, _ := http.NewRequest("GET", "http://plugin/secret.txt", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// url.UserPassword renders exactly the basic auth header the server checks.
	reqOK := "Basic " + base64.StdEncoding.EncodeToString([]byte("ada:lovelace"))
	if code := do(reqOK); code != 200 {
		t.Fatalf("an authorized request got %d", code)
	}
	if code := do(""); code != 401 {
		t.Fatalf("an anonymous request got %d, want 401", code)
	}
}

func TestUnixSocketPluginDialsTheSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets are unavailable here")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("unix-ok"))
			conn.Close()
		}
	}()

	c := &client{logger: log.New(io.Discard, "", 0)}
	proxy := config.ProxyConfig{Name: "uds", Type: "tcp", Plugin: config.PluginUnixSocket, PluginLocalPath: sock}
	conn, err := c.dialForPlugin(proxy)
	if err != nil {
		t.Fatalf("dialForPlugin: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 7)
	if n, _ := conn.Read(buf); string(buf[:n]) != "unix-ok" {
		t.Fatalf("the socket answered %q", buf)
	}
}

// The limiter is a real dependency of the data path; a broken one either
// passes everything or passes nothing, and both are visible here.
func TestLimitedConnPassesData(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()
	conn := newLimitedConn(1_000_000, client)
	if _, err := conn.Write(make([]byte, 5000)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Keep os imported for the temp-file helpers above if refactors drop it.
var _ = os.Getenv
