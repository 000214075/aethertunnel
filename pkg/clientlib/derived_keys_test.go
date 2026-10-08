package clientlib

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- per-session keys on a plain session --------------------------------------

// A server without a post-quantum exchange now draws a fresh salt per session
// and answers with it inside the channel the static cipher protects; the client
// switches to the derived key right after the response. The fake server here
// does the same switch, so the frames that follow only decode when both ends
// derived the same key.
func TestAPlainSessionSwitchesToTheKeyTheServerDerived(t *testing.T) {
	const token = "session-salt-token-0123456789"
	static, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, token, "test-salt")
	if err != nil {
		t.Fatalf("static cipher: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	agreed := make(chan struct{})
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
			OK: true, Session: "salt-test", ServerVersion: "test",
			Protocol: protocol.ProtocolVersion, HeartbeatSecs: 30,
			Encryption: static.Algorithm(), SessionSalt: salt,
		}); err != nil {
			return
		}
		// From here everything decodes only under the derived key.
		sessionFramer := protocol.NewFramerWithOptions(conn, sessionCipher, protocol.FramerOptions{MaxPayload: protocol.DefaultMaxPayload})
		deadline := time.Now().Add(5 * time.Second)
		for {
			_ = conn.SetReadDeadline(deadline)
			msg, err := sessionFramer.ReadFrame()
			if err != nil {
				return
			}
			switch msg.Type {
			case protocol.TypeRegisterProxy:
				close(agreed)
				return
			case protocol.TypeHeartbeat:
				_ = sessionFramer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeatAck})
			}
		}
	}()

	cfg := &config.Config{}
	cfg.Client.ServerAddr = listener.Addr().String()
	cfg.Client.AuthToken = token
	cfg.Client.DialTimeoutSecs = 2
	cfg.Encryption.Enabled = true
	cfg.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
	cfg.Encryption.Salt = "test-salt"
	cfg.ApplyClientDefaults()

	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: cfg.Client.ServerAddr}
	c.cipher.Store(static)
	c.proxies = []config.ProxyConfig{{Name: "web", Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: 80}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-agreed
		cancel()
	}()
	// The session ends either through the cancellation above or with an error
	// when the fake server closes; what the test asserts is the frame the
	// server decoded under the derived key.
	_ = c.runSession(ctx)
	select {
	case <-agreed:
	default:
		t.Fatal("the server never decoded a registration under the derived session key")
	}
}

// --- bandwidth on the datagram paths ------------------------------------------

// The datagram tunnel dials its UDP socket after the data-open, outside
// dialLocalService, so a configured bandwidth used to be silently skipped:
// the relayed datagrams paid no client-side rate at all.
func TestADatagramProxyCarriesItsBandwidthIntoPreparation(t *testing.T) {
	c, _ := testClient(t, 30)

	// prepareStream dials a byte-stream proxy's local service, so it needs one
	// that is actually there.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer local.Close()
	go func() {
		conn, err := local.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()
	tcpPort := local.Addr().(*net.TCPAddr).Port

	c.proxies = []config.ProxyConfig{
		{Name: "dns", Type: config.ProxyTypeUDP, LocalIP: "127.0.0.1", LocalPort: 53, Bandwidth: "1MB"},
		{Name: "web", Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: tcpPort, Bandwidth: "1MB"},
	}

	prepared, err := c.prepareStream(protocol.DataRequest{Proxy: "dns"})
	if err != nil {
		t.Fatalf("prepare the datagram stream: %v", err)
	}
	if prepared.bandwidth != 1_000_000 {
		t.Fatalf("the datagram proxy prepared a rate of %d, want 1000000", prepared.bandwidth)
	}

	// The byte-stream path keeps getting its rate from dialLocalService, so the
	// prepared rate stays empty there.
	prepared, err = c.prepareStream(protocol.DataRequest{Proxy: "web"})
	if err != nil {
		t.Fatalf("prepare the stream: %v", err)
	}
	if prepared.bandwidth != 0 {
		t.Fatalf("the byte-stream proxy prepared a rate of %d, want 0", prepared.bandwidth)
	}

	// A rate the parser refuses must not turn into a limiter nobody can grant.
	c.proxies = []config.ProxyConfig{{Name: "bad", Type: config.ProxyTypeUDP, LocalIP: "127.0.0.1", LocalPort: 53, Bandwidth: "not-a-rate"}}
	prepared, err = c.prepareStream(protocol.DataRequest{Proxy: "bad"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.bandwidth != 0 {
		t.Fatalf("an unparseable rate prepared a rate of %d, want 0", prepared.bandwidth)
	}
}

func TestADatagramRelayPaysItsConfiguredRate(t *testing.T) {
	c, _ := testClient(t, 30)

	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2000)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := echo.WriteTo(buf[:n], from); err != nil {
				return
			}
		}
	}()

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	done := make(chan struct{})
	go func() {
		c.serveDatagrams("dns", serverEnd, protocol.NewFramer(serverEnd, nil, 0), echo.LocalAddr().String(), 2000)
		close(done)
	}()

	framer := protocol.NewFramer(clientEnd, nil, 0)
	const datagrams = 4
	start := time.Now()
	for i := 0; i < datagrams; i++ {
		if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: make([]byte, 1000)}); err != nil {
			t.Fatalf("write a datagram: %v", err)
		}
	}
	for i := 0; i < datagrams; i++ {
		msg, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("read an echo: %v", err)
		}
		if len(msg.Payload) != 1000 {
			t.Fatalf("echo %d carries %d bytes, want 1000", i, len(msg.Payload))
		}
	}
	elapsed := time.Since(start)

	// 4000 bytes out and 4000 back through one 2000 B/s limiter: the bucket
	// cannot hand out more than a second's worth at once, so a run that beats
	// this bound is a run the limit never reached.
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("%d relayed datagrams took %s; the configured rate is not being paid", datagrams, elapsed)
	}
	// The relay ends when its connection does; a pipe nobody closes would
	// leave it waiting forever.
	_ = serverEnd.Close()
	_ = clientEnd.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not end after its connection closed")
	}
}
