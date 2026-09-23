package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// ProxyGroup is the published endpoint for one proxy name.
//
// It holds every client that registered that name. One member is the common case
// and behaves exactly like a single tunnel; several members, which requires each
// of them to declare the same group, are balanced according to
// [server].load_balance. Keeping the endpoint and its members in separate types
// is what lets a listener outlive the client that first created it.
type ProxyGroup struct {
	Name    string
	Type    string
	Private bool

	Domains         []string
	SecretKey       string
	AuthMethod      string
	SecretPublicKey []byte
	RemotePort      int
	Group           string
	Multipath       int

	manager     *TunnelManager
	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration
	strategy    string

	mu sync.RWMutex

	// banditRing rotates which candidate is examined first, so two untried members are
	// both reached when streams arrive one after another.
	banditRing atomic.Uint64
	members    []*Tunnel

	// allowVisitor and denyVisitor are the proxy's own visitor filters, compiled
	// from [[proxies]] allow_cidrs and deny_cidrs.
	allowVisitor []*net.IPNet
	denyVisitor  []*net.IPNet

	// policyAllow and policyDeny are the lists the server enforces for this name,
	// from [[proxies]] in the server configuration. A visitor passes both these and
	// the proxy's own lists, so a client cannot widen what the operator allows.
	policyAllow []*net.IPNet
	policyDeny  []*net.IPNet

	// endpointMu guards the published endpoint. It is separate from mu because the
	// dashboard reads Addr while the control path may be binding or closing the
	// endpoint, and neither should wait for the other's list snapshot.
	endpointMu sync.RWMutex
	listener   net.Listener
	packet     net.PacketConn
	pump       *flynet.DatagramPump
	vhost      *vhostBinding

	proxyOnce sync.Once
	proxy     *httputil.ReverseProxy

	counter atomic.Uint64
	once    sync.Once
	done    chan struct{}
}

// Tunnel is one client's registration of a proxy: a member of a group.
type Tunnel struct {
	Name       string
	Spec       protocol.ProxySpec
	Type       string
	RemotePort int
	Session    *Session

	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration

	group *ProxyGroup

	// dialLatency is an exponentially weighted average of how long this member
	// took to answer a request for a stream, in nanoseconds. The latency and
	// adaptive strategies use it, and zero means the member has never answered.
	dialLatency atomic.Int64

	// failures counts consecutive failed requests for a stream. A success resets
	// it, so it measures the member's present state rather than its history.
	failures atomic.Int64

	// banditPulls and banditReward are what the UCB1 strategy learns from: how many
	// streams this member was asked for, and the total reward they earned. A stream's
	// reward is how quickly the member answered (1 for an immediate answer, falling
	// towards 0 as it takes longer); a failure earns nothing.
	banditPulls  atomic.Uint64
	banditReward atomicFloat64

	Active   atomic.Int64
	Total    atomic.Int64
	BytesIn  atomic.Int64 // received from the client (its service's replies)
	BytesOut atomic.Int64 // sent to the client (what the visitor sent)

	closed atomic.Bool
}

// Closed reports whether this member has been removed from its group.
func (t *Tunnel) Closed() bool { return t.closed.Load() }

// healthy reports whether this member can still provide a stream.
func (t *Tunnel) healthy() bool {
	if t.closed.Load() || t.Session == nil || t.Session.IsClosed() {
		return false
	}
	if t.group != nil && t.group.manager != nil && t.group.manager.sessions != nil {
		_, ok := t.group.manager.sessions.Get(t.Session.ID)
		return ok
	}
	return true
}

// Addr reports the address the group is published on, or "" when it has none.
func (t *Tunnel) Addr() string {
	if t.group == nil {
		return ""
	}
	return t.group.Addr()
}

// Latency reports the member's observed response time.
func (t *Tunnel) Latency() time.Duration { return time.Duration(t.dialLatency.Load()) }

// PublicPort is the port this member's name is actually reachable on.
//
// A group owns one endpoint, taken from its first member: a later member that asks
// for a different port is still pooled, and its own request is not honoured. The
// port to report is therefore the group's, not the one this member asked for —
// otherwise the client would be told a port nothing is listening on. A proxy
// outside a group is reachable on the port it asked for.
func (t *Tunnel) PublicPort() int {
	if t.group != nil {
		return t.group.RemotePort
	}
	return t.RemotePort
}

// atomicFloat64 is a float64 that can be updated from several stream goroutines.
type atomicFloat64 struct{ bits atomic.Uint64 }

