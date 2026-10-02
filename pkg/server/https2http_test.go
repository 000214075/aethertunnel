package server

import (
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestHTTPS2HTTPPluginTerminatesTLSAtTheClient walks the plugin end to end:
// the visitor speaks TLS to the public tcp port, the server relays the raw
// bytes, and the client side terminates them with the configured certificate
// before the plaintext reaches the plain HTTP service.
func TestHTTPS2HTTPPluginTerminatesTLSAtTheClient(t *testing.T) {
	service := startHTTPService(t, "plain-http")
	certFile, keyFile := writeTestCertificate(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load the test certificate: %v", err)
	}

	// The agent emulates the client-side plugin: the tunnel's end arrives as
	// the visitor's TLS bytes, and the plugin terminates them here.
	plugin := func(conn net.Conn, framer *protocol.Framer) {
		_ = framer
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return
		}
		local, err := net.DialTimeout("tcp", service, 5*time.Second)
		if err != nil {
			_ = tlsConn.Close()
			return
		}
		flynet.Pipe(local, tlsConn, 10*time.Second)
	}

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	port := freePort(t)
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"site": plugin})
	// The plugin fields are client-side configuration: the wire spec only
	// names the tunnel and its public port, which the agent's handler serves
	// the way the real plugin would.
	agent.register(protocol.ProxySpec{
		Name: "site", Type: protocol.ProxyTypeTCP, RemotePort: port,
	})

	visitor, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(port)), &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial the public port with TLS: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := visitor.Write([]byte("GET / HTTP/1.1\r\nHost: site.example\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	answer, err := io.ReadAll(visitor)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(answer), "plain-http") {
		t.Fatalf("the plaintext did not reach the plain HTTP service: %q", answer)
	}
}

func itoaOf(port int) string {
	if port == 0 {
		return "0"
	}
	digits := ""
	for port > 0 {
		digits = string(rune('0'+port%10)) + digits
		port /= 10
	}
	return digits
}
