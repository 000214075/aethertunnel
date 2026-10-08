package clientlib

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// healthProbeFixture runs one proxy's health check the way startHealthCheck does
// and hands back the state it probes, so a test can read the mark the probe acts
// on. The local service accepts connections, so every probe succeeds.
func healthProbeFixture(t *testing.T, c *client, name string) (*healthState, net.Listener) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	proxy := config.ProxyConfig{
		Name: name, Type: config.ProxyTypeTCP, LocalIP: "127.0.0.1",
		LocalPort:   listener.Addr().(*net.TCPAddr).Port,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp", IntervalS: 1, TimeoutS: 1},
	}
	c.rememberSpec(proxySpec(proxy, ""))

	state := newHealthState(1)
	state.markWithdrawn()
	c.setHealthState(name, state)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.healthStopsMu.Lock()
	c.healthStops = map[string]healthProbe{name: {ctx: ctx, cancel: cancel}}
	c.healthStopsMu.Unlock()
	go c.runHealthCheck(ctx, proxy, state)
	return state, listener
}

func readRegistrationFrame(t *testing.T, peer *protocol.Framer, serverSide net.Conn, within time.Duration) protocol.ProxySpec {
	t.Helper()
	if err := serverSide.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	msg, err := peer.ReadFrame()
	if err != nil {
		t.Fatalf("no registration arrived: %v", err)
	}
	if msg.Type != protocol.TypeRegisterProxy {
		t.Fatalf("the server received %s, want %s", msg.Type, protocol.TypeRegisterProxy)
	}
	var spec protocol.ProxySpec
	if err := json.Unmarshal(msg.Payload, &spec); err != nil {
		t.Fatalf("unmarshal the registration: %v", err)
	}
	return spec
}

// A republish that the server refuses — the frame went out, the server answered
// with an error and sent no list — must leave the withdrawal mark set. The mark
// is what lets the next healthy probe try again; clearing it on the write ended
// the retries with the name unpublished while the log said it was published.
func TestARefusedRegistrationKeepsTheWithdrawalMarkAndTheProbeRetries(t *testing.T) {
	c, peer, serverSide := newWithdrawHarness(t)
	state, _ := healthProbeFixture(t, c, "web")

	if spec := readRegistrationFrame(t, peer, serverSide, 3*time.Second); spec.Name != "web" {
		t.Fatalf("the registration names %q, want web", spec.Name)
	}

	// The server refuses the registration: the frame reached it, and that is all
	// the client knows until an answer arrives.
	if err := peer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{Error: "name conflict"}); err != nil {
		t.Fatalf("write the refusal: %v", err)
	}
	if !state.isWithdrawn() {
		t.Fatal("the withdrawal mark was dropped on the write, before the server answered")
	}

	// The next tick probes again and republishes, because the mark is still set.
	if spec := readRegistrationFrame(t, peer, serverSide, 4*time.Second); spec.Name != "web" {
		t.Fatalf("the retried registration names %q, want web", spec.Name)
	}
}

// The server's proxy list is its confirmation that a registration was accepted:
// a name that appears in it is published again, so the withdrawal mark goes. A
// name the list does not carry keeps its mark, which is what keeps a refusal from
// being read as a publication.
func TestAConfirmedProxyListClearsTheWithdrawalMark(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)

	web := newHealthState(1)
	web.markWithdrawn()
	other := newHealthState(1)
	other.markWithdrawn()
	c.setHealthState("web", web)
	c.setHealthState("other", other)

	if err := peer.WriteJSON(protocol.TypeProxyList, []protocol.ProxyStatus{
		{Name: "web", Type: config.ProxyTypeTCP, RemotePort: 7000},
	}); err != nil {
		t.Fatalf("write the proxy list: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for web.isWithdrawn() {
		if time.Now().After(deadline) {
			t.Fatal("the confirmed proxy kept its withdrawal mark")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !other.isWithdrawn() {
		t.Fatal("a proxy the server did not confirm lost its withdrawal mark")
	}
}

// A confirmed withdrawal leaves the proxy unpublished, so the admin view must stop
// reporting it as confirmed. The confirmation comes from the proxy list, and the
// server answers a withdrawal with an ack instead of a fresh list, so the name has
// to leave the confirmed set here — otherwise the API reported a proxy as both
// confirmed and withdrawn.
func TestAConfirmedWithdrawalStopsReportingTheProxyAsConfirmed(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080, RemotePort: 7000}
	c.setProxyList([]config.ProxyConfig{proxy})
	c.mu.Lock()
	c.registeredNames = []string{"web"}
	c.session = "sess-1"
	c.mu.Unlock()
	// The health check's view after it withdrew the proxy: the server confirmed
	// the withdrawal, and the mark stays until the proxy is published again.
	state := newHealthState(1)
	state.markWithdrawn()
	c.setHealthState("web", state)

	done := make(chan error, 1)
	go func() { done <- c.withdrawProxy(proxy) }()

	var withdraw protocol.ProxyWithdraw
	if err := peer.ReadJSON(protocol.TypeProxyWithdraw, &withdraw); err != nil {
		t.Fatalf("read the withdrawal: %v", err)
	}
	if err := peer.WriteJSON(protocol.TypeProxyWithdrawAck, protocol.ProxyWithdrawAck{Name: "web", OK: true}); err != nil {
		t.Fatalf("write the ack: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("withdrawProxy: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("withdrawProxy did not return")
	}

	recorder := httptest.NewRecorder()
	c.adminStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var report adminStatusReport
	if err := json.NewDecoder(recorder.Body).Decode(&report); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if len(report.Proxies) != 1 {
		t.Fatalf("%d proxies in the report, want one", len(report.Proxies))
	}
	if report.Proxies[0].Confirmed {
		t.Fatal("a proxy whose withdrawal the server confirmed is still reported as confirmed")
	}
	if !report.Proxies[0].Withdrawn {
		t.Fatal("the withdrawn state is missing, so the test is not checking the contradiction it is about")
	}
}
