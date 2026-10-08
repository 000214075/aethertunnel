package server

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// readSSHOutput reads from r until one of the markers appears or the stream
// ends, and returns what it collected. Unlike readSSHUntil it leaves the choice
// of marker to the caller: the refusal and the success line are both outcomes
// this test can be handed, and only one of them is the right one.
func readSSHOutput(t *testing.T, r io.Reader, timeout time.Duration, markers ...string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				out := b.String()
				for _, marker := range markers {
					if strings.Contains(out, marker) {
						done <- out
						return
					}
				}
			}
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	select {
	case got := <-done:
		return got
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for one of %q in ssh output", markers)
		return ""
	}
}

// Two concurrent exec channels on one connection that name the same
// not-yet-published proxy share the slot between them. The bug this pins: the
// second command took no share (the name was already in the set), so the first
// command's failed registration deleted the entry outright and the second went
// on to publish a proxy holding no slot at all — max_proxies stayed exceeded
// until the connection ended.
//
// The plugin gate is what makes the interleaving deterministic: a command's
// registration calls the webhook strictly after its reserveProxySlot and
// strictly before its release (or its collapse), so each arrival proves the
// matching command holds its share. The failing command is released first and
// its error line is read before the succeeding one is let through, so the
// release that must lower the count has happened before the collapse runs.
func TestASlotSharedByTwoCommandsNamingOneProxySurvivesOneFailing(t *testing.T) {
	failPort, okPort, thirdPort := freePort(t), freePort(t), freePort(t)

	failSeen := make(chan struct{}, 1)
	okSeen := make(chan struct{}, 1)
	failGate := make(chan struct{})
	okGate := make(chan struct{})
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		content, _ := body["content"].(map[string]any)
		port, _ := content["remote_port"].(float64)
		switch int(port) {
		case failPort:
			select {
			case failSeen <- struct{}{}:
			default:
			}
			<-failGate
			return http.StatusOK, `{"reject":true,"reject_reason":"refused by the test plugin"}`
		case okPort:
			select {
			case okSeen <- struct{}{}:
			default:
			}
			<-okGate
			return http.StatusOK, `{"reject":false,"unchange":true}`
		}
		return http.StatusOK, `{"reject":false,"unchange":true}`
	})

	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr:   "127.0.0.1",
		BindPort:   port,
		KeyFile:    keyPath,
		MaxProxies: 1,
	}
	cfg.HTTPPlugins = []config.HTTPPluginConfig{
		{Name: "gate", Addr: hook.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	newExec := func(command string) io.Reader {
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		out, err := session.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout: %v", err)
		}
		if err := session.Start(command); err != nil {
			t.Fatalf("exec %q: %v", command, err)
		}
		return out
	}

	first := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", failPort, testToken))
	<-failSeen // the failing command holds its share from here on

	second := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", okPort, testToken))
	<-okSeen // the second command pinned the same slot rather than being refused

	close(failGate)
	readSSHUntil(t, first, "refused by the test plugin", 5*time.Second)

	close(okGate)
	readSSHUntil(t, second, "ProxyName: web", 5*time.Second)

	// The published proxy must still be holding the one slot: a third name is
	// refused, not published underneath the cap.
	third := newExec(fmt.Sprintf("tcp --proxy_name other --remote_port %d --token %s", thirdPort, testToken))
	got := readSSHOutput(t, third, 5*time.Second, "published its maximum of 1 proxies", "ProxyName: other")
	if !strings.Contains(got, "published its maximum of 1 proxies") {
		t.Fatalf("the third command was not refused, so the published proxy no longer holds the slot; got:\n%s", got)
	}
	if strings.Contains(got, "ProxyName: other") {
		t.Fatalf("the third command published although max_proxies = 1 is reached; got:\n%s", got)
	}
}

