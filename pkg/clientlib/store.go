package clientlib

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// The store is frp's [store] section: proxies and visitors created at runtime
// through the client's admin API, kept in a file, restored on the next start, and
// taking precedence over entries of the same name in the configuration file.
//
// A body that arrives as JSON is rendered as TOML and then parsed by the same
// decoder a configuration file goes through, so a dynamic entry gets the same
// defaults, the same remote_ports expansion and the same "this key means nothing"
// refusal as one written by hand — a store that accepted keys nothing reads would
// be exactly the failure the configuration loader exists to prevent.
//
// The file holds the bodies as JSON, keyed by the name the caller used, which is
// the shape frp's db.json has:
//
//	{"proxies": {"web": {"name": "web", "type": "tcp", "local_port": 22,
//	                      "remote_port": 6022}}, "visitors": {}}

// storeFile is the on-disk shape. Bodies are kept as they arrived, so a file
// written by hand and a file written by this code are the same thing.
type storeFile struct {
	Proxies  map[string]json.RawMessage `json:"proxies"`
	Visitors map[string]json.RawMessage `json:"visitors"`
}

// store holds the runtime entries and the file they live in. All methods are safe
// for concurrent use: the admin API serves them from its own goroutines.
type store struct {
	path   string
	logger *log.Logger
	// cfg is a snapshot of the configuration the entries live in, replaced by the
	// client whenever a reload changes it. Judging an entry needs it: the rules for
	// a plugin's target list, a visitor's bind address or a duplicated name are
	// about the set, not about the single body that was parsed. It is a copy rather
	// than the client's own configuration because a reload adopts a rotated
	// auth_token into that one while a write is validated here.
	cfg *config.Config

	mu sync.Mutex
	// proxies and visitors are keyed by the name the caller used, which is not
	// the same as a proxy's name when the body carries remote_ports: one entry
	// may expand into several proxies, exactly as it does in a file.
	proxies  map[string][]config.ProxyConfig
	visitors map[string][]config.VisitorConfig
	bodies   map[string]json.RawMessage // "proxy:name" or "visitor:name" -> the body as supplied
}

// newStore opens the store at path. An absent file is an empty store: the first
// entry the operator creates writes it.
func newStore(path string, logger *log.Logger) (*store, error) {
	s := &store{
		path:     path,
		logger:   logger,
		proxies:  map[string][]config.ProxyConfig{},
		visitors: map[string][]config.VisitorConfig{},
		bodies:   map[string]json.RawMessage{},
	}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		// An empty file is an empty store. It is what `touch store.json`, an
		// editor's first save, or a write that never happened leaves behind, and
		// refusing to start on it turns a harmless placeholder into an outage.
		return s, nil
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	// An entry this build cannot read is dropped the way an entry that fails
	// validation is dropped, and the rest of the store loads. It used to fail the
	// start, and the admin API that could delete it only comes up after a start:
	// a single body carrying a key this build does not know — a store written by
	// a newer client, then read back by an older one — was a permanent outage
	// that only hand-editing the file could clear. The dropped entry stays in the
	// file and is named again at the next start, and the first write for any
	// other entry rewrites the file without it.
	for name, body := range file.Proxies {
		proxies, err := s.parseProxies(name, body)
		if err != nil {
			logger.Printf("store %s: dropping proxy %q: %v", path, name, err)
			continue
		}
		s.proxies[name] = proxies
		s.bodies["proxy:"+name] = body
	}
	for name, body := range file.Visitors {
		visitors, err := s.parseVisitors(name, body)
		if err != nil {
			logger.Printf("store %s: dropping visitor %q: %v", path, name, err)
			continue
		}
		s.visitors[name] = visitors
		s.bodies["visitor:"+name] = body
	}
	return s, nil
}

// list returns every stored entry, sorted by name so two runs compare the same
// way. One stored entry can stand for several proxies when its body carries
// remote_ports, which is why this is a flattening step.
func (s *store) list() ([]config.ProxyConfig, []config.VisitorConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return flattenProxies(s.proxies), flattenVisitors(s.visitors)
}

// proxyNames returns the stored proxy entries' names, which is what a conflict
// message and the admin API report.
func (s *store) proxyNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.proxies))
	for name := range s.proxies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// visitorNames is proxyNames for the visitor entries.
func (s *store) visitorNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.visitors))
	for name := range s.visitors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// putProxy stores a proxy body and writes the file. The name in the body must
