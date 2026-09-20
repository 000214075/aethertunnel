package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- post-quantum key agreement -----------------------------------------------

// authenticateWithKEX runs the hybrid handshake and leaves the client switched to
// the agreed session key.
func (c *testClient) authenticateWithKEX(t *testing.T, token string) (protocol.AuthResponse, error) {
	t.Helper()

	var response protocol.AuthResponse
	public, state, err := crypto.HybridClientInit()
	if err != nil {
		t.Fatalf("hybrid init: %v", err)
	}
	c.kexState = state

	err = c.framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: token, ClientVersion: "pq-test", Protocol: protocol.ProtocolVersion, KEX: public,
	})
	if err != nil {
		return response, err
	}
	if err := c.framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		return response, err
	}
	if !response.OK {
		return response, nil
	}

	key, err := crypto.HybridClientFinish(c.kexState, response.KEX)
	if err != nil {
		t.Fatalf("hybrid finish: %v", err)
	}
	sessionCipher, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
	if err != nil {
		t.Fatalf("session cipher: %v", err)
	}
	c.framer = protocol.NewFramerWithOptions(c.conn, sessionCipher, protocol.FramerOptions{})
	c.cipher = sessionCipher
	c.sessionKey = key
	c.session = response.Session
	return response, nil
}

// streamCipherFor derives the key that protects one data connection.
func (c *testClient) streamCipherFor(t *testing.T, streamID string) *crypto.Cipher {
	t.Helper()
	if len(c.sessionKey) == 0 {
		return c.cipher
	}
	key, err := crypto.StreamKey(c.sessionKey, streamID)
	if err != nil {
		t.Fatalf("stream key: %v", err)
	}
	cipher, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
	if err != nil {
		t.Fatalf("stream cipher: %v", err)
	}
	return cipher
}

func postQuantumConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t, true)
	cfg.Encryption.PostQuantum = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestPostQuantumSessionAgreesAKey(t *testing.T) {
	echo := startEcho(t)
	cfg := postQuantumConfig(t)
	rs := startServer(t, cfg)

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	baseCipher, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, testToken, "test-salt")
	if err != nil {
		t.Fatalf("base cipher: %v", err)
	}
	client := &testClient{conn: conn, framer: protocol.NewFramer(conn, baseCipher, 0), cipher: baseCipher}

	response, err := client.authenticateWithKEX(t, testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !response.OK {
		t.Fatalf("the server refused the session: %s", response.Error)
	}
	if len(response.KEX) != crypto.HybridResponseSize {
		t.Fatalf("the server answered %d key bytes, want %d", len(response.KEX), crypto.HybridResponseSize)
	}
	if len(client.sessionKey) != crypto.SessionKeySize {
		t.Fatalf("the client derived a %d byte key, want %d", len(client.sessionKey), crypto.SessionKeySize)
	}

	// The session must now work on the agreed key: register a proxy and serve
	// one stream through it.
	publicPort := freePort(t)
	if err := client.framer.WriteJSON(protocol.TypeRegisterProxy, protocol.ProxySpec{
		Name: "pq-echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Drain the proxy list, then answer the first data request.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		msg, err := client.framer.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for the registration: %v", err)
		}
		if msg.Type == protocol.TypeProxyList {
			break
		}
		if msg.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			t.Fatalf("the server refused the proxy: %s", payload.Error)
		}
	}

	// The public connection has to come from a different socket, so dial the
	// published port rather than the control address.
	public, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the published port: %v", err)
	}
	defer public.Close()

	msg, err := client.framer.ReadFrame()
	if err != nil {
		t.Fatalf("waiting for a data request: %v", err)
	}
	if msg.Type != protocol.TypeDataRequest {
		t.Fatalf("got %s, want a data request", msg.Type)
	}
	var request protocol.DataRequest
	if err := json.Unmarshal(msg.Payload, &request); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The data connection handshake uses the configured cipher; the payload
	// uses the key derived for this stream.
	dataConn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial back: %v", err)
	}
	defer dataConn.Close()

	dataFramer := protocol.NewFramer(dataConn, baseCipher, 0)
	if err := dataFramer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session: client.session, Proxy: request.Proxy, StreamID: request.StreamID,
	}); err != nil {
		t.Fatalf("data-open: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := dataFramer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("the server refused the stream: %s", ack.Error)
	}

	streamCipher := client.streamCipherFor(t, request.StreamID)
	stream := crypto.NewStream(dataConn, streamCipher)
	local, err := net.DialTimeout("tcp", echo, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the echo service: %v", err)
	}

	// This client is the proxy's owner: it serves the data connection by copying
	// it to the local echo service. The visitor is the connection on the
	// published port.
	go flynet.Pipe(stream, local, 2*time.Second)

	payload := []byte("post-quantum payload")
	if _, err := public.Write(payload); err != nil {
		t.Fatalf("send through the tunnel: %v", err)
	}
	_ = public.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(public, got); err != nil {
		t.Fatalf("read from the tunnel: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
}

func TestPostQuantumServerRefusesAClientWithoutAKeyExchange(t *testing.T) {
	cfg := postQuantumConfig(t)
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, true)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("plain", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("a client that offered no key exchange was accepted")
	}
	if !strings.Contains(response.Error, "post-quantum") {
		t.Fatalf("unexpected refusal: %s", response.Error)
	}
}

func TestPostQuantumIsRejectedWhenEncryptionIsOff(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Encryption.PostQuantum = true
	if err := cfg.Validate(config.RoleServer); err == nil {
		t.Fatal("post_quantum without encryption.enabled was accepted")
	}
}

// --- identity -----------------------------------------------------------------

func TestIdentityIsRequiredWhenTheServerSaysSo(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("anonymous", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("a client with no identity was accepted")
	}
	if !response.IdentityRequired {
		t.Fatal("the refusal did not say that an identity is required")
	}
}

func TestIdentityIsAcceptedWhenItIsAllowed(t *testing.T) {
	identity, err := crypto.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	cfg.Identity.AllowedKeys = []string{identity.PublicKeyHex()}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	session, err := authenticateWithIdentity(t, rs.addr, identity, testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !session.OK {
		t.Fatalf("a valid identity was refused: %s", session.Error)
	}
}

func TestIdentityStopsAKeyThatIsNotAllowed(t *testing.T) {
	allowed, err := crypto.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	other, err := crypto.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	cfg.Identity.AllowedKeys = []string{allowed.PublicKeyHex()}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	session, err := authenticateWithIdentity(t, rs.addr, other, testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if session.OK {
		t.Fatal("an identity that is not in the allow list was accepted")
	}
}

func TestIdentityStopsAReplayedAssertion(t *testing.T) {
	identity, err := crypto.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	cfg.Identity.AllowedKeys = []string{identity.PublicKeyHex()}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	nonce, err := crypto.Nonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	now := time.Now().Unix()
	signature := identity.SignChallenge(nonce, now)

	first, err := authenticateWithAssertion(t, rs.addr, identity, nonce, now, signature, testToken)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if !first.OK {
		t.Fatalf("the first assertion was refused: %s", first.Error)
	}

	second, err := authenticateWithAssertion(t, rs.addr, identity, nonce, now, signature, testToken)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if second.OK {
		t.Fatal("a replayed assertion was accepted")
	}
	if !strings.Contains(second.Error, "already been used") {
		t.Fatalf("unexpected refusal: %s", second.Error)
	}
}

func authenticateWithIdentity(t *testing.T, serverAddr string, identity *crypto.Identity, token string) (protocol.AuthResponse, error) {
	t.Helper()
	nonce, err := crypto.Nonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	now := time.Now().Unix()
	return authenticateWithAssertion(t, serverAddr, identity, nonce, now, identity.SignChallenge(nonce, now), token)
}

func authenticateWithAssertion(t *testing.T, serverAddr string, identity *crypto.Identity, nonce []byte, stamp int64, signature []byte, token string) (protocol.AuthResponse, error) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return protocol.AuthResponse{}, err
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: token, ClientVersion: "identity-test", Protocol: protocol.ProtocolVersion,
		Identity: identity.PublicKey(), IdentityNonce: nonce, IdentityTime: stamp,
		IdentitySignature: signature,
	}); err != nil {
		return protocol.AuthResponse{}, err
	}

	var response protocol.AuthResponse
	err = framer.ReadJSON(protocol.TypeAuthResponse, &response)
	return response, err
}

// --- zero-knowledge proof of the proxy secret ---------------------------------

