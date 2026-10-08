package server

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// An accepted WebRTC visitor that never sends its offer used to hold this
// goroutine, the socket and the session's key material for as long as the
// process lived: the offer read had no deadline at all, while every other
// blocking read on the visitor path is bounded. The wait is generous — the
// client gathers ICE candidates for seconds, and its own ceiling is 30 s — but
// it ends, and the server closes the connection when it does.
func TestAnAcceptedWebRTCVisitorThatNeverOffersIsDropped(t *testing.T) {
	// The wait is a variable so this case does not sit out a minute of it.
	previous := webrtcOfferWait
	webrtcOfferWait = 200 * time.Millisecond
	t.Cleanup(func() { webrtcOfferWait = previous })

	echo := startEcho(t)
	cfg := testConfig(t, false)
	var logs lockedBuffer
	rs := startServerLoggingTo(t, cfg, &logs)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret",
	})

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
		Secret: "s3cret", Transport: config.TransportWebRTC,
	}); err != nil {
		t.Fatalf("send the visitor connect: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the server refused the visitor: %s", ack.Error)
	}

	// The offer never comes. The server has to give up on its own, which the
	// visitor sees as EOF rather than as its own read timing out.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil || n > 0 {
		t.Fatalf("the server sent %d byte(s) instead of closing the abandoned visitor connection", n)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("the abandoned visitor connection was still open after the offer wait: the offer read has no deadline")
	}
	if !strings.Contains(logs.String(), `webrtc visitor for proxy "private"`) {
		t.Fatalf("the server did not log the abandoned offer read; log: %q", logs.String())
	}
	if strings.Contains(logs.String(), "data channel established") {
		t.Fatalf("the server reported a data channel for a visitor that never offered: %q", logs.String())
	}
}
