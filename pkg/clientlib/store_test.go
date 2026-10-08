package clientlib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A stored entry reaches the configuration decoder as the document it would have
// been in a configuration file, and the decoder reads two fixed sections. A proxy
// entry rendered as "proxys" made the decoder report every one of its keys as
// unknown, which is how the proxy half of the store came to reject everything.
func TestAStoredEntryIsRenderedAsTheSectionTheDecoderReads(t *testing.T) {
	proxy, err := jsonBodyToTOML("proxy", "web", json.RawMessage(`{
		"type": "tcp", "local_ip": "127.0.0.1", "local_port": 8080, "remote_port": 6022,
		"health_check": {"type": "tcp", "interval_s": 5}
	}`))
	if err != nil {
		t.Fatalf("proxy entry: %v", err)
	}
	if !strings.HasPrefix(proxy, "[[proxies]]\n") {
		t.Errorf("a proxy entry starts with %q, want the [[proxies]] header", firstLine(proxy))
	}
	if strings.Contains(proxy, "proxys") {
		t.Errorf("a proxy entry mentions a section the decoder does not read:\n%s", proxy)
	}
	// The nested table has to carry the same section name, or the decoder sees a
	// table of a section it does not know.
	if !strings.Contains(proxy, "[proxies.health_check]") {
		t.Errorf("the nested table is not under the proxies section:\n%s", proxy)
	}

	visitor, err := jsonBodyToTOML("visitor", "peer", json.RawMessage(`{
		"type": "stcp", "server_name": "web", "secret_key": "s3cret", "bind_port": 7000
	}`))
	if err != nil {
		t.Fatalf("visitor entry: %v", err)
	}
	if !strings.HasPrefix(visitor, "[[visitors]]\n") {
		t.Errorf("a visitor entry starts with %q, want the [[visitors]] header", firstLine(visitor))
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// The store's whole promise is that an entry written through the API is published
// and is there again after a restart, so the test walks it: write, read back,
// survive a second store built on the same file, delete.
func TestTheStoreKeepsWhatItIsGivenAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	logger := log.New(io.Discard, "", 0)

	stored, err := newStore(path, logger)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	proxies, err := stored.putProxy("web", json.RawMessage(`{
		"type": "tcp", "local_ip": "127.0.0.1", "local_port": 8080, "remote_port": 6022
	}`))
	if err != nil {
		t.Fatalf("put proxy: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "web" || proxies[0].RemotePort != 6022 {
		t.Fatalf("the entry read back as %+v", proxies)
	}
	body, ok := stored.bodiesFor()["proxies"]["web"]
	if !ok {
		t.Fatal("the stored body is not readable back")
	}
	var storedFields map[string]any
	if err := json.Unmarshal(body, &storedFields); err != nil {
		t.Fatalf("the stored body is not JSON: %v", err)
	}
	if storedFields["remote_port"] != float64(6022) {
		t.Fatalf("the stored body holds remote_port %v, want 6022", storedFields["remote_port"])
	}

	// The file is the entry: a second store on the same path has it, which is what
	// a restart does.
	restored, err := newStore(path, logger)
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	restoredProxies, restoredVisitors := restored.list()
	if len(restoredProxies) != 1 || restoredProxies[0].Name != "web" {
		t.Fatalf("the reopened store holds %+v", restoredProxies)
	}
	if len(restoredVisitors) != 0 {
		t.Fatalf("the reopened store invented %d visitor(s)", len(restoredVisitors))
	}

	existed, err := restored.deleteProxy("web")
	if err != nil || !existed {
		t.Fatalf("delete: existed=%v err=%v", existed, err)
	}
	again, err := newStore(path, logger)
	if err != nil {
		t.Fatalf("reopen after the delete: %v", err)
	}
	if left, _ := again.list(); len(left) != 0 {
		t.Fatalf("the entry came back after it was deleted: %+v", left)
	}
	if existed, err := again.deleteProxy("web"); err != nil || existed {
		t.Fatalf("deleting a missing entry reported existed=%v err=%v", existed, err)
	}
}

// An unknown key is refused by name: a dynamic entry has no other way to tell its
// author that the key does nothing.
func TestTheStoreRefusesAKeyTheConfigurationDoesNotHave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	_, err = stored.putProxy("web", json.RawMessage(`{"type":"tcp","local_port":8080,"no_such_key":1}`))
	if err == nil {
		t.Fatal("an entry with an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("the refusal does not name the key: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		// The file may exist (the store creates it), but it must hold nothing.
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), "no_such_key") {
			t.Fatalf("the refused entry was written to the file: %s", data)
		}
	}
}