// Two concurrent commands naming one proxy that both succeed end up holding
// one published proxy, and the cap is charged once for it. Both commands
// pinned the shared slot while the registration was in flight; the collapse
// after the first success folds the doubled pin back into the single slot the
// published proxy holds. This is a characterization test of that collapse: in
// the pre-refcount version the second command simply took no share, so the
// count also came out as one and there is no red to demonstrate here.
func TestASlotSharedByTwoSucceedingCommandsHoldsOnePublishedProxy(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr:   "127.0.0.1",
		BindPort:   port,
		KeyFile:    keyPath,
		MaxProxies: 1,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	newExec := func(command string) io.Reader {
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		out, err := session.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout: %v", err)
		}
		if err := session.Start(command); err != nil {
			t.Fatalf("exec %q: %v", command, err)
		}
		return out
	}

	// Both name the same proxy on the port its group binds, so the second
	// registration replaces the first member of the session the way a
	// reconnecting client does — a replacement that moved the port would be
	// refused, since the endpoint the group already bound is what serves it.
	published := freePort(t)
	first := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", published, testToken))
	second := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", published, testToken))
	readSSHUntil(t, first, "ProxyName: web", 5*time.Second)
	readSSHUntil(t, second, "ProxyName: web", 5*time.Second)

	third := newExec(fmt.Sprintf("tcp --proxy_name other --remote_port %d --token %s", freePort(t), testToken))
	got := readSSHOutput(t, third, 5*time.Second, "published its maximum of 1 proxies", "ProxyName: other")
	if !strings.Contains(got, "published its maximum of 1 proxies") {
		t.Fatalf("the third command was not refused, so two successful commands charged the cap twice; got:\n%s", got)
	}
	if strings.Contains(got, "ProxyName: other") {
		t.Fatalf("the third command published although the two commands hold one proxy between them; got:\n%s", got)
	}
}

// The join path marks a member reachable only once it woke on g.bound. Waking
// on g.done — the first member's bind failed and the group is dead — used to
// leave the mark set: extendHostnames returns without an error on that path, so
// the join reported success, Register's re-check saw a reachable member and
// kept the counter, and the teardown billed a 0-byte ledger line for a proxy
// whose endpoint never came up — the outcome TestABindFailureLeavesNoLedgerEntry
// exists to prevent.
func TestAJoinerThatWakesOnADeadFirstBindIsNotMarkedReachable(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP, Group: "pool"}, manager)
	manager.groups["web"] = group

	// A first member whose bind is still in flight, hand-built so that neither
	// g.bound nor g.done is closed and the join below parks on the wait. The
	// join branch only admits members of a named pool, so the group carries one.
	group.mu.Lock()
	group.members = []*Tunnel{{Name: "web", Session: &Session{ID: "first"}, group: group}}
	group.mu.Unlock()

	type joined struct {
		member *Tunnel
		err    error
	}
	done := make(chan joined, 1)
	go func() {
		member, err := group.add(&Session{ID: "joiner"}, protocol.ProxySpec{
			Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1", Group: "pool",
		})
		done <- joined{member, err}
	}()

	// Let the join park on the wait, then fail the first bind the way bind's
	// caller does.
	time.Sleep(50 * time.Millisecond)
	group.close("publishing the proxy failed")

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the join of a dead group returned an error: %v", got.err)
		}
		if got.member.reachable.Load() {
			t.Fatal("a joiner woken by a dead first bind was marked reachable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the join never returned although the group closed")
	}
}

// Characterization: a joiner whose bind succeeded is marked reachable — the
// mark is what lets a visitor be handed to the member from the moment it
// entered the list, before its own hostnames are installed. There is no failing
// demonstration: the pre-fix code set the mark unconditionally, which passes
// this just as well.
func TestAJoinerThatWakesOnABoundGroupIsMarkedReachable(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP, Group: "pool"}, manager)
	manager.groups["web"] = group

	group.mu.Lock()
	group.members = []*Tunnel{{Name: "web", Session: &Session{ID: "first"}, group: group}}
	group.mu.Unlock()

	type joined struct {
		member *Tunnel
		err    error
	}
	done := make(chan joined, 1)
	go func() {
		member, err := group.add(&Session{ID: "joiner"}, protocol.ProxySpec{
			Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1", Group: "pool",
		})
		done <- joined{member, err}
	}()

	time.Sleep(50 * time.Millisecond)
	// A successful bind: the wait is released and the outcome it settled is
	// recorded — the release alone no longer says the endpoint came up.
	group.bindOK.Store(true)
	close(group.bound)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the join of a bound group returned an error: %v", got.err)
		}
		if !got.member.reachable.Load() {
			t.Fatal("a joiner woken by the bound group was not marked reachable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the join never returned although the group bound")
	}
}

