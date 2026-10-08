package clientlib

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A reload that adopts a new auth_token must also rebuild the session cipher
// when encryption keys are derived from the token ([encryption] enabled with
// no passphrase of its own): the server restarted on the rotated token derives
// its key from the new one, so reconnecting with the old key dies unread —
// exactly what adopting the token live is meant to avoid.
func TestAReloadedTokenRebuildsATokenDerivedCipher(t *testing.T) {
	const (
		algorithm = crypto.AlgorithmXChaCha20Poly1305
		salt      = "test-salt"
		tokenA    = "token-a-0123456789abcdef"
		tokenB    = "token-b-0123456789abcdef"
	)
	clientCfg := &config.Config{}
	clientCfg.Client.AuthToken = tokenA
	clientCfg.Encryption.Enabled = true
	clientCfg.Encryption.Algorithm = algorithm
	clientCfg.Encryption.Salt = salt

	c := &client{cfg: clientCfg, logger: log.New(io.Discard, "", 0)}
	built, err := crypto.NewCipher(algorithm, tokenA, salt)
	if err != nil {
		t.Fatalf("build the old cipher: %v", err)
	}
	c.cipher.Store(built)

	// Sanity: before the reload the client's cipher is the old token's key.
	sealed, err := c.cipher.Load().Seal([]byte("probe"))
	if err != nil {
		t.Fatalf("seal with the old cipher: %v", err)
	}
	if _, err := built.Open(sealed); err != nil {
		t.Fatalf("the client's cipher does not match the old token's key: %v", err)
	}

	newCfg := &config.Config{}
	newCfg.Client.AuthToken = tokenB
	newCfg.Encryption.Enabled = true
	newCfg.Encryption.Algorithm = algorithm
	newCfg.Encryption.Salt = salt
	c.applyReload(newCfg)

	rebuilt := c.cipher.Load()
	sealed, err = rebuilt.Seal([]byte("probe"))
	if err != nil {
		t.Fatalf("seal with the rebuilt cipher: %v", err)
	}
	if _, err := built.Open(sealed); err == nil {
		t.Fatal("after the reload the client still encrypts with the old token's key")
	}
	fresh, err := crypto.NewCipher(algorithm, tokenB, salt)
	if err != nil {
		t.Fatalf("build the new cipher: %v", err)
	}
	if _, err := fresh.Open(sealed); err != nil {
		t.Fatalf("the rebuilt cipher does not match the new token's key: %v", err)
	}
}

// A reload that adopts a new auth_token leaves the cipher alone when the
// configuration carries its own passphrase: the key does not depend on the
// token there.
func TestAReloadedTokenKeepsAPassphraseDerivedCipher(t *testing.T) {
	const (
		algorithm  = crypto.AlgorithmXChaCha20Poly1305
		passphrase = "a passphrase that is not the token"
		salt       = "test-salt"
		tokenA     = "token-a-0123456789abcdef"
		tokenB     = "token-b-0123456789abcdef"
	)
	cfg := &config.Config{}
	cfg.Client.AuthToken = tokenA
	cfg.Encryption.Enabled = true
	cfg.Encryption.Algorithm = algorithm
	cfg.Encryption.Passphrase = passphrase
	cfg.Encryption.Salt = salt

	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	built, err := crypto.NewCipher(algorithm, passphrase, salt)
	if err != nil {
		t.Fatalf("build the cipher: %v", err)
	}
	c.cipher.Store(built)

	newCfg := &config.Config{}
	newCfg.Client.AuthToken = tokenB
	newCfg.Encryption = cfg.Encryption
	c.applyReload(newCfg)

	sealed, err := c.cipher.Load().Seal([]byte("probe"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := built.Open(sealed); err != nil {
		t.Fatal("the reload replaced a cipher that never depended on the token")
	}
}

// The token fetch rides its own HTTP budget and can outlast the visitor dial
// timeout; the visitor-connect write must be judged from after the fetch, not
// from dial time, or a slow identity provider fails every visitor with an i/o
// timeout while the server is still waiting for the frame.
func TestTheVisitorConnectWriteOutlivesASlowTokenFetch(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"test-token","expires_in":3600}`))
	}))
	defer idp.Close()

	// The visitor endpoint reads the visitor-connect frame and checks that
	// the OIDC access token the IdP issued is what the client presented —
	// the server holds a visitor to the same credentials as the control
	// connection. dialVisitor never reads here, so the exchange ends with
	// the write.
	sawToken := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		frame, err := protocol.NewFramer(conn, nil, 0).ReadFrame()
		if err != nil {
			return
		}
		var request protocol.VisitorConnect
		if json.Unmarshal(frame.Payload, &request) != nil {
			return
		}
		sawToken <- request.AuthToken
	}()

	cfg := &config.Config{}
	cfg.Client.DialTimeoutSecs = 1
	cfg.OIDC = &config.OIDCConfig{
		TokenEndpointURL: idp.URL,
		ClientID:         "test-token",
		ClientSecret:     "test-secret",
	}
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0), target: ln.Addr().String()}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, _, _, err := c.dialVisitor(ctx, config.VisitorConfig{ServerName: "slow-idp"})
	if err != nil {
		t.Fatalf("dialVisitor with a token fetch slower than the dial timeout: %v", err)
	}
	_ = conn.Close()
	select {
	case token := <-sawToken:
		if token != "test-token" {
			t.Fatalf("the visitor presented %q, want the access token the IdP issued", token)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the visitor-connect frame never reached the endpoint")
	}
}