// match the name in the request when it carries one: a body that names another
// tunnel would otherwise replace a different entry than the caller asked for.
func (s *store) putProxy(name string, body []byte) ([]config.ProxyConfig, error) {
	if err := checkBodyName(name, body); err != nil {
		return nil, err
	}
	proxies, err := s.parseProxies(name, body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	newProxies := maps.Clone(s.proxies)
	if newProxies == nil {
		newProxies = map[string][]config.ProxyConfig{}
	}
	newBodies := maps.Clone(s.bodies)
	if newBodies == nil {
		newBodies = map[string]json.RawMessage{}
	}
	newProxies[name] = proxies
	newBodies["proxy:"+name] = append(json.RawMessage{}, body...)
	// Before it is written and applied: parsing a body checks nothing about the
	// rules a proxy has to satisfy, and an entry that only failed at the next start
	// would have been live, and reachable, until then.
	if err := s.validateEntriesLocked(newProxies, s.visitors); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.writeStateLocked(newProxies, s.visitors, newBodies); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.proxies = newProxies
	s.bodies = newBodies
	s.mu.Unlock()
	return proxies, nil
}

// putVisitor stores a visitor body and writes the file.
func (s *store) putVisitor(name string, body []byte) ([]config.VisitorConfig, error) {
	if err := checkBodyName(name, body); err != nil {
		return nil, err
	}
	visitors, err := s.parseVisitors(name, body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	newVisitors := maps.Clone(s.visitors)
	if newVisitors == nil {
		newVisitors = map[string][]config.VisitorConfig{}
	}
	newBodies := maps.Clone(s.bodies)
	if newBodies == nil {
		newBodies = map[string]json.RawMessage{}
	}
	newVisitors[name] = visitors
	newBodies["visitor:"+name] = append(json.RawMessage{}, body...)
	if err := s.validateEntriesLocked(s.proxies, newVisitors); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.writeStateLocked(s.proxies, newVisitors, newBodies); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.visitors = newVisitors
	s.bodies = newBodies
	s.mu.Unlock()
	return visitors, nil
}

// deleteProxy removes a stored proxy entry. It reports whether there was one.
func (s *store) deleteProxy(name string) (bool, error) {
	s.mu.Lock()
	if _, ok := s.proxies[name]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	newProxies := maps.Clone(s.proxies)
	newBodies := maps.Clone(s.bodies)
	delete(newProxies, name)
	delete(newBodies, "proxy:"+name)
	if err := s.writeStateLocked(newProxies, s.visitors, newBodies); err != nil {
		s.mu.Unlock()
		return true, err
	}
	s.proxies = newProxies
	s.bodies = newBodies
	s.mu.Unlock()
	return true, nil
}

// deleteVisitor removes a stored visitor entry. It reports whether there was one.
func (s *store) deleteVisitor(name string) (bool, error) {
	s.mu.Lock()
	if _, ok := s.visitors[name]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	newVisitors := maps.Clone(s.visitors)
	newBodies := maps.Clone(s.bodies)
	delete(newVisitors, name)
	delete(newBodies, "visitor:"+name)
	if err := s.writeStateLocked(s.proxies, newVisitors, newBodies); err != nil {
		s.mu.Unlock()
		return true, err
	}
	s.visitors = newVisitors
	s.bodies = newBodies
	s.mu.Unlock()
	return true, nil
}

// bodiesFor returns the stored bodies, which is what GET /api/store reports.
func (s *store) bodiesFor() map[string]map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]json.RawMessage{"proxies": {}, "visitors": {}}
	for key, body := range s.bodies {
		if name, ok := strings.CutPrefix(key, "proxy:"); ok {
			out["proxies"][name] = body
			continue
		}
		if name, ok := strings.CutPrefix(key, "visitor:"); ok {
			out["visitors"][name] = body
		}
	}
	return out
}

// errStoreWrite marks a failure to persist the store: the handler answers it
// with a 5xx, because the caller's entry was fine but the disk refused it.
var errStoreWrite = errors.New("the store file could not be written")