// A pool owns the endpoint its first member bound: a joiner's remote_port was
// never adopted, and its own re-registration announces nothing — the DHT record
// belongs to the single-member moment. Refusing it for "moving" the port turned
// a healthy replacement away — the SSH gateway's second channel re-sending its
// spec, or a reload's registerQuietly on a server without withdrawals — and the
// refusal even named a port the endpoint never had.
func TestAPoolMemberMayReplaceItsOwnRegistrationWithItsOwnPort(t *testing.T) {
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)
	owner := startAgent(t, rs.addr, false, map[string]dataHandler{})
	joiner := startAgent(t, rs.addr, false, map[string]dataHandler{})

	owned := freePort(t)
	owner.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: owned, Group: "pool",
	})

	// The single owner still may not move the port the group bound: its
	// replacement is the one Register would announce at a new address.
	if err := owner.client.register(protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: freePort(t), Group: "pool",
	}); err != nil {
		t.Fatalf("send the owner's port move: %v", err)
	}
	if refusal := owner.expectRefused("pooled"); !strings.Contains(refusal, "remote_port") {
		t.Fatalf("the owner's port move was not refused: %s", refusal)
	}

	// A joiner names its own port: the join itself never checks it against the
	// group's, so a replacement of that joiner must not either.
	spec := protocol.ProxySpec{
		Name: "pooled", Type: protocol.ProxyTypeTCP, LocalAddr: "127.0.0.1:1",
		RemotePort: freePort(t), Group: "pool",
	}
	joiner.register(spec)
	joiner.register(spec)
}

// A wildcard binding serves its base domain and every subdomain of it — that is
// how vhost's lookup and the tcpmux and SNI tables match a request. A
// replacement that names one of those hosts keeps a hostname the binding really
// serves, so refusing it as "not published" sent the operator off to withdraw
// and re-register for no reason.
func TestAReplacementMayKeepAHostnameTheWildcardBindingServes(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.HTTPPort = freePort(t)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	agent := startAgent(t, rs.addr, false, map[string]dataHandler{})

	port := freePort(t)
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"*.example.com"}, RemotePort: port,
	})

	// The wildcard serves its subdomains and its base alike, so a replacement
	// naming one of them stays published for the host it names.
	agent.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"api.example.com"}, RemotePort: port,
	})

	// A host the wildcard does not cover is still no part of this binding.
	if err := agent.client.register(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"other.example.org"}, RemotePort: port,
	}); err != nil {
		t.Fatalf("send the re-registration: %v", err)
	}
	if refusal := agent.expectRefused("web"); !strings.Contains(refusal, "not published for hostname") {
		t.Fatalf("the refusal does not name the unserved hostname: %s", refusal)
	}
}

// The label encoding has to be injective: two different names must render as
// two different label values, or one scrape carries two identical series lines
// and a compliant parser rejects the whole exposition. A name is not validated
// where it is registered — an SSH gateway exec command can carry any byte — so
// the byte-level replacement is what keeps two names apart.
func TestAPromLabelEncodingSeparatesNamesThatDifferOnlyByAnInvalidByte(t *testing.T) {
	if promLabel("a\xffb") == promLabel("a?b") {
		t.Fatal("a name with an invalid byte and a name with a literal ? render as one label value")
	}
	if promLabel("a\xfeb") == promLabel("a\xffb") {
		t.Fatal("two names differing only in which byte is invalid render as one label value")
	}
	// The encoding of an invalid byte must not collide with a legal name that
	// spells the same escape, so a literal '%' is escaped as well.
	if promLabel("a\xffb") == promLabel("a%FFb") {
		t.Fatal("a name with an invalid byte and a name spelling its escape render as one label value")
	}
	if promLabel("50%") == promLabel("50\ufffd") {
		t.Fatal("a name with a literal % and a name with a valid U+FFFD render as one label value")
	}
	// A valid U+FFFD is a rune of its own and passes through untouched, the way
	// every valid UTF-8 name does.
	if got := promLabel("replaced\ufffd"); got != "\"replaced\ufffd\"" {
		t.Fatalf("promLabel of a name with a valid U+FFFD = %q, want it unchanged", got)
	}
}

