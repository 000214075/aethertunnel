package clientlib

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

func newPluginTestClient(t *testing.T) *client {
	t.Helper()
	return &client{
		cfg:    &config.Config{Client: config.ClientConfig{DialTimeoutSecs: 5, IdleTimeoutSecs: 30}},
		logger: log.New(io.Discard, "", 0),
	}
}

func TestHTTPProxyPluginForwardsGuardsAndBounds(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "origin-ok")
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse the target url: %v", err)
	}

	proxy := config.ProxyConfig{
		Name: "exit", Type: config.ProxyTypeTCP, Plugin: config.PluginHTTPProxy,
		AllowTargets:   []string{"127.0.0.0/8"},
		PluginHTTPUser: "ops", PluginHTTPPassword: "s3cret",
	}
	c := newPluginTestClient(t)
	pluginServer := httptest.NewServer(c.httpProxyHandler(proxy))
	defer pluginServer.Close()
	pluginURL, err := url.Parse(pluginServer.URL)
	if err != nil {
		t.Fatalf("parse the plugin url: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pluginURL)},
		Timeout:   10 * time.Second,
	}

	// Without credentials the proxy answers 407 before anything is dialed.
	request, err := http.NewRequest(http.MethodGet, "http://"+targetURL.Host+"/", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send without credentials: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("without credentials: status %d, want 407", response.StatusCode)
	}

	// With the credentials the absolute-form request reaches the origin.
	request, err = http.NewRequest(http.MethodGet, "http://"+targetURL.Host+"/", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	// A proxy client sends Proxy-Authorization, not Authorization.
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("ops:s3cret")))
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("send with credentials: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "origin-ok" {
		t.Fatalf("through the proxy: status %d body %q", response.StatusCode, body)
	}

	// A target the allow list does not cover is refused even with credentials.
	request, err = http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("ops:s3cret")))
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("send out of range: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("out of range: status %d, want 403", response.StatusCode)
	}
}

// TestHTTPProxyPluginConnectTunnel walks the CONNECT branch with raw bytes:
// the plugin answers 200 after dialing the named authority and the bytes after
// it belong to that connection.
func TestHTTPProxyPluginConnectTunnel(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tunneled-origin")
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse the target url: %v", err)
	}
	targetHost := targetURL.Host

	proxy := config.ProxyConfig{
		Name: "exit", Type: config.ProxyTypeTCP, Plugin: config.PluginHTTPProxy,
		AllowTargets: []string{"127.0.0.0/8"},
	}
	c := newPluginTestClient(t)
	pluginServer := httptest.NewServer(c.httpProxyHandler(proxy))
	defer pluginServer.Close()
	pluginURL, err := url.Parse(pluginServer.URL)
	if err != nil {
		t.Fatalf("parse the plugin url: %v", err)
	}

	conn, err := net.DialTimeout("tcp", pluginURL.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetHost, targetHost); err != nil {
		t.Fatalf("send CONNECT: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want 200", response.StatusCode)
	}
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", targetHost); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	answer, err := io.ReadAll(conn)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if !strings.Contains(string(answer), "tunneled-origin") {
		t.Fatalf("the CONNECT tunnel carried %q", answer)
	}
}

// TestHTTP2HTTPSPluginWrapsTheLocalLeg answers plain HTTP on the plugin side
// and reaches a local HTTPS origin, whose self-signed certificate needs no
// trusting; the rewritten Host is what the origin reports.
func TestHTTP2HTTPSPluginWrapsTheLocalLeg(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tls-origin host="+r.Host)
	}))
	defer origin.Close()

	proxy := config.ProxyConfig{
		Name: "site", Type: config.ProxyTypeTCP, Plugin: config.PluginHTTP2HTTPS,
		HostHeaderRewrite: "rewritten.example",
	}
	proxy.LocalPort = 1
	// local_addr comes from local_ip:local_port; the test needs the origin's
	// real address, so set the fields the handler reads.
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse the origin url: %v", err)
	}
	host, portStr, err := net.SplitHostPort(originURL.Host)
	if err != nil {
		t.Fatalf("split the origin addr: %v", err)
	}
	port := 0
	for _, digit := range portStr {
		port = port*10 + int(digit-'0')
	}
	proxy.LocalIP = host
	proxy.LocalPort = port

	c := newPluginTestClient(t)
	pluginServer := httptest.NewServer(c.http2HTTPSHandler(proxy))
	defer pluginServer.Close()

	// Windows has turned the plugin's clean close into an abortive one under
	// the reader ("wsarecv: The I/O operation has been aborted..."), so a
	// single aborted read is retried; the assertion is about the wrapped local
	// leg, not about the platform's close semantics.
	var response *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		response, err = (&http.Client{Timeout: 10 * time.Second}).Get(pluginServer.URL + "/")
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			break
		}
	}
	if err != nil {
		t.Fatalf("plain http to the plugin: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if !strings.Contains(string(body), "tls-origin host=rewritten.example") {
		t.Fatalf("the local leg did not carry the rewritten host: %q", body)
	}
}
