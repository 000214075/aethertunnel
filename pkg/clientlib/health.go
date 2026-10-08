package clientlib

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
)

// errLocalUnhealthy is the refusal a visitor sees when the proxy's local
// service is failing its health check.
var errLocalUnhealthy = errors.New("the local service is unhealthy (health check); refusing")

// probeTransport carries every health probe. It is deliberately not
// http.DefaultTransport, whose Proxy is http.ProxyFromEnvironment: a probe
// pointed at a local_addr that is not loopback was sent through whatever
// HTTP_PROXY the process inherited, so a healthy service behind an unreachable
// proxy was judged down and its tunnel was withdrawn with no service fault
// behind it. Keep-alives are off: the next probe is a fresh look at a service
// that may be gone.
var probeTransport = &http.Transport{
	Proxy:             nil,
	DisableKeepAlives: true,
}

// probeHealth runs one health check against the local service behind a proxy.
//
// A tcp probe only has to connect. An http probe has to get a 2xx answer: the
// probe is what decides whether the public endpoint stays published, and a
// service that answers "gone" or "error" is not serving. A redirect is followed,
// which is what frp's probe does as well: the status that decides is the one at the
// end of the chain. frp accepts exactly 200
// where this accepts the whole 2xx class — 204 from a health endpoint is a healthy
// answer — and frp's healthCheck.httpHeaders, which this implements, is how an
// endpoint that wants a credential is probed at all.
func probeHealth(proxy config.ProxyConfig, timeout time.Duration) bool {
	h := proxy.HealthCheck
	if h == nil {
		return true
	}
	if h.Type == "http" {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s%s", proxy.LocalAddr(), h.Path), nil)
		if err != nil {
			return false
		}
		for name, value := range h.HTTPHeaders {
			req.Header.Set(name, value)
		}
		resp, err := (&http.Client{Timeout: timeout, Transport: probeTransport}).Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	conn, err := net.DialTimeout("tcp", proxy.LocalAddr(), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// healthEvent is what one probe changed.
type healthEvent int

const (
	eventNone healthEvent = iota
	eventRecovered
	eventRefused
)

// healthState is one proxy's view of its local service. It starts healthy, so
// a probe that has not run yet never refuses traffic. withdrawn records that
// this client asked the server to stop publishing the proxy while the service
// is down; a recovered probe publishes it again.
type healthState struct {
	mu        sync.Mutex
	healthy   bool
	failed    int
	maxFail   int
	withdrawn bool
}

func newHealthState(maxFail int) *healthState {
	return &healthState{healthy: true, maxFail: maxFail}
}

// record folds a probe result in and reports what it changed: a service that
// crosses max_failed consecutive failures is refused until a probe succeeds.
func (h *healthState) record(ok bool) healthEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		// A success resets the run, whether or not it crossed the threshold:
		// max_failed counts *consecutive* failures, and leaving the count in place
		// let unrelated one-off failures add up until the tunnel was withdrawn for
		// a service that never failed three times in a row.
		h.failed = 0
		if h.healthy {
			return eventNone
		}
		h.healthy = true
		return eventRecovered
	}
	if !h.healthy {
		return eventNone
	}
	h.failed++
	if h.failed >= h.maxFail {
		h.healthy = false
		return eventRefused
	}
	return eventNone
}

func (h *healthState) isHealthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

// markWithdrawn records that the server stopped publishing this proxy.
func (h *healthState) markWithdrawn() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.withdrawn = true
}

func (h *healthState) isWithdrawn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.withdrawn
}

// clearWithdrawn drops the mark once the proxy really is published again.
func (h *healthState) clearWithdrawn() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.withdrawn = false
}

// resetWithdrawn drops a stale withdrawal mark before a fresh session
// registers every proxy again.
func (h *healthState) resetWithdrawn() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.withdrawn = false
}

// setHealthState installs one proxy's health state.
func (c *client) setHealthState(name string, state *healthState) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	if c.health == nil {
		c.health = map[string]*healthState{}
	}
	c.health[name] = state
}

// healthStateFor returns one proxy's health state, nil when the proxy has no
// check.
func (c *client) healthStateFor(name string) *healthState {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	return c.health[name]
}

// proxyHealthy reports whether the proxy's local service passed its health
// check. A proxy without a check, or one whose check has not started, is
// healthy by definition.
func (c *client) proxyHealthy(proxy config.ProxyConfig) bool {
	if proxy.HealthCheck == nil {
		return true
	}
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	state := c.health[proxy.Name]
	return state == nil || state.isHealthy()
}

