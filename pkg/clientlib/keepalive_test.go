package clientlib

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// client.tcp_keepalive_seconds reaches the dialer that opens the server
// connection. Zero leaves the field unset, which is how net.Dialer asks for its
// own fifteen-second default.
func TestTCPKeepAliveReachesTheServerDialer(t *testing.T) {
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}

	if got := c.dialer().KeepAlive; got != 0 {
		t.Errorf("without the key the dialer's keep-alive field is %s, want 0 (the dialer's own default)", got)
	}

	c.cfg.Client.TCPKeepAliveSeconds = 7200
	if got := c.dialer().KeepAlive; got != 7200*time.Second {
		t.Errorf("the dialer's keep-alive is %s, want 2h", got)
	}
}

// client.connect_server_local_ip becomes the source address of the dialer that
// opens the server connection.
func TestConnectServerLocalIPReachesTheDialer(t *testing.T) {
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	if got := c.dialer().LocalAddr; got != nil {
		t.Errorf("without the key the dialer has a local address %v, want none", got)
	}

	c.cfg.Client.ConnectServerLocalIP = "10.1.2.3"
	local, ok := c.dialer().LocalAddr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("the dialer's local address is %T, want *net.TCPAddr", c.dialer().LocalAddr)
	}
	if !local.IP.Equal(net.ParseIP("10.1.2.3")) {
		t.Errorf("the dialer binds %v, want 10.1.2.3", local.IP)
	}
}
