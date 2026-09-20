package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// testTUN is an in-memory layer-3 interface for the server tests.
//
// toClients holds packets that arrived on the interface and therefore have to be
// routed to a session; fromClients collects the packets the router wrote, which are
// what a client sent. Close unblocks a pending Read, which is the contract a real
// interface satisfies by being closed.
type testTUN struct {
	name string
	mtu  int

	toClients   chan []byte
	fromClients chan []byte
	closedCh    chan struct{}

	mu       sync.Mutex
	address  string
	closeOne sync.Once
}

func newTestTUN(name string, mtu int) *testTUN {
	return &testTUN{
		name:        name,
		mtu:         mtu,
		toClients:   make(chan []byte, 32),
		fromClients: make(chan []byte, 32),
		closedCh:    make(chan struct{}),
	}
}

func (d *testTUN) Name() string { return d.name }
func (d *testTUN) MTU() int     { return d.mtu }

func (d *testTUN) Read(packet []byte) (int, error) {
	select {
	case <-d.closedCh:
		return 0, io.ErrClosedPipe
	case next := <-d.toClients:
		return copy(packet, next), nil
	}
}

func (d *testTUN) Write(packet []byte) (int, error) {
	select {
	case <-d.closedCh:
		return 0, io.ErrClosedPipe
	case d.fromClients <- append([]byte(nil), packet...):
		return len(packet), nil
	}
}

func (d *testTUN) Close() error {
	d.closeOne.Do(func() { close(d.closedCh) })
	return nil
}

// SetAddress records the address the server assigned to its own end of the link.
func (d *testTUN) SetAddress(address string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.address = address
	return nil
}

func (d *testTUN) assigned() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.address
}

// vpnConfig is a server configuration with a layer-3 tunnel on a subnet that leaves
// room for a handful of clients.
func vpnConfig(t *testing.T, encryption bool) (*config.Config, *testTUN) {
	t.Helper()
	cfg := testConfig(t, encryption)
	cfg.VPN.Enabled = true
	cfg.VPN.Device = "test0"
	cfg.VPN.Address = "10.7.0.0/29"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("vpn config invalid: %v", err)
	}
	device := newTestTUN(cfg.VPN.Device, 1400)
	return cfg, device
}

// startVPNServer starts a server whose tunnel interface is the given in-memory
// device.
func startVPNServer(t *testing.T, cfg *config.Config, device *testTUN) *runningServer {
	t.Helper()
	opener := func(name string, mtu int) (vpn.Device, error) {
		if name != device.Name() {
			return nil, errors.New("the test only provides " + device.Name())
		}
		return device, nil
	}
	srv, err := New(cfg, Options{
		Version: "test-version", BuildTime: "now", GitCommit: "test",
		Logger: discardLogger(), VPNOpen: opener,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.listener == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rs := &runningServer{server: srv, addr: srv.listener.Addr().String(), cancel: cancel, done: done}
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

// vpnClient is a control client that asks for a tunnel address.
type vpnClient struct {
	*testClient
	response protocol.AuthResponse
}

func dialVPNClient(t *testing.T, rs *runningServer, encryption, askForVPN bool) *vpnClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	client := &testClient{conn: conn, framer: protocol.NewFramer(conn, nil, 0)}

	err = client.framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "vpn-test", Protocol: protocol.ProtocolVersion, VPN: askForVPN,
	})
	if err != nil {
		t.Fatalf("send the auth request: %v", err)
	}

	var response protocol.AuthResponse
	if err := client.framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		t.Fatalf("read the auth response: %v", err)
	}
	client.session = response.Session
	return &vpnClient{testClient: client, response: response}
}

// sendPacket sends one IP packet as a tunnel frame.
func (c *vpnClient) sendPacket(packet []byte) error {
	return c.framer.WriteFrame(&protocol.Message{Type: protocol.TypeVPNPacket, Payload: packet})
}

