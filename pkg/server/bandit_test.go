package server

import (
	"math"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// banditGroup builds a group with plain members in it and no sockets anywhere, and the
// tests drive pick() rather than the selector directly: the switch that decides which
// strategy runs is part of what has to keep working, and a test that calls the selector
// itself would not notice if that switch stopped choosing the bandit.
func banditGroup(members int) (*ProxyGroup, []*Tunnel) {
	g := &ProxyGroup{Name: "pooled", Type: config.ProxyTypeTCP, strategy: config.LoadBalanceBandit}
	candidates := make([]*Tunnel, 0, members)
	for i := 0; i < members; i++ {
		member := &Tunnel{Name: "pooled", group: g}
		g.members = append(g.members, member)
		candidates = append(candidates, member)
	}
	return g, candidates
}

// observe records one outcome on a member the way the stream path does.
func observe(g *ProxyGroup, member *Tunnel, rewarded bool, latency time.Duration) {
	member.banditPulls.Add(1)
	if rewarded {
		member.banditReward.Add(banditRewardFor(latency))
	}
}

func TestBanditTriesEveryMemberBeforeRepeatingOne(t *testing.T) {
	g, _ := banditGroup(3)

	picked := map[*Tunnel]int{}
	for i := 0; i < 3; i++ {
		picked[g.pick()]++
	}
	if len(picked) != 3 {
		t.Fatalf("three picks reached %d of 3 members: %v", len(picked), picked)
	}
}

func TestBanditConvergesOnTheBetterMember(t *testing.T) {
	g, candidates := banditGroup(2)
	slow, fast := candidates[0], candidates[1] // slow first: a selector stuck on the first member must fail here

	picks := make([]*Tunnel, 0, 600)
	for i := 0; i < 600; i++ {
		chosen := g.pick()
		picks = append(picks, chosen)
		// The fast member answers in a millisecond, the slow one in a tenth of a second,
		// so the rewards are roughly 1 and roughly 0.33.
		if chosen == fast {
			observe(g, fast, true, time.Millisecond)
		} else {
			observe(g, slow, true, 100*time.Millisecond)
		}
	}

	// Over the second half the selector has estimates for both members and should be
	// exploiting the better one almost always, while still sampling the other
	// occasionally. It must not collapse to 100%: that would mean the exploration term
	// stopped working and a member could never recover.
	fastPicks := 0
	for _, chosen := range picks[300:] {
		if chosen == fast {
			fastPicks++
		}
	}
	if fastPicks < 240 {
		t.Errorf("the better member served %d of the last 300 streams, want at least 240", fastPicks)
	}
	if fastPicks == 300 {
		t.Error("the better member served every stream: the exploration term is not working, so a member that improved would never be found")
	}
}

// A member that fails every stream earns no reward, so the selector must move to the one
// that answers — this is the case the live check also covers.
func TestBanditAvoidsAMemberThatKeepsFailing(t *testing.T) {
	g, candidates := banditGroup(2)
	dead, alive := candidates[0], candidates[1] // the failing member is first, so a stuck selector sends everything to it

	attemptsOnDead := 0
	for i := 0; i < 300; i++ {
		chosen := g.pick()
		switch chosen {
		case dead:
			attemptsOnDead++
			observe(g, dead, false, 0)
		default:
			observe(g, alive, true, 2*time.Millisecond)
		}
	}

	if attemptsOnDead > 30 {
		t.Errorf("the selector sent %d of 300 streams to a member that never answered, want at most 30", attemptsOnDead)
	}
	if attemptsOnDead == 0 {
		t.Error("the failing member was never tried again, so a member that recovered could never come back")
	}
}

// A member whose estimates are worse must still be sampled sometimes, or a member that
// was temporarily slow could never recover.
func TestBanditKeepsSamplingAWorseMember(t *testing.T) {
	g, candidates := banditGroup(2)
	poor, good := candidates[0], candidates[1]

	// Give the poor member a long, bad history and the good one a long, good one.
	for i := 0; i < 200; i++ {
		observe(g, poor, true, 500*time.Millisecond)
		observe(g, good, true, time.Millisecond)
	}
	explored := false
	for i := 0; i < 200 && !explored; i++ {
		if g.pick() == poor {
			explored = true
		}
	}
	if !explored {
		t.Error("a worse member was never sampled again, so a member that recovered would stay excluded")
	}
}

// The score is what the strategy reasons with, so its shape is worth pinning: an untried
// member scores higher than any tried one, and the exploration term shrinks with use.
func TestUCBScorePrefersTheUntriedAndShrinksWithUse(t *testing.T) {
	untried := &Tunnel{}
	if score := ucbScore(untried, 10); !math.IsInf(score, 1) {
		t.Errorf("an untried member scores %v, want +Inf so it is tried first", score)
	}

	few := &Tunnel{}
	few.banditPulls.Store(1)
	few.banditReward.Add(1)
	many := &Tunnel{}
	many.banditPulls.Store(100)
	many.banditReward.Add(100)

	if ucbScore(few, 101) <= ucbScore(many, 101) {
		t.Error("a member with one observation behind it should be explored more than one with a hundred")
	}
}

// The reward has to fall as an answer takes longer, and stay inside (0, 1].
func TestBanditRewardFallsWithLatency(t *testing.T) {
	instant := banditRewardFor(0)
	quick := banditRewardFor(5 * time.Millisecond)
	slow := banditRewardFor(500 * time.Millisecond)
	if !(instant > quick && quick > slow) {
		t.Fatalf("rewards do not fall with latency: %v %v %v", instant, quick, slow)
	}
	if instant > 1 || slow <= 0 {
		t.Fatalf("reward outside (0, 1]: instant=%v slow=%v", instant, slow)
	}
}