func (a *atomicFloat64) Add(delta float64) {
	for {
		old := a.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if a.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

func (a *atomicFloat64) Load() float64 { return math.Float64frombits(a.bits.Load()) }

// --- group lifecycle ----------------------------------------------------------

func newProxyGroup(spec protocol.ProxySpec, manager *TunnelManager) *ProxyGroup {
	policyAllow, policyDeny := manager.policies.visitorRules(spec.Name)
	return &ProxyGroup{
		Name:         spec.Name,
		Type:         spec.Type,
		Private:      config.IsPrivateProxyType(spec.Type),
		Domains:      append([]string(nil), spec.Domains...),
		SecretKey:    spec.SecretKey,
		AuthMethod:   spec.AuthMethod,
		RemotePort:   spec.RemotePort,
		Group:        spec.Group,
		Multipath:    spec.Multipath,
		allowVisitor: compileCIDRs(spec.AllowCIDRs),
		denyVisitor:  compileCIDRs(spec.DenyCIDRs),
		policyAllow:  policyAllow,
		policyDeny:   policyDeny,
		manager:      manager,
		logger:       manager.logger,
		cipher:       manager.cipher,
		metrics:      manager.metrics,
		idleTimeout:  time.Duration(manager.cfg.Server.ReadTimeoutSecs) * time.Second,
		dialTimeout:  time.Duration(manager.cfg.Server.DialTimeoutSecs) * time.Second,
		strategy:     manager.cfg.Server.LoadBalance,
		done:         make(chan struct{}),
	}
}

// compileCIDRs parses a list config validation has already accepted.
func compileCIDRs(entries []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		if _, network, err := net.ParseCIDR(strings.TrimSpace(entry)); err == nil {
			nets = append(nets, network)
		}
	}
	return nets
}

// ipOfAddr extracts the address of a peer, whatever form the caller has it in.
func ipOfAddr(addr net.Addr) net.IP {
	switch typed := addr.(type) {
	case *net.TCPAddr:
		return typed.IP
	case *net.UDPAddr:
		return typed.IP
	default:
		if addr == nil {
			return nil
		}
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}

// Addr returns the address the group is published on.
func (g *ProxyGroup) Addr() string {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	switch {
	case g.listener != nil:
		return g.listener.Addr().String()
	case g.packet != nil:
		return g.packet.LocalAddr().String()
	default:
		return ""
	}
}

// vhostBinding returns the group's HTTP hostname binding, if it has one.
func (g *ProxyGroup) vhostBinding() *vhostBinding {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	return g.vhost
}

// datagramPump returns the pump serving a udp proxy, if it has one.
func (g *ProxyGroup) datagramPump() *flynet.DatagramPump {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	return g.pump
}

// published reports whether the group has been given its endpoint: a listener, a
// packet socket or a hostname binding.
func (g *ProxyGroup) published() bool {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	return g.listener != nil || g.packet != nil || g.vhost != nil
}

// Members returns a snapshot of the group's members, oldest first.
func (g *ProxyGroup) Members() []*Tunnel {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]*Tunnel(nil), g.members...)
}

func (g *ProxyGroup) memberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.members)
}

// add registers a member, binding the group's public endpoint the first time.
func (g *ProxyGroup) add(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	member := &Tunnel{
		Name:        spec.Name,
		Spec:        spec,
		Type:        spec.Type,
		RemotePort:  spec.RemotePort,
		Session:     session,
		logger:      g.logger,
		cipher:      g.cipher,
		metrics:     g.metrics,
		idleTimeout: g.idleTimeout,
		dialTimeout: g.dialTimeout,
		group:       g,
	}

	g.mu.Lock()

	// A session re-registering its own proxy replaces its previous member; that
	// is what a reconnecting client does.
	for index, existing := range g.members {
		if existing.Session != session {
			continue
		}
		existing.closed.Store(true)
		g.members[index] = member
		g.mu.Unlock()
		g.logger.Printf("proxy %q: session %s replaced its own registration", g.Name, session.ID)
		return member, nil
	}

	if len(g.members) > 0 {
		first := g.members[0]
		if g.Group == "" || spec.Group != g.Group {
			g.mu.Unlock()
			return nil, fmt.Errorf(
				"proxy %q is already published by session %s; set the same group on both clients to pool them",
				g.Name, first.Session.ID)
		}
		if spec.SecretKey == "" && g.SecretKey != "" {
			g.mu.Unlock()
			return nil, errors.New("a pool of private proxies requires the same secret_key from every member")
		}
		if g.SecretKey != "" && !g.matchesSecret(spec.SecretKey) {
			g.mu.Unlock()
			return nil, errors.New("a pool of private proxies requires the same secret_key from every member")
		}
		if spec.LocalAddr == "" {
			g.mu.Unlock()
			return nil, errors.New("a pool member needs a local_addr")
		}
	}

	g.members = append(g.members, member)
	first := len(g.members) == 1
	g.mu.Unlock()

	if first {
		if err := g.bind(); err != nil {
			g.mu.Lock()
			g.members = g.members[:0]
			g.mu.Unlock()
			return nil, err
		}
	} else if binding := g.vhostBinding(); binding != nil {
		// A pooled http tunnel may add hostnames the first member did not use.
		if err := binding.extend(spec.Domains); err != nil {
			g.mu.Lock()
			g.members = g.members[:len(g.members)-1]
			g.mu.Unlock()
			return nil, err
		}
	}

	return member, nil
}

