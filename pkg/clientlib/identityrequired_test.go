package clientlib

import (
	"context"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// refusalServer answers the first authentication request with a refusal. With
// withFlag set it carries the IdentityRequired flag and a text that says
// nothing more, which is the shape a server with
// detailed_errors_to_client = false produces and the shape the client has to
// explain on its own.
func refusalServer(t *testing.T, withFlag bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// The client's configuration here has no [encryption], so both
				// sides speak plaintext with the same framing options.
				framer := protocol.NewFramerWithOptions(conn, nil, protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload})
				if _, err := framer.ReadFrame(); err != nil {
					return
				}
				reason := "invalid auth token"
				if withFlag {
					reason = "identity check failed"
				}
				_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
					OK:               false,
					ServerVersion:    "test",
					Protocol:         protocol.ProtocolVersion,
					Error:            reason,
					IdentityRequired: withFlag,
				})
			}()
		}
	}()
	return listener.Addr().String()
}

// A refusal that only carries the flag is still actionable: the client names
// the keys to set, because the server's text does not.
func TestTheClientExplainsARequiredIdentity(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = refusalServer(t, true)
	cfg.Client.AuthToken = "test-token"
	cfg.Client.LoginFailExit = true
	cfg.Client.DialTimeoutSecs = 5
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Run(ctx, cfg, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("Run returned nil for a refused login with login_fail_exit set")
	}
	if !strings.Contains(err.Error(), "identity check failed") {
		t.Fatalf("the refusal was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "[identity] enabled = true") {
		t.Fatalf("the client did not explain what to configure: %v", err)
	}
}

// The hint belongs to the flag: an ordinary refusal is reported as it arrives,
// with nothing appended to it.
func TestAnOrdinaryRefusalIsReportedVerbatim(t *testing.T) {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = refusalServer(t, false)
	cfg.Client.AuthToken = "test-token"
	cfg.Client.LoginFailExit = true
	cfg.Client.DialTimeoutSecs = 5
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Run(ctx, cfg, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("Run returned nil for a refused login with login_fail_exit set")
	}
	if strings.Contains(err.Error(), "[identity]") {
		t.Fatalf("the identity hint was added to a refusal without the flag: %v", err)
	}
}
