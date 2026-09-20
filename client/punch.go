package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/reliable"
)

// punchTimeout bounds a whole direct-path attempt. It is short enough that a
// caller waiting for a visitor connection notices the fallback to the relayed
// path but long enough to survive a lost first datagram.
const punchTimeout = 12 * time.Second

// tryPunch attempts a direct path from this visitor to the proxy's owner.
//
// The rendezvous server tells each end the address the other end's NAT mapped it
// to. Both ends then send from the very socket that will carry the direct path,
// so the mapping towards the peer is the one the peer has been told about; that
// is what lets the packets through when the NATs are not symmetric, and it is why
// the address exchange has to happen over the socket that later carries the
// stream rather than over a throwaway one.
func (c *client) tryPunch(ctx context.Context, conn net.Conn, framer *protocol.Framer, offer protocol.P2PPeer) (net.Conn, error) {
	if offer.Token == "" {
		return nil, errors.New("the server did not offer a hole-punch token")
	}

	rendezvous, err := c.rendezvousAddr()
	if err != nil {
		return nil, err
	}

	socket, err := reliable.Listen(":0")
	if err != nil {
		return nil, fmt.Errorf("open a punch socket: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, punchTimeout)
	defer cancel()

	request := protocol.EncodePunchRequest(protocol.PunchRoleVisitor, offer.Token)
	reply, _, err := socket.Exchange(ctx, rendezvous, request, rendezvousAccept(offer.Token))
	if err != nil {
		_ = socket.Close()
		return nil, fmt.Errorf("the rendezvous server never answered: %w", err)
	}
	peer, err := punchPeer(reply, offer.Token)
	if err != nil {
		_ = socket.Close()
		return nil, err
	}

	socket.SetToken([]byte(offer.Token))
	stream, err := socket.Punch(ctx, peer)
	if err != nil {
		_ = socket.Close()
		return nil, fmt.Errorf("hole punching to %s failed: %w", peer, err)
	}
	c.logger.Printf("direct path to %s established", peer)
	return stream, nil
}

// servePunch opens the owner's side of a hole-punch attempt. When the punch
// succeeds the proxy's local service is served over the direct path, and the
// server never sees the traffic.
func (c *client) servePunch(prepare protocol.P2PPrepare) {
	proxy, err := c.findProxy(prepare.Proxy)
	if err != nil {
		c.logger.Printf("punch for %q: %v", prepare.Proxy, err)
		return
	}

	rendezvous, err := c.rendezvousAddr()
	if err != nil {
		c.logger.Printf("punch for %q: %v", prepare.Proxy, err)
		return
	}

	socket, err := reliable.Listen(":0")
	if err != nil {
		c.logger.Printf("punch for %q: cannot open a punch socket: %v", prepare.Proxy, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), punchTimeout)
	defer cancel()

	request := protocol.EncodePunchRequest(protocol.PunchRoleOwner, prepare.Token)
	reply, _, err := socket.Exchange(ctx, rendezvous, request, rendezvousAccept(prepare.Token))
	if err != nil {
		c.logger.Printf("punch for %q: the rendezvous server never answered: %v", prepare.Proxy, err)
		_ = socket.Close()
		return
	}
	peer, err := punchPeer(reply, prepare.Token)
	if err != nil {
		c.logger.Printf("punch for %q: %v", prepare.Proxy, err)
		_ = socket.Close()
		return
	}

	socket.SetToken([]byte(prepare.Token))
	stream, err := socket.Punch(ctx, peer)
	if err != nil {
		c.logger.Printf("punch for %q: no direct path to %s (%v); the visitor will use the relay",
			prepare.Proxy, peer, err)
		_ = socket.Close()
		return
	}
	defer socket.Close()

	c.logger.Printf("punch for %q succeeded: direct path to %s", prepare.Proxy, peer)

	if config.IsDatagramProxyType(proxy.Type) {
		// Only xtcp tunnels are ever punched, and xtcp carries a byte stream;
		// a datagram proxy would need sequence-aware framing, which the direct
		// path does not provide.
		c.logger.Printf("punch for %q: %s is a datagram proxy and cannot use the direct path",
			prepare.Proxy, proxy.Type)
		return
	}

	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	local, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
	if err != nil {
		c.logger.Printf("punch for %q: cannot reach the local service %s: %v",
			prepare.Proxy, proxy.LocalAddr(), err)
		return
	}

	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toPeer, fromPeer := flynet.Pipe(local, c.wrapWith(stream, c.cipher), idle)
	c.logger.Printf("punch for %q: direct session finished (sent %d, received %d)",
		prepare.Proxy, toPeer, fromPeer)
}

// rendezvousAddr derives the server's UDP rendezvous address from the control
// address and the port the server reported at authentication time.
func (c *client) rendezvousAddr() (net.Addr, error) {
	c.mu.Lock()
	port := c.p2pPort
	c.mu.Unlock()
	if port == 0 {
		return nil, errors.New("the server does not offer hole punching (server.p2p_port is not set)")
	}

	host, _, err := net.SplitHostPort(c.cfg.Client.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("client.server_addr %q is not host:port", c.cfg.Client.ServerAddr)
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("resolve the rendezvous address: %w", err)
	}
	return addr, nil
}

// rendezvousAccept selects the rendezvous server's answer out of the datagrams
// arriving on a punch socket. The peer's hello datagrams arrive on the same
// socket as soon as the server has told it where to send, so the reply has to be
// recognised rather than taken as whatever comes first.
func rendezvousAccept(token string) func([]byte) bool {
	return func(datagram []byte) bool {
		got, _, ok := protocol.DecodePunchResponse(datagram)
		return ok && got == token
	}
}

// punchPeer decodes a rendezvous reply into the peer's address.
func punchPeer(reply []byte, token string) (*net.UDPAddr, error) {
	got, text, ok := protocol.DecodePunchResponse(reply)
	if !ok || got != token {
		return nil, errors.New("malformed rendezvous reply")
	}
	addr, err := net.ResolveUDPAddr("udp", text)
	if err != nil {
		return nil, fmt.Errorf("the rendezvous server reported an unusable address %q: %w", text, err)
	}
	return addr, nil
}
