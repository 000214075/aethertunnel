package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// testClient builds the smallest client the heartbeat decision needs: a config and a
// logger whose output the test can read.
func testClient(t *testing.T, clientHeartbeatSeconds int) (*client, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	cfg := &config.Config{}
	cfg.Client.HeartbeatSeconds = clientHeartbeatSeconds
	return &client{cfg: cfg, logger: log.New(buf, "", 0)}, buf
}

// The server decides how often a client heartbeats, because the server is what drops
// a client that has been quiet for three intervals.
func TestHeartbeatIntervalFollowsTheServer(t *testing.T) {
	c, logs := testClient(t, 60)

	if got := c.heartbeatInterval(15); got != 15*time.Second {
		t.Fatalf("interval is %s, want the server's 15s", got)
	}
	if !strings.Contains(logs.String(), "client.heartbeat_seconds") {
		t.Errorf("a configured value that is overridden was not reported: %q", logs.String())
	}
}

// A server that does not state an interval leaves the configured value in charge:
// that is what makes [client].heartbeat_seconds a setting rather than a decoration.
func TestHeartbeatIntervalFallsBackToTheConfiguredValue(t *testing.T) {
	c, logs := testClient(t, 45)

	if got := c.heartbeatInterval(0); got != 45*time.Second {
		t.Fatalf("interval is %s, want the configured 45s", got)
	}
	if logs.Len() != 0 {
		t.Errorf("the fallback logged something: %q", logs.String())
	}
	// A negative value cannot mean an interval either; treat it like a missing one.
	if got := c.heartbeatInterval(-1); got != 45*time.Second {
		t.Fatalf("interval for a negative server value is %s, want the configured 45s", got)
	}
}

// A config built without applyDefaults has zero everywhere, and a ticker needs a
// positive period, so there is a floor under it.
func TestHeartbeatIntervalHasADefaultWhenNothingIsSet(t *testing.T) {
	c, _ := testClient(t, 0)

	if got := c.heartbeatInterval(0); got != 30*time.Second {
		t.Fatalf("interval is %s, want the 30s default", got)
	}
}

// The same interval is agreed on, so nothing claims an override happened.
func TestHeartbeatIntervalIsSilentWhenBothAgree(t *testing.T) {
	c, logs := testClient(t, 30)

	if got := c.heartbeatInterval(30); got != 30*time.Second {
		t.Fatalf("interval is %s, want 30s", got)
	}
	if logs.Len() != 0 {
		t.Errorf("an agreeing pair logged something: %q", logs.String())
	}
}

// A client with no server_addr uses the resolved address as its control address, so the
// record it resolves has to be one that names the control port. Only a private proxy's
// record does; the others name where visitors reach the proxy, and the client warns
// instead of leaving the operator to guess why the connection keeps failing.
func TestOnlyAPrivateProxyRecordNamesTheControlPort(t *testing.T) {
	for _, proxyType := range []string{"stcp", "sudp", "xtcp"} {
		if !namesAControlPort(proxyType) {
			t.Errorf("a %s record names the control port; the client would not warn about it", proxyType)
		}
	}
	for _, proxyType := range []string{"tcp", "udp", "http", "https", "socks5", ""} {
		if namesAControlPort(proxyType) {
			t.Errorf("a %s record does not name the control port, so resolving one as a server address is a mistake the client should report", proxyType)
		}
	}
}

// A client that names its server is not redirected by the DHT. Measured with real
// processes before this was fixed: with server_addr = 127.0.0.1:17703 and
// dht.discover = "moved", where the record pointed at 127.0.0.1:17701, the client logged
// "the configured address is used and the DHT is not consulted" and then dialled
// 127.0.0.1:17701. The record is unsigned unless dht.trusted_keys says otherwise, so
// following it means handing an auth_token to whoever answers for the name.
func TestAConfiguredAddressIsNotOverriddenByDhtDiscover(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"address and discover", &config.Config{
			Client: config.ClientConfig{ServerAddr: "127.0.0.1:17703"},
			DHT:    config.DHTConfig{Enabled: true, Discover: "moved"},
		}, false},
		{"discover only", &config.Config{
			DHT: config.DHTConfig{Enabled: true, Discover: "moved"},
		}, true},
		{"address only", &config.Config{
			Client: config.ClientConfig{ServerAddr: "127.0.0.1:17703"},
			DHT:    config.DHTConfig{Enabled: true},
		}, false},
		{"discover without the dht section", &config.Config{
			DHT: config.DHTConfig{Discover: "moved"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{cfg: tc.cfg}
			if got := c.usesDiscoveredAddress(); got != tc.want {
				t.Fatalf("usesDiscoveredAddress() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The confirmation line is the client's only report of what the server published, and
// the port it names is the one the server listens on: a member of a pool that was
// already published asks for one port and gets another, and an operator who read the
// requested one would test a port nothing is listening on.
func TestTheConfirmationLineNamesThePortTheServerListensOn(t *testing.T) {
	got := describePublishedProxy(protocol.ProxyStatus{
		Name: "pooled", Type: "tcp", RemotePort: 6022,
	})
	if !strings.Contains(got, "on port 6022") {
		t.Errorf("the confirmation names %q, want the server's port", got)
	}
	if !strings.Contains(got, "tcp") {
		t.Errorf("the confirmation names %q, want the proxy type", got)
	}
}

// A name can be published by several clients, and the share count is what tells the
// operator of a second client that it joined the existing pool instead of publishing a
// second endpoint.
func TestTheConfirmationLineReportsASharedName(t *testing.T) {
	shared := describePublishedProxy(protocol.ProxyStatus{
		Name: "pooled", Type: "tcp", RemotePort: 6022, GroupMembers: 3,
	})
	if !strings.Contains(shared, "3") {
		t.Errorf("a name shared by three clients reads %q, want the share count", shared)
	}
	single := describePublishedProxy(protocol.ProxyStatus{
		Name: "solo", Type: "tcp", RemotePort: 6023, GroupMembers: 1,
	})
	if strings.Contains(single, "shared by") {
		t.Errorf("a proxy served by one client was reported as shared: %q", single)
	}
}

// A stream reaches a client either as a connection to the published port or as a
// visitor asking for the proxy by name. The per-stream log line is where that
// difference is visible, so the flag has to be read rather than assumed.
func TestAStreamFromAVisitorIsLabelledAsOne(t *testing.T) {
	if got := streamOrigin(protocol.DataRequest{Visitor: true}); !strings.Contains(got, "visitor") {
		t.Errorf("a visitor stream is labelled %q", got)
	}
	if got := streamOrigin(protocol.DataRequest{}); strings.Contains(got, "visitor") {
		t.Errorf("a stream from the public port is labelled %q", got)
	}
}
