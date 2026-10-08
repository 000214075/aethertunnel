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
	"github.com/aethertunnel/aethertunnel/pkg/logging"
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

	Domains           []string
	Locations         []string
	Subdomain         string
	HostHeaderRewrite string
	HTTPUser          string
	RouteByHTTPUser   string
	HTTPPassword      string
	TLSPassthrough    bool
	RequestHeaders    map[string]string
	ResponseHeaders   map[string]string
	vhostTimeout      time.Duration
	udpPacketSize     int
	tcpmux            *tcpmuxBinding
	sni               *sniBinding
	SecretKey         string
	AuthMethod        string
	SecretPublicKey   []byte
	RemotePort        int
	Group             string
	Multipath         int

	manager     *TunnelManager
	logger      *log.Logger
	cipher      *crypto.Cipher
	metrics     *Metrics
	idleTimeout time.Duration
	dialTimeout time.Duration
	// userConnTimeout bounds the wait for a client to hand back the data
	// connection a published connection asked for; zero means dialTimeout,
	// which is what frp's userConnTimeout bounds separately (its default is 10s).
	userConnTimeout time.Duration
	strategy        string

	mu sync.RWMutex

	// bound is closed once the first member's bind has returned. A member that joins
	// while that bind is still in flight has to wait for it before it can add its own
	// hostnames to the vhost binding: reading the field before bind installed it saw
	// nil and dropped them, so the names 404'd for the life of the registration.
	//
	// boundOnce closes it. remove() closes a group that empties, and a registration
	// that reaches the name before Unregister drops it from the map appends itself to
	// that closed group and binds a second time: a plain deferred close panicked
	// there ("close of closed channel"), taking the server down on a race between an
	// unregistration and a registration.
	bound     chan struct{}
	boundOnce sync.Once
	// bindOK records that the bind returned nil. Closing bound only says the
	// bind ended — a failed bind closes it too, after closing done — so a
	// waiter woken by either channel must read this instead of inferring the
	// outcome from the channel that woke it.
	bindOK atomic.Bool

	// banditRing rotates which candidate is examined first, so two untried members are
	// both reached when streams arrive one after another.
	banditRing atomic.Uint64
	members    []*Tunnel
	// lastMember is the member that most recently left. A datagram session that
	// ends as the group empties — removing the last member shuts the pump down, and
	// its sessions finish then — books its bytes here, because there is no member
	// left in the list to attribute them to and the session being torn down is the
	// one that carried them.
	lastMember *Tunnel

	// allowVisitor and denyVisitor are the proxy's own visitor filters, compiled
	// from [[proxies]] allow_cidrs and deny_cidrs.
	allowVisitor []*net.IPNet
	denyVisitor  []*net.IPNet

	// ownerUser is the identity the publishing client gave in [client].user and
	// allowUsers the identities that may visit this private proxy; an empty
	// allowUsers admits ownerUser alone, which is frp's allow_users.
	ownerUser  string
	allowUsers []string
	// Annotations are the labels the publisher attached to this proxy, shown
	// on the dashboard and read by nothing else.
	Annotations map[string]string
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
	// probeCounter counts picks for the never-answered probe, separately from the
	// counter the strategies use for their own rotation: sharing one would make the
	// probe's cadence depend on which strategy is running.
	probeCounter atomic.Uint64
	once         sync.Once
	done         chan struct{}
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
	// userConnTimeout bounds the wait for the client to hand back a data
	// connection; zero means dialTimeout.
	userConnTimeout time.Duration

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
	// totals is the session's counter for this proxy name, shared with every
	// member that publishes the name. It outlives the member: the ledger bills it
	// at teardown, after the member may have been replaced or withdrawn.
	totals *trafficTotals
	// createdUsage records that this registration created the counter above: a
	// rollback that leaves the name unpublished can then drop it, so the ledger
	// does not bill an empty entry for a name nothing ever served. It must not be
	// set when the counter already existed, because a stream from an earlier
	// registration may still be writing to it.
	createdUsage bool

	closed atomic.Bool
	// reachable records that this registration's name reached a live endpoint,
	// so a visitor stream may already hold totals and write to it. It is what a
	// rollback has to consult before it drops the counter: the safe case for
	// forgetUsage is a registration whose endpoint never came up, and a member
	// that joined a group that was already published is reachable from the
	// moment it is in the member list.
	reachable atomic.Bool
}