// withdrawUnhealthy unpublishes a proxy whose local service is failing its
// check: the server drops the tunnel and its public endpoint, so visitors stop
// reaching a dead backend. A server that predates withdrawals stays silent, a
// session that is not up yet has no control connection, and in both cases the
// proxy stays registered and the dial refusal in serveStream protects visitors.
// The currency check and the withdrawal are one step under reloadMu. A
// reload that starts after the check waits for the withdrawal to finish,
// so the fresh registration it writes lands on the server after this
// frame instead of being withdrawn by it; a reload that already replaced
// this probe fails the check. Without both, a stale failure withdraws the
// proxy the reload just published, and the replacement starts healthy
// with no withdrawal mark, so nothing would publish the name again until
// the next reconnect. refreshDispatch and reWithdrawUnhealthy take the
// same lock for the same reason; the lock order is reloadMu before the
// withdrawMu withdrawProxy takes, like every other holder.
func (c *client) withdrawUnhealthy(ctx context.Context, proxy config.ProxyConfig, state *healthState) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	c.healthStopsMu.Lock()
	entry, live := c.healthStops[proxy.Name]
	c.healthStopsMu.Unlock()
	if ctx.Err() != nil || (live && entry.ctx != ctx) {
		return
	}
	logging.Warnf(c.logger, "proxy %q: the local service failed its health check; withdrawing the tunnel until it recovers", proxy.Name)
	switch err := c.withdrawProxy(proxy); {
	case err == nil:
		state.markWithdrawn()
	case errors.Is(err, errWithdrawUnsupported):
		logging.Warnf(c.logger, "proxy %q: dials are refused locally until it recovers", proxy.Name)
	default:
		logging.Warnf(c.logger, "proxy %q: the withdrawal did not go through (%v); dials are refused locally until it recovers", proxy.Name, err)
	}
}

// resetHealthWithdrawals clears stale withdrawal markers: a fresh session is
// about to publish every configured proxy again, so a withdrawal recorded
// against the previous session no longer describes the server.
func (c *client) resetHealthWithdrawals() {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	for _, state := range c.health {
		state.resetWithdrawn()
	}
}

// reWithdrawUnhealthy runs right after a session publishes the proxies and
// withdraws again those whose local service was already failing: the fresh
// registration put a dead backend back on a public endpoint, and the health
// state will not fire a new refusal event for a failure it already recorded.
func (c *client) reWithdrawUnhealthy() {
	// The whole pass runs under reloadMu: a reload that lands mid-pass would
	// stop the health check and drop the state this pass just read, and the
	// withdrawal it then writes could land after the reload's register of the
	// replacement spec — the server would withdraw the proxy the reload just
	// published, with nothing to re-register it until the next reconnect.
	// refreshDispatch takes the same lock for the same reason.
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	for _, proxy := range c.proxyList() {
		if proxy.HealthCheck == nil {
			continue
		}
		c.healthMu.Lock()
		state := c.health[proxy.Name]
		c.healthMu.Unlock()
		if state == nil || state.isHealthy() || state.isWithdrawn() {
			continue
		}
		if err := c.withdrawProxy(proxy); err == nil {
			state.markWithdrawn()
			c.logger.Printf("proxy %q: the local service is still unhealthy; the tunnel stays withdrawn", proxy.Name)
		}
	}
}

// startHealthChecks probes every proxy that declares a health check for the
// client's lifetime. The first probe runs at once, so a service that is down
// when the client starts is refused from the first visitor.
func (c *client) startHealthChecks(ctx context.Context) {
	for _, proxy := range c.proxyList() {
		c.startHealthCheck(proxy)
	}
}

// startHealthCheck probes one proxy until a reload stops it. Its context hangs
// healthProbe is one running health-check probe: the context that stops it
// and the cancel function paired with it.
type healthProbe struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// off the client's lifetime, so stopping one probe never touches another.
func (c *client) startHealthCheck(proxy config.ProxyConfig) {
	if proxy.HealthCheck == nil {
		return
	}
	c.healthStopsMu.Lock()
	if _, live := c.healthStops[proxy.Name]; live {
		c.healthStopsMu.Unlock()
		return
	}
	probeCtx, cancel := context.WithCancel(c.baseCtx)
	if c.healthStops == nil {
		c.healthStops = map[string]healthProbe{}
	}
	c.healthStops[proxy.Name] = healthProbe{ctx: probeCtx, cancel: cancel}
	c.healthStopsMu.Unlock()

	maxFail := proxy.HealthCheck.MaxFailed
	if maxFail <= 0 {
		// config.Load fills this default in, and Run accepts a configuration built in
		// code, where a zero would refuse the proxy on its first failed probe instead
		// of the documented three consecutive ones.
		maxFail = 3
	}
	state := newHealthState(maxFail)
	c.setHealthState(proxy.Name, state)
	go func() {
		// Drop the entry only while it is still this goroutine's probe: a
		// reload that replaced the probe in the meantime must not let this
		// finishing one unregister its successor, which would leave the new
		// probe uncancellable.
		defer c.forgetHealthStop(proxy.Name, probeCtx)
		c.runHealthCheck(probeCtx, proxy, state)
	}()
}

// stopAllHealthChecks cancels every running probe. It is what a failed Run
// uses to undo startHealthChecks: an embedder's context stays alive after Run
// has returned an error, so the probes must not keep dialing local services.
func (c *client) stopAllHealthChecks() {
	c.healthStopsMu.Lock()
	names := make([]string, 0, len(c.healthStops))
	for name := range c.healthStops {
		names = append(names, name)
	}
	c.healthStopsMu.Unlock()
	for _, name := range names {
		c.stopHealthCheck(name)
	}
}

