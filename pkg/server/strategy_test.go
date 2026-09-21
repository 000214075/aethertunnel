package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// memberFor returns the pool member whose local service is the given address.
func memberFor(t *testing.T, group *ProxyGroup, local string) *Tunnel {
	t.Helper()
	for _, member := range group.Members() {
		if member.Spec.LocalAddr == local {
			return member
		}
	}
	t.Fatalf("no member publishes %s", local)
	return nil
}

// TestRandomStrategyReachesEveryMember exercises the random strategy the way a
// visitor does: over the published port, with real requests. Random selection has
// no memory, so the only thing that can be asserted is that repeated requests
// reach every member; with two members, missing one 40 times in a row has a
// probability below 2e-12.
func TestRandomStrategyReachesEveryMember(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceRandom)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)
	joinPool(t, rs, "pooled", countingEcho(t, "B"), "pool", "", port)

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	seen := map[string]int{}
	for attempt := 0; attempt < 40; attempt++ {
		seen[echoOnce(target, "hello")]++
	}
	if seen["A"] == 0 || seen["B"] == 0 {
		t.Fatalf("random selection never reached one of the members: %v", seen)
	}
}

// TestLatencyStrategyKeepsChoosingTheFasterMember exercises the latency strategy
// over the published port. Both members answer in well under a millisecond on the
// loopback interface, which is far too close for the strategy to have an opinion,
// so the measured averages are seeded with a difference a real link would produce
// and the requests then have to keep going to the faster member.
func TestLatencyStrategyKeepsChoosingTheFasterMember(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceLatency)
	rs := startServer(t, cfg)
	port := freePort(t)

	slow := countingEcho(t, "slow")
	fast := countingEcho(t, "fast")
	joinPool(t, rs, "pooled", slow, "pool", "", port)
	joinPool(t, rs, "pooled", fast, "pool", "", port)

	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	memberFor(t, group, slow).dialLatency.Store(int64(80 * time.Millisecond))
	memberFor(t, group, fast).dialLatency.Store(int64(2 * time.Millisecond))

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	for attempt := 1; attempt <= 5; attempt++ {
		reply := echoOnce(target, "hello")
		if reply != "fast" {
			t.Fatalf("request %d reached %q, want the member with the lower measured latency", attempt, reply)
		}
	}

	// A member with no measurement at all has to be tried, otherwise a newly
	// joined member would never be given the chance to prove itself.
	memberFor(t, group, slow).dialLatency.Store(0)
	if reply := echoOnce(target, "hello"); reply != "slow" {
		t.Fatalf("the unmeasured member was skipped, reply %q", reply)
	}
}
