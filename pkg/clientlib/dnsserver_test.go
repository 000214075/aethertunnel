package clientlib

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// client.dns_server sends the server-address lookup to the named resolver
// instead of the system one: the lookup fails against a closed loopback port by
// naming that port, which the system resolver would never do.
func TestDNSServerResolverIsTheConfiguredOne(t *testing.T) {
	// A port with nothing on it: the resolver dials it and the lookup fails
	// there rather than anywhere the system resolver would look.
	dead := refusedAddr(t)
	c := &client{cfg: &config.Config{Client: config.ClientConfig{DNSServer: dead, DialTimeoutSecs: 2}},
		logger: log.New(io.Discard, "", 0)}

	dialer := c.dialer()
	if dialer.Resolver == nil {
		t.Fatal("client.dns_server did not reach the dialer's resolver")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialer.Resolver.LookupHost(ctx, "server.example")
	if err == nil {
		t.Fatal("the lookup against a closed resolver succeeded")
	}
	if !strings.Contains(err.Error(), dead) {
		t.Errorf("the lookup error %q does not name the configured resolver %s", err, dead)
	}
}

// Without the key the dialer keeps the system resolver.
func TestWithoutDNSServerTheDialerKeepsTheSystemResolver(t *testing.T) {
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	if c.dialer().Resolver != nil {
		t.Error("a client without client.dns_server set its own resolver")
	}
}