// writeStateLocked renders and persists the state given to it, without
// touching the in-memory maps: a write that fails leaves them untouched, so
// the caller can answer with an error and keep serving what it served before.
// It is written beside its destination and moved into place, so a crash
// halfway through leaves the previous file whole rather than a half-written
// one that the next start refuses. The caller holds the lock.
func (s *store) writeStateLocked(proxies map[string][]config.ProxyConfig, visitors map[string][]config.VisitorConfig, bodies map[string]json.RawMessage) error {
	if s.path == "" {
		return nil
	}
	file := storeFile{Proxies: map[string]json.RawMessage{}, Visitors: map[string]json.RawMessage{}}
	for key, body := range bodies {
		if name, ok := strings.CutPrefix(key, "proxy:"); ok {
			file.Proxies[name] = body
			continue
		}
		if name, ok := strings.CutPrefix(key, "visitor:"); ok {
			file.Visitors[name] = body
		}
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(s.path)
	temp, err := os.CreateTemp(dir, ".aethertunnel-store-*")
	if err != nil {
		// A failure of the store's own directory, not of the body: the admin API
		// answers 500 for it, and a 400 would tell automation not to retry.
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	// The rename lands metadata without the data blocks on some
	// filesystems; the Sync is what makes a crash after the rename leave a
	// parseable file rather than an empty one the next start refuses.
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: %s: %v", errStoreWrite, s.path, err)
	}
	// The rename is durable only once the directory entry that carries it is:
	// without this flush a crash right after the write can leave the name
	// pointing at nothing, and the next start finds no store at all — every
	// runtime entry lost with it. pkg/ledger's New flushes the same way for the
	// same reason.
	if err := syncParentDir(s.path); err != nil {
		return fmt.Errorf("%w: %s: flush the directory: %v", errStoreWrite, s.path, err)
	}
	return nil
}

// writeLocked persists the current in-memory state. The caller holds the lock.
func (s *store) writeLocked() error {
	return s.writeStateLocked(s.proxies, s.visitors, s.bodies)
}

// dropConflicts removes the runtime entries that cannot coexist with the file
// configuration or with each other — a port claimed twice, a name used twice —
// and persists the trimmed state. It returns one line per dropped entry, so a
// start that would otherwise fail forever on a bad entry comes up without it
// and says exactly what it left out. The first entry in name order wins: the
// choice is deterministic, so a restart does not keep a different one.
func (s *store) dropConflicts(cfg *config.Config) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	var dropped []string

	// Nothing is dropped when the configuration itself is invalid: the problem is
	// then not a stored entry, and trimming them would destroy the operator's
	// entries to fix something they cannot fix by editing the store. The caller
	// reports the validation error and the client does not start.
	base := *cfg
	base.Proxies = append([]config.ProxyConfig(nil), cfg.Proxies...)
	base.Visitors = append([]config.VisitorConfig(nil), cfg.Visitors...)
	base.Warnings = nil
	if err := base.Validate(config.RoleClient); err != nil {
		return nil
	}

	// Visitors go first, validated against the file configuration and the
	// visitors accepted before them. Only accepted entries take part, so an
	// entry that is itself dropped cannot drag a valid one down with it, and
	// the proxy pass then sees a visitor list that is already known good.
	//
	// Each trial is merged the way the dispatch list merges: a stored entry
	// replaces a configured one of the same name, which is the store's rule.
	// Appending it to the file list instead made a legitimate override look like
	// a duplicate name and deleted it.
	var acceptedVisitors []config.VisitorConfig
	for _, name := range sortedKeys(s.visitors) {
		entry := s.visitors[name]
		trial := *cfg
		trial.Proxies = mergeProxies(cfg.Proxies, nil)
		trial.Visitors = mergeVisitors(cfg.Visitors, append(append([]config.VisitorConfig(nil), acceptedVisitors...), entry...))
		trial.Warnings = nil
		if err := trial.Validate(config.RoleClient); err != nil {
			delete(s.visitors, name)
			delete(s.bodies, "visitor:"+name)
			dropped = append(dropped, fmt.Sprintf("visitor %q: %v", name, err))
			continue
		}
		acceptedVisitors = append(acceptedVisitors, entry...)
	}

	// Proxies are validated against the file configuration, the proxies
	// accepted before them, and the visitors that survived the first pass.
	var acceptedProxies []config.ProxyConfig
	for _, name := range sortedKeys(s.proxies) {
		entry := s.proxies[name]
		trial := *cfg
		trial.Proxies = mergeProxies(cfg.Proxies, append(append([]config.ProxyConfig(nil), acceptedProxies...), entry...))
		trial.Visitors = mergeVisitors(cfg.Visitors, acceptedVisitors)
		trial.Warnings = nil
		if err := trial.Validate(config.RoleClient); err != nil {
			delete(s.proxies, name)
			delete(s.bodies, "proxy:"+name)
			dropped = append(dropped, fmt.Sprintf("proxy %q: %v", name, err))
			continue
		}
		acceptedProxies = append(acceptedProxies, entry...)
	}

	if len(dropped) > 0 {
		// Persist the trimmed state. A failed write leaves the file with the
		// dropped entries, which the next start trims again — the same
		// warning twice beats a start that cannot come up at all.
		_ = s.writeLocked()
	}
	return dropped
}

