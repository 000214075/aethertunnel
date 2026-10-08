package clientlib

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
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
//
// The signal channel is registered once per process and never stopped, with
// the watchers hanging off a registry instead: a Notify/Stop pair per client
// instance churns the signal machinery on every test and every reconnect, and
// that churn is what trips os/signal's signal_recv into its inconsistent
// state crash on some platforms.
// stopReloadWatcher deregisters this client's SIGHUP watcher, so a later
// SIGHUP in the embedding process does not drive reloads of a client that
// never came up. The watcher goroutine and its done channel stay with the
// client's context, which is what closes them.
func (c *client) stopReloadWatcher() {
	if c.cfg.SourceFile == "" {
		return
	}
	reloadMu.Lock()
	delete(reloadWatchers, c)
	reloadMu.Unlock()
}

func (c *client) startReloadWatcher() {
	if c.cfg.SourceFile == "" {
		return
	}
	reloadMu.Lock()
	if reloadWatchers == nil {
		reloadWatchers = make(map[*client]chan struct{})
	}
	done := make(chan struct{})
	reloadWatchers[c] = done
	if reloadSignals == nil {
		reloadSignals = make(chan os.Signal, 1)
		signal.Notify(reloadSignals, syscall.SIGHUP)
		go distributeReloadSignals(reloadSignals)
	}
	reloadMu.Unlock()

	go func() {
		// The service lifetime, not baseCtx: see the field's comment.
		// stopStartupServices cancels it, so a Run that fails leaves no watcher
		// behind.
		<-c.serviceLifetime().Done()
		reloadMu.Lock()
		delete(reloadWatchers, c)
		reloadMu.Unlock()
		// The watcher's own lifetime ends here; the distributor drops it the
		// next time a SIGHUP arrives, and a client whose session is over has
		// nothing left to reload.
		close(done)
	}()
}

var (
	reloadMu       sync.Mutex
	reloadWatchers map[*client]chan struct{}
	reloadSignals  chan os.Signal
)

// distributeReloadSignals is the process's single SIGHUP consumer: every
// client that asked for a reload gets its own goroutine, so one slow reload
// never holds the others back.
func distributeReloadSignals(signals chan os.Signal) {
	for range signals {
		reloadMu.Lock()
		watchers := make([]*client, 0, len(reloadWatchers))
		for client := range reloadWatchers {
			watchers = append(watchers, client)
		}
		reloadMu.Unlock()
		for _, c := range watchers {
			go c.reloadFromFile(c.cfg.SourceFile)
		}
	}
}

