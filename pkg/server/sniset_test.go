package server

import (
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// tlsOriginHandler plays the client side of a passthrough tunnel: the visitor's
// raw TLS bytes arrive here, this end terminates them with its own certificate
// and forwards the plaintext to the plain HTTP service. The server never sees
// the certificate.
func tlsOriginHandler(t *testing.T, cert tls.Certificate, service string) dataHandler {
	return func(conn net.Conn, framer *protocol.Framer) {
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
}

// TestSNIPassthroughRelaysUntouched walks the passthrough path end to end: the
// visitor's ClientHello names the domain, the passthrough listener routes by
// SNI, and the TLS session is relayed byte for byte — the handshake completes
// against the client-side certificate, which the server never terminated.
func TestSNIPassthroughRelaysUntouched(t *testing.T) {
	service := startHTTPService(t, "sni-origin")
	certFile, keyFile := writeTestCertificate(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load the test certificate: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Server.HTTPSPort = freePort(t)
	cfg.Server.HTTPSCertFile = certFile
	cfg.Server.HTTPSKeyFile = keyFile
	cfg.Server.HTTPSPassthroughPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"site": tlsOriginHandler(t, cert, service)})
	agent.register(protocol.ProxySpec{
		Name: "site", Type: protocol.ProxyTypeHTTPS, LocalAddr: service,
		Domains:        []string{"sni.example"},
		TLSPassthrough: true,
	})

	visitor, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(cfg.Server.HTTPSPassthroughPort)), &tls.Config{
		ServerName:         "sni.example",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial the passthrough port with TLS: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := visitor.Write([]byte("GET / HTTP/1.1\r\nHost: sni.example\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	answer, err := io.ReadAll(visitor)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(answer), "sni-origin") {
		t.Fatalf("the plaintext did not reach the origin behind the client: %q", answer)
	}
}

// TestSNIPassthroughRefusesUnknownNames checks the routing edges: an SNI nobody
// published fails the handshake, and an https proxy without tls_passthrough
// stays on the terminating listener — it is not reachable through the
// passthrough port.
func TestSNIPassthroughRefusesUnknownNames(t *testing.T) {
	service := startHTTPService(t, "sni-origin")
	certFile, keyFile := writeTestCertificate(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load the test certificate: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Server.HTTPSPort = freePort(t)
	cfg.Server.HTTPSCertFile = certFile
	cfg.Server.HTTPSKeyFile = keyFile
	cfg.Server.HTTPSPassthroughPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{
		"site": tlsOriginHandler(t, cert, service),
	})
	agent.register(protocol.ProxySpec{
		Name: "site", Type: protocol.ProxyTypeHTTPS, LocalAddr: service,
		Domains:        []string{"sni.example"},
		TLSPassthrough: true,
	})
	agent.register(protocol.ProxySpec{
		Name: "plain", Type: protocol.ProxyTypeHTTPS, LocalAddr: service,
		Domains: []string{"terminating.example"},
	})

	visitor, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(cfg.Server.HTTPSPassthroughPort)), &tls.Config{
		ServerName:         "nobody.example",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err == nil {
		_ = visitor.Close()
		t.Fatal("an unknown SNI completed a handshake on the passthrough port")
	}

	visitor, err = tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(cfg.Server.HTTPSPassthroughPort)), &tls.Config{
		ServerName:         "terminating.example",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err == nil {
		_ = visitor.Close()
		t.Fatal("an https proxy without tls_passthrough answered on the passthrough port")
	}

	// The terminating listener still owns the proxy without the flag: its
	// handshake completes against the SERVER's certificate.
	terminator, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(cfg.Server.HTTPSPort)), &tls.Config{
		ServerName:         "terminating.example",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("the terminating listener lost the plain https proxy: %v", err)
	}
	_ = terminator.Close()
}

// TestSNIPassthroughWildcardRoutesSubdomains mirrors the vhost router's
// wildcard rule on the passthrough listener: "*.example" owns its subdomains.
func TestSNIPassthroughWildcardRoutesSubdomains(t *testing.T) {
	service := startHTTPService(t, "wild-origin")
	certFile, keyFile := writeTestCertificate(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load the test certificate: %v", err)
	}

	cfg := testConfig(t, false)
	cfg.Server.HTTPSPassthroughPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"wild": tlsOriginHandler(t, cert, service)})
	agent.register(protocol.ProxySpec{
		Name: "wild", Type: protocol.ProxyTypeHTTPS, LocalAddr: service,
		Domains:        []string{"*.passthrough.example"},
		TLSPassthrough: true,
	})

	visitor, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoaOf(cfg.Server.HTTPSPassthroughPort)), &tls.Config{
		ServerName:         "deep.passthrough.example",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial the passthrough port for a wildcard SNI: %v", err)
	}
	defer visitor.Close()
	_ = visitor.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := visitor.Write([]byte("GET / HTTP/1.1\r\nHost: deep.passthrough.example\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	answer, err := io.ReadAll(visitor)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(answer), "wild-origin") {
		t.Fatalf("the wildcard did not route to the tunnel: %q", answer)
	}
}

// TestSNIPassthroughWithoutPortFailsRegistration pins the misconfiguration
// path: a passthrough proxy on a server without the listener is refused.
func TestSNIPassthroughWithoutPortFailsRegistration(t *testing.T) {
	service := startHTTPService(t, "sni-origin")
	certFile, keyFile := writeTestCertificate(t)
	cfg := testConfig(t, false)
	cfg.Server.HTTPSPort = freePort(t)
	cfg.Server.HTTPSCertFile = certFile
	cfg.Server.HTTPSKeyFile = keyFile
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"site": framedStreamHandler(service)})
	if err := agent.client.register(protocol.ProxySpec{
		Name: "site", Type: protocol.ProxyTypeHTTPS, LocalAddr: service,
		Domains:        []string{"sni.example"},
		TLSPassthrough: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if refusal := agent.expectRefused("site"); !strings.Contains(refusal, "https_passthrough_port") {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
}
