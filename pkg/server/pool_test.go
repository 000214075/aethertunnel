package server

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// park offers a data connection to the server the way the real client's pool
// does: a data-open marked as a pool offer, answered with the ack that says the
// connection is being kept.
func (c *testClient) park(t *testing.T, serverAddr string) (net.Conn, *protocol.Framer) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	framer := protocol.NewFramer(conn, c.cipher, 0)
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{Session: c.session, Pool: true}); err != nil {
		_ = conn.Close()
		t.Fatalf("send the pool offer: %v", err)
	}
	var ack protocol.DataOpenAck
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		_ = conn.Close()
		t.Fatalf("read the pool ack: %v", err)
	}
	if !ack.OK {
		_ = conn.Close()
		t.Fatalf("the server refused the parked connection: %s", ack.Error)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, framer
}

// serveParkedStreamOn does on a parked connection what the real client does: it
// waits for the request the server writes on it, answers with a data-open on the
// same connection, and relays the bytes to the local service.
func (c *testClient) serveParkedStreamOn(t *testing.T, conn net.Conn, framer *protocol.Framer, localAddr string, timeout time.Duration) error {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	msg, err := framer.ReadFrame()
	if err != nil {
		return err
	}
	if msg.Type != protocol.TypeDataRequest {
		return fmt.Errorf("expected a data request, got %s", msg.Type)
	}
	var request protocol.DataRequest
	if err := jsonUnmarshal(msg.Payload, &request); err != nil {
		return err
	}

	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: c.session, Proxy: request.Proxy, StreamID: request.StreamID,
	}); err != nil {
		return err
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		return err
	}
	if !ack.OK {
		return fmt.Errorf("the server refused the stream: %s", ack.Error)
	}
	_ = conn.SetDeadline(time.Time{})

	local, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer local.Close()

	var serverSide net.Conn = &cryptoStreamConn{Stream: crypto.NewStream(conn, c.streamCipher(request)), conn: conn}
	if request.Compressed {
		serverSide = flynet.CompressConn(serverSide)
	}
	flynet.Pipe(serverSide, local, 30*time.Second)
	return nil
}

// A stream is served on the connection the client parked in advance: the server
// does not dial, so the visitor's first byte leaves sooner.
func TestAStreamIsServedOnAConnectionParkedInAdvance(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	publicPort := freePort(t)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read the proxy list: %v", err)
	}

	parked, parkedFramer := client.park(t, rs.addr)
	defer parked.Close()

	served := make(chan error, 1)
	go func() { served <- client.serveParkedStreamOn(t, parked, parkedFramer, echoAddr, 10*time.Second) }()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := []byte("through a parked connection")
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the visitor read %q, want %q", got, payload)
	}

	// The point of the test: this stream went over the parked connection, not over
	// the control connection.
	if used := rs.server.metrics.poolUsed.Load(); used != 1 {
		t.Fatalf("pool_used is %d, want 1", used)
	}
	if missed := rs.server.metrics.poolMissed.Load(); missed != 0 {
		t.Fatalf("pool_missed is %d, want 0", missed)
	}
	visitor.Close()
	if err := <-served; err != nil {
		t.Fatalf("serving the parked stream: %v", err)
	}
}

// A parked connection the client has since dropped costs a round trip, not the
// stream: the server notices when it puts the connection to use and asks over the
// control connection, which is what it did before the pool existed.
func TestAStaleParkedConnectionFallsBackToTheControlConnection(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	publicPort := freePort(t)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read the proxy list: %v", err)
	}

	parked, _ := client.park(t, rs.addr)
	// The client goes away without telling the server, which is what a restart
	// looks like from here: the parked connection is closed, the server's list
	// still holds it.
	if err := parked.Close(); err != nil {
		t.Fatalf("close the parked connection: %v", err)
	}

	// The stream is served the old way, over the control connection.
	served := make(chan error, 1)
	go func() { served <- client.serveOneStream(rs.addr, echoAddr, 10*time.Second) }()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := []byte("after the client dropped its connection")
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the visitor read %q, want %q", got, payload)
	}
	if missed := rs.server.metrics.poolMissed.Load(); missed != 1 {
		t.Fatalf("pool_missed is %d, want 1", missed)
	}
	if used := rs.server.metrics.poolUsed.Load(); used != 0 {
		t.Fatalf("pool_used is %d, want 0: the stream was not served on the parked connection", used)
	}
	visitor.Close()
	if err := <-served; err != nil {
		t.Fatalf("serving the stream over the control connection: %v", err)
	}
}