// reloadFromFile re-reads and validates the configuration file. A file that
// fails to load leaves the running client untouched: a running tunnel that
// ignores a half-finished edit beats one that dies on it.
func (c *client) reloadFromFile(path string) error {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	// The SIGHUP that scheduled this run may predate the client stopping:
	// the distributor snapshots the registry before stopStartupServices
	// removes the watcher, so the goroutine can outlive the client it was
	// handed. Everything below starts long-lived services under the
	// caller's context, so a stopped client must not apply any of it.
	if c.stopped.Load() {
		return nil
	}

	newCfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient})
	if newCfg != nil {
		for _, warning := range newCfg.Warnings {
			c.logger.Printf("warning: %s", warning)
		}
	}
	if err != nil {
		logging.Warnf(c.logger, "reload of %s refused; the running configuration stays: %v", path, err)
		return err
	}
	// config.Load has read the file and run validation, which is long enough
	// for Run to fail in between: the entry check above cannot see a stop
	// that happens during it, and applyReload would start services for a
	// client that no longer exists.
	if c.stopped.Load() {
		return nil
	}
	c.applyReload(newCfg)
	return nil
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
		// Every section a client configuration can carry is either applied by
		// this reload ([[proxies]], [[visitors]], client.start) or announced
		// here: a change that neither takes effect nor says so is a change the
		// operator believes happened. The server-only sections stay out, since
		// a client configuration carrying one is warned about at load time.
		{"[client]", withoutStartAndToken(c.cfg.Client), withoutStartAndToken(newCfg.Client)},
		{"[transport]", c.cfg.Transport, newCfg.Transport},
		{"[encryption]", c.cfg.Encryption, newCfg.Encryption},
		{"[identity]", c.cfg.Identity, newCfg.Identity},
		{"[obfuscation]", c.cfg.Obfuscation, newCfg.Obfuscation},
		{"[dht]", c.cfg.DHT, newCfg.DHT},
		{"[vpn]", c.cfg.VPN, newCfg.VPN},
		{"[oidc]", c.cfg.OIDC, newCfg.OIDC},
		// [log] is read once, when the process builds its logger, so a change
		// to where the log goes waits for the next start like the rest.
		{"[log]", c.cfg.Log, newCfg.Log},
		// [store] is read once at startup, so a changed path takes effect then.
		{"[store]", c.cfg.Store, newCfg.Store},
	} {
		if !reflect.DeepEqual(section.old, section.latest) {
			c.logger.Printf("reload: %s changed; it applies after a restart", section.name)
		}
	}

	// The auth token — including one re-read from auth_token_file — is the
	// one part of [client] that applies live: the next reconnect sends it.
	// Without adopting it here, a rotated token would leave the client
	// looping on auth rejections until a real restart.
	c.cfgMu.Lock()
	tokenChanged := newCfg.Client.AuthToken != "" && newCfg.Client.AuthToken != c.cfg.Client.AuthToken
	// Read while the old token is in hand: whether the previous configuration
	// carried a token at all is decided inside the same section that would
	// change it, not after the unlock.
	hadToken := c.cfg.Client.AuthToken != ""
	if tokenChanged {
		c.cfg.Client.AuthToken = newCfg.Client.AuthToken
	}
	c.cfgMu.Unlock()
	if tokenChanged {
		c.logger.Printf("reload: a new client.auth_token applies on the next reconnect")
		// When encryption keys are derived from the auth token ([encryption]
		// enabled with no passphrase of its own), the cipher the client was
		// built with belongs to the old token: a server restarted on the
		// rotated token could not decrypt anything this client sends, and
		// every reconnect would die unread — the very symptom adopting the
		// token here is meant to avoid. The swap goes through the atomic the
		// dial paths read, so a dial in flight mid-reload uses one cipher or
		// the other, never a torn one. The algorithm and salt stay the ones
		// the session started with: an [encryption] change says it applies
		// after a restart, and only the token is adopted live.
		if c.cfg.Encryption.Enabled && c.cfg.Encryption.Passphrase == "" {
			rebuilt, err := crypto.NewCipher(c.cfg.Encryption.Algorithm, newCfg.Client.AuthToken, c.cfg.Encryption.Salt)
			if err != nil {
				c.logger.Printf("reload: cannot rebuild the encryption key from the new auth token: %v", err)
			} else {
				c.cipher.Store(rebuilt)
			}
		}
	}

	// A deleted token has no live adoption to apply: there is no new value to
	// send, so the running client can only keep authenticating with the old
	// one — and the [client] comparison above is blind to it, because it drops
	// the token fields. Without this line the operator deletes auth_token_file
	// (or the inline token), sees no reaction at all, and the old token stays
	// in the running process until the server rotates its key and every
	// session starts failing authentication. Saying it is all a reload can do.
	if newCfg.Client.AuthToken == "" && hadToken {
		c.logger.Printf("reload: the new configuration carries no auth token; the running client keeps the previous one")
	}

	selectedProxies, selectedVisitors := selectByStart(newCfg, c.logger)
	// The file's selection replaces the file's side of the dispatch lists; the
	// store's entries are merged back on, so a reload of the file neither drops
	// a runtime entry nor resurrects one that was deleted.
	c.setFileLists(selectedProxies, selectedVisitors)
	if c.store != nil {
		// Judging a later runtime write needs the lists this reload just selected,
		// so the store's snapshot is refreshed with them.
		c.store.own(c.validationConfig())
	}
	mergedProxies, mergedVisitors := c.effectiveLists()
	if c.store != nil {
		// The file and the store are merged by name, so this reload can introduce a
		// conflict neither side can repair — a port the file now asks for that a
		// stored entry already holds. Every later admin write is judged against that
		// set, and its refusal would name a pair the operator never put together.
		storedProxies, storedVisitors := c.store.list()
		if err := validateEntries(c.validationConfig(), storedProxies, storedVisitors); err != nil {
			c.logger.Printf("reload: the file and the stored entries do not coexist, so until one of them changes the store cannot be written: %v", err)
		}
	}
	c.applyProxyReload(mergedProxies)
	c.applyVisitorReload(mergedVisitors)
}

