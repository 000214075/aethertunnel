package clientlib

import (
	"context"
	"io"
	"log"
	"net"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A server without a post-quantum agreement sends a salt in its first frame;
// the visitor derives the session key from the token it presented and that
// salt, and everything after the frame — the proof included — goes out under
// the derived key. A server older than the salt sends none, and the visitor
// keeps the configured cipher.
func TestAVisitorSwitchesToTheKeyDerivedFromTheChallengeSalt(t *testing.T) {
	configured, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "configured-passphrase", "test-salt")
	if err != nil {
		t.Fatalf("configured cipher: %v", err)
	}

	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	cfg := &config.Config{}
	cfg.Client.DialTimeoutSecs = 5
	cfg.Encryption.Algorithm = crypto.AlgorithmXChaCha20Poly1305
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	c.cipher.Store(configured)
	c.baseCtx = context.Background()
	framer := protocol.NewFramer(clientSide, configured, 0)

	const presentedToken = "the-token-this-visitor-presented"
	serverErr := make(chan error, 1)
	go func() {
		salt, err := crypto.SessionSalt()
		if err != nil {
			serverErr <- err
			return
		}
		static := protocol.NewFramer(serverSide, configured, 0)
		if err := static.WriteJSON(protocol.TypeVisitorChallenge, protocol.VisitorChallenge{
			Proxy: "ssh", Nonce: []byte("nonce"), SessionSalt: salt,
		}); err != nil {
			serverErr <- err
			return
		}
		// The proof that follows only makes sense under the key derived from
		// the salt, so reading it with the derived framer proves the client
		// switched before sending it.
		key, err := crypto.SessionKey(presentedToken, salt)
		if err != nil {
			serverErr <- err
			return
		}
		derived, err := crypto.NewCipherFromKey(crypto.AlgorithmXChaCha20Poly1305, key)
		if err != nil {
			serverErr <- err
			return
		}
		peer := protocol.NewFramer(serverSide, derived, 0)
		var prove protocol.VisitorProve
		if err := peer.ReadJSON(protocol.TypeVisitorProve, &prove); err != nil {
			serverErr <- err
			return
		}
		if err := peer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	offer, ready, err := c.awaitVisitorReady(clientSide, framer, c.baseCtx, config.VisitorConfig{
		Name: "ssh", ServerName: "ssh", SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	}, nil, presentedToken, nil)
	if err != nil {
		select {
		case serr := <-serverErr:
			t.Fatalf("awaitVisitorReady: %v (server side: %v)", err, serr)
		default:
			t.Fatalf("awaitVisitorReady: %v (server side still running)", err)
		}
	}
	if offer != nil {
		t.Fatalf("the ack produced a punch offer: %+v", offer)
	}
	if ready.cipher == configured {
		t.Fatal("the visitor kept the configured cipher after a salted challenge")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// The fallback-ack path is where the agreed cipher has to survive; a salt-less
// exchange must leave the configured cipher in place exactly as before.
func TestAVisitorKeepsTheConfiguredCipherWithoutASalt(t *testing.T) {
	configured, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "configured-passphrase", "test-salt")
	if err != nil {
		t.Fatalf("configured cipher: %v", err)
	}

	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	cfg := &config.Config{}
	cfg.Client.DialTimeoutSecs = 5
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	c.cipher.Store(configured)
	c.baseCtx = context.Background()
	framer := protocol.NewFramer(clientSide, configured, 0)

	go func() {
		peer := protocol.NewFramer(serverSide, configured, 0)
		_ = peer.WriteJSON(protocol.TypeVisitorChallenge, protocol.VisitorChallenge{
			Proxy: "ssh", Nonce: []byte("nonce"),
		})
		_ = peer.ReadJSON(protocol.TypeVisitorProve, &protocol.VisitorProve{})
		_ = peer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true})
	}()

	_, ready, err := c.awaitVisitorReady(clientSide, framer, c.baseCtx, config.VisitorConfig{
		Name: "ssh", ServerName: "ssh", SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	}, nil, "the-token-this-visitor-presented", nil)
	if err != nil {
		t.Fatalf("awaitVisitorReady: %v", err)
	}
	if ready.cipher != configured {
		t.Fatal("a challenge without a salt replaced the configured cipher")
	}
}