// addTraffic books a finished stream, request or datagram session on this member
// and on the session's counter for the proxy name. Both are needed: the
// member's own counters are what the dashboard shows for a live
// registration, and the session's are what the ledger bills for the name.
func (t *Tunnel) addTraffic(toClient, fromClient int64) {
	t.BytesOut.Add(toClient)
	t.BytesIn.Add(fromClient)
	if t.totals != nil {
		t.totals.bytesOut.Add(toClient)
		t.totals.bytesIn.Add(fromClient)
	}
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
		Name:              spec.Name,
		Type:              spec.Type,
		Private:           config.IsPrivateProxyType(spec.Type),
		Domains:           append([]string(nil), spec.Domains...),
		Locations:         append([]string(nil), spec.Locations...),
		Subdomain:         spec.Subdomain,
		HostHeaderRewrite: spec.HostHeaderRewrite,
		HTTPUser:          spec.HTTPUser,
		RouteByHTTPUser:   spec.RouteByHTTPUser,
		HTTPPassword:      spec.HTTPPassword,
		TLSPassthrough:    spec.TLSPassthrough,
		SecretKey:         spec.SecretKey,
		AuthMethod:        spec.AuthMethod,
		RequestHeaders:    spec.RequestHeaders,
		ResponseHeaders:   spec.ResponseHeaders,
		vhostTimeout:      time.Duration(manager.cfg.Server.VhostHTTPTimeout) * time.Second,
		udpPacketSize:     manager.cfg.Server.UDPPacketSize,
		RemotePort:        spec.RemotePort,
		Group:             spec.Group,
		Multipath:         spec.Multipath,
		ownerUser:         spec.User,
		allowUsers:        spec.AllowUsers,
		Annotations:       spec.Annotations,
		allowVisitor:      compileCIDRs(spec.AllowCIDRs),
		denyVisitor:       compileCIDRs(spec.DenyCIDRs),
		policyAllow:       policyAllow,
		policyDeny:        policyDeny,
		manager:           manager,
		logger:            manager.logger,
		cipher:            manager.cipher,
		metrics:           manager.metrics,
		idleTimeout:       time.Duration(manager.cfg.Server.ReadTimeoutSecs) * time.Second,
		dialTimeout:       time.Duration(manager.cfg.Server.DialTimeoutSecs) * time.Second,
		userConnTimeout:   time.Duration(manager.cfg.Server.UserConnTimeoutSeconds) * time.Second,
		strategy:          manager.cfg.Server.LoadBalance,
		done:              make(chan struct{}),
		bound:             make(chan struct{}),
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

// extendHostnames adds a joining member's hostnames to whichever binding routes
// this group by name, and reports the conflict that refuses them.
//
// Every one of the three bindings was built from the first member's list, so a
// second member's own names have to be added here or they never resolve: the
// listener chooses a member by the name the visitor sent, and a name that was
// never registered answers as if no proxy owned it. tcpmux and the TLS
// passthrough listener had no extend at all; both have one now. A group whose
// type has no such binding has nothing to extend.
func (g *ProxyGroup) extendHostnames(domains []string) error {
	// Wait like vhostBinding does: a member that joins while the first bind is
	// still in flight would read nil and drop its names. The wait is not
	// conditional on the joiner having names of its own: one that brought none
	// still has to wake on g.done when the first bind failed, or it returns
	// believing it joined a published group and never reaches the rollback in
	// Register, which is the only thing that takes it out of a dead group.
	select {
	case <-g.bound:
	case <-g.done:
		return nil
	}
	if len(domains) == 0 {
		return nil
	}
	g.endpointMu.RLock()
	vhost, tcpmux, sni := g.vhost, g.tcpmux, g.sni
	g.endpointMu.RUnlock()
	switch {
	case vhost != nil:
		return vhost.extend(domains)
	case tcpmux != nil:
		return tcpmux.set.extend(tcpmux, domains)
	case sni != nil:
		return sni.set.extend(sni, domains)
	}
	return nil
}

// datagramPump returns the pump serving a udp proxy, if it has one.
func (g *ProxyGroup) datagramPump() *flynet.DatagramPump {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	return g.pump
}

// published reports whether the group has been given its endpoint: a listener, a
// packet socket, a hostname binding (vhost, tcpmux or TLS-passthrough).
func (g *ProxyGroup) published() bool {
	g.endpointMu.RLock()
	defer g.endpointMu.RUnlock()
	return g.listener != nil || g.packet != nil || g.vhost != nil || g.tcpmux != nil || g.sni != nil
}

// Members returns a snapshot of the group's members, oldest first.
func (g *ProxyGroup) Members() []*Tunnel {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]*Tunnel(nil), g.members...)
}

// isClosed reports whether close has run: the group's endpoint is gone for
// good and a bind on it would leak the listener it opened.
func (g *ProxyGroup) isClosed() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

func (g *ProxyGroup) memberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.members)
}

// admitVisitor runs the [[http_plugins]] newUserConn webhooks for one visitor
// connection and returns the reason a plugin refused, empty when every plugin
// allows. Every path that serves a visitor goes through it, so a plugin that is
// the operator's gate cannot be bypassed by reaching a proxy over HTTP, the tcpmux
// multiplexer, the TLS passthrough or a UDP datagram instead of a plain tcp accept.
func (g *ProxyGroup) admitVisitor(remote net.Addr) string {
	owner := g.ownerSession()
	if owner == nil {
		return ""
	}
	return g.manager.httpPlugins.runNewUserConn(owner, g, remote.String())
}

// ownerSession names the session that registered this group, for the
// [[http_plugins]] newUserConn webhooks: the webhook is asked about the proxy's
// owner, whichever member will carry this particular stream. Nil when no member
// is up — the visitor then fails on the normal path anyway.
func (g *ProxyGroup) ownerSession() *Session {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, member := range g.members {
		return member.Session
	}
	return nil
}