// The reverse order of TestASlotSharedByTwoCommandsNamingOneProxySurvivesOneFailing:
// the command that finishes first succeeds and turns the name's slot into the
// published proxy's own, and the twin that finishes second fails — its
// registration refused — and must release nothing. With the slot as a converging
// count, the twin's failure deleted the last entry and the published proxy held
// no slot, so max_proxies stayed exceeded until the connection ended.
func TestAFailingCommandAfterAnotherPublishedTheProxyLeavesItsSlotIntact(t *testing.T) {
	failPort, okPort, thirdPort := freePort(t), freePort(t), freePort(t)

	failSeen := make(chan struct{}, 1)
	okSeen := make(chan struct{}, 1)
	failGate := make(chan struct{})
	okGate := make(chan struct{})
	hook := newHookServer(t, func(op string, body map[string]any) (int, string) {
		content, _ := body["content"].(map[string]any)
		port, _ := content["remote_port"].(float64)
		switch int(port) {
		case failPort:
			select {
			case failSeen <- struct{}{}:
			default:
			}
			<-failGate
			return http.StatusOK, `{"reject":true,"reject_reason":"refused by the test plugin"}`
		case okPort:
			select {
			case okSeen <- struct{}{}:
			default:
			}
			<-okGate
			return http.StatusOK, `{"reject":false,"unchange":true}`
		}
		return http.StatusOK, `{"reject":false,"unchange":true}`
	})

	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr:   "127.0.0.1",
		BindPort:   port,
		KeyFile:    keyPath,
		MaxProxies: 1,
	}
	cfg.HTTPPlugins = []config.HTTPPluginConfig{
		{Name: "gate", Addr: hook.URL, Path: "/hook", Ops: []string{config.HTTPPluginOpNewProxy}},
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	newExec := func(command string) io.Reader {
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		out, err := session.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout: %v", err)
		}
		if err := session.Start(command); err != nil {
			t.Fatalf("exec %q: %v", command, err)
		}
		return out
	}

	first := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", okPort, testToken))
	second := newExec(fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", failPort, testToken))
	<-okSeen // each arrival proves its command pinned the shared slot
	<-failSeen

	// The succeeding command goes first: it publishes, and the slot becomes the
	// published proxy's own before the twin is let through to fail.
	close(okGate)
	readSSHUntil(t, first, "ProxyName: web", 5*time.Second)

	close(failGate)
	readSSHUntil(t, second, "refused by the test plugin", 5*time.Second)

	third := newExec(fmt.Sprintf("tcp --proxy_name other --remote_port %d --token %s", thirdPort, testToken))
	got := readSSHOutput(t, third, 5*time.Second, "published its maximum of 1 proxies", "ProxyName: other")
	if !strings.Contains(got, "published its maximum of 1 proxies") {
		t.Fatalf("the third command was not refused, so the twin's failure freed the published proxy's slot; got:\n%s", got)
	}
	if strings.Contains(got, "ProxyName: other") {
		t.Fatalf("the third command published although max_proxies = 1 is reached; got:\n%s", got)
	}
}

