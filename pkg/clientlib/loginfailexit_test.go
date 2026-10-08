package clientlib

import (
	"bytes"
	"context"
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

// refusedAddr hands back a loopback address whose listener is already closed,
// so dialing it fails at once with "connection refused".
func refusedAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open a listener: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}
	return addr
}

func loginFailExitConfig(addr string, loginFailExit bool) *config.Config {
	cfg := &config.Config{}
	cfg.Client.ServerAddr = addr
	cfg.Client.ReconnectSeconds = 1
	cfg.Client.MaxReconnectSeconds = 1
	cfg.Client.LoginFailExit = loginFailExit
	return cfg
}

// With client.login_fail_exit set, a client whose very first login is refused
// stops instead of retrying forever, and Run reports why.
func TestLoginFailExitStopsTheClientOnARefusedFirstLogin(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a refused first login with login_fail_exit set")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run is still retrying after a refused first login with login_fail_exit set")
	}
}

// The default keeps retrying a refused login: a client without login_fail_exit
// behaves as before and never gives up on its own.
func TestWithoutLoginFailExitTheClientKeepsRetrying(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log.New(io.Discard, "", 0)) }()

	select {
	case err := <-done:
		t.Fatalf("Run stopped on its own after a refused login: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// login_fail_exit only guards the very first login: a client whose session was
// established once keeps reconnecting when the session drops.
func TestLoginFailExitKeepsReconnectingAnEstablishedSession(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: cfg.Client.ServerAddr}
	c.baseCtx = context.Background()
	c.sessionEstablished.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()

	// The first refused session comes back around for a reconnect instead of
	// stopping the client; the reconnect wait is jitter(1s), so nothing ends
	// the run inside this window except a wrongly taken give-up path.
	select {
	case <-done:
		t.Fatal("run stopped a client whose session was already established")
	case <-time.After(300 * time.Millisecond):
	}
}

// A refused first login is reported through the logger before Run returns.
func TestLoginFailExitNamesTheReasonWhenItStops(t *testing.T) {
	cfg := loginFailExitConfig(refusedAddr(t), true)
	buf := &bytes.Buffer{}
	err := Run(context.Background(), cfg, log.New(buf, "", 0))
	if err == nil {
		t.Fatal("Run returned nil for a refused first login with login_fail_exit set")
	}
	if want := "login failed and client.login_fail_exit is set"; !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Errorf("the log names the stop reason %q, got:\n%s", want, buf.String())
	}
}

// R2-2's ruling: a TypeError that refuses a registration is not a login
// failure. The session is up and the proxy simply is not published, so with
// client.login_fail_exit set the client keeps running through it and repeats
// what the server said — exiting here is what a client does when the login
// itself is refused, and this case is what stops such a reading coming back.
func TestLoginFailExitKeepsRunningThroughARefusedRegistration(t *testing.T) {
	const token = "register-refused-token-0123456"
	static, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, token, "test-salt")
	if err != nil {
		t.Fatalf("static cipher: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	refused := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		framer := protocol.NewFramerWithOptions(conn, static, protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload})
		var request protocol.AuthRequest
		if err := framer.ReadJSON(protocol.TypeAuthRequest, &request); err != nil {
			return
		}
		// The full handshake the fake server does: the response travels
		// under the static key, and everything after it under the key derived
		// from the session salt that response announces.
		salt, err := crypto.SessionSalt()
		if err != nil {
			return
		}
		key, err := crypto.SessionKey(token, salt)
		if err != nil {
			return
		}
		sessionCipher, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
		if err != nil {
			return
		}
		if err := framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: true, Session: "refused-registration", ServerVersion: "test",
			Protocol: protocol.ProtocolVersion, HeartbeatSecs: 30,
			Encryption: static.Algorithm(), SessionSalt: salt,
		}); err != nil {
			return
		}
		sessionFramer := protocol.NewFramerWithOptions(conn, sessionCipher, protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload})
		for {
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			msg, err := sessionFramer.ReadFrame()
			if err != nil {
				return
			}
			switch msg.Type {
			case protocol.TypeRegisterProxy:
				// The refusal an allow_ports or a policy check produces, sent
				// as an ordinary error frame on the live session.
				if err := sessionFramer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{
					Error: `proxy "web": remote port 59101 is outside allow_ports`,
				}); err != nil {
					return
				}
				select {
				case refused <- struct{}{}:
				default:
				}
			case protocol.TypeHeartbeat:
				_ = sessionFramer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeatAck})
			}
		}
	}()

	cfg := &config.Config{}
	cfg.Client.ServerAddr = listener.Addr().String()
	cfg.Client.AuthToken = token
	cfg.Client.LoginFailExit = true
	cfg.Client.DialTimeoutSecs = 2
	cfg.Encryption.Enabled = true
	cfg.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
	cfg.Encryption.Salt = "test-salt"
	cfg.Proxies = []config.ProxyConfig{{
		Name: "web", Type: config.ProxyTypeTCP,
		LocalIP: "127.0.0.1", LocalPort: 18080, RemotePort: 59101,
	}}
	cfg.ApplyClientDefaults()

	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log.New(logs, "", 0)) }()

	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never received the registration to refuse")
	}
	// Wait until the client has said the refusal out loud, so the quiet window
	// below measures a client that saw it, not one still reading the frame.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "server reported:") {
		if time.Now().After(deadline) {
			t.Fatalf("the client never repeated the server's refusal; the log says:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The refused registration leaves the client alive.
	select {
	case err := <-done:
		t.Fatalf("Run stopped on a refused registration: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after the context was cancelled, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