// add registers a member, binding the group's public endpoint the first time.
func (g *ProxyGroup) add(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	member := &Tunnel{
		Name:            spec.Name,
		Spec:            spec,
		Type:            spec.Type,
		RemotePort:      spec.RemotePort,
		Session:         session,
		logger:          g.logger,
		cipher:          g.cipher,
		metrics:         g.metrics,
		idleTimeout:     g.idleTimeout,
		dialTimeout:     g.dialTimeout,
		userConnTimeout: g.userConnTimeout,
		group:           g,
		// totals is filled in below, once this registration is accepted: creating
		// it here would leave a counter for a name that a refusal path rejects,
		// and the ledger bills the names the session carries, not the ones it
		// was asked for.
	}

	createdUsage := false
	g.mu.Lock()

	// A session re-registering its own proxy replaces its previous member; that
	// is what a reconnecting client does.
	for index, existing := range g.members {
		if existing.Session != session {
			continue
		}
		// A replacement keeps the endpoint the group already bound: nothing
		// below rebinds it, and while this member is the group's only one,
		// Register announces the new spec to the DHT after this returns. A
		// changed remote_port there, or a hostname the group's binding does
		// not serve, would then be announced at an address nothing answers on
		// while the old endpoint keeps serving without anybody pointing at it.
		// In a pool the endpoint belongs to the member that built the group
		// and a later joiner's remote_port was never adopted, so its own
		// replacement moves nothing and is free to name its own port.
		if err := g.checkReplacementEndpoint(spec); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		existing.closed.Store(true)
		// The ledger bills a session's proxies at teardown from the members that
		// are current then, so the bytes the replaced member carried have to move
		// to its replacement: otherwise every reconnect silently drops the usage
		// of the registration that just ended.
		member.BytesIn.Store(existing.BytesIn.Load())
		member.BytesOut.Store(existing.BytesOut.Load())
		// The counters of this proxy name on this session, shared with the member
		// this one replaces and kept after it is withdrawn, because the ledger
		// bills the name, not the registration.
		member.totals, _ = session.usageOf(spec.Name)
		g.members[index] = member
		// It takes the slot of a member of a group that was already published,
		// so a stream can be handed to it from here on.
		member.reachable.Store(true)
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

	// Only now, past every refusal: the name is published, so its bytes belong to
	// the session's ledger for this name.
	member.totals, createdUsage = session.usageOf(spec.Name)
	// Recorded on the member so a rollback in Register — which runs after this
	// returns — can tell a counter this registration created from one an earlier
	// registration of the same name left behind.
	member.createdUsage = createdUsage
	g.members = append(g.members, member)
	first := len(g.members) == 1
	g.mu.Unlock()

	if first {
		if err := g.bind(); err != nil {
			g.mu.Lock()
			g.detachLocked(member)
			g.mu.Unlock()
			if createdUsage {
				// The endpoint never came up, so no visitor can have been served and
				// the counter this attempt created would only be billed as an empty
				// entry at teardown.
				session.forgetUsage(spec.Name)
			}
			// Nothing was published, and a session that joined concurrently
			// must not end up in a group with no endpoint reporting success:
			// closing it sends those joiners through Register's re-check,
			// which rolls them back with an error to retry.
			g.close("publishing the proxy failed")
			return nil, err
		}
		// The endpoint is live: from here on a visitor can be handed a stream
		// for this name, so the counter has to survive every later rollback.
		member.reachable.Store(true)
	} else {
		// A joining member is in the member list of a group that is already
		// published, so a visitor can reach it — and be writing into this
		// session's counter for the name — before its own hostnames are
		// installed below. That is only true once the endpoint is really up.
		select {
		case <-g.bound:
		case <-g.done:
		}
		// Which channel woke this select is not the question — on a failed
		// first bind both are closed (done first, then bound) and either can
		// win. The bind's own outcome decides: only an up endpoint can hand
		// this member a stream, and only then may the counter it created
		// survive. On a dead first bind the mark stays false, and Register's
		// re-check forgets the counter instead of the teardown billing a
		// 0-byte ledger line for a proxy that was never published.
		if g.bindOK.Load() {
			member.reachable.Store(true)
		}
		if err := g.extendHostnames(spec.Domains); err != nil {
			// A pooled member may bring hostnames the first member did not use. The
			// binding that routes by name has to take them, or the joining member's own
			// names would never resolve — and would never be conflict-checked either.
			g.mu.Lock()
			g.detachLocked(member)
			empty := len(g.members) == 0
			g.mu.Unlock()
			// The counter this registration created is not dropped here: add
			// hands the member back with the error, and Register's add-error
			// branch judges it by the same gate its re-check applies —
			// createdUsage and not reachable. A member that woke to a
			// successful bind keeps its mark, so a stream may already be
			// writing to the counter and it survives; one that woke to a bind
			// that did not succeed is forgotten there. forgetUsage is a map
			// delete, so it stays harmless where add's own bind-failure path
			// has already forgotten the name.
			//
			// The bind failure above closes the group unconditionally. This path
			// has to close it when the failure emptied the pool: a member that
			// left while the extend ran removes itself without closing (the pool
			// still held us then), so once our detach takes the last member out
			// nobody is left to close it — the name bindings would stay installed
			// for a group the manager has already dropped, and every future
			// registration of the name would lose the conflict check against
			// those orphaned entries until restart. Close is once-guarded, and a
			// member joining concurrently is rolled back through Register's
			// re-check, exactly as on the bind path.
			if empty {
				g.close("publishing the proxy failed")
			}
			// The member travels with the error so its caller can see
			// createdUsage — the counter's fate is decided there, not here.
			return member, err
		}
	}

	return member, nil
}

// routesByHostname reports whether a proxy type is reached through a shared
// listener that selects the proxy by the hostname the visitor sent, instead of
// through a public port of its own. bind installs a hostname binding for these
// and opens no port, which is why a remote_port on one of them is not honoured
// and must not be counted against a session's port budget either.
func routesByHostname(proxyType string) bool {
	switch proxyType {
	case protocol.ProxyTypeHTTP, protocol.ProxyTypeHTTPS, protocol.ProxyTypeTCPMux:
		return true
	}
	return false
}

// checkReplacementEndpoint refuses a same-session re-registration that changes
// the endpoint the group already bound. mu must be held.
//
// The group keeps its endpoint for good: Register publishes the new spec to the
// DHT after this returns — but only while the group holds this one member, so
// the port rule is that case alone. In a pool the endpoint belongs to the member
// that built the group, a later joiner's own remote_port was never adopted, and
// its replacement publishes nothing, so its port is not held against the
// group's. A hostname the binding does not serve is refused for every member
// alike: a replacement's domains are never installed into the binding, so an
// unserved one would be announced to nobody. Endpoint-neutral changes —
// local_addr, plugins, compression — are what a replacement is for and are not
// drift.
func (g *ProxyGroup) checkReplacementEndpoint(spec protocol.ProxySpec) error {
	// The members counted here do not include the replacement itself: it is
	// still the loop's candidate, not a list entry.
	if len(g.members) == 1 && spec.RemotePort != g.RemotePort {
		return fmt.Errorf("proxy %q is published on remote_port %d and a re-registration may not move it to %d: withdraw the proxy first, or reconnect and publish it again",
			g.Name, g.RemotePort, spec.RemotePort)
	}
	if !routesByHostname(g.Type) {
		return nil
	}
	for _, raw := range spec.Domains {
		domain := normalizeDomain(raw)
		if domain == "" || g.servesDomain(domain) {
			continue
		}
		return fmt.Errorf("proxy %q is not published for hostname %q: withdraw the proxy first, or reconnect and publish it again",
			g.Name, domain)
	}
	return nil
}

// servesDomain reports whether the binding that carries this group routes a
// request for this already-normalized domain to this group. The question a
// replacement is held to is the router's answer for a request, not the group's
// published list alone: every router ranks an exact name over a wildcard —
// vhost's lookup, and the tcpmux and SNI tables, all answer an exact entry
// first — and the routers put exact names and wildcards in separate buckets, so
// "*.example.com" and "example.com" can be published by two proxies at once
// while every request for the base name goes to the exact one. A replacement
// onto such a name would pass a list check the wildcard owns and still be
// answered by the other proxy, so the binding itself is what decides. The
// binding was built from the first member's list, and every later member's own
// hostnames were added to it as it joined — a join whose extension failed left
// the member list again. The three bindings are read under the endpoint lock, as
// published() and extendHostnames read them, and the lookups then take the
// router's own lock the way the extensions extendHostnames drives do.
func (g *ProxyGroup) servesDomain(domain string) bool {
	g.endpointMu.RLock()
	vhost, tcpmux, sni := g.vhost, g.tcpmux, g.sni
	g.endpointMu.RUnlock()
	switch {
	case vhost != nil:
		for _, routed := range vhost.router.lookup(domain) {
			if routed == g {
				return true
			}
		}
		return false
	case tcpmux != nil:
		return tcpmux.set.lookup(domain) == g
	case sni != nil:
		return sni.set.lookup(domain) == g
	}
	return false
}

// detachLocked removes exactly the member given, wherever it sits in the slice;
// a failure path must not drop a member that another session added
// concurrently. mu must be held.
func (g *ProxyGroup) detachLocked(member *Tunnel) {
	for index, existing := range g.members {
		if existing == member {
			g.members = append(g.members[:index], g.members[index+1:]...)
			return
		}
	}
}

// remove drops one member and closes the endpoint once the last one is gone.
//
// removed reports whether the member was in the group, and empty whether no member
// is left: a pool that loses one of its members keeps the name published, and the
// caller has to tell the two cases apart to know whether the endpoint and the DHT
// record still belong to somebody.
//
// The audit record is written here rather than at the call sites because a member
// leaves through whichever of them runs first: the control handler tearing a
// session down, or the session closing itself while the server shuts down. Both
// end the same registration, and only the removal knows whether there was anything
// to remove.
func (g *ProxyGroup) remove(member *Tunnel, reason string) (removed, empty bool) {
	member.closed.Store(true)

	g.mu.Lock()
	for index, existing := range g.members {
		if existing != member {
			continue
		}
		g.members = append(g.members[:index], g.members[index+1:]...)
		removed = true
		g.lastMember = member
		break
	}
	empty = len(g.members) == 0
	g.mu.Unlock()

	if removed {
		clientID := ""
		if member.Session != nil {
			// ClientName, like proxy_registered and client_disconnected: the
			// audit records for one client have to be joinable by the same
			// client_id, and the session's random ID is not it.
			clientID = member.Session.ClientName()
		}
		g.manager.auditor.Record(AuditEvent{
			Event: EventProxyRemoved, ClientID: clientID, Proxy: g.Name,
			Outcome: "removed", Detail: reason,
		})
	}

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
		listener, packet, pump, binding, mux, sni := g.listener, g.packet, g.pump, g.vhost, g.tcpmux, g.sni
		g.listener, g.packet, g.pump, g.vhost, g.tcpmux, g.sni = nil, nil, nil, nil, nil, nil
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
		if mux != nil {
			mux.remove()
		}
		if sni != nil {
			sni.remove()
		}
		g.logger.Printf("proxy %q closed (%s)", g.Name, reason)
	})
}

