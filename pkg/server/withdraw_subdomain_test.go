package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// readUntil reads frames from a test client until one of the wanted type
// arrives, skipping everything else the server may interleave.
func readUntil(t *testing.T, client *testClient, wanted protocol.MessageType) *protocol.Message {
	t.Helper()
	if err := client.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	for {
		message, err := client.framer.ReadFrame()
		if err != nil {
			t.Fatalf("read %s: %v", wanted, err)
		}
		if message.Type == wanted {
			return message
		}
	}
}

func readWithdrawAck(t *testing.T, client *testClient) protocol.ProxyWithdrawAck {
	t.Helper()
	message := readUntil(t, client, protocol.TypeProxyWithdrawAck)
	var ack protocol.ProxyWithdrawAck
	if err := json.Unmarshal(message.Payload, &ack); err != nil {
		t.Fatalf("malformed withdraw ack: %v", err)
	}
	return ack
}

// TestAWithdrawalRemovesTheTunnel walks the withdrawal exchange on the wire:
// the server confirms the first withdrawal of a proxy, and reports "no such
// proxy" for the second, which is the proof that the tunnel and its public
// endpoint are gone.
func TestAWithdrawalRemovesTheTunnel(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	port := freePort(t)
	rs := startServer(t, cfg)

	raw, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer raw.close()
	if _, err := raw.authenticate("withdraw-agent", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := raw.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: port,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	readUntil(t, raw, protocol.TypeProxyList)

	if err := raw.framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: "web"}); err != nil {
		t.Fatalf("send the withdrawal: %v", err)
	}
	ack := readWithdrawAck(t, raw)
	if !ack.OK {
		t.Fatalf("the first withdrawal was refused: %s", ack.Error)
	}

	if err := raw.framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: "web"}); err != nil {
		t.Fatalf("send the second withdrawal: %v", err)
	}
	ack = readWithdrawAck(t, raw)
	if ack.OK {
		t.Fatal("the second withdrawal succeeded: the tunnel is still registered")
	}
	if !strings.Contains(ack.Error, "no such proxy") {
		t.Fatalf("the second withdrawal error is %q, want a no-such-proxy refusal", ack.Error)
	}
}

// TestAWithdrawalOfAnUnknownProxyIsRefused pins the answer a client gets for a
// name it never registered, so a withdrawn-then-recovered proxy and a typo
// look different on the wire.
func TestAWithdrawalOfAnUnknownProxyIsRefused(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	rs := startServer(t, cfg)

	raw, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer raw.close()
	if _, err := raw.authenticate("withdraw-agent", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if err := raw.framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: "ghost"}); err != nil {
		t.Fatalf("send the withdrawal: %v", err)
	}
	ack := readWithdrawAck(t, raw)
	if ack.OK {
		t.Fatal("the withdrawal of an unknown proxy succeeded")
	}
	if !strings.Contains(ack.Error, "no such proxy") {
		t.Fatalf("the refusal is %q, want a no-such-proxy message", ack.Error)
	}
}

func vhostRequest(t *testing.T, port int, host string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Host = host
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", host, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// TestAClientSubdomainJoinsTheServerHost covers the client-supplied label: a
// proxy that declares subdomain = "shop" is published as
// shop.<server.subdomain_host>, the label is case-normalised, and the name
// stops naming the hostname once a subdomain is given.
func TestAClientSubdomainJoinsTheServerHost(t *testing.T) {
	service := startHTTPService(t, "label-service")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.SubdomainHost = "tunnel.example"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(service)})
	agent.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeHTTP, LocalAddr: service, Subdomain: "Shop",
	})

	status, body := vhostRequest(t, cfg.Server.HTTPPort, "shop.tunnel.example")
	if status != http.StatusOK {
		t.Fatalf("shop.tunnel.example: status %d (%s)", status, body)
	}
	if want := "label-service|host=shop.tunnel.example"; body != want {
		t.Fatalf("shop.tunnel.example: body %q, want %q", body, want)
	}

	status, _ = vhostRequest(t, cfg.Server.HTTPPort, "app.tunnel.example")
	if status != http.StatusNotFound {
		t.Fatalf("app.tunnel.example: status %d, want 404 once the subdomain owns the hostname", status)
	}
}

// TestAClientSubdomainNeedsTheServerHost documents the refusal when a client
// asks for a subdomain on a server that has no subdomain_host to host it under.
func TestAClientSubdomainNeedsTheServerHost(t *testing.T) {
	service := startHTTPService(t, "no-host")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(service)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeHTTP, LocalAddr: service, Subdomain: "shop",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	refusal := agent.expectRefused("app")
	if !strings.Contains(refusal, "subdomain_host") {
		t.Fatalf("the refusal does not name the setting that is missing: %s", refusal)
	}
}

// TestAClientSubdomainMustBeADNSLabel pins the server-side check for a label
// the client configuration layer would have rejected: a raw protocol client
// gets a refusal, not a broken hostname.
func TestAClientSubdomainMustBeADNSLabel(t *testing.T) {
	service := startHTTPService(t, "label-service")

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	cfg.Server.SubdomainHost = "tunnel.example"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"app": framedStreamHandler(service)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "app", Type: protocol.ProxyTypeHTTP, LocalAddr: service, Subdomain: "bad_label!",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	refusal := agent.expectRefused("app")
	if !strings.Contains(refusal, "subdomain") {
		t.Fatalf("the refusal does not name the subdomain: %s", refusal)
	}
}

// A withdrawn proxy has to leave the session's own list, not only the manager's.
// That list is what /api/clients reports, what the session's teardown walks, and
// what the max_ports_per_client count reads, so a name left in it makes the
// dashboard show a proxy that is gone and lets the next registration of that name
// go uncharged.
func TestAWithdrawalLeavesTheSessionsList(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	port := freePort(t)
	rs := startServer(t, cfg)

	raw, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer raw.close()
	response, err := raw.authenticate("withdraw-agent", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := raw.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1", RemotePort: port,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	readUntil(t, raw, protocol.TypeProxyList)

	session, ok := rs.server.sessions.Get(response.Session)
	if !ok {
		t.Fatalf("the server does not hold session %s", response.Session)
	}
	if names := session.Proxies(); len(names) != 1 || names[0] != "web" {
		t.Fatalf("the session lists %v, want [web] before the withdrawal", names)
	}

	if err := raw.framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: "web"}); err != nil {
		t.Fatalf("send the withdrawal: %v", err)
	}
	if ack := readWithdrawAck(t, raw); !ack.OK {
		t.Fatalf("the withdrawal was refused: %s", ack.Error)
	}

	if names := session.Proxies(); len(names) != 0 {
		t.Fatalf("the session still lists %v after the withdrawal", names)
	}
}
