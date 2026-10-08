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

// The server sends its post-quantum key-agreement response in one frame only, so
// the acknowledgement that answers a failed punch carries none. The cipher the
// first exchange agreed has to be carried into the second read: re-deriving it from
// nothing would wrap the relayed stream in the configured cipher while the server
// uses the agreed key, and every byte of the fallback would be garbage.
func TestTheVisitorCipherSurvivesTheFallbackAck(t *testing.T) {
	configured, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "configured-passphrase", "test-salt")
	if err != nil {
		t.Fatalf("configured cipher: %v", err)
	}
	agreed, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "agreed-passphrase", "test-salt")
	if err != nil {
		t.Fatalf("agreed cipher: %v", err)
	}
	if agreed == configured {
		t.Fatal("the two ciphers are the same object, so the test cannot tell them apart")
	}

	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	// The dial timeout is the read deadline awaitVisitorReady arms, and a zero one
	// is already expired; the configuration applies a default in production.
	cfg := &config.Config{}
	cfg.Client.DialTimeoutSecs = 5
	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	c.cipher.Store(configured)
	c.baseCtx = context.Background()
	framer := protocol.NewFramer(clientSide, agreed, 0)

	go func() {
		peer := protocol.NewFramer(serverSide, agreed, 0)
		_ = peer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true})
	}()

	offer, ready, err := c.awaitVisitorReady(clientSide, framer, c.baseCtx, config.VisitorConfig{Name: "ssh", ServerName: "ssh"}, nil, "", agreed)
	if err != nil {
		t.Fatalf("awaitVisitorReady: %v", err)
	}
	if offer != nil {
		t.Fatalf("the ack produced a punch offer: %+v", offer)
	}
	if ready.cipher != agreed {
		t.Fatal("the agreed cipher was replaced by the configured one")
	}

	// With nothing carried in, the first call still starts from the configured
	// cipher: that is what the connection is protected by before the agreement.
	clientSide2, serverSide2 := net.Pipe()
	defer clientSide2.Close()
	defer serverSide2.Close()
	framer2 := protocol.NewFramer(clientSide2, configured, 0)
	go func() {
		peer := protocol.NewFramer(serverSide2, configured, 0)
		_ = peer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true})
	}()
	_, first, err := c.awaitVisitorReady(clientSide2, framer2, c.baseCtx, config.VisitorConfig{Name: "ssh", ServerName: "ssh"}, nil, "", nil)
	if err != nil {
		t.Fatalf("first awaitVisitorReady: %v", err)
	}
	if first.cipher != configured {
		t.Fatal("the first call does not start from the configured cipher")
	}
}
