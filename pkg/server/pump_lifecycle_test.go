package server

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A datagram pump must not be started over the socket a concurrent close already
// took: the pump would carry a nil socket and panic in its background goroutine,
// where nothing recovers it and the whole server dies.
func TestNoPumpStartsOverASocketTheGroupLost(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	group := newProxyGroup(protocol.ProxySpec{
		Name: "lost", Type: protocol.ProxyTypeUDP, RemotePort: freePort(t),
	}, rs.server.tunnels)

	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	group.endpointMu.Lock()
	group.packet = packet
	group.endpointMu.Unlock()

	// The window the panic lived in: the socket is installed, and the group is
	// closed before startUDP reads it.
	group.close("test")
	group.startUDP()

	if pump := group.datagramPump(); pump != nil {
		t.Fatal("a pump was started for a group that was already closed")
	}
}

// One HTTP request is one stream. The request rides the tunnel stream the reverse
// proxy keeps for several requests, and that stream already counts itself, so
// counting the request as a second one doubled the gauge for every request in
// flight and left the extra count there while the transport kept the connection.
func TestAnOpenHTTPRequestCountsOneStream(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = io.WriteString(w, "held")
	})}
	go func() { _ = server.Serve(origin) }()
	t.Cleanup(func() { _ = server.Close() })

	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"web": framedStreamHandler(origin.Addr().String())})
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: origin.Addr().String(),
		Domains: []string{"held.example"},
	})
	waitForListener(t, rs.server, "web")

	body := make(chan string, 1)
	go func() { body <- getViaHTTP(t, cfg.Server.HTTPPort, "held.example", "/") }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the request never reached the origin")
	}

	group, sessions, totals := streamCounts(t, rs, "web")
	if group != 1 || sessions != 1 || totals != 1 {
		t.Errorf("while one http request is in flight: proxy=%d session(s)=%d metric=%d, want 1 everywhere",
			group, sessions, totals)
	}
	close(release)
	if got := <-body; !strings.Contains(got, "held") {
		t.Errorf("the request answered %q, want the origin's body", got)
	}
}