// A parked connection that answers nothing at all — the shape a NAT gives when it
// forgets a mapping, with no FIN and no RST — is given up on inside the visitor's
// window, and the stream is served over the control connection instead.
func TestAParkedConnectionThatNeverAnswersFallsBack(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.UserConnTimeoutSeconds = 2
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	publicPort := freePort(t)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := client.register(protocol.ProxySpec{Name: "echo", Type: "tcp", LocalAddr: echoAddr, RemotePort: publicPort}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.framer.ReadFrame(); err != nil {
		t.Fatalf("read the proxy list: %v", err)
	}

	// Parked, and then never answered: nothing reads the request the server writes
	// on it, so the connection stays silent until the server gives up on it.
	parked, _ := client.park(t, rs.addr)
	defer parked.Close()

	served := make(chan error, 1)
	go func() { served <- client.serveOneStream(rs.addr, echoAddr, 10*time.Second) }()

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := []byte("served after the parked connection went quiet")
	if _, err := visitor.Write(payload); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the visitor read %q, want %q", got, payload)
	}
	if missed := rs.server.metrics.poolMissed.Load(); missed != 1 {
		t.Fatalf("pool_missed is %d, want 1", missed)
	}
	visitor.Close()
	if err := <-served; err != nil {
		t.Fatalf("serving the stream over the control connection: %v", err)
	}
}

// The session holds at most config.MaxPoolCount connections, oldest first, and a
// session that has ended holds none.
func TestParkedConnectionsAreBoundedAndTakenOldestFirst(t *testing.T) {
	selfConn, selfPeer := net.Pipe()
	defer selfConn.Close()
	defer selfPeer.Close()
	session := &Session{conn: selfConn, done: make(chan struct{}), tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}

	conns := make([]net.Conn, 0, config.MaxPoolCount+1)
	for i := 0; i < config.MaxPoolCount; i++ {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		if err := session.Park(server, protocol.NewFramer(server, nil, 0)); err != nil {
			t.Fatalf("Park %d: %v", i, err)
		}
		session.ConfirmParked(server)
		conns = append(conns, server)
	}
	extra, extraServer := net.Pipe()
	defer extra.Close()
	defer extraServer.Close()
	if err := session.Park(extraServer, protocol.NewFramer(extraServer, nil, 0)); err != errPoolFull {
		t.Fatalf("Park past the cap returned %v, want errPoolFull", err)
	}

	first, _, ok := session.TakeParked()
	if !ok {
		t.Fatal("TakeParked found nothing")
	}
	if first != conns[0] {
		t.Fatal("TakeParked did not return the connection that was parked first")
	}
	for session.parkedCount() > 0 {
		if _, _, ok := session.TakeParked(); !ok {
			t.Fatal("TakeParked stopped before the list was empty")
		}
	}
	if _, _, ok := session.TakeParked(); ok {
		t.Fatal("TakeParked returned a connection from an empty list")
	}

	// A closed session refuses to hold anything.
	closedConn, closedPeer := net.Pipe()
	defer closedConn.Close()
	defer closedPeer.Close()
	closed := &Session{conn: closedConn, done: make(chan struct{}), tunnels: map[string]*Tunnel{}, pending: map[string]*pendingStream{}}
	closed.Close("test")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := closed.Park(b, protocol.NewFramer(b, nil, 0)); err == nil {
		t.Fatal("a closed session parked a connection")
	}
}
