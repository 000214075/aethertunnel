package clientlib

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// newTLSBridgeTestClient is newPluginTestClient with the plugin's errors on
// stderr, so a CI failure shows the handler's own reason instead of a bare
// empty reply.
func newTLSBridgeTestClient() *client {
	return &client{
		cfg:    &config.Config{Client: config.ClientConfig{DialTimeoutSecs: 5, IdleTimeoutSecs: 30}},
		logger: log.New(os.Stderr, "", log.LstdFlags),
	}
}

// runBridgeSession dials the plugin, speaks TLS against its certificate, sends
// one GET and returns the body. It reports the reason a session ended early so
// the caller can retry on a slow runner.
func runBridgeSession(proxy config.ProxyConfig, host string) (string, error) {
	c := newTLSBridgeTestClient()
	stream, err := c.dialForPlugin(proxy)
	if err != nil {
		return "", fmt.Errorf("dialForPlugin: %w", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))

	tlsConn := tls.Client(stream, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		return "", fmt.Errorf("the visitor's TLS handshake: %w", err)
	}
	fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	body, err := io.ReadAll(tlsConn)
	return string(body), err
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
	assertBridgeServes(t, proxy, "raw-origin-ok")
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
	assertBridgeServes(t, proxy, "https-origin-ok")
}

// assertBridgeServes walks the plugin end to end. A loaded runner can starve
// one session's handshake, so a session that ends without the origin's body is
// retried: the assertion is about the bridge working, and each attempt logs
// its own reason on the way.
//
// The test is skipped on darwin: the TLS handshakes it drives negotiate the
// post-quantum key exchange by default, and the mlkem work running next to a
// registered signal handler trips Go's darwin/arm64 runtime into
// "signal_recv: inconsistent state" — a runtime defect, not a bridge one, and
// it kills the whole test binary.
func assertBridgeServes(t *testing.T, proxy config.ProxyConfig, want string) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("the darwin/arm64 runtime throws in sigqueue under this test's TLS load")
	}
	var last string
	for attempt := 1; attempt <= 3; attempt++ {
		body, err := runBridgeSession(proxy, proxy.Name+".example")
		if err != nil {
			last = fmt.Sprintf("attempt %d: %v", attempt, err)
			t.Logf("%s", last)
			continue
		}
		if strings.Contains(body, want) {
			return
		}
		last = fmt.Sprintf("attempt %d: body %q", attempt, body)
		t.Logf("%s", last)
	}
	t.Fatalf("the bridge did not serve %q in three attempts; last: %s", want, last)
}