// remove drops one member and closes the endpoint once the last one is gone.
//
// removed reports whether the member was in the group, and empty whether no member
// is left: a pool that loses one of its members keeps the name published, and the
// caller has to tell the two cases apart to know whether the endpoint and the DHT
// record still belong to somebody.
func (g *ProxyGroup) remove(member *Tunnel) (removed, empty bool) {
	member.closed.Store(true)

	g.mu.Lock()
	for index, existing := range g.members {
		if existing != member {
			continue
		}
		g.members = append(g.members[:index], g.members[index+1:]...)
		removed = true
		break
	}
	empty = len(g.members) == 0
	g.mu.Unlock()

	if empty {
		g.close("no member is left")
	}
	return removed, empty
}

// close releases the group's endpoint exactly once.
//
// The endpoint is detached under the lock and torn down outside it: closing a
// listener unblocks its accept loop, and that loop must not need the lock to
// observe the group's done channel.
func (g *ProxyGroup) close(reason string) {
	g.once.Do(func() {
		close(g.done)

		g.endpointMu.Lock()
		listener, packet, pump, binding := g.listener, g.packet, g.pump, g.vhost
		g.listener, g.packet, g.pump, g.vhost = nil, nil, nil, nil
		g.endpointMu.Unlock()

		if listener != nil {
			_ = listener.Close()
		}
		if packet != nil {
			_ = packet.Close()
		}
		if pump != nil {
			pump.Shutdown()
		}
		if binding != nil {
			binding.remove()
		}
		g.logger.Printf("proxy %q closed (%s)", g.Name, reason)
	})
}

// bind gives the group the endpoint its type calls for.
func (g *ProxyGroup) bind() error {
	switch g.Type {
	case protocol.ProxyTypeTCP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.Server.BindAddr, strconv.Itoa(g.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s: %w", g.Name, addr, err)
		}
		g.endpointMu.Lock()
		g.listener = listener
		g.endpointMu.Unlock()
		go g.acceptLoop()
		g.logger.Printf("proxy %q (tcp) published on %s", g.Name, addr)

	case protocol.ProxyTypeUDP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.Server.BindAddr, strconv.Itoa(g.RemotePort))
		packet, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s/udp: %w", g.Name, addr, err)
		}
		g.endpointMu.Lock()
		g.packet = packet
		g.endpointMu.Unlock()
		g.startUDP()
		g.logger.Printf("proxy %q (udp) published on %s", g.Name, addr)

	case protocol.ProxyTypeHTTP, protocol.ProxyTypeHTTPS:
		if g.manager.vhost == nil {
			return fmt.Errorf("proxy %q is type %s but server.http_port is not configured", g.Name, g.Type)
		}
		binding, err := g.manager.vhost.add(g)
		if err != nil {
			return err
		}
		g.endpointMu.Lock()
		g.vhost = binding
		g.endpointMu.Unlock()

	case protocol.ProxyTypeSTCP, protocol.ProxyTypeSUDP, protocol.ProxyTypeXTCP:
		if g.AuthMethod == config.AuthMethodNIZK {
			public, err := crypto.SchnorrPublicKey([]byte(g.SecretKey))
			if err != nil {
				return fmt.Errorf("proxy %q: cannot derive the proof key: %w", g.Name, err)
			}
			g.SecretPublicKey = public
		}
		g.logger.Printf("proxy %q (%s) registered as private, reachable by visitors", g.Name, g.Type)

	case protocol.ProxyTypeSOCKS:
		if g.RemotePort == 0 {
			return fmt.Errorf("proxy %q: a socks5 endpoint needs a remote port", g.Name)
		}
		addr := net.JoinHostPort(g.manager.cfg.Server.BindAddr, strconv.Itoa(g.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s: %w", g.Name, addr, err)
		}
		g.endpointMu.Lock()
		g.listener = listener
		g.endpointMu.Unlock()
		go g.acceptLoop()
		g.logger.Printf("proxy %q (socks5) published on %s", g.Name, addr)

	default:
		return fmt.Errorf("proxy type %q has no binding", g.Type)
	}
	return nil
}

