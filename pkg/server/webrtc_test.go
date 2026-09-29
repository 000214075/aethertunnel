package server

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/webrtcvisitor"
)

// A webrtc-transport visitor signs in on the control connection like any other
// visitor, then moves its data onto a DataChannel: the signaling is the control
// connection's offer and answer frames, and the bytes prove the channel works
// end to end.
func TestWebRTCVisitorCarriesBytesThroughADataChannel(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

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
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

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
		t.Fatalf("the server refused the webrtc visitor: %s", ack.Error)
	}

	offerSDP, accept, cleanup, err := webrtcvisitor.VisitorOffer()
	if err != nil {
		t.Fatalf("build the offer: %v", err)
	}
	defer cleanup()

	if err := framer.WriteJSON(protocol.TypeVisitorWebRTCOffer, protocol.VisitorWebRTCOffer{
		Proxy: "private", SDP: offerSDP,
	}); err != nil {
		t.Fatalf("send the offer: %v", err)
	}
	var answer protocol.VisitorWebRTCAnswer
	if err := framer.ReadJSON(protocol.TypeVisitorWebRTCAnswer, &answer); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	dataPath, err := accept(answer.SDP)
	if err != nil {
		t.Fatalf("the data channel never opened: %v", err)
	}
	defer dataPath.Close()

	payload := []byte("bytes that traveled over a data channel")
	if _, err := dataPath.Write(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = dataPath.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(dataPath, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the echo returned %q, want %q", got, payload)
	}
}

// A transport the build does not know is refused at registration time.
func TestAnUnknownVisitorTransportIsRefused(t *testing.T) {
	cfg := &config.Config{}
	cfg.Visitors = append(cfg.Visitors, config.VisitorConfig{
		Name: "private", Type: "stcp", ServerName: "private",
		SecretKey: "s3cret", Transport: "carrier-pigeon",
	})
	if err := cfg.Validate("client"); err == nil {
		t.Fatal("an unknown transport was accepted")
	}
}