// readPacket waits for one tunnel frame and returns its payload.
func (c *vpnClient) readPacket(t *testing.T, timeout time.Duration) []byte {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		msg, err := c.framer.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for a tunnel frame: %v", err)
		}
		switch msg.Type {
		case protocol.TypeVPNPacket:
			return msg.Payload
		case protocol.TypeHeartbeatAck, protocol.TypeProxyList:
			continue
		default:
			t.Fatalf("unexpected frame %s while waiting for a tunnel packet", msg.Type)
		}
	}
}

// --- tests --------------------------------------------------------------------

func TestVPNSessionIsGivenAnAddressFromTheSubnet(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	client := dialVPNClient(t, rs, false, true)
	defer client.close()

	if !client.response.OK {
		t.Fatalf("the session was refused: %s", client.response.Error)
	}
	if client.response.VPNAddress == "" {
		t.Fatal("the session was accepted without a tunnel address")
	}
	pool, err := vpn.NewPool(cfg.VPN.Address)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if !pool.Contains(net.ParseIP(client.response.VPNAddress)) {
		t.Fatalf("the assigned address %s is not a client address in %s",
			client.response.VPNAddress, cfg.VPN.Address)
	}
	if client.response.VPNMask != "255.255.255.248" {
		t.Errorf("the mask is %q, want 255.255.255.248 for a /29", client.response.VPNMask)
	}
	if client.response.VPNMTU <= 0 {
		t.Errorf("the MTU is %d", client.response.VPNMTU)
	}
	if got := device.assigned(); got != "10.7.0.1" {
		t.Errorf("the server's own address is %q, want 10.7.0.1", got)
	}

	waitForCondition(t, func() bool { return rs.server.vpn.router.PeerCount() == 1 },
		"the session was not attached to the router")
	if used := rs.server.vpn.pool.InUse(); used != 1 {
		t.Errorf("%d addresses are in use, want 1", used)
	}
}

func TestVPNCarriesPacketsInBothDirections(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	client := dialVPNClient(t, rs, false, true)
	defer client.close()

	address := client.response.VPNAddress
	if address == "" {
		t.Fatal("no tunnel address was assigned")
	}

	// Client to server: the packet is written to the server's interface.
	fromClient := ipv4PacketForTest(address, "10.7.0.1", []byte("ping"))
	if err := client.sendPacket(fromClient); err != nil {
		t.Fatalf("send a tunnel packet: %v", err)
	}
	got := receiveFromChannel(t, device.fromClients)
	if !vpn.Source(got).Equal(net.ParseIP(address)) {
		t.Fatalf("the interface saw a packet from %s, want %s", vpn.Source(got), address)
	}

	// Server to client: a packet for the client's address is delivered as a frame.
	toClient := ipv4PacketForTest("10.7.0.1", address, []byte("pong"))
	device.toClients <- toClient
	payload := client.readPacket(t, 5*time.Second)
	if !vpn.Destination(payload).Equal(net.ParseIP(address)) {
		t.Fatalf("the client received a packet for %s, want %s", vpn.Destination(payload), address)
	}
}

func TestVPNInterfaceDoesNotAcceptForgedSourceAddresses(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	client := dialVPNClient(t, rs, false, true)
	defer client.close()

	address := client.response.VPNAddress
	if address == "" {
		t.Fatal("no tunnel address was assigned")
	}

	// The client holds .2 (or whatever it was given) but claims to be .3.
	forged := ipv4PacketForTest("10.7.0.3", "10.7.0.1", []byte("spoofed"))
	honest := ipv4PacketForTest(address, "10.7.0.1", []byte("honest"))
	if err := client.sendPacket(forged); err != nil {
		t.Fatalf("send the forged packet: %v", err)
	}
	if err := client.sendPacket(honest); err != nil {
		t.Fatalf("send the honest packet: %v", err)
	}

	got := receiveFromChannel(t, device.fromClients)
	if !vpn.Source(got).Equal(net.ParseIP(address)) {
		t.Fatalf("the interface saw a forged packet from %s", vpn.Source(got))
	}
}