// --- per-proxy visitor access control ------------------------------------------

// visitorAllowed reports whether a visitor source address may use this proxy, and
// why not when it may not.
//
// [server] allow_cidrs and deny_cidrs decide who may reach the server at all; the
// lists on a proxy decide which of those visitors may use this particular tunnel,
// so a server that publishes one proxy to the world can keep another to a single
// network. The server's own [[proxies]] policy is applied first: it is the
// operator's rule, and it holds whatever the registering client asked for.
func (g *ProxyGroup) visitorAllowed(remote net.Addr) (bool, string) {
	if len(g.allowVisitor) == 0 && len(g.denyVisitor) == 0 && len(g.policyAllow) == 0 && len(g.policyDeny) == 0 {
		return true, ""
	}

	ip := ipOfAddr(remote)
	if ip == nil {
		// A source that cannot be parsed is refused as soon as a rule exists.
		return false, "the source address cannot be parsed"
	}

	if allowed, reason := visitorRulesAllow(g.policyAllow, g.policyDeny, ip); !allowed {
		return false, "source address rejected by the server's policy for " + g.Name + ": " + reason
	}
	if allowed, reason := visitorRulesAllow(g.allowVisitor, g.denyVisitor, ip); !allowed {
		return false, "source address rejected by the proxy's allow/deny lists: " + reason
	}
	return true, ""
}

// visitorRulesAllow applies one pair of lists to an address. Deny wins over allow,
// and an empty allow list admits everything the deny list does not name.
func visitorRulesAllow(allow, deny []*net.IPNet, ip net.IP) (bool, string) {
	for _, network := range deny {
		if network.Contains(ip) {
			return false, "it is in the deny list"
		}
	}
	if len(allow) == 0 {
		return true, ""
	}
	for _, network := range allow {
		if network.Contains(ip) {
			return true, ""
		}
	}
	return false, "it is not in the allow list"
}

// refuseVisitor records and reports a visitor that the proxy's rules excluded.
func (g *ProxyGroup) refuseVisitor(remote net.Addr, detail string) {
	g.metrics.visitorDenied.Add(1)
	g.manager.auditor.Record(AuditEvent{
		Event: EventProxyVisitorDenied, Proxy: g.Name, Remote: remote.String(),
		Outcome: "denied", Detail: detail,
	})
	g.logger.Printf("proxy %q: visitor %s refused: %s", g.Name, remote, detail)
}

// matchesSecret compares a visitor's secret with the group's in constant time.
func (g *ProxyGroup) matchesSecret(presented string) bool {
	return matchesSecret(g.SecretKey, presented)
}

// --- member selection ---------------------------------------------------------

// pick chooses the member that should serve the next visitor.
func (g *ProxyGroup) pick() *Tunnel { return g.pickExcluding(nil) }

// pickExcluding chooses among the healthy members that have not been tried, and
// falls back to every member when nothing is healthy so that a failing pool still
// reports a real error instead of silently dropping the visitor.
func (g *ProxyGroup) pickExcluding(tried map[*Tunnel]bool) *Tunnel {
	g.mu.RLock()
	candidates := make([]*Tunnel, 0, len(g.members))
	fallback := make([]*Tunnel, 0, len(g.members))
	for _, member := range g.members {
		if tried != nil && tried[member] {
			continue
		}
		fallback = append(fallback, member)
		if member.healthy() {
			candidates = append(candidates, member)
		}
	}
	g.mu.RUnlock()

	if len(candidates) == 0 {
		candidates = fallback
	}
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	switch g.strategy {
	case config.LoadBalanceRandom:
		return candidates[rand.Intn(len(candidates))]

	case config.LoadBalanceLatency:
		best := candidates[0]
		for _, member := range candidates[1:] {
			if member.dialLatency.Load() < best.dialLatency.Load() {
				best = member
			}
		}
		return best

	case config.LoadBalanceFailover:
		// Members are kept in registration order, so the first one wins until
		// it stops being healthy.
		return candidates[0]

	case config.LoadBalanceAdaptive:
		return g.pickByCost(candidates)

	case config.LoadBalanceBandit:
		return g.pickByUCB(candidates)

	default: // round-robin
		n := g.counter.Add(1) - 1
		return candidates[int(n%uint64(len(candidates)))]
	}
}

