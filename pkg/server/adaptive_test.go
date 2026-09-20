package server

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// --- the cost function --------------------------------------------------------

func TestCostPrefersTheFasterMember(t *testing.T) {
	fast := &Tunnel{}
	slow := &Tunnel{}
	fast.dialLatency.Store(int64(2 * time.Millisecond))
	slow.dialLatency.Store(int64(40 * time.Millisecond))

	if fast.cost() >= slow.cost() {
		t.Fatalf("the 2ms member costs %d and the 40ms member %d; the faster one should cost less",
			fast.cost(), slow.cost())
	}
}

func TestCostTreatsAnUnmeasuredMemberAsFreeSoItIsTried(t *testing.T) {
	unmeasured := &Tunnel{}
	measured := &Tunnel{}
	measured.dialLatency.Store(int64(time.Millisecond))

	if unmeasured.cost() != 0 {
		t.Fatalf("an unmeasured member costs %d, want 0", unmeasured.cost())
	}
	if unmeasured.cost() >= measured.cost() {
		t.Fatal("an unmeasured member does not cost less than a measured one, so it would never be tried")
	}
}

func TestCostPenalisesConsecutiveFailuresUpToACap(t *testing.T) {
	member := &Tunnel{}
	member.dialLatency.Store(int64(10 * time.Millisecond))

	base := member.cost()
	for failures := int64(1); failures <= maxCostFailures; failures++ {
		member.failures.Store(failures)
		if got, want := member.cost(), base*(1+2*failures); got != want {
			t.Fatalf("with %d failures the cost is %d, want %d", failures, got, want)
		}
	}

	// Past the cap the cost stops growing, so a member that had a long outage is
	// still more attractive than an infinitely expensive one and can recover.
	member.failures.Store(maxCostFailures + 10)
	if got, want := member.cost(), base*(1+2*maxCostFailures); got != want {
		t.Fatalf("beyond the cap the cost is %d, want %d", got, want)
	}
}

func TestCostRanksAFailingFastMemberBehindAHealthySlowerOne(t *testing.T) {
	failing := &Tunnel{}
	failing.dialLatency.Store(int64(time.Millisecond))
	failing.failures.Store(3)

	healthy := &Tunnel{}
	healthy.dialLatency.Store(int64(5 * time.Millisecond))

	if failing.cost() <= healthy.cost() {
		t.Fatalf("a member with 3 failures costs %d and the healthy one %d; the failing member should cost more",
			failing.cost(), healthy.cost())
	}
}

// --- selection ----------------------------------------------------------------

func TestPickByCostChoosesTheCheapestMember(t *testing.T) {
	group := &ProxyGroup{}
	slow := &Tunnel{}
	slow.dialLatency.Store(int64(30 * time.Millisecond))
	fastest := &Tunnel{}
	fastest.dialLatency.Store(int64(3 * time.Millisecond))
	middle := &Tunnel{}
	middle.dialLatency.Store(int64(10 * time.Millisecond))

	candidates := []*Tunnel{slow, middle, fastest}
	for i := 0; i < 8; i++ {
		if picked := group.pickByCost(candidates); picked != fastest {
			t.Fatalf("pick %d chose a member costing %d, want the %d-cost one",
				i, picked.cost(), fastest.cost())
		}
	}
}

func TestPickByCostRotatesBetweenEqualMembers(t *testing.T) {
	group := &ProxyGroup{}
	first := &Tunnel{}
	second := &Tunnel{}
	for _, member := range []*Tunnel{first, second} {
		member.dialLatency.Store(int64(5 * time.Millisecond))
	}

	candidates := []*Tunnel{first, second}
	counts := map[*Tunnel]int{}
	for i := 0; i < 20; i++ {
		counts[group.pickByCost(candidates)]++
	}
	if counts[first] != 10 || counts[second] != 10 {
		t.Fatalf("equal members were chosen %d and %d times, want 10 each", counts[first], counts[second])
	}
}

