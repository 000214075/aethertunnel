package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestAUseEncryptionTunnelCarriesBytes walks a use_encryption proxy end to end
// on a session that has no cipher of its own. The round trip only completes if
// the server wrapped the stream with the per-proxy key it derived from the token
// and the proxy's name and the agent derived the same key from the same two
// inputs, so it checks the derivation as much as the payload.
func TestAUseEncryptionTunnelCarriesBytes(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, false)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"sealed": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "sealed", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
		UseEncryption: true,
	})

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := strings.Repeat("sealed-payload-", 500)
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != payload {
		t.Fatal("the encrypted tunnel did not restore the payload")
	}

	// The server asked for the layer, and it said so, which is what lets a
	// client that knows the field wrap its side at all.
	if request := agent.requestFor(t, "sealed"); !request.Encrypted {
		t.Fatal("the server did not echo use_encryption in its DataRequest")
	}
}

// A session that already agreed a cipher does not ask for the per-proxy layer:
// the session's own cipher covers the stream, and a second layer inside it would
// add nothing while giving the two ends one more way to disagree.
func TestUseEncryptionIsNotEchoedWhenTheSessionHasACipher(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, true)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, true, map[string]dataHandler{"sealed": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "sealed", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
		UseEncryption: true,
	})

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := "encrypted session, per-proxy flag set"
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != payload {
		t.Fatalf("the encrypted session did not carry the payload: %q", reply)
	}

	if request := agent.requestFor(t, "sealed"); request.Encrypted {
		t.Fatal("the server asked for the per-proxy layer although the session already has a cipher")
	}
}

// A proxy without use_encryption is untouched by any of this: the flag has to be
// opt-in, and a stream that was never asked for the layer must stay as it was.
func TestAnOrdinaryTunnelIsNotEncryptedPerProxy(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, false)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"plain": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "plain", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
	})

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := "in the clear, as before"
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != payload {
		t.Fatalf("the plain tunnel did not carry the payload: %q", reply)
	}

	if request := agent.requestFor(t, "plain"); request.Encrypted {
		t.Fatal("a proxy without use_encryption was asked for the per-proxy layer")
	}
}

// The terminating HTTP path builds its own view of the client's data connection,
// and it skipped the per-proxy cipher and the compression layer the DataRequest
// asked for — a tcp tunnel with use_encryption worked while an http one failed
// every request. Both layers are set here so the check covers the pair.
func TestAUseEncryptionCompressedHTTPProxyCarriesBytes(t *testing.T) {
	backend := startBodyEchoHTTPService(t)
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"sealed": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "sealed", Type: protocol.ProxyTypeHTTP, LocalAddr: backend,
		Domains:       []string{"sealed.example.com"},
		UseEncryption: true, UseCompression: true,
	})
	waitForListener(t, rs.server, "sealed")

	payload := strings.Repeat("terminating-payload-", 200)
	request, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.Server.HTTPPort), strings.NewReader(payload))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Host = "sealed.example.com"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post through the encrypted http proxy: %v", err)
	}
	got, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(got), fmt.Sprintf("read %d", len(payload))) {
		t.Fatalf("the backend did not receive the body: %q", got)
	}
}