// pickByUCB returns the member a UCB1 multi-armed bandit would sample: the one with
// the highest estimated reward plus an exploration term. Every member is tried once
// before any is tried twice, which is what keeps a cold member from being passed over
// and, later, what lets a member that had a bad run be measured again.
//
// The estimates come from the streams the pool actually served, so the strategy learns
// online: nothing is trained offline and nothing has to be configured.
func (g *ProxyGroup) pickByUCB(candidates []*Tunnel) *Tunnel {
	var total uint64
	for _, member := range candidates {
		total += member.banditPulls.Load()
	}

	// An untried member first, starting at a different place each time so that two
	// fresh members are both reached when streams arrive one after another.
	attempt := g.banditRing.Add(1)
	start := int((attempt - 1) % uint64(len(candidates)))
	for offset := 0; offset < len(candidates); offset++ {
		member := candidates[(start+offset)%len(candidates)]
		if member.banditPulls.Load() == 0 {
			return member
		}
	}

	// Every so often, measure the member with the fewest observations instead of the
	// best-scoring one. The exploration term alone is not enough for this: with a wide
	// gap in estimated reward it grows far too slowly to close it, so a member that was
	// slow for a while would never be measured again even after it recovered. The
	// adaptive strategy caps its failure penalty for the same reason.
	if attempt%banditRecoveryInterval == 0 {
		// Fewest observations first, and among members observed equally often the one with
		// the worst estimate: that is the member whose information is most likely to be
		// stale, which is the case this probe exists for.
		chosen := candidates[0]
		for _, member := range candidates[1:] {
			switch {
			case member.banditPulls.Load() < chosen.banditPulls.Load():
				chosen = member
			case member.banditPulls.Load() == chosen.banditPulls.Load() &&
				meanReward(member) < meanReward(chosen):
				chosen = member
			}
		}
		return chosen
	}

	best := candidates[0]
	bestScore := ucbScore(best, total)
	for _, member := range candidates[1:] {
		if score := ucbScore(member, total); score > bestScore {
			best, bestScore = member, score
		}
	}
	return best
}

// meanReward is the member's average reward per stream so far, and zero for a member
// that has never answered.
func meanReward(member *Tunnel) float64 {
	pulls := member.banditPulls.Load()
	if pulls == 0 {
		return 0
	}
	return member.banditReward.Load() / float64(pulls)
}

// ucbScore is the UCB1 index of one member: its mean reward so far plus the exploration
// term, which shrinks as the member is used and grows as the pool is used as a whole.
func ucbScore(member *Tunnel, total uint64) float64 {
	pulls := member.banditPulls.Load()
	if pulls == 0 {
		return math.Inf(1)
	}
	mean := member.banditReward.Load() / float64(pulls)
	explore := math.Sqrt(2 * math.Log(float64(total)+1) / float64(pulls))
	return mean + explore
}

// banditRewardFor turns how long a member took into a reward in (0, 1]. An immediate
// answer earns 1, and the reward halves for every reference interval the answer takes,
// so a member that is twice as fast is clearly better without any single slow stream
// dominating the estimate.
func banditRewardFor(elapsed time.Duration) float64 {
	return 1 / (1 + elapsed.Seconds()/banditRewardReference)
}

// banditRewardReference is the response time that halves a stream's reward.
const banditRewardReference = 0.05

// banditRecoveryInterval is how many picks pass between two that go to the member with
// the fewest observations, so a member that was slow for a while is measured again.
const banditRecoveryInterval = 20

// pickByCost returns the member with the lowest cost, breaking ties by rotating
// through the candidates so that equal members share the load.
//
// Cost is the member's moving-average response time multiplied by a penalty for its
// consecutive failures. A member that has never answered a stream has no average and
// therefore the lowest possible cost: it is tried first, which is how a pool learns
// what a new member costs.
func (g *ProxyGroup) pickByCost(candidates []*Tunnel) *Tunnel {
	start := int((g.counter.Add(1) - 1) % uint64(len(candidates)))

	best := candidates[start]
	bestScore := best.cost()
	for offset := 1; offset < len(candidates); offset++ {
		candidate := candidates[(start+offset)%len(candidates)]
		if score := candidate.cost(); score < bestScore {
			best, bestScore = candidate, score
		}
	}
	return best
}

// cost is the adaptive strategy's estimate of what one stream through this member
// costs, in nanoseconds. An unmeasured member scores zero.
func (t *Tunnel) cost() int64 {
	latency := t.dialLatency.Load()
	if latency <= 0 {
		return 0
	}

	// The penalty is capped so that a member with a long outage is still tried:
	// without a cap it could never recover, because every other member would
	// outscore it forever.
	failures := t.failures.Load()
	if failures > maxCostFailures {
		failures = maxCostFailures
	}
	return latency * (1 + 2*failures)
}