// The mark a joining member gets cannot depend on which channel woke its wait:
// on a failed first bind both channels end up closed — the failure closes the
// group before it releases the wait, and the release closes bound — so either
// can win and the wake says nothing about the outcome. The bind's own result is
// what decides, which is what makes this case deterministic where waking on a
// bare close(done) was not.
func TestAJoinerWakingAfterARealBindFailureIsNeverMarkedReachable(t *testing.T) {
	manager := tunnelTestManager()
	group := newProxyGroup(protocol.ProxySpec{Name: "web", Type: protocol.ProxyTypeHTTP, Group: "pool"}, manager)
	manager.groups["web"] = group

	group.mu.Lock()
	group.members = []*Tunnel{{Name: "web", Session: &Session{ID: "first"}, group: group}}
	group.mu.Unlock()

	type joined struct {
		member *Tunnel
		err    error
	}
	done := make(chan joined, 1)
	go func() {
		member, err := group.add(&Session{ID: "joiner"}, protocol.ProxySpec{
			Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1", Group: "pool",
		})
		done <- joined{member, err}
	}()

	// Let the join park on the wait, then let the real bind fail — no
	// http_port is configured, the way the tunnel-manager bind-failure case builds
	// one, and the failure closes the group before it releases the wait.
	time.Sleep(50 * time.Millisecond)
	if err := group.bind(); err == nil {
		t.Fatal("a bind with no http_port configured succeeded")
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the join of a dead group returned an error: %v", got.err)
		}
		if got.member.reachable.Load() {
			t.Fatal("a joiner woken by a real failed bind was marked reachable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the join never returned although the bind failed")
	}
}

// A join whose hostnames are refused leaves the group with the member detached
// and the registration refused — and, when the joiner woke to a bind that did
// not succeed, with no counter of its own: Register's add-error branch judges
// the member by the same gate its re-check applies (createdUsage and not
// reachable) and forgets the counter. This is the state where the gate matters:
// the wake found no successful bind, yet a binding was still in place for the
// extension to fail against, and a missing gate leaked a 0-byte ledger line for
// a name this member never served.
func TestAJoinWhoseHostnamesAreRefusedLeavesNoCounterBehind(t *testing.T) {
	cfg := testConfig(t, false)
	allowPorts, err := config.ParsePortRanges(cfg.Server.AllowPorts)
	if err != nil {
		t.Fatalf("allow_ports: %v", err)
	}
	manager := newTunnelManager(cfg, discardLogger(), nil, newSessionManager(0), newMetrics(), nil, allowPorts)
	manager.httpPlugins = newHTTPPluginManager(cfg, discardLogger())

	// The group that already owns the hostname the joiner will ask for.
	other := newProxyGroup(protocol.ProxySpec{
		Name: "other", Type: protocol.ProxyTypeHTTP, Domains: []string{"taken.example"},
	}, manager)
	manager.groups["other"] = other
	router := newVhostRouter("http", cfg, discardLogger(), manager.metrics)
	if _, err := router.add(other); err != nil {
		t.Fatalf("bind the owner of the hostname: %v", err)
	}

	group := newProxyGroup(protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, Group: "pool", Domains: []string{"web.example"},
	}, manager)
	manager.groups["web"] = group
	// The binding bind would have installed: the group's own hostname goes in
	// clean, and the joiner's colliding one is what extend refuses.
	binding, bindErr := router.add(group)
	if bindErr != nil {
		t.Fatalf("bind the group: %v", bindErr)
	}
	group.endpointMu.Lock()
	group.vhost = binding
	group.endpointMu.Unlock()
	// The state the wake finds: the wait is released, but the bind did not
	// succeed — no mark, while a binding is still in place for the extension
	// to fail against.
	group.boundOnce.Do(func() { close(group.bound) })

	group.mu.Lock()
	group.members = []*Tunnel{{Name: "web", Session: &Session{ID: "first"}, group: group}}
	group.mu.Unlock()

	session := &Session{ID: "joiner"}
	if _, err := manager.Register(session, protocol.ProxySpec{
		Name: "web", Type: protocol.ProxyTypeHTTP, LocalAddr: "127.0.0.1:1",
		Domains: []string{"taken.example"}, Group: "pool",
	}); err == nil {
		t.Fatal("a join whose hostname is owned by another proxy was published")
	}
	if len(session.usageCounters) != 0 {
		t.Fatalf("the refused join left a counter behind: %v", session.usageCounters)
	}
}
