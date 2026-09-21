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

// secondLoopback returns a second loopback address this machine can bind, or an
// empty string when it has only one.
//
// Linux and Windows answer on every address in 127.0.0.0/8, so 127.0.0.2 is local
// and can be used as a second source. macOS assigns only 127.0.0.1 to lo0, and
// binding another address in the range fails with "can't assign requested
// address". A test that needs two distinct source addresses calls this and skips
// when it comes back empty; the allow/deny rules are covered on every platform by
// the tests that use one address with ranges that do or do not contain it.
func secondLoopback(t *testing.T) string {
	t.Helper()

	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		return ""
	}
	_ = probe.Close()
	return "127.0.0.2"
}

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

// --- refusal counters ----------------------------------------------------------

// TestEveryPreHandshakeRefusalIsCounted covers the aggregate aethertunnel_control_rejected_total
// and the four series that explain it. The aggregate is the one an alert watches when
// it does not care why a connection was turned away, so every path that refuses a
// connection before the handshake has to move it: a source in deny_cidrs, a source
// over its connection rate, and a source whose ban window is still open. The ACL and
// rate-limit paths used to leave it at zero while moving only their own counter.
func TestEveryPreHandshakeRefusalIsCounted(t *testing.T) {
	dir := t.TempDir()
	auditPath := dir + "/audit.jsonl"

	// A second loopback address is the cleanest way to get an ACL refusal that cannot
	// also be a rate limit or a ban: the other rules are keyed on the address it is
	// denied for.
	other := secondLoopback(t)

	cfg := testConfig(t, false)
	cfg.Audit.Enabled = true
	cfg.Audit.Path = auditPath
	cfg.Server.RateLimitPerSecond = 1
	cfg.Server.RateLimitBurst = 1
	cfg.Server.BanAfterFailures = 1
	cfg.Server.BanSeconds = 60
	cfg.Server.BanMaxSeconds = 300
	if other != "" {
		cfg.Server.DenyCIDRs = []string{other + "/32"}
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	metrics := rs.server.metrics
	refusedBefore := metrics.controlRejected.Load()
	acceptedBefore := metrics.controlAccepted.Load()

	// A source in deny_cidrs is refused before the handshake.
	if other != "" {
		conn, err := dialFrom(t, other, rs.addr)
		if err != nil {
			t.Fatalf("dial from %s: %v", other, err)
		}
		conn.Close()
		waitForAuditEvents(t, auditPath, 1)
		if got := metrics.aclDenied.Load(); got != 1 {
			t.Errorf("the acl counter is %d after one refusal from a denied source", got)
		}
		if got := metrics.controlRejected.Load(); got != refusedBefore+1 {
			t.Errorf("the rejection counter is %d after one deny_cidrs refusal, want %d", got, refusedBefore+1)
		}
	} else {
		t.Log("this platform has no second loopback address, so the deny_cidrs refusal is not covered here")
	}

	// The first connection is inside the burst; the ones after it are over the rate and
	// are refused before the handshake.
	deadline := time.Now().Add(10 * time.Second)
	for metrics.rateLimited.Load() < 1 && time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.Close()
		time.Sleep(30 * time.Millisecond)
	}
	if got := metrics.rateLimited.Load(); got < 1 {
		t.Fatalf("no connection was rate limited although the burst is one")
	}
	refusedAtRate := metrics.controlRejected.Load()
	if refusedAtRate <= refusedBefore {
		t.Errorf("the rejection counter did not move for a rate-limited source: %d then %d", refusedBefore, refusedAtRate)
	}

	// A wrong token bans the source. The token bucket has to have refilled for the
	// attempt to reach authentication at all, so it is retried until it does.
	deadline = time.Now().Add(15 * time.Second)
	var banRate = metrics.rateLimited.Load()
	for metrics.authFailures.Load() < 1 && time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		framer := protocol.NewFramer(conn, nil, 0)
		if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
			Token: "not-the-token", ClientVersion: "counter-test", Protocol: protocol.ProtocolVersion,
		}); err != nil {
			t.Fatalf("send the bad token: %v", err)
		}
		var response protocol.AuthResponse
		_ = framer.ReadJSON(protocol.TypeAuthResponse, &response)
		conn.Close()
		if metrics.authFailures.Load() < 1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if got := metrics.authFailures.Load(); got < 1 {
		t.Fatalf("no attempt with a wrong token reached authentication: the rate limit kept refusing it")
	}
	if metrics.rateLimited.Load() > banRate {
		t.Log("the bad-token attempt was rate limited first, which is why it is retried")
	}

	// The banned source is now refused for the ban, which is a different counter again.
	deadline = time.Now().Add(15 * time.Second)
	for metrics.banRefused.Load() < 1 && time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial while banned: %v", err)
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	if got := metrics.banRefused.Load(); got < 1 {
		t.Fatalf("no attempt from a banned source was refused")
	}
	refusedAtBan := metrics.controlRejected.Load()
	if refusedAtBan <= refusedAtRate {
		t.Errorf("the rejection counter did not move for a banned source: %d then %d", refusedAtRate, refusedAtBan)
	}

	// Nothing that was refused counts as accepted.
	if got := metrics.controlAccepted.Load(); got != acceptedBefore {
		t.Errorf("the accepted counter is %d, want it to stay at %d: a refused connection is not an accepted one",
			got, acceptedBefore)
	}
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

	// Two failures with the wrong token, from the address the default dialer uses.
	for attempt := 1; attempt <= 2; attempt++ {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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
}