// maxCostFailures bounds how many consecutive failures are counted against a member.
const maxCostFailures = 4

// --- tcp ----------------------------------------------------------------------

func (g *ProxyGroup) acceptLoop() {
	// The listener is captured once: closing the group clears the field, and the
	// loop has to keep serving the endpoint it was started for until it closes.
	g.endpointMu.RLock()
	listener := g.listener
	g.endpointMu.RUnlock()
	if listener == nil {
		return
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-g.done:
				return
			default:
			}
			g.logger.Printf("proxy %q: accept error: %v", g.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go g.serveVisit(conn)
	}
}

// serveVisit handles one accepted public connection.
//
// A socks5 endpoint speaks the SOCKS5 greeting before anything is dialled, so it
// has its own handler; every other type is handed straight to serveVisit.
func (g *ProxyGroup) serveVisit(public net.Conn) {
	if g.Type != protocol.ProxyTypeSOCKS {
		g.serveStream(public)
		return
	}
	defer public.Close()

	if allowed, reason := g.visitorAllowed(public.RemoteAddr()); !allowed {
		g.refuseVisitor(public.RemoteAddr(), reason)
		return
	}

	request, err := socks.ReadRequest(public, g.dialTimeout)
	if err != nil {
		// ReadRequest answers the negotiation itself; a caller that got an error
		// only has to make sure the visitor is not left waiting.
		g.logger.Printf("proxy %q: socks5 request from %s refused: %v", g.Name, public.RemoteAddr(), err)
		g.metrics.visitorDenied.Add(1)
		return
	}

	tried := make(map[*Tunnel]bool)
	attempts := g.memberCount()
	if attempts == 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		member := g.pickExcluding(tried)
		if member == nil {
			break
		}
		tried[member] = true

		stream, err := g.openStreamFor(member, false, request.Target)
		if err != nil {
			lastErr = err
			g.logger.Printf("proxy %q: member %s did not serve %s: %v", g.Name, member.Session.ID, request.Target, err)
			continue
		}

		if err := socks.WriteReply(public, socks.ReplySucceeded); err != nil {
			_ = stream.dc.Close()
			stream.release()
			return
		}
		g.metrics.socksRequests.Add(1)
		_ = member.pipeStream(public, stream.dc, request.Target)
		// The stream is over, so its bookkeeping has to be released here: this is
		// the end of the only path that serves it.
		stream.release()
		return
	}

	if lastErr == nil {
		lastErr = errors.New("no member is available")
	}
	_ = socks.WriteReply(public, socks.ReplyFor(lastErr))
	g.logger.Printf("proxy %q: %s asked for %s and was refused: %v",
		g.Name, public.RemoteAddr(), request.Target, lastErr)
}

// serveStream matches one public connection with a member, moving on to the next
// member when one cannot provide a stream.
func (g *ProxyGroup) serveStream(public net.Conn) {
	if allowed, reason := g.visitorAllowed(public.RemoteAddr()); !allowed {
		g.refuseVisitor(public.RemoteAddr(), reason)
		_ = public.Close()
		return
	}
	tried := make(map[*Tunnel]bool)
	attempts := g.memberCount()
	if attempts == 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		member := g.pickExcluding(tried)
		if member == nil {
			break
		}
		tried[member] = true

		stream, err := g.openStream(member, false)
		if err != nil {
			lastErr = err
			g.logger.Printf("proxy %q: member %s did not answer: %v", g.Name, member.Session.ID, err)
			continue
		}

		if len(tried) > 1 {
			g.logger.Printf("proxy %q: served %s from member %s after %d attempt(s)",
				g.Name, public.RemoteAddr(), member.Session.ID, len(tried))
		}
		_ = member.pipeStream(public, stream.dc, public.RemoteAddr().String())
		// The stream is over, so its bookkeeping has to be released here: this is
		// the end of the only path that serves it.
		stream.release()
		return
	}

	_ = public.Close()
	if lastErr != nil {
		g.logger.Printf("proxy %q: no member could serve %s: %v", g.Name, public.RemoteAddr(), lastErr)
	}
}

// openedStream carries a data connection together with the bookkeeping that
// releases it.
type openedStream struct {
	dc      *dataConn
	release func()
}

// openStream asks one member for a data connection and records how long it took,
// which is what the latency strategy compares.
func (g *ProxyGroup) openStream(member *Tunnel, visitor bool) (*openedStream, error) {
	return g.openStreamFor(member, visitor, "")
}