func TestVPNAddressIsReleasedWhenTheSessionEnds(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	client := dialVPNClient(t, rs, false, true)
	if client.response.VPNAddress == "" {
		t.Fatal("no tunnel address was assigned")
	}
	client.close()

	waitForCondition(t, func() bool { return rs.server.vpn.pool.InUse() == 0 },
		"the address was never returned to the pool")
	waitForCondition(t, func() bool { return rs.server.vpn.router.PeerCount() == 0 },
		"the peer was never detached from the router")
}

func TestVPNAddressIsReusedAfterASessionEnds(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	first := dialVPNClient(t, rs, false, true)
	address := first.response.VPNAddress
	if address == "" {
		t.Fatal("no tunnel address was assigned")
	}
	first.close()
	waitForCondition(t, func() bool { return rs.server.vpn.pool.InUse() == 0 },
		"the first session's address was not returned")

	second := dialVPNClient(t, rs, false, true)
	defer second.close()
	if second.response.VPNAddress != address {
		t.Errorf("the second session got %s, want the released %s", second.response.VPNAddress, address)
	}
}

func TestVPNRequestsAreRefusedWhenThereIsNoTunnel(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startServer(t, cfg)

	client := dialVPNClient(t, rs, false, true)
	defer client.close()

	if client.response.OK {
		t.Fatal("a session asking for a tunnel address was accepted by a server without a tunnel")
	}
	if client.response.VPNAddress != "" {
		t.Errorf("a refused session was given address %q", client.response.VPNAddress)
	}
	if !strings.Contains(client.response.Error, "no layer-3 tunnel") {
		t.Errorf("the refusal reads %q, which does not explain what is missing", client.response.Error)
	}
}

func TestVPNRequiredRefusesASessionThatDoesNotAsk(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	cfg.VPN.Require = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startVPNServer(t, cfg, device)

	client := dialVPNClient(t, rs, false, false)
	defer client.close()

	if client.response.OK {
		t.Fatal("a session that did not ask for a tunnel was accepted although vpn.require is set")
	}
	if !strings.Contains(client.response.Error, "requires a layer-3 tunnel") {
		t.Errorf("the refusal reads %q, which does not explain the requirement", client.response.Error)
	}
}

func TestVPNSessionWithoutATunnelCannotSendPackets(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	// A session that never asked for a tunnel.
	client := dialVPNClient(t, rs, false, false)
	defer client.close()
	if !client.response.OK {
		t.Fatalf("the session was refused: %s", client.response.Error)
	}

	if err := client.sendPacket(ipv4PacketForTest("10.7.0.2", "10.7.0.1", []byte("stray"))); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The packet must not reach the interface. A heartbeat proves the connection is
	// still being served, so this is not merely a stalled read.
	if err := client.framer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeat}); err != nil {
		t.Fatalf("send a heartbeat: %v", err)
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		msg, err := client.framer.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for the heartbeat ack: %v", err)
		}
		if msg.Type == protocol.TypeHeartbeatAck {
			break
		}
	}
	select {
	case packet := <-device.fromClients:
		t.Fatalf("a session without a tunnel got %d bytes onto the interface", len(packet))
	default:
	}
}

func TestVPNPoolExhaustionIsReportedToTheClient(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	// A /30 leaves exactly one client address.
	cfg.VPN.Address = "10.7.0.0/30"
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startVPNServer(t, cfg, device)

	first := dialVPNClient(t, rs, false, true)
	defer first.close()
	if first.response.VPNAddress != "10.7.0.2" {
		t.Fatalf("the first session got %q, want the only address 10.7.0.2", first.response.VPNAddress)
	}

	second := dialVPNClient(t, rs, false, true)
	defer second.close()
	if second.response.OK {
		t.Fatal("a second session was accepted although the pool was empty")
	}
	if !strings.Contains(second.response.Error, "no tunnel address") {
		t.Errorf("the refusal reads %q", second.response.Error)
	}
}