func TestPickByCostHandlesASingleCandidate(t *testing.T) {
	group := &ProxyGroup{}
	only := &Tunnel{}
	only.dialLatency.Store(int64(time.Millisecond))
	if picked := group.pickByCost([]*Tunnel{only}); picked != only {
		t.Fatal("the only candidate was not chosen")
	}
}

// --- end to end ---------------------------------------------------------------

func TestAdaptiveStrategyServesEveryVisitFromAPool(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceAdaptive)
	rs := startServer(t, cfg)
	port := freePort(t)

	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)
	joinPool(t, rs, "pooled", countingEcho(t, "B"), "pool", "", port)

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	for i := 0; i < 6; i++ {
		reply := echoOnce(target, "hello")
		if reply != "A" && reply != "B" {
			t.Fatalf("visit %d was answered with %q, want one of the pool's members", i, reply)
		}
	}

	// Both members have to be usable afterwards: an adaptive pool that has decided
	// one member is broken must still fail over rather than return nothing.
	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("look the pool up: %v", err)
	}
	members := group.Summary()
	if len(members) != 2 {
		t.Fatalf("the pool reports %d members, want 2", len(members))
	}
	for _, member := range members {
		if member.Failures != 0 {
			t.Errorf("member %s has %d consecutive failures after a successful run", member.ClientID, member.Failures)
		}
		if !member.Healthy {
			t.Errorf("member %s is not healthy", member.ClientID)
		}
	}
}

func TestFailedStreamsAreCountedAgainstTheMember(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceAdaptive)
	rs := startServer(t, cfg)
	port := freePort(t)

	// A member whose agent has no handler for the proxy: the server asks for a
	// stream and waits until the dial timeout, which is a failure against the
	// member.
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})
	agent.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: port, Group: "pool",
	})
	waitForListener(t, rs.server, "pooled")

	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("look the pool up: %v", err)
	}
	member := group.Members()[0]

	target := net.JoinHostPort(cfg.Server.BindAddr, fmt.Sprint(port))
	if reply := echoOnce(target, "hello"); reply != "" {
		t.Fatalf("a visit was served with %q although the only member never answers", reply)
	}

	waitForCondition(t, func() bool { return member.failures.Load() > 0 },
		"the failed stream was not counted against the member")
}

// TestCostPenaltyAppliesOnceAMemberHasBeenMeasured covers the interaction between the
// two counters: the failure penalty multiplies a measured latency, so a member that
// has never answered is still the cheapest thing in the pool and gets tried again.
func TestCostPenaltyAppliesOnceAMemberHasBeenMeasured(t *testing.T) {
	group := &ProxyGroup{}

	measured := &Tunnel{}
	measured.dialLatency.Store(int64(time.Millisecond))
	measured.failures.Store(1)

	unmeasured := &Tunnel{}

	if measured.cost() <= unmeasured.cost() {
		t.Fatalf("a measured member that failed once costs %d and an unmeasured one %d; "+
			"the measured member should be the more expensive while it is failing",
			measured.cost(), unmeasured.cost())
	}
	if picked := group.pickByCost([]*Tunnel{measured, unmeasured}); picked != unmeasured {
		t.Fatal("the pool did not prefer the member it has not tried yet")
	}
}

func TestAdaptiveStrategyIsReportedByTheDashboard(t *testing.T) {
	cfg := loadBalancedConfig(t, config.LoadBalanceAdaptive)
	rs := startServer(t, cfg)
	port := freePort(t)
	joinPool(t, rs, "pooled", countingEcho(t, "A"), "pool", "", port)

	group, err := rs.server.tunnels.Get("pooled")
	if err != nil {
		t.Fatalf("look the pool up: %v", err)
	}
	if group.strategy != config.LoadBalanceAdaptive {
		t.Fatalf("the pool's strategy is %q, want %q", group.strategy, config.LoadBalanceAdaptive)
	}

	body, err := json.Marshal(group.Summary())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) == 0 {
		t.Error("the member summary marshalled to nothing")
	}
}
