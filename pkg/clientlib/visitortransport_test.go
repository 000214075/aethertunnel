package clientlib

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The server picks the data path from the visitor-connect frame, so the visitor's
// transport has to be in that frame. It was not: a visitor configured for a WebRTC
// data channel was relayed like any other, and the offer it sent next came back
// through the relay as though it were the private service's own bytes.
func TestTheVisitorConnectCarriesTheConfiguredTransport(t *testing.T) {
	cases := []struct {
		transport string
		name      string
	}{
		{config.TransportWebRTC, "webrtc"},
		{"", "the default relay"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			seen := make(chan protocol.VisitorConnect, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				framer := protocol.NewFramerWithOptions(conn, nil, protocol.FramerOptions{})
				msg, err := framer.ReadFrame()
				if err != nil {
					return
				}
				var request protocol.VisitorConnect
				if err := json.Unmarshal(msg.Payload, &request); err != nil {
					return
				}
				seen <- request
			}()

			c := &client{
				cfg: &config.Config{Client: config.ClientConfig{DialTimeoutSecs: 5}},
				// The frame is what this test is about, so the logger's output is
				// not collected.
				logger: log.New(io.Discard, "", 0),
				target: listener.Addr().String(),
			}
			conn, _, _, _, err := c.dialVisitor(context.Background(), config.VisitorConfig{
				Name: "channel", Type: "stcp", ServerName: "private",
				SecretKey: "s3cret", AuthMethod: config.AuthMethodSecret,
				Transport: tc.transport,
			})
			if err != nil {
				t.Fatalf("dialVisitor: %v", err)
			}
			defer conn.Close()

			select {
			case got := <-seen:
				if got.Transport != tc.transport {
					t.Errorf("the visitor-connect frame carries transport %q, want %q", got.Transport, tc.transport)
				}
				if got.Proxy != "private" {
					t.Errorf("the frame names proxy %q, want the visitor's server_name", got.Proxy)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the server never read a visitor-connect frame")
			}
		})
	}
}