// The name in the body and the name in the path are one entry, so a mismatch is a
// mistake worth refusing rather than silently overwriting another entry.
func TestTheStoreRefusesABodyThatNamesAnotherEntry(t *testing.T) {
	stored, err := newStore(filepath.Join(t.TempDir(), "store.json"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	_, err = stored.putProxy("web", json.RawMessage(`{"name":"elsewhere","type":"tcp","local_port":8080}`))
	if err == nil {
		t.Fatal("a body naming another entry was accepted")
	}
	if !strings.Contains(err.Error(), "elsewhere") || !strings.Contains(err.Error(), "web") {
		t.Fatalf("the refusal does not name both: %v", err)
	}
}

// The store's entries are validated against the configuration they are merged
// into, which is what catches two entries asking for one public port.
func TestStoredEntriesAreValidatedAgainstTheConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := stored.putProxy("web", json.RawMessage(`{
		"type": "tcp", "local_ip": "127.0.0.1", "local_port": 8080, "remote_port": 6022
	}`)); err != nil {
		t.Fatalf("put proxy: %v", err)
	}

	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "token"
	cfg.Proxies = []config.ProxyConfig{{
		Name: "taken", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 9090, RemotePort: 6022,
	}}
	if err := stored.validateAgainst(cfg); err == nil {
		t.Fatal("a stored entry sharing a port with the configuration passed validation")
	}

	// The stored entry wins the name it carries, so the same port is fine once the
	// configuration does not ask for it.
	cfg.Proxies = nil
	if err := stored.validateAgainst(cfg); err != nil {
		t.Fatalf("a stored entry that conflicts with nothing was refused: %v", err)
	}
}

// A conflict between two visitors used to drag down every proxy: the proxy pass
// validated each proxy against every stored visitor, so the visitor pair's own
// clash — a port bound twice — made the proxy look invalid. The passes accept one
// entry at a time now, and an unrelated proxy survives.
func TestDropConflictsLeavesAnUnrelatedProxyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	stored.proxies["web"] = []config.ProxyConfig{{
		Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080, RemotePort: 6022,
	}}
	visitor := func(name string) []config.VisitorConfig {
		return []config.VisitorConfig{{
			Name: name, Type: "stcp", ServerName: "svc", SecretKey: "s3cret",
			AuthMethod: "secret", BindPort: 7100,
		}}
	}
	stored.visitors["peer-a"] = visitor("peer-a")
	stored.visitors["peer-b"] = visitor("peer-b")

	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "token"

	dropped := stored.dropConflicts(cfg)
	if len(dropped) != 1 {
		t.Fatalf("dropped %d entries (%v), want exactly the one visitor that lost the port", len(dropped), dropped)
	}
	if _, ok := stored.proxies["web"]; !ok {
		t.Error("the proxy was dropped over a conflict between two visitors")
	}
	if _, ok := stored.visitors["peer-a"]; !ok {
		t.Error("the first visitor in name order lost the port to the second")
	}
	if _, ok := stored.visitors["peer-b"]; ok {
		t.Error("the second visitor kept a port the first one already bound")
	}
}

// A runtime entry is judged in the configuration that owns it before it is written
// and applied. Parsing a body checks nothing about the rules a proxy has to satisfy:
// a plugin that dials whatever the visitor names, with no allow_targets, is an exit
// for everything the client can reach, and a static_file plugin with no local path
// serves the client's working directory on a public port.
func TestARuntimeEntryIsValidatedBeforeItIsStored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "0123456789abcdef"
	cfg.Store.Path = path
	cfg.Client.Admin = &config.AdminConfig{Enabled: true, Port: 8765}
	stored.own(cfg)

	// Each of these is a live tunnel the moment it is accepted. The body names the
	// entry, as the admin API requires, so the refusal cannot come from a name
	// mismatch.
	for _, plugin := range []string{"http_proxy", "socks5", "static_file"} {
		name := "exit-" + plugin
		body := fmt.Sprintf(`{"name":%q,"type":"tcp","plugin":%q}`, name, plugin)
		if _, err := stored.putProxy(name, []byte(body)); err == nil {
			t.Errorf("the store accepted %s", body)
		}
	}
	broken := `{"name":"broken","type":"tcp","plugin":"https2http","local_port":-1,"plugin_cert_file":"c.pem","plugin_key_file":"k.pem"}`
	if _, err := stored.putProxy("broken", []byte(broken)); err == nil {
		t.Errorf("the store accepted %s", broken)
	}

	// A body that is fine still lands, so the check is not refusing everything.
	if _, err := stored.putProxy("ok", []byte(`{"name":"ok","type":"tcp","local_port":8080,"remote_port":9000}`)); err != nil {
		t.Fatalf("the store refused a valid entry: %v", err)
	}
}