// openStreamFor asks a member for a stream, optionally naming the address it
// should reach. Only a socks5 proxy passes a target.
func (g *ProxyGroup) openStreamFor(member *Tunnel, visitor bool, target string) (*openedStream, error) {
	started := time.Now()
	dc, release, err := member.openStreamFor(visitor, target)
	if err != nil {
		if errors.Is(err, errServerDraining) {
			// The refusal is the server's decision, not a fault of this member, so
			// it must not count towards the strategy's failure penalty.
			g.metrics.drainRefused.Add(1)
			return nil, err
		}
		member.failures.Add(1)
		member.banditPulls.Add(1)
		return nil, err
	}
	member.failures.Store(0)

	elapsed := time.Since(started).Nanoseconds()
	if previous := member.dialLatency.Load(); previous == 0 {
		member.dialLatency.Store(elapsed)
	} else {
		// A quarter-weight moving average reacts to a change without letting one
		// slow connection redefine the member.
		member.dialLatency.Store(previous - previous/4 + elapsed/4)
	}
	member.banditPulls.Add(1)
	member.banditReward.Add(banditRewardFor(time.Duration(elapsed)))
	return &openedStream{dc: dc, release: release}, nil
}

// --- udp ----------------------------------------------------------------------

// startUDP publishes a udp proxy. Each visitor address gets its own datagram
// session, and a session may be spread over several members when the proxy asks
// for more than one path.
func (g *ProxyGroup) startUDP() {
	paths := g.Multipath
	if paths < 1 {
		paths = 1
	}

	g.endpointMu.RLock()
	packet := g.packet
	g.endpointMu.RUnlock()

	pump := &flynet.DatagramPump{
		Socket: packet,
		Paths:  paths,
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			if allowed, reason := g.visitorAllowed(addr); !allowed {
				g.refuseVisitor(addr, reason)
				return nil, nil, errors.New("the visitor source is not allowed by this proxy")
			}
			member := g.pick()
			if member == nil {
				return nil, nil, errors.New("no member is available")
			}
			stream, err := g.openStream(member, false)
			if err != nil {
				return nil, nil, err
			}
			return stream.dc.framer, func() {
				_ = stream.dc.Close()
				stream.release()
			}, nil
		},
		Close: func(addr net.Addr, toPeer, fromPeer int64) {
			g.recordSession(toPeer, fromPeer)
		},
		OnDatagram:  func(toPeer bool, n int) { g.metrics.udpDatagrams.Add(1) },
		OnSession:   func(delta int) { g.metrics.udpSessions.Add(int64(delta)) },
		IdleTimeout: g.idleTimeout,
		Logger:      g.logger,
	}
	g.endpointMu.Lock()
	g.pump = pump
	g.endpointMu.Unlock()
	pump.Start()
}

// recordSession books the traffic of one finished datagram session against the
// group's members in proportion, so the dashboard and the metrics stay consistent
// without attributing one session to a single member it may not have used alone.
func (g *ProxyGroup) recordSession(toPeer, fromPeer int64) {
	members := g.Members()
	if len(members) == 0 {
		return
	}
	shareOut, shareIn := toPeer/int64(len(members)), fromPeer/int64(len(members))
	for _, member := range members {
		member.BytesOut.Add(shareOut)
		member.BytesIn.Add(shareIn)
		member.Total.Add(1)
		member.Session.RecordTraffic(shareOut, shareIn)
	}
	g.metrics.recordStream(g.Name, toPeer, fromPeer)
}

// openForVisitor asks one member for a data connection, trying the others when
// one cannot answer.
func (g *ProxyGroup) openForVisitor() (*Tunnel, *openedStream, error) {
	tried := make(map[*Tunnel]bool)
	attempts := g.memberCount()
	if attempts == 0 {
		return nil, nil, errors.New("no client is publishing this proxy")
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		member := g.pickExcluding(tried)
		if member == nil {
			break
		}
		tried[member] = true

		stream, err := g.openStream(member, true)
		if err != nil {
			lastErr = err
			continue
		}
		return member, stream, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no client could provide a stream")
	}
	return nil, nil, lastErr
}

// --- http ---------------------------------------------------------------------

