package webrtcvisitor

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// waitUntil polls cond until it holds or the timeout passes.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The visitor side accepts one data channel. A second one used to be wrapped as
// well and then dropped on the floor by the select in watch: the extra channel
// stayed open with nobody reading it, and, because every Conn registers a
// connection-state callback on the shared peer connection, the dropped Conn's
// registration replaced the adopted one's — a connection that failed no longer
// woke the relay's readers. Only the first channel is taken now, and the rest
// are closed.
func TestASecondDataChannelIsClosedInsteadOfBeingWrapped(t *testing.T) {
	visitor, err := newPeerConnection()
	if err != nil {
		t.Fatalf("visitor peer connection: %v", err)
	}
	t.Cleanup(func() { _ = visitor.Close() })

	first, err := visitor.CreateDataChannel("aethertunnel", nil)
	if err != nil {
		t.Fatalf("create the first data channel: %v", err)
	}
	extra, err := visitor.CreateDataChannel("extra", nil)
	if err != nil {
		t.Fatalf("create the second data channel: %v", err)
	}
	received := make(chan []byte, 4)
	first.OnMessage(func(m webrtc.DataChannelMessage) { received <- append([]byte(nil), m.Data...) })
	extra.OnMessage(func(m webrtc.DataChannelMessage) { received <- append([]byte(nil), m.Data...) })

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
	t.Cleanup(func() { _ = server.Close() })
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
		defer conn.Close()
	case <-time.After(20 * time.Second):
		t.Fatal("the server never adopted a data channel")
	}

	// One of the two channels was closed by the server and the other is the
	// one it took. Which one it takes is the order its peer connection accepts
	// them in, so the pair is counted rather than named.
	closed := func() int {
		n := 0
		for _, dc := range []*webrtc.DataChannel{first, extra} {
			if dc.ReadyState() == webrtc.DataChannelStateClosed {
				n++
			}
		}
		return n
	}
	waitUntil(t, 5*time.Second, "the server to close the data channel it did not take", func() bool {
		return closed() > 0
	})
	if n := closed(); n != 1 {
		t.Errorf("the server closed %d of the two data channels, want exactly 1", n)
	}

	// The adopted channel still carries bytes, so closing the extra one did
	// not take the real data path with it. Both channels feed the same read,
	// and the closed one cannot deliver, so this is the adopted one's bytes.
	payload := []byte("through the adopted channel")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through the adopted channel: %v", err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Errorf("the adopted channel delivered %q, want %q", got, payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the adopted channel never delivered the bytes")
	}
}
