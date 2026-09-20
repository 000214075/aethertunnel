package server

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// AccessControl decides whether an incoming connection may proceed.
//
// Rules are evaluated in this order, and the first one that produces a verdict
// wins: an explicit deny match, then an allow-list (when non-empty), then the
// per-source rate limit. An empty allow list means "allow every source".
type AccessControl struct {
	allow []*net.IPNet
	deny  []*net.IPNet

	limiter *rateLimiter
}

// NewAccessControl compiles the configured CIDR lists. Invalid CIDRs must have
// been rejected by config validation before this point.
func NewAccessControl(allowCIDRs, denyCIDRs []string, perSecond float64, burst int) (*AccessControl, error) {
	parse := func(list []string) ([]*net.IPNet, error) {
		nets := make([]*net.IPNet, 0, len(list))
		for _, entry := range list {
			_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", entry, err)
			}
			nets = append(nets, network)
		}
		return nets, nil
	}

	allow, err := parse(allowCIDRs)
	if err != nil {
		return nil, err
	}
	deny, err := parse(denyCIDRs)
	if err != nil {
		return nil, err
	}

	ac := &AccessControl{allow: allow, deny: deny}
	if perSecond > 0 {
		ac.limiter = newRateLimiter(perSecond, burst)
	}
	return ac, nil
}

// Decision is the outcome of an access-control check.
type Decision int

const (
	// Allow permits the connection.
	Allow Decision = iota
	// DenyCIDR refuses the connection because of the allow/deny lists.
	DenyCIDR
	// DenyRate refuses the connection because the source exceeded its rate.
	DenyRate
)

// Check evaluates the source IP of conn.
func (a *AccessControl) Check(conn net.Conn) Decision {
	return a.CheckAddr(remoteIP(conn))
}

// CheckAddr evaluates an already-parsed source address.
func (a *AccessControl) CheckAddr(ip net.IP) Decision {
	if ip == nil {
		// An unparseable source address is treated as a deny when any rule
		// exists, and as an allow when none does.
		if len(a.allow) > 0 || len(a.deny) > 0 {
			return DenyCIDR
		}
		return Allow
	}

	for _, network := range a.deny {
		if network.Contains(ip) {
			return DenyCIDR
		}
	}
	if len(a.allow) > 0 {
		matched := false
		for _, network := range a.allow {
			if network.Contains(ip) {
				matched = true
				break
			}
		}
		if !matched {
			return DenyCIDR
		}
	}

	if a.limiter != nil && !a.limiter.allow(ip.String()) {
		return DenyRate
	}
	return Allow
}

// remoteIP extracts the IP from a connection's remote address.
func remoteIP(conn net.Conn) net.IP {
	if conn == nil || conn.RemoteAddr() == nil {
		return nil
	}
	switch addr := conn.RemoteAddr().(type) {
	case *net.TCPAddr:
		return addr.IP
	case *net.UDPAddr:
		return addr.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}

// --- per-source token bucket ---------------------------------------------------

type rateLimiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	buckets  map[string]*bucket
	lastGC   time.Time
	gcPeriod time.Duration
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newRateLimiter(perSecond float64, burst int) *rateLimiter {
	if burst <= 0 {
		burst = 1
	}
	return &rateLimiter{
		rate:     perSecond,
		burst:    float64(burst),
		buckets:  make(map[string]*bucket),
		lastGC:   time.Now(),
		gcPeriod: 5 * time.Minute,
	}
}

// allow consumes one token for key, refilling proportionally to elapsed time.
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.collect(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.seen).Seconds()
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.seen = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// collect drops buckets that have been idle long enough to be full again, so a
// long-running server does not retain an entry per source address it has seen.
func (l *rateLimiter) collect(now time.Time) {
	if now.Sub(l.lastGC) < l.gcPeriod {
		return
	}
	l.lastGC = now
	idle := time.Duration(l.burst/l.rate*float64(time.Second)) + time.Minute
	for key, b := range l.buckets {
		if now.Sub(b.seen) > idle {
			delete(l.buckets, key)
		}
	}
}

// Size reports how many buckets are tracked.
func (l *rateLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