// The store judges a write against a snapshot of the client's configuration, not
// the live object. Two things follow, and both used to be wrong: a reload that
// rotates client.auth_token writes the live configuration from its own goroutine,
// so validating a write against that object is a data race; and a reload replaces
// the file's selection without touching cfg.Proxies, so judging a write against
// it used the proxies the file held at startup rather than the running ones.
func TestTheStoreJudgesAWriteAgainstALiveSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}

	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "0123456789abcdef"
	cfg.Store.Path = path
	cfg.Client.Admin = &config.AdminConfig{Enabled: true, Port: 8765}
	// What the file held when the client started: a proxy on 9000.
	cfg.Proxies = []config.ProxyConfig{{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80, RemotePort: 9000}}

	c := &client{cfg: cfg, logger: log.New(io.Discard, "", 0)}
	// What the last reload selected: "web" is gone and "db" took 9001.
	c.fileProxies = []config.ProxyConfig{{Name: "db", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 5432, RemotePort: 9001}}

	snapshot := c.validationConfig()
	if len(snapshot.Proxies) != 1 || snapshot.Proxies[0].Name != "db" {
		t.Fatalf("the snapshot carries %+v, want the reload's selection", snapshot.Proxies)
	}

	// A reload adopts a rotated token into the live object; the snapshot must not
	// follow it, because that read is what the store makes from its own goroutine.
	c.cfgMu.Lock()
	c.cfg.Client.AuthToken = "rotated-token-9876543210"
	c.cfgMu.Unlock()
	if snapshot.Client.AuthToken != "0123456789abcdef" {
		t.Fatal("the snapshot followed a later write to the live configuration")
	}

	stored.own(snapshot)
	// 9000 is the port of the proxy the reload removed, so it is free.
	if _, err := stored.putProxy("exit", []byte(`{"name":"exit","type":"tcp","local_ip":"127.0.0.1","local_port":1080,"remote_port":9000}`)); err != nil {
		t.Fatalf("the port of the proxy the reload removed was refused: %v", err)
	}
	if _, err := stored.putProxy("exit", []byte(`{"name":"exit","type":"tcp","local_ip":"127.0.0.1","local_port":1080,"remote_port":9001}`)); err == nil {
		t.Fatal("a runtime entry on the running file proxy's port was accepted")
	}
}

// A stored entry that overrides a configured one of the same name is the store's
// rule, not a conflict. The repair pass used to append it to the file list, where
// it read as a duplicate name, so one unrelated bad entry cost the operator the
// override as well.
func TestDropConflictsKeepsAnOverrideOfAConfiguredProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	configured := config.ProxyConfig{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80, RemotePort: 6022}
	stored.proxies["web"] = []config.ProxyConfig{{
		Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080, RemotePort: 6023,
	}}
	// An entry that cannot be published: an http_proxy plugin with no allow_targets
	// is an exit for everything the client can reach.
	stored.proxies["bad"] = []config.ProxyConfig{{
		Name: "bad", Type: "tcp", Plugin: config.PluginHTTPProxy,
	}}

	cfg := &config.Config{}
	cfg.Client.ServerAddr = "127.0.0.1:7000"
	cfg.Client.AuthToken = "0123456789abcdef"
	cfg.Proxies = []config.ProxyConfig{configured}

	dropped := stored.dropConflicts(cfg)
	if len(dropped) != 1 {
		t.Fatalf("dropped %d entries (%v), want only the one that cannot be published", len(dropped), dropped)
	}
	if _, ok := stored.proxies["bad"]; ok {
		t.Error("the entry that cannot be published was kept")
	}
	kept, ok := stored.proxies["web"]
	if !ok {
		t.Fatal("the override of a configured proxy was dropped as a duplicate name")
	}
	if kept[0].RemotePort != 6023 {
		t.Fatalf("the override now asks for remote_port %d, want 6023", kept[0].RemotePort)
	}
}

// A failure to create the store's temporary file is a failure of the store's own
// directory, not of the body: the admin API answers 500 for it, and a 400 would
// tell automation the request was wrong and should not be retried.
func TestAStoreDirectoryFailureIsAWriteError(t *testing.T) {
	dir := t.TempDir()
	stored, err := newStore(filepath.Join(dir, "missing", "store.json"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	_, err = stored.putProxy("ok", []byte(`{"name":"ok","type":"tcp","local_port":8080,"remote_port":9000}`))
	if !errors.Is(err, errStoreWrite) {
		t.Fatalf("the write failure is %v, want errStoreWrite", err)
	}
}

// An empty store file is what `touch store.json` or an editor's first save
// leaves. It used to be a JSON syntax error, so a client with a placeholder in
// [store].path refused to start.
func TestAnEmptyStoreFileIsAnEmptyStore(t *testing.T) {
	for _, content := range []string{"", "  \n\t"} {
		path := filepath.Join(t.TempDir(), "store.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
		stored, err := newStore(path, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatalf("an empty store file was refused: %v", err)
		}
		proxies, visitors := stored.list()
		if len(proxies) != 0 || len(visitors) != 0 {
			t.Fatalf("an empty store file held %d proxies and %d visitors", len(proxies), len(visitors))
		}
	}
}