// visitorProve performs the nizk handshake and returns the connection once the
// server has accepted it.
func visitorProve(t *testing.T, serverAddr, proxy, secret string) (net.Conn, *protocol.Framer, error) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: proxy, Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		conn.Close()
		return nil, nil, err
	}

	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if len(challenge.Nonce) == 0 {
		conn.Close()
		t.Fatal("the server sent an empty challenge nonce")
	}

	proof, err := crypto.SchnorrProve([]byte(secret), protocol.VisitorProofContext(proxy, challenge.Nonce))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: proof}); err != nil {
		conn.Close()
		return nil, nil, err
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if !ack.OK {
		conn.Close()
		return nil, nil, &visitorRefused{reason: ack.Error}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, framer, nil
}

type visitorRefused struct{ reason string }

func (e *visitorRefused) Error() string { return e.reason }

func TestNIZKVisitorIsAcceptedWithTheRightSecret(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	})

	conn, _, err := visitorProve(t, rs.addr, "private", "s3cret")
	if err != nil {
		t.Fatalf("visitor handshake: %v", err)
	}
	defer conn.Close()

	payload := []byte("proved knowledge without sending the secret")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo returned %q, want %q", got, payload)
	}
}

func TestNIZKVisitorIsRefusedWithTheWrongSecret(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	})

	_, _, err := visitorProve(t, rs.addr, "private", "guessing")
	if err == nil {
		t.Fatal("a visitor that proved knowledge of the wrong secret was accepted")
	}
	if !strings.Contains(err.Error(), "proof") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestNIZKIgnoresASecretSentInTheClear(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo,
		SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	})

	// Sending the secret instead of a proof must not be enough: the whole point
	// of nizk is that the secret is never on the wire.
	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: "private", Secret: "s3cret", Type: protocol.ProxyTypeSTCP, AuthToken: testToken,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var challenge protocol.VisitorChallenge
	if err := framer.ReadJSON(protocol.TypeVisitorChallenge, &challenge); err != nil {
		t.Fatalf("expected a challenge, got: %v", err)
	}

	// Answer with the secret itself rather than a proof.
	if err := framer.WriteJSON(protocol.TypeVisitorProve, protocol.VisitorProve{Proof: []byte("s3cret")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.OK {
		t.Fatal("the server accepted the secret where a proof was required")
	}
}

// --- TLS transport ------------------------------------------------------------

func TestTLSTransportCarriesAControlSession(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t)

	cfg := testConfig(t, false)
	cfg.Transport.EnableTLS = true
	cfg.Transport.CertFile = certFile
	cfg.Transport.KeyFile = keyFile
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	// A plain TCP client must not be able to speak to a TLS control port.
	plain, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	framer := protocol.NewFramer(plain, nil, 0)
	_ = plain.SetDeadline(time.Now().Add(3 * time.Second))
	_ = framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "plain", Protocol: protocol.ProtocolVersion,
	})
	var refused protocol.AuthResponse
	if err := framer.ReadJSON(protocol.TypeAuthResponse, &refused); err == nil && refused.OK {
		t.Fatal("a plain-text client was accepted on a TLS control port")
	}
	_ = plain.Close()

	// A TLS client, trusting the certificate, must get a working session.
	echo := startEcho(t)
	conn, err := tls.Dial("tcp", rs.addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()

	secureClient := &testClient{conn: conn, framer: protocol.NewFramer(conn, nil, 0)}
	if _, err := secureClient.authenticate("tls-client", testToken); err != nil {
		t.Fatalf("authenticate over TLS: %v", err)
	}
	if secureClient.session == "" {
		t.Fatal("the TLS session did not get an id")
	}

	publicPort := freePort(t)
	if err := secureClient.framer.WriteJSON(protocol.TypeRegisterProxy, protocol.ProxySpec{
		Name: "tls-echo", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: publicPort,
	}); err != nil {
		t.Fatalf("register over TLS: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := secureClient.framer.ReadFrame()
	if err != nil {
		t.Fatalf("waiting for the registration: %v", err)
	}
	if msg.Type != protocol.TypeProxyList {
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(msg.Payload, &payload)
		t.Fatalf("registration over TLS failed: %s", payload.Error)
	}
}

func TestTLSIsRefusedWithoutACertificate(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Transport.EnableTLS = true
	if err := cfg.Validate(config.RoleServer); err == nil {
		t.Fatal("enable_tls without a certificate was accepted")
	}
	if _, err := cfg.ServerTLSConfig(); err == nil {
		t.Fatal("ServerTLSConfig built a configuration without a certificate")
	}
}

// writeTestCertificate creates a self-signed certificate for 127.0.0.1 and
// returns the paths to the certificate and the key.
func writeTestCertificate(t *testing.T) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "aethertunnel-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}
