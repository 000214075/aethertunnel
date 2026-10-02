package clientlib

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"reflect"
	"syscall"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// proxyList returns the dispatch list: what registerProxies publishes, what
// findProxy serves a stream from, and what a reload diffs against. It is a
// snapshot; the list a reload installs replaces it wholesale.
func (c *client) proxyList() []config.ProxyConfig {
	c.proxiesMu.RLock()
	defer c.proxiesMu.RUnlock()
	return c.proxies
}

func (c *client) setProxyList(latest []config.ProxyConfig) {
	c.proxiesMu.Lock()
	defer c.proxiesMu.Unlock()
	c.proxies = latest
}

func (c *client) visitorList() []config.VisitorConfig {
	c.visitorsMu.Lock()
	defer c.visitorsMu.Unlock()
	return c.visitors
}

// startReloadWatcher watches SIGHUP when the configuration came from a file:
// the signal re-reads it and applies the difference to the running client. A
// configuration that arrived as a string — the mobile binding's path — has no
// file to re-read and gets no watcher.
func (c *client) startReloadWatcher() {
	if c.cfg.SourceFile == "" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-c.baseCtx.Done():
				return
			case <-signals:
				c.reloadFromFile(c.cfg.SourceFile)
			}
		}
	}()
}

// reloadFromFile re-reads and validates the configuration file. A file that
// fails to load leaves the running client untouched: a running tunnel that
// ignores a half-finished edit beats one that dies on it.
func (c *client) reloadFromFile(path string) {
	newCfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient})
	if newCfg != nil {
		for _, warning := range newCfg.Warnings {
			c.logger.Printf("warning: %s", warning)
		}
	}
	if err != nil {
		c.logger.Printf("reload of %s refused; the running configuration stays: %v", path, err)
		return
	}
	c.applyReload(newCfg)
}

// applyReload swaps the parts of the configuration the running client can pick
// up live — the proxy and visitor sets. Sections the session cannot change
// keep the values they started with, and a change there says so by name: the
// client tells the operator it applies after a restart rather than changing
// something silently.
func (c *client) applyReload(newCfg *config.Config) {
	for _, section := range []struct {
		name   string
		old    any
		latest any
	}{
		{"[client]", c.cfg.Client, newCfg.Client},
		{"[transport]", c.cfg.Transport, newCfg.Transport},
		{"[encryption]", c.cfg.Encryption, newCfg.Encryption},
		{"[identity]", c.cfg.Identity, newCfg.Identity},
		{"[obfuscation]", c.cfg.Obfuscation, newCfg.Obfuscation},
		{"[dht]", c.cfg.DHT, newCfg.DHT},
		{"[vpn]", c.cfg.VPN, newCfg.VPN},
	} {
		if !reflect.DeepEqual(section.old, section.latest) {
			c.logger.Printf("reload: %s changed; it applies after a restart", section.name)
		}
	}

	c.applyProxyReload(newCfg.Proxies)
	c.applyVisitorReload(newCfg.Visitors)
}

// applyProxyReload diffs the proxy set by name and converges the running
// client on it: the dispatch list swaps at once, health checks follow their
// proxies, and the server is told what changed — withdrawals for removed and
// replaced proxies, registrations for added and replaced ones. A session that
// is down (or a server that predates withdrawals) hears about it when the
// session rebuilds: the new dispatch list is what registerProxies publishes.
func (c *client) applyProxyReload(latest []config.ProxyConfig) {
	previous := make(map[string]config.ProxyConfig, len(c.proxyList()))
	for _, proxy := range c.proxyList() {
		previous[proxy.Name] = proxy
	}
	current := make(map[string]config.ProxyConfig, len(latest))
	for _, proxy := range latest {
		current[proxy.Name] = proxy
	}

	var added, changed, removed []string
	for name, proxy := range current {
		was, exists := previous[name]
		switch {
		case !exists:
			added = append(added, name)
		case !reflect.DeepEqual(was, proxy):
			changed = append(changed, name)
		}
	}
	for name := range previous {
		if _, still := current[name]; !still {
			removed = append(removed, name)
		}
	}

	// The dispatch list swaps first: a stream that arrives from now on is
	// served by the new configuration, whatever the exchange with the server
	// below manages to say.
	c.setProxyList(latest)

	for _, name := range changed {
		c.stopHealthCheck(name)
		c.forgetStaticFile(name)
	}
	for _, name := range removed {
		c.stopHealthCheck(name)
		c.forgetStaticFile(name)
	}
	for _, name := range added {
		c.startHealthCheck(current[name])
	}
	for _, name := range changed {
		c.startHealthCheck(current[name])
	}

	for _, name := range removed {
		c.withdrawQuietly(previous[name])
	}
	for _, name := range changed {
		c.withdrawQuietly(previous[name])
	}
	for _, name := range changed {
		c.registerQuietly(current[name])
	}
	for _, name := range added {
		c.registerQuietly(current[name])
	}

	c.logger.Printf("reload: %d proxy(es) added, %d changed, %d removed, %d unchanged",
		len(added), len(changed), len(removed), len(current)-len(added)-len(changed))
}

// withdrawQuietly removes a proxy's registration from the server. A server that
// predates withdrawals stays silent and the proxy keeps its endpoint until the
// session rebuilds; a session that is down has nothing to hear about it.
func (c *client) withdrawQuietly(proxy config.ProxyConfig) {
	if err := c.withdrawProxy(proxy); err == nil {
		c.logger.Printf("reload: %q withdrawn from the server", proxy.Name)
	} else if !errors.Is(err, errWithdrawUnsupported) {
		c.logger.Printf("reload: withdraw %q: %v; the tunnel disappears when the session rebuilds", proxy.Name, err)
	}
}

// registerQuietly publishes a proxy the new configuration introduced or
// replaced. Without a live session the spec is remembered instead: the next
// registerProxies publishes it.
func (c *client) registerQuietly(proxy config.ProxyConfig) {
	spec := proxySpec(proxy)
	c.rememberSpec(spec)
	framer := c.currentFramer()
	if framer == nil {
		c.logger.Printf("reload: %q takes effect when the session comes back", proxy.Name)
		return
	}
	if err := framer.WriteJSON(protocol.TypeRegisterProxy, spec); err != nil {
		c.logger.Printf("reload: register %q: %v; it takes effect when the session comes back", proxy.Name, err)
		return
	}
	c.logger.Printf("reload: %q registered", proxy.Name)
}

// applyVisitorReload restarts the visitor listeners when the set changed. The
// old context's cancellation closes every listener, so a visitor connection in
// flight is dropped exactly once per reload — the same thing a restart does,
// minus the restart.
func (c *client) applyVisitorReload(latest []config.VisitorConfig) {
	current := c.visitorList()
	if reflect.DeepEqual(current, latest) {
		return
	}
	c.visitorsMu.Lock()
	c.visitors = latest
	if c.visitorsCancel != nil {
		c.visitorsCancel()
	}
	c.visitorsCtx, c.visitorsCancel = context.WithCancel(c.baseCtx)
	ctx := c.visitorsCtx
	c.visitorsMu.Unlock()
	c.logger.Printf("reload: the visitor set changed; restarting %d listener(s)", len(latest))
	c.runVisitors(ctx)
}