// checkBodyName refuses a body that names a tunnel other than the one in the
// request, which would silently move an entry instead of editing it.
func checkBodyName(name string, body []byte) error {
	var probe struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("the body is not a JSON object: %w", err)
	}
	if probe.Name != "" && probe.Name != name {
		return fmt.Errorf("the body names %q but the request names %q", probe.Name, name)
	}
	return nil
}

// parseProxies turns one stored body into the proxy entries it defines. An
// unknown key is refused rather than ignored, because a dynamic entry has no
// other way to tell its author that a key does nothing.
func (s *store) parseProxies(name string, body []byte) ([]config.ProxyConfig, error) {
	text, err := jsonBodyToTOML("proxy", name, body)
	if err != nil {
		return nil, err
	}
	proxies, _, warnings, err := config.LoadFragment(text, "store entry "+name, config.ValidateOptions{
		Role:              config.RoleClient,
		RejectUnknownKeys: true,
	})
	if err != nil {
		return nil, err
	}
	for _, warning := range warnings {
		s.logger.Printf("store: proxy %q: %s", name, warning)
	}
	if len(proxies) == 0 {
		return nil, errors.New("the entry did not define a proxy")
	}
	return proxies, nil
}

// parseVisitors is parseProxies for a visitor entry.
func (s *store) parseVisitors(name string, body []byte) ([]config.VisitorConfig, error) {
	text, err := jsonBodyToTOML("visitor", name, body)
	if err != nil {
		return nil, err
	}
	_, visitors, warnings, err := config.LoadFragment(text, "store entry "+name, config.ValidateOptions{
		Role:              config.RoleClient,
		RejectUnknownKeys: true,
	})
	if err != nil {
		return nil, err
	}
	for _, warning := range warnings {
		s.logger.Printf("store: visitor %q: %s", name, warning)
	}
	if len(visitors) == 0 {
		return nil, errors.New("the entry did not define a visitor")
	}
	return visitors, nil
}

// jsonBodyToTOML renders a JSON object as TOML, so the entry reaches the
// configuration decoder as the document it would have been. Only the value kinds
// a proxy or visitor entry can hold are accepted: strings, numbers, booleans,
// arrays of those, and one level of tables (health_check, request_headers,
// metas). Anything else is refused by name rather than dropped, which is the same
// promise the rest of the configuration makes.
func jsonBodyToTOML(kind, name string, body []byte) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return "", fmt.Errorf("the body is not a JSON object: %w", err)
	}
	if len(fields) == 0 {
		return "", errors.New("the body is empty")
	}
	// The name in the request wins, and the entry is completed with it, so a body
	// that leaves it out still defines one named tunnel.
	fields["name"] = name

	var scalars, tables []string
	for _, key := range sortedKeys(fields) {
		value := fields[key]
		if table, ok := value.(map[string]any); ok {
			rendered, err := tomlTable(kind, key, table)
			if err != nil {
				return "", err
			}
			tables = append(tables, rendered)
			continue
		}
		rendered, err := tomlValue(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		scalars = append(scalars, key+" = "+rendered)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "[[%s]]\n", tomlSection(kind))
	for _, line := range scalars {
		out.WriteString(line)
		out.WriteString("\n")
	}
	for _, table := range tables {
		out.WriteString(table)
	}
	return out.String(), nil
}

// tomlSection names the section a stored entry is decoded as. An entry is rendered
// as the document it would have been in the configuration file, and the decoder
// reads exactly two sections, so the plural is not the kind with an "s" appended: a
// proxy entry is [[proxies]]. Rendering "proxys" made every proxy entry unreadable
// — the decoder reported each of its keys as unknown — so the proxy half of the
// store could not accept anything at all.
func tomlSection(kind string) string {
	if kind == "proxy" {
		return "proxies"
	}
	return kind + "s"
}

