package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// waitForAuditEvents reads the audit log until it holds at least n events. Events
// are written by the connection handlers as they run, so a reader has to allow
// for that rather than reading the file the moment a test triggers one.
func waitForAuditEvents(t *testing.T, path string, n int) []AuditEvent {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			events := make([]AuditEvent, 0, n)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var event AuditEvent
				if err := json.Unmarshal([]byte(line), &event); err == nil {
					events = append(events, event)
				}
			}
			if len(events) >= n {
				return events
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log at %s holds fewer than %d events after 5s", path, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// strangerAddress is a second loopback address. It makes "a source that is not
// the client" testable on one machine: 127.0.0.1 stays the trusted client while
// 127.0.0.2 plays the visitor that a rule is meant to exclude.
const strangerAddress = "127.0.0.2"

// dialFrom opens a TCP connection whose source address is from.
func dialFrom(t *testing.T, from, target string) (net.Conn, error) {
	t.Helper()

	remote, err := net.ResolveTCPAddr("tcp", target)
	if err != nil {
		t.Fatalf("resolve %s: %v", target, err)
	}
	local, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(from, "0"))
	if err != nil {
		t.Fatalf("resolve %s: %v", from, err)
	}

	dialer := &net.Dialer{LocalAddr: local, Timeout: 5 * time.Second}
	return dialer.Dial("tcp", remote.String())
}

// --- automatic bans ------------------------------------------------------------

func TestRepeatedAuthFailuresBanTheSource(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 2
	cfg.Server.BanSeconds = 60
	cfg.Server.BanMaxSeconds = 300
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	// Two failures with the wrong token, from the stranger address.
	for attempt := 1; attempt <= 2; attempt++ {
		conn, err := dialFrom(t, strangerAddress, rs.addr)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		framer := protocol.NewFramer(conn, nil, 0)
		if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
			Token: "not-the-token", ClientVersion: "ban-test", Protocol: protocol.ProtocolVersion,
		}); err != nil {
			t.Fatalf("attempt %d: send: %v", attempt, err)
		}
		var response protocol.AuthResponse
		_ = framer.ReadJSON(protocol.TypeAuthResponse, &response)
		conn.Close()
	}

	if banned := rs.server.bans.banned(); banned != 1 {
		t.Fatalf("the list reports %d banned sources, want 1", banned)
	}

	// The next connection from that address is refused before the handshake, so
	// even a correct token gets nothing.
	conn, err := dialFrom(t, strangerAddress, rs.addr)
	if err != nil {
		t.Fatalf("banned attempt: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	framer := protocol.NewFramer(conn, nil, 0)
	if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "ban-test", Protocol: protocol.ProtocolVersion,
	}); err != nil {
		t.Fatalf("banned attempt: send: %v", err)
	}
	var response protocol.AuthResponse
	if err := framer.ReadJSON(protocol.TypeAuthResponse, &response); err == nil && response.OK {
		t.Fatal("a banned source authenticated")
	}

	// A different source is unaffected, which is the point of banning per address.
	other, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("another source: %v", err)
	}
	defer other.close()
	if _, err := other.authenticate("still-welcome", testToken); err != nil {
		t.Fatalf("a source that never failed was refused: %v", err)
	}
}

func TestABanIsRecordedInTheAuditLog(t *testing.T) {
	dir := t.TempDir()
	auditPath := dir + "/audit.jsonl"

	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 1
	cfg.Server.BanSeconds = 60
	cfg.Audit.Enabled = true
	cfg.Audit.Path = auditPath
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	conn, err := dialFrom(t, strangerAddress, rs.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: "wrong", ClientVersion: "audit-test", Protocol: protocol.ProtocolVersion,
	})
	var response protocol.AuthResponse
	_ = framer.ReadJSON(protocol.TypeAuthResponse, &response)
	conn.Close()

	events := waitForAuditEvents(t, auditPath, 2)
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Event] = true
	}
	if !seen[EventAuthFailed] {
		t.Errorf("the audit log has no %s event", EventAuthFailed)
	}
	if !seen[EventSourceBanned] {
		t.Errorf("the audit log has no %s event: %v", EventSourceBanned, events)
	}
}

