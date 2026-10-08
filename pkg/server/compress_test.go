package server

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestACompressedTunnelCarriesBytes walks a use_compression proxy end to end:
// the server echoes the setting in the DataRequest, the test agent wraps its
// side, and a compressible payload makes the round trip.
func TestACompressedTunnelCarriesBytes(t *testing.T) {
	backend := echoService(t)

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(backend)})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeTCP, LocalAddr: backend, RemotePort: port,
		UseCompression: true,
	})

	visitor, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))

	payload := strings.Repeat("compress-me-", 2000)
	if _, err := visitor.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != payload {
		t.Fatal("the compressed tunnel did not restore the payload")
	}
}