// serveHTTP forwards one request to a member's local web server over a fresh
// tunnelled connection, moving on to another member if the first cannot answer.
func (g *ProxyGroup) serveHTTP(w http.ResponseWriter, r *http.Request) {
	proxy := g.httpProxy()
	if proxy == nil {
		http.Error(w, "this proxy has no http handler", http.StatusInternalServerError)
		return
	}

	remote, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
	if err == nil {
		if allowed, reason := g.visitorAllowed(remote); !allowed {
			g.refuseVisitor(remote, reason)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	g.metrics.httpRequests.Add(1)
	g.metrics.tunnel(g.Name).httpRequests.Add(1)
	g.metrics.streamOpened(g.Name)
	defer g.metrics.streamClosed(g.Name)

	counter := &countingResponseWriter{ResponseWriter: w}
	proxy.ServeHTTP(counter, r)

	toClient := r.ContentLength
	if toClient < 0 {
		toClient = 0
	}
	fromClient := counter.written
	g.recordHTTPTraffic(toClient, fromClient)
	g.metrics.recordStream(g.Name, toClient, fromClient)
}

// recordHTTPTraffic books one served request against the group's members.
//
// The members are what the dashboard, the per-session counters and the bandwidth
// ledger read, and the reverse proxy picks whichever member answers — over a
// connection it may keep for several requests — so the bytes of a request are
// credited to every member in proportion. A datagram session spread over several
// paths is booked the same way.
func (g *ProxyGroup) recordHTTPTraffic(toClient, fromClient int64) {
	members := g.Members()
	if len(members) == 0 {
		return
	}
	shareOut, shareIn := toClient/int64(len(members)), fromClient/int64(len(members))
	for _, member := range members {
		member.BytesOut.Add(shareOut)
		member.BytesIn.Add(shareIn)
		member.Total.Add(1)
		member.Session.RecordTraffic(shareOut, shareIn)
	}
}

// httpProxy builds the group's reverse proxy once.
func (g *ProxyGroup) httpProxy() *httputil.ReverseProxy {
	g.proxyOnce.Do(func() {
		if g.Type == protocol.ProxyTypeHTTP || g.Type == protocol.ProxyTypeHTTPS {
			g.proxy = g.buildHTTPProxy()
		}
	})
	return g.proxy
}

func (g *ProxyGroup) buildHTTPProxy() *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			tried := make(map[*Tunnel]bool)
			attempts := g.memberCount()
			if attempts == 0 {
				return nil, errors.New("no member is available")
			}

			var lastErr error
			for attempt := 0; attempt < attempts; attempt++ {
				member := g.pickExcluding(tried)
				if member == nil {
					break
				}
				tried[member] = true

				stream, err := g.openStream(member, false)
				if err != nil {
					lastErr = err
					continue
				}
				// stream.dc.cipher is the key derived for this stream: the
				// configured cipher unless the session agreed a post-quantum key.
				return &tunnelHTTPConn{
					Stream:  crypto.NewStream(stream.dc.conn, stream.dc.cipher),
					conn:    stream.dc.conn,
					tunnel:  member,
					release: stream.release,
				}, nil
			}
			if lastErr == nil {
				lastErr = errors.New("no member could provide a stream")
			}
			return nil, lastErr
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: g.dialTimeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
		// The client's web server chose the encoding; re-encoding here would
		// change the body the visitor receives.
		DisableCompression: true,
	}

	target := &url.URL{Scheme: "http", Host: "tunnel"}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorLog:  log.New(&prefixWriter{logger: g.logger, prefix: "http " + g.Name + ": "}, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.logger.Printf("proxy %q: http request for %s failed: %v", g.Name, r.Host, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// --- totals -------------------------------------------------------------------

// Totals aggregates the group's counters.
func (g *ProxyGroup) Totals() (active, total, in, out int64) {
	for _, member := range g.Members() {
		active += member.Active.Load()
		total += member.Total.Load()
		in += member.BytesIn.Load()
		out += member.BytesOut.Load()
	}
	return active, total, in, out
}

// MemberSummary describes one member for the dashboard and the API.
type MemberSummary struct {
	ClientID  string  `json:"client_id"`
	LocalAddr string  `json:"local_addr"`
	LatencyMS float64 `json:"latency_ms"`
	Failures  int64   `json:"consecutive_failures"`
	Active    int64   `json:"active_connections"`
	Total     int64   `json:"total_connections"`
	BytesIn   int64   `json:"bytes_in"`
	BytesOut  int64   `json:"bytes_out"`
	Healthy   bool    `json:"healthy"`
}

// Summary lists the group's members, oldest first.
func (g *ProxyGroup) Summary() []MemberSummary {
	out := make([]MemberSummary, 0, g.memberCount())
	for _, member := range g.Members() {
		out = append(out, MemberSummary{
			ClientID:  member.Session.ID,
			LocalAddr: member.Spec.LocalAddr,
			LatencyMS: float64(member.Latency().Microseconds()) / 1000,
			Failures:  member.failures.Load(),
			Active:    member.Active.Load(),
			Total:     member.Total.Load(),
			BytesIn:   member.BytesIn.Load(),
			BytesOut:  member.BytesOut.Load(),
			Healthy:   member.healthy(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out
}