// setFileLists records what the configuration itself selects.
func (c *client) setFileLists(proxies []config.ProxyConfig, visitors []config.VisitorConfig) {
	c.fileMu.Lock()
	c.fileProxies, c.fileVisitors = proxies, visitors
	c.fileMu.Unlock()
}

// effectiveLists is the file's selection with the store's entries merged on top.
// A store entry wins over a configured one of the same name, which is frp's rule
// for the same conflict.
func (c *client) effectiveLists() ([]config.ProxyConfig, []config.VisitorConfig) {
	c.fileMu.Lock()
	proxies := append([]config.ProxyConfig(nil), c.fileProxies...)
	visitors := append([]config.VisitorConfig(nil), c.fileVisitors...)
	c.fileMu.Unlock()
	if c.store == nil {
		return proxies, visitors
	}
	storedProxies, storedVisitors := c.store.list()
	// selectByStart already dropped the file's enabled = false entries; the store's
	// entries were merged after it and never judged, so a dynamic entry carrying
	// enabled = false was published while the same entry in the file was not.
	return enabledOnly(mergeProxies(proxies, storedProxies)),
		enabledOnly(mergeVisitors(visitors, storedVisitors))
}

// enabledOnly drops the entries a configuration turned off with enabled = false.
func enabledOnly[T interface{ IsEnabled() bool }](in []T) []T {
	disabled := false
	for _, entry := range in {
		if !entry.IsEnabled() {
			disabled = true
			break
		}
	}
	if !disabled {
		return in
	}
	out := make([]T, 0, len(in))
	for _, entry := range in {
		if entry.IsEnabled() {
			out = append(out, entry)
		}
	}
	return out
}

// mergeProxies appends the entries that are not already named, and replaces the
// ones that are, keeping the configured order for everything the store does not
// touch — the first member of a pool owns its endpoint, so the order matters.
func mergeProxies(base, over []config.ProxyConfig) []config.ProxyConfig {
	replaced := make(map[string]bool, len(over))
	for _, proxy := range over {
		replaced[proxy.Name] = true
	}
	merged := make([]config.ProxyConfig, 0, len(base)+len(over))
	seen := make(map[string]bool, len(base)+len(over))
	for _, proxy := range base {
		if replaced[proxy.Name] {
			continue
		}
		merged = append(merged, proxy)
		seen[proxy.Name] = true
	}
	for _, proxy := range over {
		if !seen[proxy.Name] {
			merged = append(merged, proxy)
			seen[proxy.Name] = true
		}
	}
	return merged
}

// mergeVisitors is mergeProxies for the visitor lists.
func mergeVisitors(base, over []config.VisitorConfig) []config.VisitorConfig {
	replaced := make(map[string]bool, len(over))
	for _, visitor := range over {
		replaced[visitor.Name] = true
	}
	merged := make([]config.VisitorConfig, 0, len(base)+len(over))
	seen := make(map[string]bool, len(base)+len(over))
	for _, visitor := range base {
		if replaced[visitor.Name] {
			continue
		}
		merged = append(merged, visitor)
		seen[visitor.Name] = true
	}
	for _, visitor := range over {
		if !seen[visitor.Name] {
			merged = append(merged, visitor)
			seen[visitor.Name] = true
		}
	}
	return merged
}

