package clientlib

import (
	"errors"
	"fmt"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// errWithdrawUnsupported reports that the server ignored a withdrawal — an
// older release whose control loop logs unknown frames and keeps reading. The
// caller keeps the proxy registered and falls back to refusing dials while the
// health check fails.
var errWithdrawUnsupported = errors.New("the server does not support proxy withdrawal")

// setControlFramer records the live session's framer. Withdrawals and
// re-registrations write through it; a nil framer means no session is up.
func (c *client) setControlFramer(framer *protocol.Framer) {
	c.controlFramerMu.Lock()
	defer c.controlFramerMu.Unlock()
	c.controlFramer = framer
	if c.withdrawWait == nil {
		c.withdrawWait = make(chan protocol.ProxyWithdrawAck, 1)
	}
}

// withdrawIsUnsupported reports that this server already ignored a withdrawal,
// so later health checks skip straight to the local dial refusal.
func (c *client) withdrawIsUnsupported() bool {
	c.withdrawMu.Lock()
	defer c.withdrawMu.Unlock()
	return c.withdrawUnsupported
}

func (c *client) currentFramer() *protocol.Framer {
	c.controlFramerMu.Lock()
	defer c.controlFramerMu.Unlock()
	return c.controlFramer
}

// rememberSpec mirrors a registered proxy's spec, so a health check that
// recovers can register the same proxy again without rebuilding it.
func (c *client) rememberSpec(spec protocol.ProxySpec) {
	c.specsMu.Lock()
	defer c.specsMu.Unlock()
	if c.specs == nil {
		c.specs = map[string]protocol.ProxySpec{}
	}
	c.specs[spec.Name] = spec
}

func (c *client) specFor(name string) (protocol.ProxySpec, bool) {
	c.specsMu.Lock()
	defer c.specsMu.Unlock()
	spec, ok := c.specs[name]
	return spec, ok
}

func (c *client) forgetSpec(name string) {
	c.specsMu.Lock()
	defer c.specsMu.Unlock()
	delete(c.specs, name)
}

// deliverWithdrawAck routes the server's ack to the waiting withdrawal.
func (c *client) deliverWithdrawAck(ack protocol.ProxyWithdrawAck) {
	select {
	case c.withdrawWait <- ack:
	default:
	}
}

// withdrawProxy unpublishes one proxy by name. A server that knows the
// withdrawal removes the tunnel and its public endpoint; a server that stays
// silent — an older release — leaves the endpoint up, and the health check
// falls back to refusing dials.
func (c *client) withdrawProxy(proxy config.ProxyConfig) error {
	c.withdrawMu.Lock()
	defer c.withdrawMu.Unlock()
	if c.withdrawUnsupported {
		return errWithdrawUnsupported
	}
	framer := c.currentFramer()
	if framer == nil {
		return errors.New("no control connection to withdraw on")
	}
	// Drain a stale ack so the wait below sees the answer to this request.
	select {
	case <-c.withdrawWait:
	default:
	}
	if err := framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: proxy.Name}); err != nil {
		return fmt.Errorf("send the withdrawal: %w", err)
	}
	select {
	case ack := <-c.withdrawWait:
		if !ack.OK {
			return fmt.Errorf("the server refused the withdrawal: %s", ack.Error)
		}
		c.forgetSpec(proxy.Name)
		return nil
	case <-time.After(1500 * time.Millisecond):
		c.withdrawUnsupported = true
		c.logger.Printf("the server ignored the withdrawal of %q (an older release); the proxy stays registered and the health check refuses dials instead", proxy.Name)
		return errWithdrawUnsupported
	}
}

// registerProxyAgain re-registers a proxy whose health check recovered. The
// server re-opens the public endpoint; the confirmation arrives as the
// regular proxy list, which the client logs when it lands.
func (c *client) registerProxyAgain(proxy config.ProxyConfig) error {
	framer := c.currentFramer()
	if framer == nil {
		return errors.New("no control connection to register on")
	}
	spec, ok := c.specFor(proxy.Name)
	if !ok {
		spec = proxySpec(proxy)
	}
	if err := framer.WriteJSON(protocol.TypeRegisterProxy, spec); err != nil {
		return fmt.Errorf("send the registration: %w", err)
	}
	c.rememberSpec(spec)
	return nil
}
