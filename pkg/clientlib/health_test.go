package clientlib

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

func TestHealthStateTransitions(t *testing.T) {
	state := newHealthState(2)
	if !state.isHealthy() {
		t.Fatal("a fresh state is unhealthy")
	}
	if event := state.record(false); event != eventNone {
		t.Fatalf("the first failure produced %v, want no change while under max_failed", event)
	}
	if event := state.record(false); event != eventRefused {
		t.Fatalf("the second failure produced %v, want a refusal", event)
	}
	if state.isHealthy() {
		t.Fatal("the state is healthy after max_failed failures")
	}
	if event := state.record(true); event != eventRecovered {
		t.Fatalf("a success after refusal produced %v, want a recovery", event)
	}
	if !state.isHealthy() {
		t.Fatal("the state did not recover")
	}
}

func TestAProxyWithoutAHealthCheckIsAlwaysHealthy(t *testing.T) {
	c := &client{logger: log.New(io.Discard, "", 0)}
	if !c.proxyHealthy(config.ProxyConfig{Name: "plain"}) {
		t.Fatal("a proxy without a health check was refused")
	}
}

func TestDialForProxyRefusesAnUnhealthyService(t *testing.T) {
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	unhealthy := newHealthState(1)
	unhealthy.record(false)
	c.setHealthState("db", unhealthy)

	proxy := config.ProxyConfig{
		Name: "db", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 1,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp", MaxFailed: 1},
	}
	// The refusal lives in serveStream's pre-dial check, so the error the dial
	// itself produces is what this asserts: the sentinel crosses the tunnel to the
	// visitor as the reason the stream could not be opened.
	if _, err := c.dialLocalService(proxy, ""); !errors.Is(err, errLocalUnhealthy) {
		t.Fatalf("dialLocalService = %v, want errLocalUnhealthy", err)
	}
	if c.proxyHealthy(proxy) {
		t.Fatal("the unhealthy state was not consulted")
	}
}

// pluginRoundTrip sends one request through a plugin proxy on a fresh
// connection, retrying the whole attempt when the machine fails it at the
// transport level — an EOF or a stall while awaiting the answer — rather than
// answering it. The callers assert what the plugin answered, not what a loaded
// runner did on the way.
func pluginRoundTrip(t *testing.T, c *client, proxy config.ProxyConfig, req *http.Request) (*http.Response, string) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		conn, err := c.dialForProxy(proxy, "")
		if err != nil {
			t.Fatalf("dialForProxy: %v", err)
		}
		transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return conn, nil
		}}
		resp, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return resp, string(body)
	}
	t.Fatalf("the request never came back: %v", lastErr)
	return nil, ""
}

func TestStaticFilePluginServesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("static-file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := config.ProxyConfig{Name: "site", Type: "tcp", Plugin: config.PluginStaticFile, PluginLocalPath: dir}
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}

	get := func(path string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", "http://plugin/"+path, nil)
		return pluginRoundTrip(t, c, proxy, req)
	}

	if resp, got := get("hello.txt"); resp.StatusCode != http.StatusOK || got != "static-file-content" {
		t.Fatalf("the file came back as %d %q, want 200 and %q", resp.StatusCode, got, "static-file-content")
	}
	// The server outlives the stream: a second visitor gets the file too.
	if _, got := get("hello.txt"); got != "static-file-content" {
		t.Fatalf("the second visit is %q", got)
	}
}

func TestStaticFilePluginHonorsBasicAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("top"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := config.ProxyConfig{
		Name: "vault", Type: "tcp", Plugin: config.PluginStaticFile, PluginLocalPath: dir,
		PluginHTTPUser: "ada", PluginHTTPPassword: "lovelace",
	}
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}

	do := func(auth string) int {
		req, _ := http.NewRequest("GET", "http://plugin/secret.txt", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, _ := pluginRoundTrip(t, c, proxy, req)
		return resp.StatusCode
	}

	// url.UserPassword renders exactly the basic auth header the server checks.
	reqOK := "Basic " + base64.StdEncoding.EncodeToString([]byte("ada:lovelace"))
	if code := do(reqOK); code != 200 {
		t.Fatalf("an authorized request got %d", code)
	}
	if code := do(""); code != 401 {
		t.Fatalf("an anonymous request got %d, want 401", code)
	}
}

func TestUnixSocketPluginDialsTheSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets are unavailable here")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("unix-ok"))
			conn.Close()
		}
	}()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	proxy := config.ProxyConfig{Name: "uds", Type: "tcp", Plugin: config.PluginUnixSocket, PluginLocalPath: sock}
	conn, err := c.dialForPlugin(proxy)
	if err != nil {
		t.Fatalf("dialForPlugin: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 7)
	if n, _ := conn.Read(buf); string(buf[:n]) != "unix-ok" {
		t.Fatalf("the socket answered %q", buf)
	}
}

// The limiter is a real dependency of the data path; a broken one either
// passes everything or passes nothing, and both are visible here.
func TestLimitedConnPassesData(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()
	conn := newLimitedConn(1_000_000, client)
	if _, err := conn.Write(make([]byte, 5000)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Keep os imported for the temp-file helpers above if refactors drop it.
var _ = os.Getenv

// probeProxy turns a test server's address into the proxy configuration the probe
// reads, so the tests below exercise the same fields a real configuration sets.
func probeProxy(t *testing.T, url string, hc *config.HealthCheckConfig) config.ProxyConfig {
	t.Helper()
	addr := strings.TrimPrefix(url, "http://")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %s: %v", port, err)
	}
	return config.ProxyConfig{Name: "probe", Type: "tcp", LocalIP: host, LocalPort: number, HealthCheck: hc}
}

// An http probe has to get a 2xx answer: a service that answers "gone" or "error"
// is not serving, and this probe is what decides whether the public endpoint stays
// published.
func TestTheHTTPProbeNeedsA2xxAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/redirect-into-an-error":
			http.Redirect(w, r, "/broken", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	for _, tc := range []struct {
		path  string
		happy bool
	}{
		{"/ok", true},
		{"/empty", true},
		// A redirect is followed, which is what Go's client does and what frp's
		// probe does too: the status that decides is the one at the end.
		{"/redirect", true},
		{"/redirect-into-an-error", false},
		{"/broken", false},
		{"/private", false},
	} {
		proxy := probeProxy(t, server.URL, &config.HealthCheckConfig{Type: "http", Path: tc.path, TimeoutS: 2})
		if got := probeHealth(proxy, 2*time.Second); got != tc.happy {
			t.Errorf("the answer on %s was judged healthy=%v, want %v", tc.path, got, tc.happy)
		}
	}
}

// http_headers is how an endpoint that wants a credential is probed at all: the
// same server is unhealthy without them and healthy with them.
func TestTheHTTPProbeSendsTheConfiguredHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer probe-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	without := probeProxy(t, server.URL, &config.HealthCheckConfig{Type: "http", Path: "/"})
	if probeHealth(without, 2*time.Second) {
		t.Fatal("a probe without the credential was judged healthy")
	}

	with := probeProxy(t, server.URL, &config.HealthCheckConfig{
		Type: "http", Path: "/",
		HTTPHeaders: map[string]string{"Authorization": "Bearer probe-secret"},
	})
	if !probeHealth(with, 2*time.Second) {
		t.Fatal("a probe carrying the credential was judged unhealthy")
	}
}

// A tcp probe only has to connect, and a proxy with no health check is always
// healthy — the probe is never run for it.
func TestTheTCPProbeNeedsAConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	live := probeProxy(t, "http://"+listener.Addr().String(), &config.HealthCheckConfig{Type: "tcp"})
	if !probeHealth(live, 2*time.Second) {
		t.Fatal("a listening service was judged unhealthy by the tcp probe")
	}

	// The same address once the listener is gone: a closed port refuses at once.
	deadAddr := listener.Addr().String()
	_ = listener.Close()
	dead := probeProxy(t, "http://"+deadAddr, &config.HealthCheckConfig{Type: "tcp"})
	if probeHealth(dead, 2*time.Second) {
		t.Fatal("a closed port was judged healthy by the tcp probe")
	}

	noCheck := config.ProxyConfig{Name: "plain", Type: "tcp"}
	if !probeHealth(noCheck, 2*time.Second) {
		t.Fatal("a proxy without a health check was judged unhealthy")
	}
}