// tomlTable renders a nested object, which TOML spells as a table header followed
// by its keys. A proxy's health_check takes this shape.
func tomlTable(kind, key string, fields map[string]any) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "[%s.%s]\n", tomlSection(kind), key)
	for _, field := range sortedKeys(fields) {
		rendered, err := tomlValue(fields[field])
		if err != nil {
			return "", fmt.Errorf("%s.%s: %w", key, field, err)
		}
		fmt.Fprintf(&out, "%s = %s\n", field, rendered)
	}
	return out.String(), nil
}

// tomlValue renders one scalar, array or inline table.
func tomlValue(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", errors.New("null has no TOML form; leave the key out instead")
	case string:
		return strconv.Quote(typed), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case json.Number:
		return typed.String(), nil
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			rendered, err := tomlValue(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, rendered)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		// A map of strings, such as request_headers: TOML has an inline table for
		// exactly this, and it keeps the entry on one line.
		parts := make([]string, 0, len(typed))
		for _, key := range sortedKeys(typed) {
			rendered, err := tomlValue(typed[key])
			if err != nil {
				return "", fmt.Errorf("%s: %w", key, err)
			}
			parts = append(parts, strconv.Quote(key)+" = "+rendered)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", fmt.Errorf("a value of type %T has no TOML form", value)
	}
}

// sortedKeys returns a map's keys in a deterministic order. The conflict
// trimming in dropConflicts relies on it: it accepts the first entry in name
// order, so a restart cannot keep a different one.
func sortedKeys[V any](fields map[string]V) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// own records the configuration the entries live in, so a write can be judged in
// the same context a start judges the file in.
func (s *store) own(cfg *config.Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// validateEntriesLocked judges the entries a write is about to commit. The caller
// holds mu.
func (s *store) validateEntriesLocked(proxies map[string][]config.ProxyConfig, visitors map[string][]config.VisitorConfig) error {
	cfg := s.cfg
	if cfg == nil {
		// A store used on its own (a test) has no configuration to judge against;
		// the startup path validates whatever it holds.
		return nil
	}
	return validateEntries(cfg, flattenProxies(proxies), flattenVisitors(visitors))
}

// validateEntries judges one set of entries in the configuration that owns them,
// which is the same judgement a start makes on the file.
func validateEntries(cfg *config.Config, proxies []config.ProxyConfig, visitors []config.VisitorConfig) error {
	merged := *cfg
	merged.Proxies = mergeProxies(cfg.Proxies, proxies)
	merged.Visitors = mergeVisitors(cfg.Visitors, visitors)
	merged.Warnings = nil
	return merged.Validate(config.RoleClient)
}

// flattenProxies lists every stored proxy, in a stable order.
func flattenProxies(groups map[string][]config.ProxyConfig) []config.ProxyConfig {
	out := make([]config.ProxyConfig, 0, len(groups))
	for _, group := range groups {
		out = append(out, group...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// flattenVisitors is flattenProxies for the visitor entries.
func flattenVisitors(groups map[string][]config.VisitorConfig) []config.VisitorConfig {
	out := make([]config.VisitorConfig, 0, len(groups))
	for _, group := range groups {
		out = append(out, group...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// validateAgainst judges the stored entries in the configuration that owns them:
// a dynamic entry has to satisfy the same rules a configured one does (a socks5
// proxy needs allow_targets, an https proxy needs a subdomain or a domain, two
// visitors cannot bind one address), and the shipping configuration is the only
// place those rules have the context to apply.
func (s *store) validateAgainst(cfg *config.Config) error {
	storedProxies, storedVisitors := s.list()
	if len(storedProxies) == 0 && len(storedVisitors) == 0 {
		return nil
	}
	return validateEntries(cfg, storedProxies, storedVisitors)
}

// refreshDispatch converges the running client on the effective lists, which is
// what the admin API calls after it changes an entry.
func (c *client) refreshDispatch() {
	// Serialized with a SIGHUP reload for the same reason two reloads are
	// serialized with each other: both paths restart probes and visitor listeners.
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	// An entry write that was in flight when Run failed can still be holding
	// the admin server's Shutdown window: everything below starts long-lived
	// services under the caller's context, so a stopped client must not
	// apply any of it — the same rule reloadFromFile applies to a SIGHUP
	// that outlived the client.
	if c.stopped.Load() {
		return
	}

	proxies, visitors := c.effectiveLists()
	c.applyProxyReload(proxies)
	c.applyVisitorReload(visitors)
}
