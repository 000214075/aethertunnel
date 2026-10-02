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
// a probe that has not run yet never refuses traffic.
type healthState struct {
	mu      sync.Mutex
	healthy bool
	failed  int
	maxFail int
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

// startHealthChecks probes every proxy that declares a health check for the
// client's lifetime. The first probe runs at once, so a service that is down
// when the client starts is refused from the first visitor.
func (c *client) startHealthChecks(ctx context.Context) {
	for _, proxy := range c.cfg.Proxies {
		if proxy.HealthCheck == nil {
			continue
		}
		state := newHealthState(proxy.HealthCheck.MaxFailed)
		c.setHealthState(proxy.Name, state)
		go c.runHealthCheck(ctx, proxy, state)
	}
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

	if event := state.record(probe()); event == eventRefused {
		c.logger.Printf("proxy %q: the local service failed its first %d health check(s); dials are refused until it recovers", proxy.Name, h.MaxFailed)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		switch event := state.record(probe()); event {
		case eventRecovered:
			c.logger.Printf("proxy %q: the local service is healthy again; dials are accepted", proxy.Name)
		case eventRefused:
			c.logger.Printf("proxy %q: the local service failed %d health checks; dials are refused until it recovers", proxy.Name, h.MaxFailed)
		}
	}
}
