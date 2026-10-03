package clientlib

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// tlsVisitor hands the plugin's stream end a TLS handshake against the
// certificate the plugin terminates with, and returns the ready connection.
func tlsVisitor(t *testing.T, stream net.Conn) net.Conn {
	t.Helper()
	tlsConn := tls.Client(stream, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("the visitor's TLS handshake against the plugin failed: %v", err)
	}
	_ = stream.SetDeadline(time.Time{})
	return tlsConn
}

func httpGetOver(t *testing.T, conn net.Conn, host string) string {
	t.Helper()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	body, err := io.ReadAll(conn)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read the answer: %v", err)
	}
	return string(body)
}

// originAddr splits an httptest URL into the LocalIP and LocalPort the
// plugin configuration names.
func originAddr(t *testing.T, originURL string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(originURL, "http://"))
	if err != nil {
		host, port, err = net.SplitHostPort(strings.TrimPrefix(originURL, "https://"))
		if err != nil {
			t.Fatalf("split the origin address: %v", err)
		}
	}
	number, err := net.LookupPort("tcp", port)
	if err != nil {
		t.Fatalf("parse the origin port: %v", err)
	}
	return host, number
}

func TestTLS2RawPluginBridgesToAPlainService(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "raw-origin-ok")
	}))
	defer origin.Close()
	certFile, keyFile := writePluginCertificate(t, "tls2raw.example")
	localIP, localPort := originAddr(t, origin.URL)

	proxy := config.ProxyConfig{
		Name: "raw", Type: config.ProxyTypeTCP, Plugin: config.PluginTLS2Raw,
		LocalIP: localIP, LocalPort: localPort,
		PluginCertFile: certFile, PluginKeyFile: keyFile,
	}
	c := newPluginTestClient(t)
	stream, err := c.dialForPlugin(proxy)
	if err != nil {
		t.Fatalf("dialForPlugin: %v", err)
	}
	defer stream.Close()

	visitor := tlsVisitor(t, stream)
	if body := httpGetOver(t, visitor, "tls2raw.example"); !strings.Contains(body, "raw-origin-ok") {
		t.Fatalf("through the plugin: %q", body)
	}
}

func TestHTTPS2HTTPSPluginBridgesToALocalHTTPSOrigin(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "https-origin-ok")
	}))
	defer origin.Close()
	certFile, keyFile := writePluginCertificate(t, "bridge.example")
	localIP, localPort := originAddr(t, origin.URL)

	proxy := config.ProxyConfig{
		Name: "bridge", Type: config.ProxyTypeTCP, Plugin: config.PluginHTTPS2HTTPS,
		LocalIP: localIP, LocalPort: localPort,
		PluginCertFile: certFile, PluginKeyFile: keyFile,
	}
	c := newPluginTestClient(t)
	stream, err := c.dialForPlugin(proxy)
	if err != nil {
		t.Fatalf("dialForPlugin: %v", err)
	}
	defer stream.Close()

	visitor := tlsVisitor(t, stream)
	if body := httpGetOver(t, visitor, "bridge.example"); !strings.Contains(body, "https-origin-ok") {
		t.Fatalf("through the plugin: %q", body)
	}
}
