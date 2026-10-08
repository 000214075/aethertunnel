package server

import (
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// recordingConn is a control connection with nowhere to go: it records what a write
// deadline was set to and how many bytes were written, so a test can see that a
// control write was bounded and that a gated write was held back.
type recordingConn struct {
	mu        sync.Mutex
	deadlines []time.Time
	written   int
}

func (c *recordingConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *recordingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.written += len(b)
	c.mu.Unlock()
	return len(b), nil
}

func (c *recordingConn) Close() error                    { return nil }
func (c *recordingConn) LocalAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *recordingConn) RemoteAddr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *recordingConn) SetDeadline(time.Time) error     { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error { return nil }
func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mu.Unlock()
	return nil
}

func (c *recordingConn) recorded() ([]time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...), c.written
}

// newRecordingSession builds a session over a recording connection, which the
// session loop's control writes can be pointed at without a socket.
func newRecordingSession(t *testing.T, srv *Server) (*Session, *recordingConn) {
	t.Helper()
	conn := &recordingConn{}
	session := newSession(srv, conn, protocol.NewFramer(conn, nil, 0), &protocol.AuthRequest{
		ClientVersion: "test-client", Protocol: protocol.ProtocolVersion, ClientID: "test-client",
	}, false, 0)
	return session, conn
}

func newRecordingServer(t *testing.T, logs io.Writer) *Server {
	t.Helper()
	srv, err := New(testConfig(t, false), Options{Version: "test", Logger: log.New(logs, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.auditor.Close() })
	return srv
}

// The heartbeat ack bounded its write; the proxy list, the withdraw ack and the
// error replies did not, so a client that registered enough proxies to fill the
// kernel's send buffer and then stopped reading parked the session loop inside the
// write — and the read deadline armed by the loop could never fire, because the
// loop never returned to ReadFrame.
func TestEveryControlWriteFromTheSessionLoopIsBounded(t *testing.T) {
	srv := newRecordingServer(t, io.Discard)
	session, conn := newRecordingSession(t, srv)

	deadline := 30 * time.Second
	srv.sendProxyList(session, deadline)
	srv.replyError(session, "boom", deadline)

	deadlines, _ := conn.recorded()
	if len(deadlines) < 4 {
		t.Fatalf("two control writes armed %d write deadline(s), want a set and a clear each", len(deadlines))
	}
	if deadlines[0].IsZero() {
		t.Fatal("the first control write did not arm a write deadline")
	}
	if last := deadlines[len(deadlines)-1]; !last.IsZero() {
		t.Fatal("a control write left the write deadline armed after it returned")
	}
}

// The session loop used to write one warning per unhandled frame with no throttle,
// while the TypeError branch right beside it throttled for exactly this reason: an
// authenticated client can repeat an unhandled type at line rate and write the log
// to disk.
func TestUnhandledControlFramesAreLoggedAtMostOncePerInterval(t *testing.T) {
	var logs lockedBuffer
	srv := newRecordingServer(t, &logs)

	conn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})
	session := newSession(srv, conn, protocol.NewFramer(conn, nil, 0), &protocol.AuthRequest{
		ClientVersion: "test-client", Protocol: protocol.ProtocolVersion, ClientID: "test-client",
	}, false, 0)

	go func() {
		framer := protocol.NewFramer(peer, nil, 0)
		for i := 0; i < 200; i++ {
			if err := framer.WriteFrame(&protocol.Message{Type: protocol.MessageType(0x7e)}); err != nil {
				return
			}
		}
		_ = peer.Close()
	}()

	srv.sessionLoop(session)

	if got := strings.Count(logs.String(), "unhandled control frame"); got != 1 {
		t.Fatalf("200 unhandled frames produced %d log line(s), want one (throttled)", got)
	}
}

// attachVPN attaches the session's tunnel transport before the auth response is
// written and before the framer switches, and the router's peer starts serving at
// once. A packet for the new address can therefore be ready in that window, where
// writing it would either precede the AuthResponse or frame it under the
// superseded cipher. The transport's outbound path stays shut until the session is
// ready.
func TestATunnelPacketIsDroppedUntilTheSessionIsReady(t *testing.T) {
	srv := newRecordingServer(t, io.Discard)
	session, conn := newRecordingSession(t, srv)

	if err := session.writeVPNPacket([]byte("early")); err != nil {
		t.Fatalf("writeVPNPacket before ready: %v", err)
	}
	if _, written := conn.recorded(); written != 0 {
		t.Fatal("a tunnel packet was written before the session was ready")
	}

	session.markVPNReady()
	if err := session.writeVPNPacket([]byte("late")); err != nil {
		t.Fatalf("writeVPNPacket after ready: %v", err)
	}
	if _, written := conn.recorded(); written == 0 {
		t.Fatal("a tunnel packet was dropped after the session became ready")
	}
}

// One refusalLog is shared by every connection goroutine that hits an unknown
// name, so the lazy clock initialisation has to happen under the lock: outside it,
// two concurrent refusals on a fresh refusalLog read nil and write the function
// pointer at the same time. Meaningful under -race.
func TestTheRefusalLogInitialisesItsClockUnderItsLock(t *testing.T) {
	var r refusalLog
	logger := log.New(io.Discard, "", 0)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.logf(logger, "refused an unknown name")
		}()
	}
	wg.Wait()

	if r.now == nil {
		t.Fatal("the refusal log never got a clock")
	}
}
