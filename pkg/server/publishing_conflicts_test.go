package server

import (
	"bytes"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// captureLogBuffer is the server's log for the tests that assert on what an
// operator would read. The server writes from several goroutines.
type captureLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *captureLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *captureLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The replacement check asks what the binding routes to this group. The routers
// rank an exact name over a wildcard and keep the two in separate buckets, so
// "*.example.com" and "example.com" can be published by two proxies at once
// while every request for the base name goes to the exact one — the check used
// to match the group's own wildcard and let the replacement take a name that
// the other proxy, not this one, answers.
func TestAReplacementMayNotTakeANameAnotherProxyPublishesExactly(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	wildcard := startAgent(t, rs.addr, false, map[string]dataHandler{})
	exact := startAgent(t, rs.addr, false, map[string]dataHandler{})

	port := freePort(t)
	wildcard.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"*.example.com"}, RemotePort: port,
	})
	exact.register(protocol.ProxySpec{
		Name: "blog", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"example.com"}, RemotePort: freePort(t),
	})

	// The wildcard owner re-registers onto the exact name the other proxy
	// published, keeping the port it already holds — the routers answer that
	// name with the exact entry, so the replacement would be announced for a
	// host this proxy never answers.
	if err := wildcard.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"example.com"}, RemotePort: port,
	}); err != nil {
		t.Fatalf("send the re-registration: %v", err)
	}
	if refusal := wildcard.expectRefused("web"); !strings.Contains(refusal, "not published for hostname") {
		t.Fatalf("the replacement onto a name another proxy publishes exactly was accepted: %s", refusal)
	}
}

// The single-member publish branch announces the group's endpoint. A pool
// joiner's own remote_port was never adopted, and its replacement is only free
// to name its own port while the founding member is still in the list — so a
// replacement accepted on that rule whose founder leaves before the publish
// branch runs would have the record point at the joiner's port, a port nothing
// listens on, until the group's lifecycle withdraws the name.
func TestADirectoryRecordForASingleMemberGroupNamesTheGroupsPort(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeTCP, RemotePort: 20000}, manager)

	got := recordForPublish(group, protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeTCP, RemotePort: 20001})
	if got.RemotePort != group.RemotePort {
		t.Fatalf("the record announced remote_port %d while the group serves on %d", got.RemotePort, group.RemotePort)
	}
	if got.Name != "web" || got.Type != protocol.ProxyTypeTCP {
		t.Fatalf("the record lost its identity: %q %q", got.Name, got.Type)
	}
}

// A pump puts the connection it was handed in the session's set, which is what
// the shutdown's disconnectStreams closes. Every production relay is handed
// stream.dc, which openStreamWith has already tracked, so this pins the pump's
// own contract — what a driver that bypasses openStreamWith gets — rather than a
// production gap: the set dedups, so the overlap is inert.
func TestAShutdownDisconnectEndsAPublicSideStream(t *testing.T) {
	s := &Session{ID: "shutdown-test"}
	tun := &Tunnel{Name: "web", Session: s, metrics: newMetrics(), logger: discardLogger()}
	publicLocal, publicRemote := net.Pipe()
	defer publicRemote.Close()
	dcSide, dcRemote := net.Pipe()
	defer dcRemote.Close()
	dc := &dataConn{conn: dcSide}

	returned := make(chan error, 1)
	go func() { returned <- tun.pipeStream(publicLocal, dc, "test") }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		tracked := len(s.streams)
		s.mu.Unlock()
		if tracked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the public-side stream is not tracked by the session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// What the shutdown's disconnect does: close the data connections the
	// session still serves.
	s.disconnectStreams()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnectStreams did not end the public-side stream")
	}
	s.mu.Lock()
	tracked := len(s.streams)
	s.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("%d stream(s) left tracked after the pump returned", tracked)
	}
}

// A rejected gateway password is an authentication failure like any other, and
// recordAuthFailure is the only place a ban is imposed and says every way of
// failing to authenticate counts. The gateway used to be a password oracle the
// ban ladder never heard about: a source the control port would have banned
// could keep guessing at the gateway forever.
func TestAWrongGatewayPasswordFeedsTheBanLadder(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  keyPath,
		User:     "u",
		Password: "p",
	}
	cfg.Server.BanAfterFailures = 2
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	waitForTCPPort(t, port)

	dial := func(password string) error {
		_, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &ssh.ClientConfig{
			User:            "u",
			Auth:            []ssh.AuthMethod{ssh.Password(password)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		})
		return err
	}
	if err := dial("wrong-one"); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if err := dial("wrong-two"); err == nil {
		t.Fatal("a second wrong password was accepted")
	}
	if got := rs.server.metrics.authFailures.Load(); got != 2 {
		t.Fatalf("the gateway's password failures counted %d auth failures, want 2", got)
	}

	// The source has failed twice, so the ban ladder refuses it before the
	// handshake: even the correct password cannot get in.
	if err := dial("p"); err == nil {
		t.Fatal("a source the ban ladder banned was still admitted by the gateway")
	}
	if banned, _ := rs.server.bans.blocked(net.ParseIP("127.0.0.1")); !banned {
		t.Fatal("the source is not banned after two failed gateway passwords")
	}
}

// A wrong --token is a failed credential check on the tunnel layer, one the
// peer can repeat within the session, so it feeds the auth-failure counter the
// way the control and visitor paths feed it. The session and its forwards stay
// up, as the gateway's documentation promises.
func TestAGatewayTokenFailureCountsAsAnAuthFailure(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  keyPath,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	before := rs.server.metrics.authFailures.Load()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := session.Start("tcp --proxy_name web --remote_port 18080 --token not-the-token"); err != nil {
		t.Fatalf("start: %v", err)
	}
	readSSHUntil(t, stdout, "authentication failed", 5*time.Second)

	if got := rs.server.metrics.authFailures.Load(); got != before+1 {
		t.Fatalf("a wrong --token moved the auth-failure count from %d to %d, want exactly one more", before, got)
	}
}

// The ignored-forward log line names the peer's address, which is the peer's
// own bytes: an address spelling the logging package's level marker used to be
// able to re-level or suppress the line that named it. The address is quoted,
// so the line carries the rendering, never the raw bytes.
func TestAnIgnoredForwardLogNamesTheAddressWithoutRawControlBytes(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  keyPath,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	logs := &captureLogBuffer{}
	startServerLoggingTo(t, cfg, logs)

	client := dialSSHGateway(t, port)
	defer client.Close()
	// One accepted remote forwarding makes every further tcpip-forward request
	// "not the rule this session publishes" — the ignored branch, whose log
	// line is the one that names the address.
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("remote forwarding: %v", err)
	}

	payload := ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"a\x000\x00b", 12345})
	ok, _, err := client.SendRequest("tcpip-forward", true, payload)
	if err != nil {
		t.Fatalf("send the second forward request: %v", err)
	}
	if !ok {
		t.Fatal("the ignored forward request was refused")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		out := logs.String()
		if strings.Contains(out, "asked to forward") {
			if strings.Contains(out, "\x00") {
				t.Fatalf("the peer's address reached the log with raw control bytes: %q", out)
			}
			if !strings.Contains(out, `"a\x000\x00b"`) {
				t.Fatalf("the logged address is not the quoted rendering: %q", out)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ignored-forward line never reached the log; got:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
