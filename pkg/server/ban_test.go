package server

import (
	"net"
	"testing"
	"time"
)

// newTestBanList builds a list whose clock the test controls.
func newTestBanList(t *testing.T, threshold int, duration, max time.Duration, ignore ...string) (*banList, func(time.Duration)) {
	t.Helper()

	list, err := newBanList(threshold, duration, max, ignore)
	if err != nil {
		t.Fatalf("ban list: %v", err)
	}
	if list == nil {
		t.Fatalf("a threshold of %d produced no list", threshold)
	}

	now := time.Now()
	list.now = func() time.Time { return now }
	return list, func(delta time.Duration) { now = now.Add(delta) }
}

func TestBanListIsDisabledByAThresholdOfZero(t *testing.T) {
	list, err := newBanList(0, time.Minute, time.Hour, nil)
	if err != nil {
		t.Fatalf("ban list: %v", err)
	}
	if list.enabled() {
		t.Error("a threshold of 0 produced an active list")
	}
	if banned, _ := list.blocked(net.ParseIP("10.0.0.1")); banned {
		t.Error("a disabled list banned a source")
	}
	if imposed, _ := list.fail(net.ParseIP("10.0.0.1")); imposed {
		t.Error("a disabled list banned a source on failure")
	}
	if list.banned() != 0 || list.size() != 0 {
		t.Error("a disabled list tracks sources")
	}
}

func TestBanListBansAfterTheConfiguredFailures(t *testing.T) {
	list, advance := newTestBanList(t, 3, time.Minute, time.Hour)
	ip := net.ParseIP("192.0.2.10")

	for attempt := 1; attempt <= 2; attempt++ {
		imposed, count := list.fail(ip)
		if imposed {
			t.Fatalf("attempt %d imposed a ban", attempt)
		}
		if count != attempt {
			t.Errorf("attempt %d counted %d failures", attempt, count)
		}
		if banned, _ := list.blocked(ip); banned {
			t.Fatalf("the source is banned after %d of 3 failures", attempt)
		}
	}

	imposed, bans := list.fail(ip)
	if !imposed {
		t.Fatal("the third failure did not ban the source")
	}
	if bans != 1 {
		t.Errorf("it is ban number %d, want 1", bans)
	}

	banned, remaining := list.blocked(ip)
	if !banned {
		t.Fatal("the source is not banned after the third failure")
	}
	if remaining <= 0 || remaining > time.Minute {
		t.Errorf("the ban has %s left, want up to 1m", remaining)
	}
	if list.banned() != 1 {
		t.Errorf("the list reports %d banned sources, want 1", list.banned())
	}

	// The ban ends on its own.
	advance(time.Minute + time.Second)
	if banned, _ := list.blocked(ip); banned {
		t.Error("the ban outlived its duration")
	}
}

func TestBanListDoublesEachBanAndStopsAtTheMaximum(t *testing.T) {
	list, advance := newTestBanList(t, 1, time.Minute, 4*time.Minute)
	ip := net.ParseIP("192.0.2.20")

	wants := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute}
	for index, want := range wants {
		if imposed, bans := list.fail(ip); !imposed || bans != index+1 {
			t.Fatalf("failure %d imposed=%v ban number %d", index+1, imposed, bans)
		}
		_, remaining := list.blocked(ip)
		if remaining > want || remaining < want-time.Second {
			t.Fatalf("ban %d lasts %s, want about %s", index+1, remaining, want)
		}
		// Sit out the ban, then fail again.
		advance(want + time.Second)
	}
}

func TestBanListForgetsFailuresWhenTheWindowPasses(t *testing.T) {
	list, advance := newTestBanList(t, 3, time.Minute, time.Hour)
	ip := net.ParseIP("192.0.2.30")

	list.fail(ip)
	list.fail(ip)
	advance(banWindow + time.Second)
	list.fail(ip)

	if banned, _ := list.blocked(ip); banned {
		t.Fatal("two failures from before the window plus one after banned the source")
	}
}