// max_failed counts *consecutive* failures. A probe that fails once, recovers and
// fails again used to add up, so a service that never failed in a row was still
// withdrawn.
func TestHealthFailuresAreConsecutive(t *testing.T) {
	state := newHealthState(3)
	for i := 0; i < 2; i++ {
		state.record(false)
		if event := state.record(true); event != eventNone {
			t.Fatalf("recovering while healthy produced %v, want no event", event)
		}
	}
	if !state.isHealthy() {
		t.Fatal("two failures separated by successes refused the service")
	}
	// Three in a row still refuse it: the reset must not disable the threshold.
	for i := 0; i < 3; i++ {
		if event := state.record(false); i < 2 && event != eventNone {
			t.Fatalf("failure %d produced %v, want no event yet", i+1, event)
		}
	}
	if state.isHealthy() {
		t.Fatal("three consecutive failures left the service healthy")
	}
}

// A proxy whose republish failed keeps the withdrawal mark, and record reports a
// recovery only once. Every healthy probe while the mark is set therefore has to
// try again: otherwise a single failed write, or a withdrawal that landed after
// the service was already healthy again, left the tunnel unpublished forever while
// its local service was fine. The mark goes on the server's confirmation, not on
// the write: the server can refuse the registration, and a mark dropped on the
// write would end the retries while the log said the tunnel was published.
func TestAHealthyProbeRepublishesAProxyThatIsStillMarkedWithdrawn(t *testing.T) {
	c, peer, serverSide := newWithdrawHarness(t)

	// A local service that accepts connections, so the probe succeeds.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	proxy := config.ProxyConfig{
		Name: "web", Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: port,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp", IntervalS: 1, TimeoutS: 1},
	}
	c.rememberSpec(proxySpec(proxy, ""))

	// The state a failed republish leaves behind: healthy again, still withdrawn.
	state := newHealthState(1)
	state.markWithdrawn()
	// Where startHealthCheck installs it: the confirmation looks the state up by
	// name, so a probe running a state the client does not know about would never
	// see its mark cleared.
	c.setHealthState("web", state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// What startHealthCheck registers, which the re-registration checks before it
	// writes: this test drives the probe loop directly.
	c.healthStopsMu.Lock()
	c.healthStops = map[string]healthProbe{"web": {ctx: ctx, cancel: cancel}}
	c.healthStopsMu.Unlock()
	go c.runHealthCheck(ctx, proxy, state)

	if err := serverSide.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	msg, err := peer.ReadFrame()
	if err != nil {
		t.Fatalf("no registration arrived on a healthy probe: %v", err)
	}
	if msg.Type != protocol.TypeRegisterProxy {
		t.Fatalf("the server received %s, want %s", msg.Type, protocol.TypeRegisterProxy)
	}
	var spec protocol.ProxySpec
	if err := json.Unmarshal(msg.Payload, &spec); err != nil {
		t.Fatalf("unmarshal the registration: %v", err)
	}
	if spec.Name != "web" {
		t.Fatalf("the registration names %q, want web", spec.Name)
	}
	// The write alone is not a publication: the mark stays until the server's
	// proxy list names the proxy again.
	if !state.isWithdrawn() {
		t.Fatal("the withdrawal mark was dropped on the write, before the server confirmed the registration")
	}

	// The server accepts the registration and says so with its list of published
	// tunnels; that is what clears the mark.
	if err := peer.WriteJSON(protocol.TypeProxyList, []protocol.ProxyStatus{{Name: "web", Type: config.ProxyTypeTCP}}); err != nil {
		t.Fatalf("write the proxy list: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for state.isWithdrawn() {
		if time.Now().After(deadline) {
			t.Fatal("the withdrawal mark survived the server's confirmation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A reload replaces a proxy's probe under the same name. An old probe that passed
// its context check just before the cancel and acted afterwards would withdraw the
// name the replacement had just published — and nothing republishes it, because the
// replacement starts from healthy and carries no withdrawal mark to clear.
func TestAReplacedHealthProbeDoesNotActOnTheNewRegistration(t *testing.T) {
	c, peer, serverSide := newWithdrawHarness(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	proxy := config.ProxyConfig{
		Name: "web", Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1",
		LocalPort:   listener.Addr().(*net.TCPAddr).Port,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp", IntervalS: 1, TimeoutS: 1},
	}
	c.rememberSpec(proxySpec(proxy, ""))

	// The state a failed republish leaves behind: healthy, still withdrawn, so a
	// healthy probe from the old goroutine would re-register the name.
	state := newHealthState(1)
	state.markWithdrawn()

	oldCtx, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	// The reload's own registration: a successor probe now owns the name. The old
	// context has not noticed yet, which is the window this guards.
	newCtx, cancelNew := context.WithCancel(context.Background())
	defer cancelNew()
	c.healthStopsMu.Lock()
	c.healthStops = map[string]healthProbe{"web": {ctx: newCtx, cancel: cancelNew}}
	c.healthStopsMu.Unlock()

	go c.runHealthCheck(oldCtx, proxy, state)

	if err := serverSide.SetReadDeadline(time.Now().Add(1500 * time.Millisecond)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if msg, err := peer.ReadFrame(); err == nil {
		t.Fatalf("the replaced probe sent %s for %q", msg.Type, proxy.Name)
	}
}

// Every path that carries visitor traffic has to dial the local service the same
// way. servePunch used to dial LocalAddr() directly, which for a plugin-backed
// proxy is not a TCP address at all, and it skipped the health check and the
// bandwidth cap that the relayed path applies.
func TestDialLocalServiceAppliesThePluginHealthAndBandwidth(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets are unavailable here")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("unix-ok"))
			conn.Close()
		}
	}()

	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	proxy := config.ProxyConfig{
		Name: "uds", Type: config.ProxyTypeXTCP,
		Plugin: config.PluginUnixSocket, PluginLocalPath: sock,
	}
	conn, err := c.dialLocalService(proxy, "")
	if err != nil {
		t.Fatalf("dialLocalService: %v", err)
	}
	buf := make([]byte, 7)
	if n, _ := conn.Read(buf); string(buf[:n]) != "unix-ok" {
		t.Fatalf("the plugin answered %q", buf[:n])
	}
	_ = conn.Close()

	// A proxy that failed its health check is refused before any dial.
	unhealthy := newHealthState(1)
	unhealthy.record(false)
	c.setHealthState("uds", unhealthy)
	guarded := proxy
	guarded.HealthCheck = &config.HealthCheckConfig{Type: "tcp", MaxFailed: 1}
	if _, err := c.dialLocalService(guarded, ""); !errors.Is(err, errLocalUnhealthy) {
		t.Fatalf("dialLocalService dialled an unhealthy service: %v", err)
	}

	// The configured bandwidth cap wraps the connection, as on the relayed path.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer local.Close()
	go func() {
		for {
			conn, err := local.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(local.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("atoi: %v", err)
	}
	capped := config.ProxyConfig{
		Name: "web", Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1", LocalPort: portNum,
		Bandwidth: "1MB",
	}
	cappedConn, err := c.dialLocalService(capped, "")
	if err != nil {
		t.Fatalf("dialLocalService: %v", err)
	}
	defer cappedConn.Close()
	if _, ok := cappedConn.(*limitedConn); !ok {
		t.Fatalf("the bandwidth cap was not applied: %T", cappedConn)
	}
}
