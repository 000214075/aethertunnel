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
)

// errLocalUnhealthy is the refusal a visitor sees when the proxy's local
// service is failing its health check.
var errLocalUnhealthy = errors.New("the local service is unhealthy (health check); refusing")

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
		if h.healthy {
			return eventNone
		}
		h.healthy = true
		h.failed = 0
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

// takeWithdrawn reports whether the proxy was withdrawn and clears the mark,
// so a recovered service is published again exactly once.
func (h *healthState) takeWithdrawn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.withdrawn
	h.withdrawn = false
	return w
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
func (c *client) withdrawUnhealthy(proxy config.ProxyConfig, state *healthState) {
	c.logger.Printf("proxy %q: the local service failed its health check; withdrawing the tunnel until it recovers", proxy.Name)
	switch err := c.withdrawProxy(proxy); {
	case err == nil:
		state.markWithdrawn()
	case errors.Is(err, errWithdrawUnsupported):
		c.logger.Printf("proxy %q: dials are refused locally until it recovers", proxy.Name)
	default:
		c.logger.Printf("proxy %q: the withdrawal did not go through (%v); dials are refused locally until it recovers", proxy.Name, err)
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
		c.healthStops = map[string]context.CancelFunc{}
	}
	c.healthStops[proxy.Name] = cancel
	c.healthStopsMu.Unlock()

	state := newHealthState(proxy.HealthCheck.MaxFailed)
	c.setHealthState(proxy.Name, state)
	go func() {
		defer c.forgetHealthStop(proxy.Name)
		c.runHealthCheck(probeCtx, proxy, state)
	}()
}

// stopHealthCheck cancels one proxy's probe and drops its state, so a reload
// that replaces the proxy starts from healthy again.
func (c *client) stopHealthCheck(name string) {
	c.healthStopsMu.Lock()
	cancel, live := c.healthStops[name]
	delete(c.healthStops, name)
	c.healthStopsMu.Unlock()
	if live {
		cancel()
	}
	c.healthMu.Lock()
	delete(c.health, name)
	c.healthMu.Unlock()
}

func (c *client) forgetHealthStop(name string) {
	c.healthStopsMu.Lock()
	delete(c.healthStops, name)
	c.healthStopsMu.Unlock()
}

func (c *client) runHealthCheck(ctx context.Context, proxy config.ProxyConfig, state *healthState) {
	h := proxy.HealthCheck
	interval := time.Duration(h.IntervalS) * time.Second
	timeout := time.Duration(h.TimeoutS) * time.Second

	probe := func() bool {
		if h.Type == "http" {
			url := fmt.Sprintf("http://%s%s", proxy.LocalAddr(), h.Path)
			client := &http.Client{Timeout: timeout}
			resp, err := client.Get(url)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return true
		}
		conn, err := net.DialTimeout("tcp", proxy.LocalAddr(), timeout)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}

	apply := func(ok bool) {
		switch event := state.record(ok); event {
		case eventRecovered:
			if state.takeWithdrawn() {
				if err := c.registerProxyAgain(proxy); err != nil {
					c.logger.Printf("proxy %q: the local service is healthy again, but re-publishing the tunnel failed: %v", proxy.Name, err)
					return
				}
				c.logger.Printf("proxy %q: the local service is healthy again; the tunnel is published again", proxy.Name)
				return
			}
			c.logger.Printf("proxy %q: the local service is healthy again; dials are accepted", proxy.Name)
		case eventRefused:
			c.withdrawUnhealthy(proxy, state)
		}
	}

	apply(probe())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ok := probe()
		apply(ok)
		// A withdrawal that raced the session coming up, or one the server
		// refused for a transient reason, is retried while the service stays
		// down. A server that stays silent has already set its flag, so this
		// costs nothing there.
		if !ok && !state.isHealthy() && !state.isWithdrawn() && !c.withdrawIsUnsupported() {
			c.withdrawUnhealthy(proxy, state)
		}
	}
}