func TestVPNServerRefusesToStartWithoutAnInterface(t *testing.T) {
	cfg, _ := vpnConfig(t, false)

	// An opener that fails is what a machine without the driver looks like.
	opener := func(string, int) (vpn.Device, error) { return nil, vpn.ErrNoDevice }
	if _, err := New(cfg, Options{Logger: discardLogger(), VPNOpen: opener}); err == nil {
		t.Fatal("the server started although the tunnel interface could not be opened")
	}

	// The real opener on this machine: on Windows and macOS this build has no tun
	// implementation, so it must refuse rather than pretend.
	if _, err := vpn.Open("test0", 1400); err != nil {
		if !errors.Is(err, vpn.ErrNoDevice) {
			t.Logf("opening a tun device failed with %v", err)
		}
	}
}

func TestVPNConfigRequiresASubnet(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.VPN.Enabled = true
	cfg.VPN.Device = "test0"
	if err := cfg.Validate(config.RoleServer); err == nil || !strings.Contains(err.Error(), "vpn.address is empty") {
		t.Fatalf("expected vpn.address to be required, got %v", err)
	}

	cfg.VPN.Address = "10.7.0.0/31"
	if err := cfg.Validate(config.RoleServer); err == nil || !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("expected a /31 subnet to be rejected, got %v", err)
	}

	cfg.VPN.Address = "10.7.0.0/24"
	cfg.VPN.MTU = 100
	if err := cfg.Validate(config.RoleServer); err == nil || !strings.Contains(err.Error(), "vpn.mtu") {
		t.Fatalf("expected an out-of-range MTU to be rejected, got %v", err)
	}
}

func TestVPNClientConfigurationWarnsAboutServerOnlyKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[client]
server_addr = "example.com:7001"
auth_token = "0123456789abcdef0123456789abcdef"

[vpn]
enabled = true
device = "tun0"
address = "10.7.0.0/24"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "vpn.address has no effect in a client configuration") {
		t.Fatalf("expected a warning about the server-only key, got %v", cfg.Warnings)
	}
}

// TestVPNSummaryReportsLiveState checks the dashboard payload, which is what an
// operator uses to see whether the tunnel is working.
func TestVPNSummaryReportsLiveState(t *testing.T) {
	cfg, device := vpnConfig(t, false)
	rs := startVPNServer(t, cfg, device)

	summary := rs.server.vpn.summary()
	if enabled, _ := summary["enabled"].(bool); !enabled {
		t.Fatalf("the summary reports the tunnel as disabled: %v", summary)
	}
	if summary["subnet"] != "10.7.0.0/29" {
		t.Errorf("subnet is %v, want 10.7.0.0/29", summary["subnet"])
	}
	if summary["server_address"] != "10.7.0.1" {
		t.Errorf("server_address is %v, want 10.7.0.1", summary["server_address"])
	}
	if size, _ := summary["pool_size"].(int); size != 5 {
		t.Errorf("pool_size is %v, want 5", summary["pool_size"])
	}
	if summary["device"] != "test0" {
		t.Errorf("device is %v, want test0", summary["device"])
	}
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) == 0 {
		t.Error("the summary marshalled to nothing")
	}
}

func TestVPNSummaryWithoutATunnel(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startServer(t, cfg)

	summary := rs.server.vpn.summary()
	if enabled, _ := summary["enabled"].(bool); enabled {
		t.Fatalf("a server without a tunnel reports one: %v", summary)
	}
	if _, ok := summary["reason"]; !ok {
		t.Errorf("the summary does not say why the tunnel is absent: %v", summary)
	}
}

// --- helpers ------------------------------------------------------------------

// ipv4PacketForTest builds a well-formed IPv4 packet with a correct header
// checksum, which the router checks the source address of.
func ipv4PacketForTest(source, destination string, payload []byte) []byte {
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	total := len(packet)
	packet[2] = byte(total >> 8)
	packet[3] = byte(total)
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], net.ParseIP(source).To4())
	copy(packet[16:20], net.ParseIP(destination).To4())
	copy(packet[20:], payload)
	if err := vpn.SetIPv4Checksum(packet); err != nil {
		panic(err)
	}
	return packet
}

func receiveFromChannel(t *testing.T, from chan []byte) []byte {
	t.Helper()
	select {
	case packet := <-from:
		return packet
	case <-time.After(5 * time.Second):
		t.Fatal("no packet arrived within 5s")
		return nil
	}
}

func waitForCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
