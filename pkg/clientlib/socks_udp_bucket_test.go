package clientlib

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// One socks5 UDP association draws from one bandwidth bucket, whatever number of
// targets its visitor probes. A bucket per target socket let each new address
// reach the configured rate again, so a single association carried up to
// maxSocksUDPTargets times the bandwidth the operator set — and the byte-stream
// socks5 path and the udp/sudp path each cap one thing, not a set.
func TestEveryTargetOfASocksUDPAssociationSharesOneBucket(t *testing.T) {
	policy, err := socks.NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	// Two targets that really read, so nothing bounces back an ICMP
	// port-unreachable the reply reader would turn into a closed socket.
	drain := func() string {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen for a target: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		go func() {
			buf := make([]byte, 64)
			for {
				if _, _, err := conn.ReadFrom(buf); err != nil {
					return
				}
			}
		}()
		return conn.LocalAddr().String()
	}
	firstTarget, secondTarget := drain(), drain()

	relay := &socksUDPRelay{
		policy:      policy,
		logger:      log.New(discardSink{}, "", 0),
		name:        "exit",
		idleTimeout: time.Second,
		dialTimeout: 5 * time.Second,
		// One byte per second with a burst of one byte, so the first write takes
		// the whole bucket and a second target has nothing left to spend.
		limiter: newBandwidthLimiter(1),
		sockets: make(map[string]*socksUDPTarget),
	}
	defer relay.close()

	first, err := relay.socketFor(firstTarget)
	if err != nil {
		t.Fatalf("socket for the first target: %v", err)
	}
	if _, err := first.conn.Write([]byte{1}); err != nil {
		t.Fatalf("the first target's write: %v", err)
	}

	second, err := relay.socketFor(secondTarget)
	if err != nil {
		t.Fatalf("socket for the second target: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := second.conn.Write([]byte{1})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a second target wrote straight through the bucket the first one had spent (err=%v); each target has its own limiter", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A stored body this build cannot parse no longer fails the start. The admin API
// that could delete it only comes up after a start, so a fatal error here was an
// outage the operator could clear only by hand-editing the file — the situation
// dropConflicts already avoids for entries that fail validation.
func TestAStoredEntryThisBuildCannotParseIsDroppedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	body := `{"proxies":{` +
		`"good":{"type":"tcp","local_ip":"127.0.0.1","local_port":8080,"remote_port":6022},` +
		`"from-newer":{"type":"tcp","local_port":9090,"no_such_key":1}` +
		`},"visitors":{}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the store file: %v", err)
	}

	var logged bytes.Buffer
	stored, err := newStore(path, log.New(&logged, "", 0))
	if err != nil {
		t.Fatalf("an unreadable entry failed the whole start: %v", err)
	}
	proxies, visitors := stored.list()
	if len(proxies) != 1 || proxies[0].Name != "good" {
		t.Fatalf("the readable entry did not survive the drop: %+v", proxies)
	}
	if len(visitors) != 0 {
		t.Fatalf("the store invented %d visitor(s)", len(visitors))
	}
	if !strings.Contains(logged.String(), "from-newer") {
		t.Errorf("the dropped entry is not named in the log: %q", logged.String())
	}
}

// The boundary stays where it was: a file that is not the store's own JSON is a
// fault in the file itself, not in one entry, so it is still fatal.
func TestAStoreFileThatIsNotJSONIsStillFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write the store file: %v", err)
	}
	if _, err := newStore(path, log.New(discardSink{}, "", 0)); err == nil {
		t.Fatal("a file that is not JSON was accepted")
	}
}

// discardSink is an io.Writer that keeps nothing, for the tests that only care
// that a log line was written somewhere.
type discardSink struct{}

func (discardSink) Write(p []byte) (int, error) { return len(p), nil }

// probeChildTargetEnv marks the child process the probe test re-runs itself as.
const probeChildTargetEnv = "AETHERTUNNEL_HEALTH_PROBE_TARGET"

// An http probe reaches the local service directly, whatever proxy settings the
// process inherited. A zero-value http.Client uses http.DefaultTransport, whose
// Proxy is http.ProxyFromEnvironment, so a probe pointed at a local_addr that is
// not loopback went to whatever HTTP_PROXY named: with that proxy unreachable, a
// healthy service was judged down and its tunnel was withdrawn with no service
// fault behind it.
//
// net/http reads the environment's proxy settings once per process and caches
// them, so the probe runs in a child of this test — a t.Setenv here would not
// reach that cache, and would also leak the dead proxy into every other test
// that makes an HTTP request through the default transport.
func TestTheHTTPProbeIgnoresTheEnvironmentProxy(t *testing.T) {
	if target := os.Getenv(probeChildTargetEnv); target != "" {
		proxy := probeProxy(t, "http://"+target, &config.HealthCheckConfig{Type: "http", Path: "/"})
		if !probeHealth(proxy, 5*time.Second) {
			t.Fatalf("the probe at %s did not reach the service, so it went through the environment's proxy", target)
		}
		return
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(nonLoopbackIPv4(t), "0"))
	if err != nil {
		t.Skipf("cannot listen on this machine's non-loopback address: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			// The case needs the proxy setting to be the only one in force.
			continue
		}
		env = append(env, entry)
	}
	// Port 1 refuses the connection at once, so a probe that used it fails
	// immediately instead of waiting out its timeout.
	env = append(env,
		probeChildTargetEnv+"="+listener.Addr().String(),
		"HTTP_PROXY=http://127.0.0.1:1")

	cmd := exec.Command(os.Args[0], "-test.run=^TestTheHTTPProbeIgnoresTheEnvironmentProxy$")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the probe used the environment's proxy: %v\n%s", err, out)
	}
}

// nonLoopbackIPv4 returns one of this machine's own addresses that net/http would
// send through a proxy, which is what the case above needs: a loopback target is
// never proxied.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, address := range addresses {
		ipNet, ok := address.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	t.Skip("this machine has no non-loopback IPv4 address")
	return ""
}

// A changed auth_token_file is not announced as restart-only. The reload re-reads
// that file through config.Load and adopts the token live, with its own line, so
// leaving the path in the [client] comparison printed "applies after a restart"
// about the very token the next line had just adopted.
func TestAChangedAuthTokenFileIsNotAnnouncedAsRestartOnly(t *testing.T) {
	before := withoutStartAndToken(config.ClientConfig{AuthTokenFile: "old.token"})
	after := withoutStartAndToken(config.ClientConfig{AuthTokenFile: "new.token"})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("a changed auth_token_file is still compared, so the reload calls it restart-only")
	}
	// The token itself stays out of the comparison: it has a live message of its own.
	if got := withoutStartAndToken(config.ClientConfig{AuthToken: "secret"}).AuthToken; got != "" {
		t.Fatalf("the auth token is back in the comparison: %q", got)
	}
}

// The store's rename is durable only once the directory entry that carries it is,
// which is the same flush pkg/ledger's New does for a file it creates. Without it
// a crash right after a write could leave the name pointing at nothing, and the
// next start would find no runtime entries at all.
func TestAStoreWriteFlushesTheDirectoryOfItsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no directory flush to assert (see syncdir_windows.go), and the permission the case injects is a unix one")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission this case relies on")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	stored, err := newStore(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	// Write and search but no read: the file can be written inside, while the
	// directory itself cannot be opened for the flush.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err = stored.putProxy("web", []byte(`{"name":"web","type":"tcp","local_port":8080,"remote_port":9000}`))
	if err == nil {
		t.Fatal("the store rewrote its file without flushing the directory entry that names it")
	}
	if !errors.Is(err, errStoreWrite) {
		t.Fatalf("the failure is %v, want errStoreWrite", err)
	}
	if !strings.Contains(err.Error(), "flush the directory") {
		t.Fatalf("the error does not name the directory flush: %v", err)
	}
}