// withoutStartAndToken drops from [client] the three fields this reload applies
// itself: the client.start selection, the auth token, and the path that token may
// be re-read from. None of them waits for a restart, so the "[client] changed; it
// applies after a restart" message must not cover them — a rotated token, or the
// token of a newly named auth_token_file, would otherwise be announced as
// restart-only one line before the reload adopts it live.
func withoutStartAndToken(cfg config.ClientConfig) config.ClientConfig {
	cfg.Start = nil
	cfg.AuthToken = ""
	cfg.AuthTokenFile = ""
	return cfg
}

// selectByStart narrows the dispatch lists to what runs: the names client.start
// selects, when it names any, and the entries whose enabled is not false. An
// empty start list keeps everything, which is what a configuration without the
// key means. A selected name that nothing defines was already reported as a
// warning by validation, so it is not repeated here.
func selectByStart(cfg *config.Config, logger *log.Logger) ([]config.ProxyConfig, []config.VisitorConfig) {
	start := cfg.Client.StartSet()
	// Nothing to narrow: hand back exactly what the configuration holds, so a
	// reload compares its selection with the running set by value and finds no
	// change — an empty slice built here would differ from a nil one and
	// restart every listener on every reload.
	if start == nil {
		enabled := 0
		for _, proxy := range cfg.Proxies {
			if proxy.IsEnabled() {
				enabled++
			}
		}
		for _, visitor := range cfg.Visitors {
			if visitor.IsEnabled() {
				enabled++
			}
		}
		if enabled == len(cfg.Proxies)+len(cfg.Visitors) {
			return cfg.Proxies, cfg.Visitors
		}
	}

	selected := func(name string) bool {
		if start == nil {
			return true
		}
		_, ok := start[name]
		return ok
	}

	disabled := 0
	proxies := make([]config.ProxyConfig, 0, len(cfg.Proxies))
	for _, proxy := range cfg.Proxies {
		if !proxy.IsEnabled() {
			disabled++
			continue
		}
		if selected(proxy.Name) {
			proxies = append(proxies, proxy)
		}
	}
	visitors := make([]config.VisitorConfig, 0, len(cfg.Visitors))
	for _, visitor := range cfg.Visitors {
		if !visitor.IsEnabled() {
			disabled++
			continue
		}
		if selected(visitor.Name) {
			visitors = append(visitors, visitor)
		}
	}
	if disabled > 0 {
		logger.Printf("%d entry(s) carry enabled = false and are left stopped", disabled)
	}
	if len(proxies) != len(cfg.Proxies) || len(visitors) != len(cfg.Visitors) {
		logger.Printf("client.start selects %d of %d tunnel(s) and %d of %d visitor(s)",
			len(proxies), len(cfg.Proxies), len(visitors), len(cfg.Visitors))
	}
	return proxies, visitors
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
		c.forgetPluginCaches(name)
	}
	for _, name := range removed {
		c.stopHealthCheck(name)
		c.forgetStaticFile(name)
		c.forgetPluginCaches(name)
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
	spec := proxySpec(proxy, c.cfg.Client.User)
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
// old context's cancellation closes the listeners, so a connection that has not
// been accepted yet is dropped once per reload; a session that already reached
// the relay is not, because flynet.Pipe is not bound to the context — it
// serves on until the connection goes idle for idle_timeout_seconds.
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
	// The new set is started here, and visitorsStarted keeps the session loop's
	// startVisitors from starting it again when this reload landed before the first
	// session: the second start would bind every visitor port twice and fail.
	c.visitorsStarted = true
	ctx := c.visitorsCtx
	c.visitorsMu.Unlock()
	c.logger.Printf("reload: the visitor set changed; restarting %d listener(s)", len(latest))
	c.runVisitors(ctx)
}
