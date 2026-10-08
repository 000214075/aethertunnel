package clientlib

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// newWithdrawHarness wires a client to a peer framer over one net.Pipe: the
// test plays the server on the other end of the control connection, and a pump
// goroutine plays the control read loop, routing acks to the waiting withdrawal
// and confirmed tunnel lists to the code that folds them in.
func newWithdrawHarness(t *testing.T) (*client, *protocol.Framer, net.Conn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); clientSide.Close() })
	if err := serverSide.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	c := &client{cfg: &config.Config{}, logger: log.New(io.Discard, "", 0)}
	// A reload derives the visitor context from this one, and production always
	// has it (runWithShell sets it before any reload can happen).
	c.baseCtx = context.Background()
	clientFramer := protocol.NewFramer(clientSide, nil, 0)
	c.setControlFramer(clientFramer)
	go func() {
		for {
			message, err := clientFramer.ReadFrame()
			if err != nil {
				return
			}
			switch message.Type {
			case protocol.TypeProxyWithdrawAck:
				var ack protocol.ProxyWithdrawAck
				_ = json.Unmarshal(message.Payload, &ack)
				c.deliverWithdrawAck(ack)
			case protocol.TypeProxyList:
				c.deliverProxyList(message.Payload)
			}
		}
	}()
	return c, protocol.NewFramer(serverSide, nil, 0), serverSide
}

func TestWithdrawProxyForgetsTheSpecWhenTheServerConfirms(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080}
	c.rememberSpec(proxySpec(proxy, ""))

	done := make(chan error, 1)
	go func() { done <- c.withdrawProxy(proxy) }()

	var withdraw protocol.ProxyWithdraw
	if err := peer.ReadJSON(protocol.TypeProxyWithdraw, &withdraw); err != nil {
		t.Fatalf("read the withdrawal: %v", err)
	}
	if withdraw.Name != "web" {
		t.Fatalf("the withdrawal names %q, want web", withdraw.Name)
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
	if _, ok := c.specFor("web"); ok {
		t.Fatal("the spec survives a confirmed withdrawal")
	}
}

func TestWithdrawProxyFallsBackWhenTheServerStaysSilent(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080}

	done := make(chan error, 1)
	go func() { done <- c.withdrawProxy(proxy) }()
	var withdraw protocol.ProxyWithdraw
	if err := peer.ReadJSON(protocol.TypeProxyWithdraw, &withdraw); err != nil {
		t.Fatalf("read the withdrawal: %v", err)
	}
	// The peer consumes the frame and never answers, which is what an older
	// release does with a frame it does not know.

	select {
	case err := <-done:
		if !errors.Is(err, errWithdrawUnsupported) {
			t.Fatalf("withdrawProxy = %v, want errWithdrawUnsupported", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("withdrawProxy did not return")
	}
	if !c.withdrawIsUnsupported() {
		t.Fatal("the unsupported flag is not set")
	}

	started := time.Now()
	if err := c.withdrawProxy(proxy); !errors.Is(err, errWithdrawUnsupported) {
		t.Fatalf("the second withdrawal = %v, want errWithdrawUnsupported", err)
	} else if time.Since(started) > 100*time.Millisecond {
		t.Fatalf("the second withdrawal took %s, want an immediate refusal", time.Since(started))
	}
}

func TestRegisterProxyAgainResendsTheStoredSpec(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	proxy := config.ProxyConfig{Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080, RemotePort: 7000}
	spec := proxySpec(proxy, "")
	c.rememberSpec(spec)

	// The registration is refused unless the probe asking for it is still the
	// registered one, so this stands in for a running health check.
	probeCtx := registerTestProbe(c, proxy.Name)

	done := make(chan error, 1)
	go func() { done <- c.registerProxyAgain(probeCtx, proxy) }()

	var got protocol.ProxySpec
	if err := peer.ReadJSON(protocol.TypeRegisterProxy, &got); err != nil {
		t.Fatalf("read the re-registration: %v", err)
	}
	if got.Name != "web" || got.RemotePort != 7000 || got.LocalAddr != spec.LocalAddr {
		t.Fatalf("the re-registered spec is %+v", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("registerProxyAgain: %v", err)
	}

	// A spec the client no longer holds is rebuilt from the configuration.
	c.forgetSpec("web")
	done = make(chan error, 1)
	go func() { done <- c.registerProxyAgain(probeCtx, proxy) }()
	if err := peer.ReadJSON(protocol.TypeRegisterProxy, &got); err != nil {
		t.Fatalf("read the rebuilt registration: %v", err)
	}
	if got.Name != "web" || got.RemotePort != 7000 {
		t.Fatalf("the rebuilt spec is %+v", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("registerProxyAgain after a forget: %v", err)
	}
}

func TestReWithdrawUnhealthyWithdrawsFailingProxies(t *testing.T) {
	c, peer, _ := newWithdrawHarness(t)
	c.setProxyList([]config.ProxyConfig{{
		Name: "web", Type: config.ProxyTypeTCP, LocalPort: 8080,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp"},
	}})
	state := newHealthState(1)
	if event := state.record(false); event != eventRefused {
		t.Fatalf("record(false) = %v, want eventRefused", event)
	}
	c.setHealthState("web", state)

	exchanged := make(chan error, 1)
	go func() {
		var withdraw protocol.ProxyWithdraw
		if err := peer.ReadJSON(protocol.TypeProxyWithdraw, &withdraw); err != nil {
			exchanged <- err
			return
		}
		exchanged <- peer.WriteJSON(protocol.TypeProxyWithdrawAck, protocol.ProxyWithdrawAck{Name: withdraw.Name, OK: true})
	}()

	c.reWithdrawUnhealthy()
	select {
	case err := <-exchanged:
		if err != nil {
			t.Fatalf("the withdrawal exchange failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the withdrawal exchange did not finish")
	}
	if !state.isWithdrawn() {
		t.Fatal("the unhealthy proxy is not marked withdrawn")
	}

	// A fresh session clears the marks before it re-registers everything.
	c.resetHealthWithdrawals()
	if state.isWithdrawn() {
		t.Fatal("resetHealthWithdrawals left the mark behind")
	}
}

func TestReWithdrawUnhealthyLeavesHealthyProxiesAlone(t *testing.T) {
	c, peer, serverSide := newWithdrawHarness(t)
	healthy := newHealthState(1)
	c.setHealthState("ok", healthy)
	c.setProxyList([]config.ProxyConfig{{
		Name: "ok", Type: config.ProxyTypeTCP, LocalPort: 8080,
		HealthCheck: &config.HealthCheckConfig{Type: "tcp"},
	}})

	// Nothing may arrive for a proxy that never failed a check.
	if err := serverSide.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	c.reWithdrawUnhealthy()
	if _, err := peer.ReadFrame(); err == nil {
		t.Fatal("a healthy proxy produced a frame")
	}
}
