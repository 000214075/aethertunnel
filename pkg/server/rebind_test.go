package server

import (
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// remove() closes a group that empties, and a registration that reaches the name
// before Unregister drops it from the map joins that closed group and calls bind a
// second time. bind's release of the joining members' wait used to be a plain
// deferred close, so the second call panicked with "close of closed channel" and
// took the whole server down on a race between an unregistration and a
// registration.
func TestASecondBindOnAClosedGroupDoesNotPanic(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	group := newProxyGroup(protocol.ProxySpec{
		Name: "late", Type: protocol.ProxyTypeTCP,
	}, rs.server.tunnels)

	// The first member's bind: RemotePort 0 means no listener, but the bind still
	// releases anyone waiting for it.
	if err := group.bind(); err != nil {
		t.Fatalf("the first bind: %v", err)
	}
	// The last member left, which closes the group.
	group.close("the last member left")

	// A registration that raced the unregistration joins this group and binds it.
	if err := group.bind(); err == nil {
		t.Fatal("a bind on a closed group succeeded")
	}
}