// TestABanDoesNotAffectAnotherSource is the half of the ban that only a second
// source address can show: the entry names one address and leaves the rest alone.
func TestABanDoesNotAffectAnotherSource(t *testing.T) {
	other := secondLoopback(t)
	if other == "" {
		t.Skip("this platform answers only on 127.0.0.1, so two source addresses cannot be told apart; " +
			"TestRepeatedAuthFailuresBanTheSource covers the ban itself")
	}

	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 1
	cfg.Server.BanSeconds = 60
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	conn, err := dialFrom(t, other, rs.addr)
	if err != nil {
		t.Fatalf("dial from %s: %v", other, err)
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: "wrong", ClientVersion: "ban-test", Protocol: protocol.ProtocolVersion,
	})
	var response protocol.AuthResponse
	_ = framer.ReadJSON(protocol.TypeAuthResponse, &response)
	conn.Close()

	if banned := rs.server.bans.banned(); banned != 1 {
		t.Fatalf("the list reports %d banned sources, want 1", banned)
	}

	// The address that never failed still authenticates.
	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("another source: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("still-welcome", testToken); err != nil {
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

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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

	good, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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

	again, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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
	cfg.Server.BanIgnoreCIDRs = []string{"127.0.0.1/32"}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	for attempt := 0; attempt < 3; attempt++ {
		conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
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

	// The address is still usable with the right token, which is what an ignored
	// source in front of a health checker needs.
	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("health-checker", testToken); err != nil {
		t.Fatalf("an ignored source was refused: %v", err)
	}
}

// --- per-proxy visitor access control ------------------------------------------

func TestAProxyRefusesAVisitorOutsideItsAllowList(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"guarded": streamHandler(echo)})
	// The list names a range the loopback visitor is not in, so the visitor that
	// does connect falls outside it.
	agent.register(protocol.ProxySpec{
		Name: "guarded", Type: protocol.ProxyTypeTCP, LocalAddr: echo,
		RemotePort: publicPort, AllowCIDRs: []string{"10.0.0.0/8"},
	})
	waitForListener(t, rs.server, "guarded")

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = visitor.Write([]byte("not welcome"))
	if n, err := visitor.Read(make([]byte, 64)); err == nil {
		t.Fatalf("the excluded visitor received %d byte(s)", n)
	}

	if denied := rs.server.metrics.visitorDenied.Load(); denied != 1 {
		t.Errorf("the metric reports %d refused visitors, want 1", denied)
	}
}

// TestAProxyAdmitsOneVisitorAddressAndRefusesAnother is the half of the check that
// needs two source addresses, so it skips where the platform has only one.
func TestAProxyAdmitsOneVisitorAddressAndRefusesAnother(t *testing.T) {
	other := secondLoopback(t)
	if other == "" {
		t.Skip("this platform answers only on 127.0.0.1, so two visitor addresses cannot be told apart; " +
			"TestAProxyRefusesAVisitorOutsideItsAllowList and TestAProxyDenyListWinsOverItsAllowList cover the rules")
	}

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
	stranger, err := dialFrom(t, other, fmt.Sprintf("127.0.0.1:%d", publicPort))
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
	closedPort := freePort(t)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"guarded": streamHandler(echo),
		"open":    streamHandler(echo),
	})
	// Both lists contain the loopback visitor, and deny decides.
	agent.register(protocol.ProxySpec{
		Name: "guarded", Type: protocol.ProxyTypeTCP, LocalAddr: echo,
		RemotePort: publicPort,
		AllowCIDRs: []string{"127.0.0.0/8"},
		DenyCIDRs:  []string{"127.0.0.1/32"},
	})
	// The same allow list without the deny entry serves the same visitor.
	agent.register(protocol.ProxySpec{
		Name: "open", Type: protocol.ProxyTypeTCP, LocalAddr: echo,
		RemotePort: closedPort, AllowCIDRs: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "guarded")
	waitForListener(t, rs.server, "open")

	denied, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("denied visitor: %v", err)
	}
	defer denied.Close()
	_ = denied.SetDeadline(time.Now().Add(3 * time.Second))
	if n, err := denied.Read(make([]byte, 64)); err == nil {
		t.Fatalf("a deny-listed visitor received %d byte(s)", n)
	}

	served, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", closedPort), 5*time.Second)
	if err != nil {
		t.Fatalf("allowed visitor: %v", err)
	}
	defer served.Close()
	_ = served.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = served.Write([]byte("still welcome"))
	got := make([]byte, len("still welcome"))
	if _, err := io.ReadFull(served, got); err != nil {
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
		// The visitor range excludes the address that connects.
		AllowCIDRs: []string{"10.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(3 * time.Second))

	// The endpoint drops the connection without answering the greeting.
	if _, err := visitor.Write([]byte{socks.Version, 1, socks.MethodNoReq}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if n, err := visitor.Read(make([]byte, 2)); err == nil {
		t.Fatalf("the excluded visitor received %d byte(s)", n)
	}
}
