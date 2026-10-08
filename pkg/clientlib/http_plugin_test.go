package clientlib

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// A request in the default-port form names its target without a port, while the
// allow list matches host:port pairs. Passing the bare host to the check refused
// every such request with 403 before anything was dialed, so a browser reaching
// the proxy with http://host/ was answered as if the target were not allowed.
func TestHTTPProxyPluginAcceptsADefaultPortTarget(t *testing.T) {
	proxy := config.ProxyConfig{
		Name: "exit", Type: config.ProxyTypeTCP, Plugin: config.PluginHTTPProxy,
		AllowTargets: []string{"192.0.2.1/32"},
	}
	c := newPluginTestClient(t)
	c.cfg.Client.DialTimeoutSecs = 1

	pluginServer := httptest.NewServer(c.httpProxyHandler(proxy))
	defer pluginServer.Close()

	conn, err := net.Dial("tcp", pluginServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the plugin: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Written by hand: http.Client would put the port of a resolved address in the
	// request line, which is the form that always worked.
	if _, err := fmt.Fprintf(conn, "GET http://192.0.2.1/ HTTP/1.1\r\nHost: 192.0.2.1\r\n\r\n"); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		t.Fatal("the plugin refused a request that named its target without a port")
	}
	// 192.0.2.1 is TEST-NET-1: the check passed, so what is left is the dial that
	// has nowhere to go.
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 from the target that cannot be reached", response.StatusCode)
	}
}

// The plugin servers are stopped when the client's run returns, and a stream
// goroutine that reaches the plugin afterwards used to build a server the stop
// snapshot had already passed: nothing would ever shut it down, so it and its
// handler stayed for the life of the process.
func TestAnInProcessPluginServerIsNotBuiltAfterTheStop(t *testing.T) {
	c := newPluginTestClient(t)
	c.stopPluginServers()

	if _, err := c.dialForPlugin(config.ProxyConfig{
		Name: "files", Type: config.ProxyTypeTCP, Plugin: config.PluginStaticFile,
		PluginLocalPath: t.TempDir(),
	}); err == nil {
		t.Fatal("dialForPlugin built a plugin server after the client had stopped them")
	}
}

// A congested tunnel drops one packet per read, so one log line per packet fills
// the log faster than the packets arrive. The warning is written once, with the
// count of what was dropped since it last was.
func TestDroppedTunnelPacketsAreNotLoggedOneByOne(t *testing.T) {
	var logs bytes.Buffer
	// A closed transport drops every packet, which is what a tunnel whose buffer
	// is full looks like from here.
	transport := vpn.NewChannelTransport(nil)
	transport.Close()
	c := newPluginTestClient(t)
	c.logger = log.New(&logs, "", 0)
	c.vpnTransport = transport

	for i := 0; i < 100; i++ {
		c.deliverVPN([]byte("packet"))
	}
	if got := strings.Count(logs.String(), "buffer is full"); got != 1 {
		t.Fatalf("100 dropped packets produced %d warnings, want 1", got)
	}
	if !strings.Contains(logs.String(), "dropped 1 tunnel packet") {
		t.Fatalf("the warning does not carry the count: %q", logs.String())
	}

	// The next warning waits for the interval, and then reports everything the
	// first one did not.
	c.vpnDropLogAt.Store(time.Now().Add(-time.Minute).Unix())
	c.deliverVPN([]byte("packet"))
	if !strings.Contains(logs.String(), "dropped 100 tunnel packet") {
		t.Fatalf("the second warning does not carry the accumulated count: %q", logs.String())
	}
}