// A source that serves its ban and comes back has to be banned for longer, which is
// the documented escalation. The path matters: the server checks blocked() when the
// connection arrives, before it ever reaches fail(), and that check is what decides
// whether the source's history is still there. A test that only calls fail() never
// sees it.
func TestBanListRemembersARepentantSourceThroughBlocked(t *testing.T) {
	list, advance := newTestBanList(t, 1, time.Minute, 8*time.Minute)
	ip := net.ParseIP("192.0.2.70")

	if imposed, _ := list.fail(ip); !imposed {
		t.Fatal("the first failure did not ban the source")
	}
	_, first := list.blocked(ip)
	if first > time.Minute || first <= 0 {
		t.Fatalf("the first ban lasts %s, want about a minute", first)
	}

	// Sit the ban out, as a repeat offender does, then come back and fail again.
	advance(time.Minute + time.Second)
	if banned, _ := list.blocked(ip); banned {
		t.Fatal("the source is still banned after its ban expired")
	}
	if imposed, bans := list.fail(ip); !imposed || bans != 2 {
		t.Fatalf("the second failure imposed=%v ban number %d, want ban number 2", imposed, bans)
	}
	_, second := list.blocked(ip)
	if second < 2*time.Minute-time.Second {
		t.Fatalf("the second ban lasts %s, want about twice the first", second)
	}

	// The memory is bounded: once a window has passed since the ban ended, the
	// source is forgotten and starts over.
	advance(2*time.Minute + banWindow + time.Second)
	if banned, _ := list.blocked(ip); banned {
		t.Fatal("the source is still banned")
	}
	if imposed, bans := list.fail(ip); !imposed || bans != 1 {
		t.Fatalf("after the memory window the count is %d, want a fresh ban number 1", bans)
	}
}

func TestBanListSuccessClearsTheFailures(t *testing.T) {
	list, _ := newTestBanList(t, 3, time.Minute, time.Hour)
	ip := net.ParseIP("192.0.2.40")

	list.fail(ip)
	list.fail(ip)
	list.succeed(ip)

	if _, count := list.fail(ip); count != 1 {
		t.Errorf("after a success the next failure is number %d, want 1", count)
	}
	if list.size() != 1 {
		t.Errorf("the list tracks %d sources, want 1", list.size())
	}
}

func TestBanListNeverBansAnIgnoredSource(t *testing.T) {
	list, _ := newTestBanList(t, 1, time.Minute, time.Hour, "127.0.0.0/8", "::1/128")
	for _, address := range []string{"127.0.0.1", "127.9.9.9", "::1"} {
		ip := net.ParseIP(address)
		for attempt := 0; attempt < 5; attempt++ {
			if imposed, _ := list.fail(ip); imposed {
				t.Fatalf("%s was banned although it is ignored", address)
			}
		}
		if banned, _ := list.blocked(ip); banned {
			t.Fatalf("%s is banned although it is ignored", address)
		}
	}

	// A source outside the ignored ranges is still banned.
	if imposed, _ := list.fail(net.ParseIP("192.0.2.50")); !imposed {
		t.Error("a source outside ban_ignore_cidrs was not banned")
	}
	if list.size() != 1 {
		t.Errorf("the ignored sources were tracked: size %d", list.size())
	}
}

func TestBanListRejectsAnInvalidIgnoredRange(t *testing.T) {
	if _, err := newBanList(1, time.Minute, time.Hour, []string{"not-a-cidr"}); err == nil {
		t.Error("an invalid ban_ignore_cidrs entry was accepted")
	}
}

func TestBanListCollectDropsExpiredEntries(t *testing.T) {
	list, advance := newTestBanList(t, 5, time.Minute, time.Hour)
	ip := net.ParseIP("192.0.2.60")

	list.fail(ip)
	list.fail(ip)
	if list.size() != 1 {
		t.Fatalf("the list tracks %d sources, want 1", list.size())
	}

	list.collect()
	if list.size() != 1 {
		t.Error("a source inside its window was collected")
	}

	advance(banWindow + time.Second)
	list.collect()
	if list.size() != 0 {
		t.Errorf("the list still tracks %d sources after the window", list.size())
	}
}

func TestBanListIgnoresAnUnparseableSource(t *testing.T) {
	list, _ := newTestBanList(t, 1, time.Minute, time.Hour)
	if imposed, _ := list.fail(nil); imposed {
		t.Error("a nil source was banned")
	}
	if banned, _ := list.blocked(nil); banned {
		t.Error("a nil source is banned")
	}
}
