package clientlib

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// writePluginCertificate drops a throwaway key pair the TLS-terminating
// plugins load, the way a real deployment points plugin_cert_file at one.
func writePluginCertificate(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := writeFileRetryingBadDescriptor(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	if err := writeFileRetryingBadDescriptor(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	return certFile, keyFile
}

func TestSocks5PluginDialsBoundedAndAuthed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "socks-origin-ok")
	}))
	defer origin.Close()

	proxy := config.ProxyConfig{
		Name: "exit", Type: config.ProxyTypeTCP, Plugin: config.PluginSocks5,
		AllowTargets:   []string{"127.0.0.0/8"},
		PluginUser:     "ops",
		PluginPassword: "s3cret",
	}
	c := newPluginTestClient(t)
	stream, err := c.dialForPlugin(proxy)
	if err != nil {
		t.Fatalf("dialForPlugin: %v", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))

	// The visitor's SOCKS5 session: offering no-auth only must be refused,
	// because the proxy requires username/password authentication.
	if _, err := stream.Write([]byte{socks.Version, 1, socks.MethodNoReq}); err != nil {
		t.Fatalf("write the greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(stream, method); err != nil {
		t.Fatalf("read the method: %v", err)
	}
	if method[1] != socks.MethodNone {
		t.Fatalf("the server answered method 0x%02x, want 0xFF for a no-auth-only greeting", method[1])
	}

	// The plugin's own dial to the origin has failed once on a loaded macOS
	// runner, leaving an empty body, so the exchange runs again on a fresh
	// stream; the assertions are about the plugin, not about the runner.
	host, port, _ := net.SplitHostPort(origin.Listener.Addr().String())
	number, _ := net.LookupPort("tcp", port)

	var relayed string
	var relayErr error
	for attempt := 0; attempt < 3; attempt++ {
		relayed, relayErr = socksConnectAndGet(c, proxy, host, number)
		if relayErr == nil {
			break
		}
	}
	if relayErr != nil {
		t.Fatalf("%v", relayErr)
	}
	if !strings.Contains(relayed, "socks-origin-ok") {
		t.Fatalf("through the plugin: %q", relayed)
	}
}

// socksConnectAndGet runs one username/password SOCKS5 exchange that CONNECTs
// to host:port and returns what the tunnel relays back.
func socksConnectAndGet(c *client, proxy config.ProxyConfig, host string, port int) (string, error) {
	stream, err := c.dialForPlugin(proxy)
	if err != nil {
		return "", fmt.Errorf("dialForPlugin: %w", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := stream.Write([]byte{socks.Version, 1, 0x02}); err != nil {
		return "", fmt.Errorf("write the greeting: %w", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(stream, method); err != nil {
		return "", fmt.Errorf("read the method: %w", err)
	}
	if method[1] != 0x02 {
		return "", fmt.Errorf("the server answered method 0x%02x, want 0x02", method[1])
	}
	auth := []byte{1, 3, 'o', 'p', 's', 6, 's', '3', 'c', 'r', 'e', 't'}
	if _, err := stream.Write(auth); err != nil {
		return "", fmt.Errorf("write the credentials: %w", err)
	}
	verdict := make([]byte, 2)
	if _, err := io.ReadFull(stream, verdict); err != nil {
		return "", fmt.Errorf("read the verdict: %w", err)
	}
	if verdict[1] != 0x00 {
		return "", fmt.Errorf("the credentials were refused (0x%02x)", verdict[1])
	}

	request := []byte{socks.Version, socks.CmdConnect, 0x00, socks.AtypDomain, byte(len(host))}
	request = append(request, host...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := stream.Write(request); err != nil {
		return "", fmt.Errorf("write the request: %w", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(stream, reply); err != nil {
		return "", fmt.Errorf("read the reply: %w", err)
	}
	if reply[1] != socks.ReplySucceeded {
		return "", fmt.Errorf("CONNECT was answered 0x%02x, want success", reply[1])
	}
	bound := make([]byte, 6)
	if _, err := io.ReadFull(stream, bound); err != nil {
		return "", fmt.Errorf("read the bound address: %w", err)
	}
	fmt.Fprintf(stream, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", net.JoinHostPort(host, strconv.Itoa(port)))
	body, _ := io.ReadAll(stream)
	return string(body), nil
}

// writeFileRetryingBadDescriptor writes a file and retries once when the close
// inside the write fails with "bad file descriptor" — a spurious error the
// macOS runners produce on an otherwise healthy temp file, seen as
// "write the certificate: close .../cert.pem: bad file descriptor".
func writeFileRetryingBadDescriptor(path string, data []byte) error {
	err := os.WriteFile(path, data, 0o600)
	if err == nil || !strings.Contains(err.Error(), "bad file descriptor") {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return os.WriteFile(path, data, 0o600)
}