// bind gives the group the endpoint its type calls for.
func (g *ProxyGroup) bind() (bindErr error) {
	// Releasing this wakes a member that joined while the bind was in flight, so
	// it can add its hostnames to the binding the bind installs. It goes through
	// boundOnce because a group that emptied is closed and a registration racing
	// the unregistration that closes it binds the same group a second time.
	defer g.boundOnce.Do(func() { close(g.bound) })
	// Registered after the release above, so it runs first: every failure has to
	// close the group before a waiter wakes, because a waiter that still sees an
	// open group joins a group with no endpoint while its client believes the
	// proxy was published. close is once-guarded, so failing after the group was
	// already closed changes nothing.
	defer func() {
		if bindErr != nil {
			g.close("publishing the proxy failed")
		} else {
			// Recorded here rather than beside the return: this defer runs
			// before bound is released, so a waiter woken by the release reads
			// an outcome the bind has already settled either way.
			g.bindOK.Store(true)
		}
	}()
	if g.isClosed() {
		return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
	}
	switch g.Type {
	case protocol.ProxyTypeTCP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.ProxyHost(), strconv.Itoa(g.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s: %w", g.Name, flynet.ListenError(addr, err))
		}
		g.endpointMu.Lock()
		if g.isClosed() {
			// close ran between the entry check and here; a listener
			// installed into a closed group would never be closed again.
			g.endpointMu.Unlock()
			_ = listener.Close()
			return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
		}
		g.listener = listener
		g.endpointMu.Unlock()
		go g.acceptLoop()
		g.logger.Printf("proxy %q (tcp) published on %s", g.Name, addr)

	case protocol.ProxyTypeUDP:
		if g.RemotePort == 0 {
			g.logger.Printf("proxy %q registered with no public port", g.Name)
			return nil
		}
		addr := net.JoinHostPort(g.manager.cfg.ProxyHost(), strconv.Itoa(g.RemotePort))
		packet, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s on %s/udp: %w", g.Name, addr, err)
		}
		g.endpointMu.Lock()
		if g.isClosed() {
			g.endpointMu.Unlock()
			_ = packet.Close()
			return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
		}
		g.packet = packet
		g.endpointMu.Unlock()
		g.startUDP()
		g.logger.Printf("proxy %q (udp) published on %s", g.Name, addr)

	case protocol.ProxyTypeHTTP, protocol.ProxyTypeHTTPS:
		if g.TLSPassthrough {
			// The visitor's TLS session is relayed untouched, so the
			// certificate the visitor sees is this client's own.
			if g.manager.sni == nil {
				return fmt.Errorf("proxy %q is type %s with tls_passthrough but server.https_passthrough_port is not configured", g.Name, g.Type)
			}
			binding, err := g.manager.sni.add(g)
			if err != nil {
				return err
			}
			g.endpointMu.Lock()
			if g.isClosed() {
				g.endpointMu.Unlock()
				binding.remove()
				return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
			}
			g.sni = binding
			g.endpointMu.Unlock()
			g.logger.Printf("proxy %q (%s) reachable on the TLS passthrough listener as %s",
				g.Name, g.Type, strings.Join(g.Domains, ", "))
			return nil
		}
		if g.manager.vhost == nil {
			return fmt.Errorf("proxy %q is type %s but server.http_port is not configured", g.Name, g.Type)
		}
		binding, err := g.manager.vhost.add(g)
		if err != nil {
			return err
		}
		g.endpointMu.Lock()
		if g.isClosed() {
			g.endpointMu.Unlock()
			binding.remove()
			return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
		}
		g.vhost = binding
		g.endpointMu.Unlock()

	case protocol.ProxyTypeTCPMux:
		if g.manager.tcpmux == nil {
			return fmt.Errorf("proxy %q is type %s but server.tcpmux_port is not configured", g.Name, g.Type)
		}
		mux, err := g.manager.tcpmux.add(g)
		if err != nil {
			return err
		}
		g.endpointMu.Lock()
		if g.isClosed() {
			g.endpointMu.Unlock()
			mux.remove()
			return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
		}
		g.tcpmux = mux
		g.endpointMu.Unlock()
		g.logger.Printf("proxy %q (%s) reachable on the tcpmux listener as %s",
			g.Name, g.Type, strings.Join(g.Domains, ", "))

	case protocol.ProxyTypeSTCP, protocol.ProxyTypeSUDP, protocol.ProxyTypeXTCP:
		// The NIZK public key was derived and stored before the group was put in the
		// manager's map (see Register), so nothing is written here that a visitor
		// could read concurrently.
		g.logger.Printf("proxy %q (%s) registered as private, reachable by visitors", g.Name, g.Type)

	case protocol.ProxyTypeSOCKS:
		if g.RemotePort == 0 {
			return fmt.Errorf("proxy %q: a socks5 endpoint needs a remote port", g.Name)
		}
		addr := net.JoinHostPort(g.manager.cfg.ProxyHost(), strconv.Itoa(g.RemotePort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot publish %s: %w", g.Name, flynet.ListenError(addr, err))
		}
		g.endpointMu.Lock()
		if g.isClosed() {
			g.endpointMu.Unlock()
			_ = listener.Close()
			return fmt.Errorf("proxy %q is being unpublished; it cannot be published again on this group", g.Name)
		}
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
	logging.Warnf(g.logger, "proxy %q: visitor %s refused: %s", g.Name, remote, detail)
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
		if member.closed.Load() {
			// A member that has left, or that a re-registration replaced, is not part
			// of the pool any more. Keeping it as the last resort handed a visitor to
			// a proxy that was no longer published here.
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
		// A member that has never answered has no average to compare, so it is
		// tried first: that is how a newly joined member gets the chance to prove
		// itself, and strategy_test.go holds the behaviour. The exception is a
		// member that has already failed without ever answering — see neverAnswered
		// — which waits behind the members that answer and is probed every so often
		// so that a service that comes back is found again.
		if probe := g.probeNeverAnswered(candidates); probe != nil {
			return probe
		}
		best := candidates[0]
		bestScore := best.latencyScore()
		for _, member := range candidates[1:] {
			if score := member.latencyScore(); score < bestScore {
				best, bestScore = member, score
			}
		}
		return best

	case config.LoadBalanceFailover:
		// Members are kept in registration order, so the first one wins until
		// it stops being healthy.
		return candidates[0]

	case config.LoadBalanceAdaptive:
		// Same probe as the latency strategy, for the same reason: the cost
		// function cannot bring back a member it has never had a measurement for.
		if probe := g.probeNeverAnswered(candidates); probe != nil {
			return probe
		}
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
// costs, in nanoseconds. An unmeasured member that has not failed scores zero: it
// is tried so that it can be measured. One that has failed without ever answering
// scores neverAnswered instead.
func (t *Tunnel) cost() int64 {
	latency := t.dialLatency.Load()
	if latency <= 0 {
		return t.latencyScore()
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

// latencyScore is what the two latency-based strategies compare: the member's
// measured average, or a score for a member that has no measurement yet.
//
// An unmeasured member scores zero, which puts it first — that is what gives a
// newly joined member its chance. A member that has already failed and has still
// never answered is different: it never gets a measurement, so the free score would
// keep it ahead of the members that do answer on every single visit. Measured on one
// live member and one whose local service is down, that cost the latency strategy 31
// of 30 visits and the adaptive strategy 30 of 30, against 15 for round-robin and 4
// for the bandit. It now waits behind the members that answer, and while every
// member is in that state (a pool whose members are all down) the strategies still
// work through them: latency keeps registration order and adaptive rotates its
// starting point, so a member that recovers is found again.
func (t *Tunnel) latencyScore() int64 {
	if latency := t.dialLatency.Load(); latency > 0 {
		return latency
	}
	if t.failures.Load() > 0 {
		return neverAnswered
	}
	return 0
}

// neverAnswered ranks a member that has failed and has never once answered behind
// every member that has been measured. It is larger than any measured cost: a
// nanosecond measurement with the failure penalty applied is worth a few seconds at
// most.
const neverAnswered = math.MaxInt64

// neverAnsweredProbeEvery is how often the two latency-based strategies put a member
// that has failed without ever answering back in the running. Ranking it last is what
// stops a dead service from taking every visit, but on its own it would also mean a
// service that came back is never found again: the member has no measurement for the
// failure penalty to work on, so nothing else would bring it round. Probing it is the
// same idea as the bandit's exploration, which samples the member it knows least
// about, and it is what the comment on the capped penalty promises — a member with a
// long outage is still tried.
const neverAnsweredProbeEvery = 20

// probeNeverAnswered returns such a member on every neverAnsweredProbeEvery-th pick,
// and nil on the others. When every member is in that state the strategies keep
// working through them anyway, so the probe only has to cover the mixed pool.
func (g *ProxyGroup) probeNeverAnswered(candidates []*Tunnel) *Tunnel {
	if len(candidates) < 2 {
		return nil
	}
	if (g.probeCounter.Add(1)-1)%neverAnsweredProbeEvery != 0 {
		return nil
	}
	var probe *Tunnel
	for _, member := range candidates {
		if member.dialLatency.Load() > 0 || member.failures.Load() == 0 {
			continue
		}
		// The one that has failed least often, so a member that failed once is
		// preferred over one that has never worked at all.
		if probe == nil || member.failures.Load() < probe.failures.Load() {
			probe = member
		}
	}
	return probe
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
	// acceptLoop serves every visitor on a goroutine of its own, and the guard the
	// control connection has (see Server.handleConn) does not reach it: a panic while
	// serving one visitor would end the process that is carrying every tunnel. The
	// visitor's own connection is what is given up instead.
	defer func() {
		if rec := recover(); rec != nil {
			logging.Errorf(g.logger, "proxy %q: panic while serving a visitor from %s: %v", g.Name, public.RemoteAddr(), rec)
			_ = public.Close()
		}
	}()

	// The [[http_plugins]] newUserConn webhooks see every visitor connection
	// before it is served: a reject turns it away with the plugin's reason. A
	// socks5 endpoint speaks its greeting before anything is dialled and does not
	// go through serveStream, so the webhooks run here; every other type reaches
	// them from serveStream.
	if g.Type != protocol.ProxyTypeSOCKS {
		g.serveStream(public)
		return
	}
	if reason := g.admitVisitor(public.RemoteAddr()); reason != "" {
		g.refuseVisitor(public.RemoteAddr(), reason)
		// The reject path owns the accepted connection: the defer below is
		// registered only for the branch that serves it, so without this the
		// fd is leaked for every visitor the webhook turns away.
		_ = public.Close()
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
		//
		// This counts as its own series, not as a visitor denied by a list: it is a
		// connection that did not carry a SOCKS5 request, and an operator watching
		// for policy refusals should not have to read a scanner's garbage as one.
		logging.Warnf(g.logger, "proxy %q: socks5 request from %s refused: %v", g.Name, public.RemoteAddr(), err)
		g.metrics.socksMalformed.Add(1)
		return
	}

	if request.Command == socks.CmdAssoc {
		g.serveSocksUDP(public, request)
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
			logging.Warnf(g.logger, "proxy %q: member %s did not serve %s: %v", g.Name, member.Session.ID, request.Target, err)
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
	logging.Warnf(g.logger, "proxy %q: %s asked for %s and was refused: %v",
		g.Name, public.RemoteAddr(), request.Target, lastErr)
}

// serveSocksUDP serves a SOCKS5 UDP ASSOCIATE request: it allocates a relay
// socket, tells the visitor where to send datagrams, and relays each wrapped
// datagram through a member's data connection. The association lives until the
// visitor's control connection closes.
func (g *ProxyGroup) serveSocksUDP(public net.Conn, request socks.Request) {
	relay, err := net.ListenPacket("udp", net.JoinHostPort(g.manager.cfg.ProxyHost(), "0"))
	if err != nil {
		_ = socks.WriteReply(public, socks.ReplyGeneralFailure)
		logging.Warnf(g.logger, "proxy %q: udp associate from %s refused: cannot open a relay socket: %v",
			g.Name, public.RemoteAddr(), err)
		return
	}
	defer relay.Close()

	member, stream, err := g.openSocksUDP()
	if err != nil {
		_ = socks.WriteReply(public, socks.ReplyFor(err))
		logging.Warnf(g.logger, "proxy %q: udp associate from %s refused: %v", g.Name, public.RemoteAddr(), err)
		return
	}
	defer stream.release()
	defer stream.dc.Close()

	// The reply names the address the visitor sends datagrams to: the address it
	// already reached us on, with the relay's port. Reporting the wildcard the
	// relay may have bound would send the visitor to an unreachable 0.0.0.0.
	ip := ipOfAddr(public.LocalAddr())
	if ip == nil {
		_ = socks.WriteReply(public, socks.ReplyGeneralFailure)
		return
	}
	port := relay.LocalAddr().(*net.UDPAddr).Port
	if err := socks.WriteReplyBound(public, socks.ReplySucceeded, ip, port); err != nil {
		return
	}
	g.metrics.socksUDPAssoc.Add(1)

	// Restrict the relay to the address the association came from, unless the
	// visitor asked for a specific client address in its request.
	expectedIP := ipOfAddr(public.RemoteAddr())
	if host, _, err := net.SplitHostPort(request.ClientAddr); err == nil {
		if address := net.ParseIP(host); address != nil && !address.IsUnspecified() {
			expectedIP = address
		}
	}

	member.pipeSocksUDP(relay, stream.dc, expectedIP, public, public.RemoteAddr().String())
}

// openSocksUDP picks a member and asks it for a socks5 UDP data connection,
// moving on to another member when the first cannot answer.
func (g *ProxyGroup) openSocksUDP() (*Tunnel, *openedStream, error) {
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

		stream, err := g.openSocksUDPStream(member)
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

// serveStream matches one public connection with a member, moving on to the next
// member when one cannot provide a stream.
func (g *ProxyGroup) serveStream(public net.Conn) {
	// The accept loop, the tcpmux multiplexer and the TLS passthrough all arrive
	// here, so the newUserConn webhooks are run once at this one point: a plugin
	// that is the operator's gate must not be bypassable by the protocol a visitor
	// happens to speak.
	if reason := g.admitVisitor(public.RemoteAddr()); reason != "" {
		g.refuseVisitor(public.RemoteAddr(), reason)
		_ = public.Close()
		return
	}
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
			logging.Warnf(g.logger, "proxy %q: member %s did not answer: %v", g.Name, member.Session.ID, err)
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
	return g.openStreamWith(member, func() (*dataConn, func(), error) {
		return member.openStreamFor(visitor, target)
	})
}

// openSocksUDPStream asks a member for the data connection that carries a socks5
// UDP ASSOCIATE.
func (g *ProxyGroup) openSocksUDPStream(member *Tunnel) (*openedStream, error) {
	return g.openStreamWith(member, member.openSocksUDPStream)
}

// openStreamWith wraps a member's stream open with the latency and failure
// bookkeeping the load-balancing strategies read.
func (g *ProxyGroup) openStreamWith(member *Tunnel, open func() (*dataConn, func(), error)) (*openedStream, error) {
	started := time.Now()
	dc, release, err := open()
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
	// The connection is tracked on the session as well as released by the caller:
	// a session that ends closes the data connections of the streams it is still
	// carrying, which nothing else does — they are sockets of their own, separate
	// from the control connection.
	member.Session.trackStream(dc)
	inner := release
	release = func() {
		member.Session.untrackStream(dc)
		if inner != nil {
			inner()
		}
	}
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
	if packet == nil {
		// bind() read the field under the same lock and close() takes it under
		// that lock, so a nil here means the group was closed in between: the
		// pump's socket is gone and starting one over a nil interface panics in
		// its background goroutine, where nothing recovers it.
		return
	}

	pump := &flynet.DatagramPump{
		Socket: packet,
		Paths:  paths,
		Open: func(addr net.Addr) (*protocol.Framer, func(), error) {
			// A datagram session is a visitor connection as much as a tcp accept is,
			// so the newUserConn gate applies here too.
			if reason := g.admitVisitor(addr); reason != "" {
				g.refuseVisitor(addr, reason)
				return nil, nil, errors.New("the visitor source was refused by an http plugin")
			}
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
		OnDatagram: func(toPeer bool, n int) { g.metrics.udpDatagrams.Add(1) },
		OnOversize: func(n int) { g.metrics.udpOversize.Add(1) },
		OnSessionLimit: func(net.Addr) {
			// Each tracked source address holds a goroutine and a stream to the
			// client, and the source address is the visitor's to forge, so a public
			// datagram port has to stop somewhere; this series is how an operator
			// sees that it did.
			g.metrics.udpSessionLimit.Add(1)
		},
		MaxDatagram: g.udpPacketSize,
		OnSession:   func(delta int) { g.metrics.udpSessions.Add(int64(delta)) },
		IdleTimeout: g.idleTimeout,
		Logger:      g.logger,
	}
	g.endpointMu.Lock()
	if g.isClosed() {
		// close() ran after the check above and has already taken g.packet: it
		// saw no pump to shut down, so installing this one would leave a pump
		// running over a socket nobody owns.
		g.endpointMu.Unlock()
		return
	}
	g.pump = pump
	g.endpointMu.Unlock()
	pump.Start()
}

// recordSession books the traffic of one finished datagram session against the
// group's members in proportion, so the dashboard and the metrics stay consistent
// without attributing one session to a single member it may not have used alone.
func (g *ProxyGroup) recordSession(toPeer, fromPeer int64) {
	// An established datagram session always carries its first datagram, so
	// zero on both sides is a session whose paths never opened — a refused
	// visitor, or a first send the session lost. Booking it would count a
	// connection every member never served, on a port whose source addresses
	// are the sender's to forge.
	if toPeer == 0 && fromPeer == 0 {
		return
	}
	// The global and per-tunnel counters come first: a datagram session that is
	// finishing because the last member left (a client disconnecting, or a
	// shutdown) still carried those bytes, and dropping them made the totals
	// under-report traffic the public port really moved.
	g.metrics.recordDatagramSession(g.Name, toPeer, fromPeer)

	members := g.Members()
	if len(members) == 0 {
		g.bookToLastMember(toPeer, fromPeer)
		return
	}
	shareOut, shareIn := toPeer/int64(len(members)), fromPeer/int64(len(members))
	// The division truncates, and the ledger bills the members' own counters, so
	// without the remainder every finished session loses up to len(members)-1
	// bytes. The first member carries it: the sum stays exact and no member is
	// off its proportional share by more than the member count.
	restOut, restIn := toPeer%int64(len(members)), fromPeer%int64(len(members))
	for index, member := range members {
		out, in := shareOut, shareIn
		if index == 0 {
			out, in = out+restOut, in+restIn
		}
		member.addTraffic(out, in)
		// One connection per member: a datagram session is spread over the
		// paths the pool chose, and each member served part of it. The
		// dashboard's per-member connection totals therefore add up to more
		// than the group's own count; that is deliberate, and no evidence has
		// shown the two views being read as one.
		member.Total.Add(1)
		member.Session.RecordTraffic(out, in)
	}
}

// bookToLastMember books a finished stream or request to the member that left last,
// which is what there is to book to when the group has emptied: the sessions that
// carried the bytes are being torn down right now, and the ledger reads the
// counters this writes. Dropping them made the metrics and the ledger disagree.
func (g *ProxyGroup) bookToLastMember(toClient, fromClient int64) {
	g.mu.RLock()
	last := g.lastMember
	g.mu.RUnlock()
	if last == nil {
		return
	}
	last.addTraffic(toClient, fromClient)
	last.Total.Add(1)
	if last.Session != nil {
		last.Session.RecordTraffic(toClient, fromClient)
	}
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

	// The newUserConn webhooks see an HTTP visitor too: an operator who uses them as
	// a gate would otherwise have it enforced for every type but http and https,
	// which are the ones a browser reaches. The webhook precedes the local checks,
	// as it does on every other path.
	if err == nil {
		if reason := g.admitVisitor(remote); reason != "" {
			g.refuseVisitor(remote, reason)
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}

	// Basic auth is checked before anything about the request is acted on, and
	// the failure is the response a browser knows how to prompt for. The
	// comparison runs in constant time: the password is a credential, and a
	// timing side channel across the shared listener would leak it byte by byte.
	if g.HTTPUser != "" || g.HTTPPassword != "" {
		user, password, ok := r.BasicAuth()
		if !ok || !crypto.EqualTokens(user, g.HTTPUser) || !crypto.EqualTokens(password, g.HTTPPassword) {
			if err == nil {
				g.refuseVisitor(remote, "basic auth mismatch")
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	if err == nil {
		if allowed, reason := g.visitorAllowed(remote); !allowed {
			g.refuseVisitor(remote, reason)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	g.metrics.httpRequests.Add(1)
	// The series the request is counted on, taken without touching the active
	// gauge: the request rides a tunnel stream the reverse proxy keeps for several
	// requests, and that stream is what openStream already counted. Calling
	// streamOpened here counted one in-flight request twice and left the extra
	// count on the gauge for as long as the transport kept the connection.
	// An in-flight request can outlive its group: the last member left,
	// forgetTunnel dropped the series, and this request — dispatched before
	// that — must not put it back, or a dead name sits in every scrape for
	// the life of the process. lookupTunnel counts only while the name exists.
	if entry, ok := g.metrics.lookupTunnel(g.Name); ok {
		entry.httpRequests.Add(1)
	}

	counter := &countingResponseWriter{ResponseWriter: w}
	body := &countingReadCloser{ReadCloser: r.Body}
	r.Body = body
	proxy.ServeHTTP(counter, r)

	// The request side is what the reverse proxy actually read from the body: a
	// chunked body has ContentLength -1, but its bytes still cross the tunnel.
	toClient := body.read.Load()
	// A protocol switch moves its bytes on the hijacked connection instead of
	// through Write, so both directions are added back here.
	toClient += counter.hijackRead.Load()
	fromClient := counter.written + counter.hijackSent.Load()
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
		// A request can finish after the last member left — the health check
		// withdraws a proxy while its answer is still streaming — and its bytes
		// belong to the session that carried them just as much as a datagram
		// session's do.
		g.bookToLastMember(toClient, fromClient)
		return
	}
	shareOut, shareIn := toClient/int64(len(members)), fromClient/int64(len(members))
	// The remainder is carried by the first member for the same reason as in
	// recordSession: the truncation would otherwise leave bytes the ledger
	// never bills.
	restOut, restIn := toClient%int64(len(members)), fromClient%int64(len(members))
	for index, member := range members {
		out, in := shareOut, shareIn
		if index == 0 {
			out, in = out+restOut, in+restIn
		}
		member.addTraffic(out, in)
		// The request is credited with one connection on every member: the
		// reverse proxy may reach any of them over a connection it keeps, so
		// each member did carry part of the work. The per-member totals do not
		// sum to the group's own connection count, which is deliberate.
		member.Total.Add(1)
		member.Session.RecordTraffic(out, in)
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

// responseHeaderTimeout is how long the terminating path waits for a local
// service's response headers: the virtual-host timeout when the operator set
// one, the dial timeout otherwise.
func responseHeaderTimeout(dial, vhost time.Duration) time.Duration {
	if vhost > 0 {
		return vhost
	}
	return dial
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
				// The DataRequest told the client to wrap this stream because the
				// proxy asked for use_encryption or use_compression; this side has
				// to apply the same layers, or every request fails to decode.
				clientSide, wrapErr := member.wrapDataStream(stream.dc)
				if wrapErr != nil {
					stream.release()
					_ = stream.dc.Close()
					lastErr = wrapErr
					continue
				}
				return &tunnelHTTPConn{
					Conn:    clientSide,
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
		ResponseHeaderTimeout: responseHeaderTimeout(g.dialTimeout, g.vhostTimeout),
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
			if g.HostHeaderRewrite != "" {
				// The operator named the Host the local service should see;
				// virtual hosts and locations stop at the server.
				pr.Out.Host = g.HostHeaderRewrite
			}
			pr.SetXForwarded()
			for name, value := range g.RequestHeaders {
				pr.Out.Header.Set(name, value)
			}
		},
		ModifyResponse: func(answer *http.Response) error {
			for name, value := range g.ResponseHeaders {
				answer.Header.Set(name, value)
			}
			return nil
		},
		Transport: transport,
		ErrorLog:  log.New(&prefixWriter{logger: g.logger, prefix: "http " + g.Name + ": "}, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logging.Warnf(g.logger, "proxy %q: http request for %s failed: %v", g.Name, r.Host, err)
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
			ClientID:  member.Session.ClientName(),
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

// visitorIdentityAllow reports whether a visitor's identity may use this private
// proxy, and why not when it may not. An empty allow_users admits the
// publisher's own identity alone, which is frp's rule for a private proxy: the
// secret key is the shared half of the guard, and this is the half that says
// whose client may use it.
func (g *ProxyGroup) visitorIdentityAllow(visitorUser string) (bool, string) {
	if len(g.allowUsers) == 0 {
		if visitorUser == g.ownerUser {
			return true, ""
		}
		return false, fmt.Sprintf(
			"proxy %q is published by identity %q, and this visitor is %q: the proxy lists no allow_users, so only the publisher's own identity may visit it",
			g.Name, g.ownerUser, visitorUser)
	}
	for _, allowed := range g.allowUsers {
		if allowed == visitorUser {
			return true, ""
		}
	}
	return false, fmt.Sprintf("proxy %q allows the identities %s, and this visitor is %q",
		g.Name, strings.Join(quoteAll(g.allowUsers), ", "), visitorUser)
}

// quoteAll renders the allow_users list for a refusal message.
func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strconv.Quote(name))
	}
	return out
}