// stopHealthCheck cancels one proxy's probe and drops its state, so a reload
// that replaces the proxy starts from healthy again.
func (c *client) stopHealthCheck(name string) {
	c.healthStopsMu.Lock()
	probe, live := c.healthStops[name]
	delete(c.healthStops, name)
	c.healthStopsMu.Unlock()
	if live {
		probe.cancel()
	}
	c.healthMu.Lock()
	delete(c.health, name)
	c.healthMu.Unlock()
}

// forgetHealthStop removes the registration for name only when it still names
// the probe identified by ctx.
func (c *client) forgetHealthStop(name string, ctx context.Context) {
	c.healthStopsMu.Lock()
	if cur, ok := c.healthStops[name]; ok && cur.ctx == ctx {
		delete(c.healthStops, name)
	}
	c.healthStopsMu.Unlock()
}

// probeCurrent reports whether ctx is still the registered probe for name. Unlike
// the stale check a running probe makes, an absent entry counts as not current: it
// means a reload stopped this probe, so its results no longer describe the proxy
// that is running now.
func (c *client) probeCurrent(name string, ctx context.Context) bool {
	c.healthStopsMu.Lock()
	defer c.healthStopsMu.Unlock()
	entry, live := c.healthStops[name]
	return live && entry.ctx == ctx
}

func (c *client) runHealthCheck(ctx context.Context, proxy config.ProxyConfig, state *healthState) {
	h := proxy.HealthCheck
	interval := time.Duration(h.IntervalS) * time.Second
	if interval <= 0 {
		// config.Load fills this default in, but Run also takes a configuration
		// built in code, and time.NewTicker panics on a non-positive interval.
		interval = 10 * time.Second
	}
	timeout := time.Duration(h.TimeoutS) * time.Second
	if timeout <= 0 {
		// The same path: a zero dial timeout means no timeout at all, so the
		// probe would never return and the schedule would never tick again.
		timeout = 3 * time.Second
	}

	probe := func() bool { return probeHealth(proxy, timeout) }

	// stale reports whether a newer probe has replaced this one for the same name.
	// A reload cancels this context and registers the replacement under the same
	// name; a result produced before the cancel that acted afterwards would withdraw
	// the name the replacement just published, and nothing would publish it again —
	// the replacement starts from healthy, so it carries no withdrawal mark to clear
	// and never re-registers. An absent entry means no replacement exists.
	stale := func() bool {
		c.healthStopsMu.Lock()
		defer c.healthStopsMu.Unlock()
		entry, live := c.healthStops[proxy.Name]
		return live && entry.ctx != ctx
	}

	apply := func(ok bool) {
		if ctx.Err() != nil || stale() {
			return
		}
		if event := state.record(ok); event == eventRefused {
			c.withdrawUnhealthy(ctx, proxy, state)
			return
		} else if event == eventRecovered && !state.isWithdrawn() {
			c.logger.Printf("proxy %q: the local service is healthy again; dials are accepted", proxy.Name)
		}
		// Every healthy probe while the mark is set republishes, not only the one
		// that first reported the recovery: record reports a recovery once, and a
		// republish that fails on a session which is briefly wedged — or a
		// withdrawal that landed after the service was already healthy again —
		// would otherwise leave the mark set and the proxy unpublished while its
		// local service is fine.
		if !ok || !state.isWithdrawn() {
			return
		}
		if err := c.registerProxyAgain(ctx, proxy); err != nil {
			logging.Warnf(c.logger, "proxy %q: the local service is healthy again, but re-publishing the tunnel failed: %v", proxy.Name, err)
			return
		}
		// The write only asked the server to publish the proxy again; the server
		// can still answer with a refusal, which the session reader records as one
		// log line. Dropping the mark here would stop the retries with the name
		// unpublished while the log says it is published. The mark goes when the
		// server's proxy list names the proxy again, in the session reader.
		c.logger.Printf("proxy %q: the local service is healthy again; re-sent the registration, waiting for the server to confirm", proxy.Name)
	}

	first := probe()
	if ctx.Err() != nil || stale() {
		// A reload replaced this proxy and cancelled the probe while this first
		// result was being produced: it is about the old registration and must not
		// be applied to the new one. The ticker's own probe below says the same.
		return
	}
	apply(first)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ok := probe()
		if ctx.Err() != nil || stale() {
			// A reload replaced this proxy and cancelled the probe while it ran:
			// the result is about the old registration and must not be applied to
			// the new one, or a stale failure would withdraw the reloaded proxy.
			return
		}
		apply(ok)
		// A withdrawal that raced the session coming up, or one the server
		// refused for a transient reason, is retried while the service stays
		// down. A server that stays silent has already set its flag, so this
		// costs nothing there.
		if !ok && !state.isHealthy() && !state.isWithdrawn() && !c.withdrawIsUnsupported() && !stale() {
			c.withdrawUnhealthy(ctx, proxy, state)
		}
	}
}