func TestASuccessfulLoginClearsTheFailureCount(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 2
	cfg.Server.BanSeconds = 60
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	// One failure, then a success from the same address, then one failure: the
	// counter was cleared, so nothing is banned.
	conn, err := dialFrom(t, strangerAddress, rs.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: "wrong", ClientVersion: "typo", Protocol: protocol.ProtocolVersion,
	})
	var failed protocol.AuthResponse
	_ = framer.ReadJSON(protocol.TypeAuthResponse, &failed)
	conn.Close()

	good, err := dialFrom(t, strangerAddress, rs.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	goodFramer := protocol.NewFramer(good, nil, 0)
	if err := goodFramer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "typo", Protocol: protocol.ProtocolVersion,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var ok protocol.AuthResponse
	if err := goodFramer.ReadJSON(protocol.TypeAuthResponse, &ok); err != nil || !ok.OK {
		t.Fatalf("the corrected token was refused: %v (%s)", err, ok.Error)
	}
	good.Close()

	again, err := dialFrom(t, strangerAddress, rs.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	againFramer := protocol.NewFramer(again, nil, 0)
	_ = againFramer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: "wrong", ClientVersion: "typo", Protocol: protocol.ProtocolVersion,
	})
	var second protocol.AuthResponse
	_ = againFramer.ReadJSON(protocol.TypeAuthResponse, &second)
	again.Close()

	if banned := rs.server.bans.banned(); banned != 0 {
		t.Errorf("%d source(s) were banned although the counter was cleared", banned)
	}
}

func TestBanIgnoreCIDRsKeepsChosenSourcesConnected(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 1
	cfg.Server.BanSeconds = 60
	cfg.Server.BanIgnoreCIDRs = []string{strangerAddress + "/32"}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	for attempt := 0; attempt < 3; attempt++ {
		conn, err := dialFrom(t, strangerAddress, rs.addr)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		framer := protocol.NewFramer(conn, nil, 0)
		_ = framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
			Token: "wrong", ClientVersion: "ignored", Protocol: protocol.ProtocolVersion,
		})
		var response protocol.AuthResponse
		// The refusal is the ordinary auth rejection, not a closed connection.
		if err := framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
			t.Fatalf("attempt %d: the connection was dropped instead of answered: %v", attempt, err)
		}
		conn.Close()
	}

	if banned := rs.server.bans.banned(); banned != 0 {
		t.Errorf("%d source(s) were banned although ban_ignore_cidrs names them", banned)
	}
}

// --- per-proxy visitor access control ------------------------------------------

func TestAProxyRefusesAVisitorOutsideItsAllowList(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"guarded": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "guarded", Type: protocol.ProxyTypeTCP, LocalAddr: echo,
		RemotePort: publicPort, AllowCIDRs: []string{"127.0.0.1/32"},
	})
	waitForListener(t, rs.server, "guarded")

	// The allowed visitor is served.
	allowed, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("allowed visitor: %v", err)
	}
	defer allowed.Close()
	_ = allowed.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := allowed.Write([]byte("welcome")); err != nil {
		t.Fatalf("allowed visitor: write: %v", err)
	}
	got := make([]byte, len("welcome"))
	if _, err := io.ReadFull(allowed, got); err != nil {
		t.Fatalf("allowed visitor: read: %v", err)
	}
	if string(got) != "welcome" {
		t.Fatalf("the allowed visitor received %q", got)
	}

	// The visitor from another address is dropped without being served.
	stranger, err := dialFrom(t, strangerAddress, fmt.Sprintf("127.0.0.1:%d", publicPort))
	if err != nil {
		t.Fatalf("stranger: %v", err)
	}
	defer stranger.Close()
	_ = stranger.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = stranger.Write([]byte("not welcome"))
	if n, err := stranger.Read(make([]byte, 64)); err == nil {
		t.Fatalf("the excluded visitor received %d byte(s)", n)
	}

	if denied := rs.server.metrics.visitorDenied.Load(); denied != 1 {
		t.Errorf("the metric reports %d refused visitors, want 1", denied)
	}
}

func TestAProxyDenyListWinsOverItsAllowList(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"guarded": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "guarded", Type: protocol.ProxyTypeTCP, LocalAddr: echo,
		RemotePort: publicPort,
		AllowCIDRs: []string{"127.0.0.0/8"},
		DenyCIDRs:  []string{strangerAddress + "/32"},
	})
	waitForListener(t, rs.server, "guarded")

	stranger, err := dialFrom(t, strangerAddress, fmt.Sprintf("127.0.0.1:%d", publicPort))
	if err != nil {
		t.Fatalf("stranger: %v", err)
	}
	defer stranger.Close()
	_ = stranger.SetDeadline(time.Now().Add(3 * time.Second))
	if n, err := stranger.Read(make([]byte, 64)); err == nil {
		t.Fatalf("a deny-listed visitor received %d byte(s)", n)
	}

	allowed, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("allowed visitor: %v", err)
	}
	defer allowed.Close()
	_ = allowed.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = allowed.Write([]byte("still welcome"))
	got := make([]byte, len("still welcome"))
	if _, err := io.ReadFull(allowed, got); err != nil {
		t.Fatalf("the allowed visitor was not served: %v", err)
	}
}

