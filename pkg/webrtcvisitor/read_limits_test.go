package webrtcvisitor

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// pairWebRTCConns brings up a real peer connection pair on this host — no ICE
// servers, host candidates only, the way the commitment tests do — and returns the
// visitor side's raw data channel (the sender) together with the Conn the
// server side wraps it in (the receiver), so a test can flood the one and read
// from the other.
func pairWebRTCConns(t *testing.T) (*webrtc.DataChannel, *Conn, func()) {
	t.Helper()

	visitor, err := newPeerConnection()
	if err != nil {
		t.Fatalf("visitor peer connection: %v", err)
	}
	channel, err := visitor.CreateDataChannel("aethertunnel", nil)
	if err != nil {
		t.Fatalf("create the visitor's data channel: %v", err)
	}

	offer, err := visitor.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create the visitor's offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(visitor)
	if err := visitor.SetLocalDescription(offer); err != nil {
		t.Fatalf("apply the visitor's offer: %v", err)
	}
	<-gathered

	server, err := newPeerConnection()
	if err != nil {
		t.Fatalf("server peer connection: %v", err)
	}
	p := &pending{pc: server, channel: make(chan *Conn, 1)}
	p.watch()
	if err := server.SetRemoteDescription(*visitor.LocalDescription()); err != nil {
		t.Fatalf("apply the visitor's offer on the server: %v", err)
	}
	answer, err := server.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("build the server's answer: %v", err)
	}
	gathered = webrtc.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(answer); err != nil {
		t.Fatalf("apply the server's answer: %v", err)
	}
	<-gathered
	if err := visitor.SetRemoteDescription(*server.LocalDescription()); err != nil {
		t.Fatalf("apply the server's answer on the visitor: %v", err)
	}

	var conn *Conn
	select {
	case conn = <-p.channel:
	case <-time.After(20 * time.Second):
		t.Fatal("the server never adopted a data channel")
	}

	cleanup := func() {
		_ = visitor.Close()
		_ = server.Close()
	}
	return channel, conn, cleanup
}

// When the peer streams past maxBuffered while nothing reads, the connection is
// failed with errReceiveOverflow rather than grown without bound. The receiver
// does not read during the flood; a write through the receiver's own Conn is
// what tells the test the overflow has fired (Write reports the failure the
// connection ended with), so the reads below start only once nothing more can
// land in the buffer and cannot drain the flood ahead of it.
func TestAReceiveBufferPushedPastItsLimitFailsTheRead(t *testing.T) {
	channel, conn, cleanup := pairWebRTCConns(t)
	defer cleanup()

	// 17 × 64KiB = 1,114,112: past maxBuffered (1<<20) by more than the single
	// 1KiB read below can ever have drained, so the overflow is the sender's
	// doing and not the reader's.
	const chunk = 64 << 10
	for i := 0; i < 17; i++ {
		payload := make([]byte, chunk)
		payload[0] = byte(i)
		if err := channel.Send(payload); err != nil {
			t.Fatalf("send chunk %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := conn.Write([]byte{0}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the receiver's connection never failed although the sender pushed past its receive buffer")
		}
		time.Sleep(10 * time.Millisecond)
	}

	results := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				results <- err
				return
			}
		}
	}()
	select {
	case err := <-results:
		if !errors.Is(err, errReceiveOverflow) {
			t.Fatalf("the read ended with %v, want errReceiveOverflow", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the read never reported the receive overflow")
	}
}

// A read deadline that has already passed ends the read at once: the Read loop
// checks the deadline before it waits for a message, the way a caller that set
// a deadline in the past asks to skip the wait entirely.
func TestAPastReadDeadlineEndsAReadAtOnce(t *testing.T) {
	_, conn, cleanup := pairWebRTCConns(t)
	defer cleanup()

	if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	start := time.Now()
	n, err := conn.Read(make([]byte, 16))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the read returned %v, want os.ErrDeadlineExceeded", err)
	}
	if n != 0 {
		t.Fatalf("the read returned %d bytes along the deadline error", n)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("the read took %s; a deadline already in the past must end it at once", elapsed)
	}
}

// When the peer closes its channel, the local Conn's Read ends with io.EOF.
// That is conn.go's design, and it differs from what this package's review
// ledger once said to assert: a remote that goes away without an error of its
// own (goneErr == nil, an ordinary channel close) reads as io.EOF, the same
// reading a TCP peer's FIN gets. net.ErrClosed is reserved for a Close this
// side called, and a peer close must not be reported as one.
func TestAPeerCloseEndsAReadWithEOF(t *testing.T) {
	channel, conn, cleanup := pairWebRTCConns(t)
	defer cleanup()

	results := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		results <- err
	}()

	if err := channel.Close(); err != nil {
		t.Fatalf("close the peer's data channel: %v", err)
	}
	select {
	case err := <-results:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("the read returned %v, want io.EOF for an ordinary peer close", err)
		}
		if errors.Is(err, net.ErrClosed) {
			t.Fatalf("a peer close is reported as net.ErrClosed, which belongs to a local Close only")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the read never ended after the peer closed its channel")
	}
}
