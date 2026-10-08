package clientlib

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// staticFramerOptions is what both ends build their framers with when the test
// leaves [obfuscation] out: the frame ceiling, no padding, no jitter.
var staticFramerOptions = protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload}

// The server seals every connection's first frame with the cipher it was built
// with, so a data connection of a session that is already up has to carry the
// cipher that session authenticated under. A reload that adopted a rotated
// auth_token rebuilds the client's key; if a data connection started afterwards
// read that rebuilt key, every stream of the live session would be refused for
// as long as the session lasts — until the reconnect the reload's own log line
// claims it waits for.
func TestDataConnectionsOfALiveSessionKeepTheCipherItAuthenticatedWith(t *testing.T) {
	const (
		algorithm = crypto.AlgorithmXChaCha20Poly1305
		salt      = "test-salt"
		tokenA    = "token-a-0123456789abcdef"
		tokenB    = "token-b-0123456789abcdef"
	)
	oldCipher, err := crypto.NewCipher(algorithm, tokenA, salt)
	if err != nil {
		t.Fatalf("cipher for the first token: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// The control connection is the first one to arrive; everything after it is a
	// data connection, which this side reads with the cipher the session
	// authenticated under — exactly what a server built on the old token does.
	control := make(chan *protocol.Framer, 1)
	dataOpens := make(chan protocol.DataOpen, 1)
	dataErrors := make(chan error, 1)

	go func() {
		first := true
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if first {
				first = false
				go func() {
					framer := protocol.NewFramerWithOptions(conn, oldCipher, staticFramerOptions)
					msg, err := framer.ReadFrame()
					if err != nil {
						dataErrors <- err
						_ = conn.Close()
						return
					}
					if msg.Type != protocol.TypeAuthRequest {
						dataErrors <- err
						_ = conn.Close()
						return
					}
					var request protocol.AuthRequest
					if err := json.Unmarshal(msg.Payload, &request); err != nil {
						dataErrors <- err
						_ = conn.Close()
						return
					}
					if request.Token != tokenA {
						dataErrors <- err
						_ = conn.Close()
						return
					}
					if err := framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
						OK: true, Session: "test-session", ServerVersion: "test",
						Protocol: protocol.ProtocolVersion, HeartbeatSecs: 30,
						Encryption: algorithm,
					}); err != nil {
						_ = conn.Close()
						return
					}
					control <- framer
					// Hold the control connection open and answer the client's
					// heartbeats, so the session stays up for the whole test.
					for {
						if _, err := framer.ReadFrame(); err != nil {
							return
						}
					}
				}()
				continue
			}
			go func() {
				defer conn.Close()
				framer := protocol.NewFramerWithOptions(conn, oldCipher, staticFramerOptions)
				msg, err := framer.ReadFrame()
				if err != nil {
					dataErrors <- err
					return
				}
				var open protocol.DataOpen
				if err := json.Unmarshal(msg.Payload, &open); err != nil {
					dataErrors <- err
					return
				}
				dataOpens <- open
				_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{
					OK: false, Error: "nothing is waiting for that stream",
				})
			}()
		}
	}()

	cfg := &config.Config{}
	cfg.Client.ServerAddr = listener.Addr().String()
	cfg.Client.AuthToken = tokenA
	cfg.Client.DialTimeoutSecs = 5
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1
	cfg.Encryption.Enabled = true
	cfg.Encryption.Algorithm = algorithm
	cfg.Encryption.Salt = salt

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: cfg.Client.ServerAddr}
	c.cipher.Store(oldCipher)
	c.baseCtx = ctx
	c.serviceCtx, c.serviceCancel = context.WithCancel(ctx)
	c.proxies, c.visitors = selectByStart(cfg, c.logger)
	go c.run(ctx)

	var controlFramer *protocol.Framer
	select {
	case controlFramer = <-control:
	case err := <-dataErrors:
		t.Fatalf("the control connection could not be read with the cipher the client started with: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the client never authenticated")
	}

	// A reload that rotates the token: the cipher the client dials with from now
	// on belongs to the new token, and the fake server does not have it.
	reloaded := &config.Config{}
	reloaded.Client.AuthToken = tokenB
	reloaded.Encryption = cfg.Encryption
	c.applyReload(reloaded)

	// The server now names a stream the client cannot serve, which is the one path
	// that needs no local service: the client answers on a fresh data connection.
	if err := controlFramer.WriteJSON(protocol.TypeDataRequest, protocol.DataRequest{
		Proxy: "ghost", StreamID: "test-stream",
	}); err != nil {
		t.Fatalf("ask the client for a stream: %v", err)
	}

	select {
	case open := <-dataOpens:
		if open.Proxy != "ghost" || open.StreamID != "test-stream" {
			t.Fatalf("the data connection opened %+v, want the stream the server asked for", open)
		}
	case err := <-dataErrors:
		t.Fatalf("the data connection of a live session was sealed with the rebuilt key: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the client opened no data connection for the stream it was asked for")
	}
}

// The caller's context bounds the whole dial, TLS handshake included: a visitor
// whose fallback_timeout_ms expires has to be sent to the fallback now, and a
// shutdown has to stop waiting now. The layer underneath the handshake already
// honoured it; the handshake itself used to run on context.Background(), which
// put the two deadlines back to client.dial_timeout_seconds.
func TestTheTLSHandshakeIsBoundedByTheCallersContext(t *testing.T) {
	// A listener that accepts and then says nothing: the handshake can only end
	// when a deadline fires, so which deadline fires is the whole question.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	cfg := &config.Config{}
	cfg.Client.DialTimeoutSecs = 3
	c := &client{
		cfg:       cfg,
		logger:    log.New(io.Discard, "", 0),
		target:    listener.Addr().String(),
		tlsConfig: &tls.Config{InsecureSkipVerify: true},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	conn, err := c.dialServer(ctx)
	elapsed := time.Since(started)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("dialServer succeeded against a server that never answered the handshake")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("the TLS handshake ran on its own budget: it ended after %s, and the caller's context expired after 200ms", elapsed)
	}
}

// Only the parts of a reload that the running client cannot pick up are announced
// as restart-only. The auth token is adopted live, so a reload that changes
// nothing else must not tell the operator to restart — and the live half has to
// still say what it did.
func TestAReloadThatOnlyRotatesTheTokenDoesNotAskForARestart(t *testing.T) {
	buf := &bytes.Buffer{}
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()
	c.cfg.Client.AuthToken = "old-token-0123456789"

	reloaded := &config.Config{}
	reloaded.Client.AuthToken = "new-token-0123456789"
	c.applyReload(reloaded)

	if strings.Contains(buf.String(), "[client] changed") {
		t.Fatalf("a reload that only rotated the token asked for a restart it does not need: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "a new client.auth_token applies on the next reconnect") {
		t.Fatalf("the reload did not report the token it adopted: %q", buf.String())
	}
}
