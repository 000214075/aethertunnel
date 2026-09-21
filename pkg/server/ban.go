package server

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// banWindow is how long a source may accumulate authentication failures before
// its counter starts over. It is deliberately longer than a ban, so a source that
// keeps failing while banned reaches the next ban quickly.
const banWindow = 10 * time.Minute

// banList refuses sources that keep failing authentication.
//
// The first failure starts an observation window; ban_after_failures failures
// inside that window ban the source for ban_seconds. A source that is banned and
// fails again gets a ban twice as long as the previous one, up to
// ban_max_seconds, so waiting out one timeout does not help. Entries that have
// neither failures nor a ban left are dropped, so the list is bounded by the
// sources that are failing now rather than by everything that has ever connected.
type banList struct {
	mu      sync.Mutex
	entries map[string]*banEntry
	ignore  []*net.IPNet

	threshold int
	initial   time.Duration
	max       time.Duration
	window    time.Duration

	// now is time.Now in production and a controllable clock in tests.
	now func() time.Time
}

type banEntry struct {
	failures    int
	windowStart time.Time
	bannedUntil time.Time
	bans        int
}

// newBanList compiles the ban settings. A threshold of zero or less disables the
// list, which is what the returned nil means.
func newBanList(threshold int, duration, max time.Duration, ignoreCIDRs []string) (*banList, error) {
	if threshold <= 0 {
		return nil, nil
	}
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	if max < duration {
		max = duration
	}

	ignore := make([]*net.IPNet, 0, len(ignoreCIDRs))
	for _, entry := range ignoreCIDRs {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		_, network, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid ban_ignore_cidrs entry %q: %w", entry, err)
		}
		ignore = append(ignore, network)
	}

	return &banList{
		entries:   make(map[string]*banEntry),
		ignore:    ignore,
		threshold: threshold,
		initial:   duration,
		max:       max,
		window:    banWindow,
		now:       time.Now,
	}, nil
}

// enabled reports whether the list is active.
func (b *banList) enabled() bool { return b != nil }

func (b *banList) ignored(ip net.IP) bool {
	for _, network := range b.ignore {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// blocked reports whether a source is banned, and how much of its ban is left.
func (b *banList) blocked(ip net.IP) (bool, time.Duration) {
	if b == nil || ip == nil || b.ignored(ip) {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	key := ip.String()
	entry, ok := b.entries[key]
	if !ok {
		return false, 0
	}

	now := b.now()
	if entry.bannedUntil.After(now) {
		return true, entry.bannedUntil.Sub(now)
	}
	if b.obsolete(entry, now) {
		delete(b.entries, key)
	}
	return false, 0
}

// fail records one authentication failure from a source. It reports whether this
// failure imposed a ban, and how many failures the source has accumulated in the
// current window.
func (b *banList) fail(ip net.IP) (bool, int) {
	if b == nil || ip == nil || b.ignored(ip) {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	key := ip.String()
	entry, ok := b.entries[key]
	if !ok {
		entry = &banEntry{}
		b.entries[key] = entry
	}

	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > b.window {
		entry.windowStart = now
		entry.failures = 0
	}
	entry.failures++

	if entry.failures < b.threshold {
		return false, entry.failures
	}

	// Double the previous ban, so a source that keeps failing is removed for
	// longer each time without an unbounded growth of the ban duration.
	entry.bans++
	duration := b.initial
	for i := 1; i < entry.bans && duration < b.max; i++ {
		duration *= 2
	}
	if duration > b.max {
		duration = b.max
	}
	entry.bannedUntil = now.Add(duration)
	entry.failures = 0
	entry.windowStart = time.Time{}

	return true, entry.bans
}

// succeed clears a source's failures, which is what a valid authentication does.
// A ban is not lifted by a success: the source already proved it was abusive.
func (b *banList) succeed(ip net.IP) {
	if b == nil || ip == nil || b.ignored(ip) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	key := ip.String()
	entry, ok := b.entries[key]
	if !ok {
		return
	}
	entry.failures = 0
	entry.windowStart = time.Time{}
	if b.obsolete(entry, b.now()) {
		delete(b.entries, key)
	}
}

// obsolete reports whether an entry carries no information worth keeping.
func (b *banList) obsolete(entry *banEntry, now time.Time) bool {
	if entry.bannedUntil.After(now) {
		return false
	}
	if entry.failures == 0 {
		return true
	}
	return now.Sub(entry.windowStart) > b.window
}

// banned reports how many sources are banned right now.
func (b *banList) banned() int {
	if b == nil {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	count := 0
	for _, entry := range b.entries {
		if entry.bannedUntil.After(now) {
			count++
		}
	}
	return count
}

// size reports how many sources the list tracks, banned or not.
func (b *banList) size() int {
	if b == nil {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// collect drops entries that no longer carry a failure or a ban.
func (b *banList) collect() {
	if b == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	for key, entry := range b.entries {
		if b.obsolete(entry, now) {
			delete(b.entries, key)
		}
	}
}
