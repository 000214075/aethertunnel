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

// A member whose local service is down never answers, so it never gets a
// measurement, and a strategy that reads "no measurement" as "free" hands it every
// visit. Measured through the real binaries against one live member, the latency
// strategy spent 31 of 30 attempts on the dead one and the adaptive strategy 30 of 30,
// against 15 for round-robin and 4 for the bandit. A failed attempt is what tells such
// a member apart from one that has merely not been measured yet.
//
// What the strategies hand out is asserted here by asking them: a visit cannot show it,
// because the data path retries a member that fails and a fake member that cannot reach
// its service closes the connection silently rather than reporting the failure the way
// a real client does. The observable end of this lives in scripts/functional-linux.sh,
// where both ends are real processes.
//
// The two properties the strategies have to hold at once are that the member that
// answers gets the visits and that the member that never answered is still probed, so
// that a service which comes back is found again.
func TestAMemberThatFailedWithoutAnsweringWaitsBehindTheOthers(t *testing.T) {
	for _, strategy := range []string{config.LoadBalanceLatency, config.LoadBalanceAdaptive} {
		t.Run(strategy, func(t *testing.T) {
			cfg := loadBalancedConfig(t, strategy)
			rs := startServer(t, cfg)
			port := freePort(t)

			// Nothing listens on this address: this is the member whose service is
			// down, and it registers first, so it is not merely the later one.
			deadAddress := net.JoinHostPort("127.0.0.1", fmt.Sprint(freePort(t)))
			liveAddress := countingEcho(t, "live")
			joinPool(t, rs, "pooled", deadAddress, "pool", "", port)
			joinPool(t, rs, "pooled", liveAddress, "pool", "", port)

			group, err := rs.server.tunnels.Get("pooled")
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			dead := memberFor(t, group, deadAddress)
			live := memberFor(t, group, liveAddress)

			// The state one failed attempt leaves behind: a failure, and still no
			// measurement of this member. The member that answers has been measured.
			dead.failures.Store(1)
			dead.dialLatency.Store(0)
			live.dialLatency.Store(int64(2 * time.Millisecond))

			// Forty picks: the dead member is the one the probe exists for, and the
			// probe runs one pick in twenty, so it gets two of them and the member
			// that answers gets the rest.
			const picks = 40
			toDead, toLive := 0, 0
			for i := 0; i < picks; i++ {
				picked := group.pickExcluding(nil)
				switch picked {
				case dead:
					toDead++
				case live:
					toLive++
				case nil:
					t.Fatalf("pick %d chose no member", i+1)
				default:
					t.Fatalf("pick %d chose an unexpected member", i+1)
				}
			}

			if toLive != picks-2 || toDead != 2 {
				t.Fatalf("under %s the member that answers got %d of %d picks and the one that "+
					"never answered %d, want %d and 2 (one probe per twenty)",
					strategy, toLive, picks, toDead, picks-2)
			}
		})
	}
}