func TestRegistrationRefusesAProxyWhoseVisitorCIDRsAreInvalid(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"broken": streamHandler(echo)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "broken", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
		AllowCIDRs: []string{"not-a-cidr"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if reason := agent.expectRefused("broken"); !strings.Contains(reason, "not valid") {
		t.Errorf("the refusal reads %q", reason)
	}
}

// --- socks5 --------------------------------------------------------------------

// socksRequest performs the SOCKS5 exchange for target and returns the reply code.
func socksRequest(t *testing.T, conn net.Conn, target string) byte {
	t.Helper()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{socks.Version, 1, socks.MethodNoReq}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("method reply: %v", err)
	}
	if reply[0] != socks.Version || reply[1] != socks.MethodNoReq {
		t.Fatalf("the endpoint answered %v", reply)
	}

	host, port, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("target %q: %v", target, err)
	}
	request := []byte{socks.Version, socks.CmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		request = append(request, socks.AtypIPv4)
		request = append(request, ip.To4()...)
	} else {
		request = append(request, socks.AtypDomain, byte(len(host)))
		request = append(request, []byte(host)...)
	}
	number, err := net.LookupPort("tcp", port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(number))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("request: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	// Bound address and port, whatever their length.
	length := 0
	switch head[3] {
	case socks.AtypIPv4:
		length = 4
	case socks.AtypIPv6:
		length = 16
	case socks.AtypDomain:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			t.Fatalf("reply address: %v", err)
		}
		length = int(size[0]) + 1
	}
	if _, err := io.ReadFull(conn, make([]byte, length+2)); err != nil {
		t.Fatalf("reply address: %v", err)
	}
	return head[1]
}

func TestSocks5TunnelReachesTheTargetTheVisitorAsksFor(t *testing.T) {
	first := startEcho(t)
	second := startEcho(t)

	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	policy, err := socks.NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	agent.setDialTarget(func(target string) (net.Conn, error) { return policy.Dial(target, 5*time.Second) })
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	for _, target := range []string{first, second} {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
		if err != nil {
			t.Fatalf("%s: dial: %v", target, err)
		}
		if code := socksRequest(t, conn, target); code != socks.ReplySucceeded {
			t.Fatalf("%s: reply code %d", target, code)
		}

		payload := []byte("through the exit:" + target)
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("%s: write: %v", target, err)
		}
		echoed := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, echoed); err != nil {
			t.Fatalf("%s: read: %v", target, err)
		}
		if string(echoed) != string(payload) {
			t.Fatalf("%s: received %q", target, echoed)
		}
		conn.Close()
	}

	if served := rs.server.metrics.socksRequests.Load(); served != 2 {
		t.Errorf("the metric reports %d served requests, want 2", served)
	}
}

func TestSocks5TunnelRefusesATargetOutsideTheAllowList(t *testing.T) {
	echo := startEcho(t)

	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	// The client allows only a range the echo service is not in.
	policy, err := socks.NewTargetPolicy([]string{"10.99.0.0/16"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	agent.setDialTarget(func(target string) (net.Conn, error) { return policy.Dial(target, 5*time.Second) })
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"10.99.0.0/16"},
	})
	waitForListener(t, rs.server, "exit")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if code := socksRequest(t, conn, echo); code != socks.ReplyNotAllowed {
		t.Errorf("the reply code is %d, want %d (not allowed)", code, socks.ReplyNotAllowed)
	}
}

func TestSocks5RegistrationNeedsAnAllowList(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, nil)
	if err := agent.client.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: freePort(t),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if reason := agent.expectRefused("exit"); !strings.Contains(reason, "allow_targets") {
		t.Errorf("the refusal reads %q", reason)
	}
}

func TestSocks5EndpointRefusesAVisitorOutsideItsAllowList(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	policy, err := socks.NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	agent.setDialTarget(func(target string) (net.Conn, error) { return policy.Dial(target, 5*time.Second) })
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
		AllowCIDRs:   []string{"127.0.0.1/32"},
	})
	waitForListener(t, rs.server, "exit")

	stranger, err := dialFrom(t, strangerAddress, fmt.Sprintf("127.0.0.1:%d", publicPort))
	if err != nil {
		t.Fatalf("stranger: %v", err)
	}
	defer stranger.Close()
	_ = stranger.SetDeadline(time.Now().Add(3 * time.Second))

	// The endpoint drops the connection without answering the greeting.
	if _, err := stranger.Write([]byte{socks.Version, 1, socks.MethodNoReq}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if n, err := stranger.Read(make([]byte, 2)); err == nil {
		t.Fatalf("the excluded visitor received %d byte(s)", n)
	}
}
