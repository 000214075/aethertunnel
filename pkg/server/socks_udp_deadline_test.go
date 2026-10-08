package server

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A socks5 UDP association whose client keeps sending datagrams but never
// reads them must end at its idle timeout: the relay's frame write to that
// client parks on a full socket, and only a write deadline can end the park.
// Without the bound the association pins its relay socket, data connection and
// goroutines for as long as the client cares to keep its control connection
// open — one per attempt, on an unauthenticated port.
func TestAStalledSocksUDPAssociationEndsAtItsWriteDeadline(t *testing.T) {
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer relay.Close()

	sender, err := net.DialUDP("udp", nil, relay.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sender.Close()
	if _, err := sender.Write([]byte("one datagram")); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The client's data connection is a pipe nobody reads: a frame write to
	// it parks until a deadline ends it.
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := &dataConn{conn: clientSide, framer: protocol.NewFramer(clientSide, nil, 0)}

	// The watcher reads the visitor's control connection and never gets a
	// byte, so the association can only end through the relay itself.
	control, controlOther := net.Pipe()
	defer controlOther.Close()

	tunnel := &Tunnel{logger: log.New(io.Discard, "", 0), idleTimeout: 200 * time.Millisecond, metrics: &Metrics{}}
	done := make(chan struct{})
	go func() {
		tunnel.relaySocksUDP(relay, dc, nil, control)
		close(done)
	}()

	// Let the inbound datagram reach the parked write before the client
	// starts talking: its traffic keeps the read side fed, which is what
	// keeps the association alive without the write bound.
	time.Sleep(50 * time.Millisecond)
	framer := protocol.NewFramer(serverSide, nil, 0)
	go func() {
		for {
			time.Sleep(50 * time.Millisecond)
			if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: []byte("ping")}); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the association never ended: the parked frame write carries no deadline")
	}
}

// The datagram relay itself carries the same bound per write: a peer that
// keeps its side fed but never reads parks the opposite direction's frame
// write, and only that write's own deadline ends the park — the shared read
// deadline stays refreshed by the very traffic the stalled reader produces.
func TestARelayedDatagramWriteToAStalledReaderEndsAtItsDeadline(t *testing.T) {
	clientA, serverA := net.Pipe()
	defer serverA.Close()
	clientB, serverB := net.Pipe()
	defer serverB.Close()

	fa := protocol.NewFramer(clientA, nil, 0)
	fb := protocol.NewFramer(clientB, nil, 0)
	done := make(chan struct{})
	go func() {
		relayDatagrams(fa, fb, clientA, clientB, 200*time.Millisecond, nil)
		close(done)
	}()

	// The stalled reader (B) keeps sending datagrams of its own: its traffic
	// keeps both read deadlines refreshed forever, so only the reply
	// direction's own write deadline can end the write parked on B's full
	// pipe. A also keeps sending, and its side is drained, so the parked
	// direction is the only thing wrong with the association.
	go func() {
		feed := protocol.NewFramer(serverA, nil, 0)
		for {
			time.Sleep(50 * time.Millisecond)
			if err := feed.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: []byte("ping")}); err != nil {
				return
			}
			if _, err := protocol.NewFramer(serverA, nil, 0).ReadFrame(); err != nil {
				return
			}
		}
	}()
	go func() {
		feed := protocol.NewFramer(serverB, nil, 0)
		for {
			time.Sleep(50 * time.Millisecond)
			if err := feed.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: []byte("pong")}); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never ended: the parked frame write carries no deadline")
	}
}
