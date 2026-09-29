package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/socks"
)

// socksUDPAssociate performs the SOCKS5 handshake and sends a UDP ASSOCIATE
// request, returning the relay address the server answered with.
func socksUDPAssociate(t *testing.T, conn net.Conn) string {
	t.Helper()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte{socks.Version, 1, socks.MethodNoReq}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("method reply: %v", err)
	}
	if method[0] != socks.Version || method[1] != socks.MethodNoReq {
		t.Fatalf("the endpoint answered %v", method)
	}

	request := []byte{socks.Version, socks.CmdAssoc, 0x00, socks.AtypIPv4, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("associate request: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("associate reply: %v", err)
	}
	if head[1] != socks.ReplySucceeded {
		t.Fatalf("the associate reply code is %d, want %d", head[1], socks.ReplySucceeded)
	}

	switch head[3] {
	case socks.AtypIPv4:
		buf := make([]byte, 6)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("associate reply address: %v", err)
		}
		ip := net.IP(buf[:4])
		return net.JoinHostPort(ip.String(), strconv.Itoa(int(binary.BigEndian.Uint16(buf[4:6]))))
	case socks.AtypIPv6:
		buf := make([]byte, 18)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("associate reply address: %v", err)
		}
		ip := net.IP(buf[:16])
		return net.JoinHostPort(ip.String(), strconv.Itoa(int(binary.BigEndian.Uint16(buf[16:18]))))
	case socks.AtypDomain:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			t.Fatalf("associate reply address: %v", err)
		}
		buf := make([]byte, int(size[0])+2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("associate reply address: %v", err)
		}
		return net.JoinHostPort(string(buf[:int(size[0])]), strconv.Itoa(int(binary.BigEndian.Uint16(buf[len(buf)-2:]))))
	default:
		t.Fatalf("the associate reply uses address type %d", head[3])
		return ""
	}
}

// TestSocks5UDPAssociateRelaysDatagrams runs a full UDP ASSOCIATE exchange
// through a socks5 endpoint: the visitor is told where to send datagrams, and a
// wrapped datagram it sends comes back with the reply from the UDP target.
func TestSocks5UDPAssociateRelaysDatagrams(t *testing.T) {
	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	agent.setUDPDialTarget(func(target string) (net.Conn, error) {
		return net.DialTimeout("udp", target, 5*time.Second)
	})
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	control, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the socks5 endpoint: %v", err)
	}
	defer control.Close()

	relayAddr := socksUDPAssociate(t, control)

	visitor, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("dial the relay at %s: %v", relayAddr, err)
	}
	defer visitor.Close()

	wrapped, err := socks.WrapUDPDatagram(echo, []byte("through the udp relay"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if _, err := visitor.Write(wrapped); err != nil {
		t.Fatalf("send the datagram: %v", err)
	}

	_ = visitor.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 2048)
	n, err := visitor.Read(buf)
	if err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	target, data, err := socks.ParseUDPDatagram(buf[:n])
	if err != nil {
		t.Fatalf("parse the reply: %v", err)
	}
	if target != echo {
		t.Errorf("the reply names %q, want the echo at %q", target, echo)
	}
	if string(data) != "through the udp relay" {
		t.Errorf("the reply carries %q", data)
	}

	if got := rs.server.metrics.socksUDPAssoc.Load(); got != 1 {
		t.Errorf("the association counter is %d, want 1", got)
	}
	if got := rs.server.metrics.socksUDPDatagrams.Load(); got < 2 {
		t.Errorf("the datagram counter is %d, want at least 2 (one each way)", got)
	}
}

// TestSocks5UDPAssociateRefusesADatagramFromAnotherSource covers the relay's
// source restriction: a datagram from an address that did not open the
// association is dropped before it reaches the client.
func TestSocks5UDPAssociateRefusesADatagramFromAnotherSource(t *testing.T) {
	other := secondLoopback(t)
	if other == "" {
		t.Skip("this platform answers only on 127.0.0.1, so a second UDP source cannot be told apart")
	}

	echo := startUDPEcho(t)
	cfg := testConfig(t, false)
	rs := startServer(t, cfg)

	publicPort := freePort(t)
	agent := startAgent(t, rs.addr, false, nil)
	agent.setUDPDialTarget(func(target string) (net.Conn, error) {
		return net.DialTimeout("udp", target, 5*time.Second)
	})
	agent.register(protocol.ProxySpec{
		Name: "exit", Type: protocol.ProxyTypeSOCKS, RemotePort: publicPort,
		AllowTargets: []string{"127.0.0.0/8"},
	})
	waitForListener(t, rs.server, "exit")

	control, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the socks5 endpoint: %v", err)
	}
	defer control.Close()

	relayAddr := socksUDPAssociate(t, control)

	// A UDP socket on a second loopback address, which is not the association's
	// source. It must not be relayed to the client.
	stranger, err := net.ListenPacket("udp", net.JoinHostPort(other, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", other, err)
	}
	defer stranger.Close()

	wrapped, err := socks.WrapUDPDatagram(echo, []byte("intruder"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if _, err := stranger.WriteTo(wrapped, mustUDPAddr(t, relayAddr)); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Give the relay a moment to drop it; nothing may reach the client.
	time.Sleep(300 * time.Millisecond)
	if got := rs.server.metrics.socksUDPDatagrams.Load(); got != 0 {
		t.Errorf("the datagram counter is %d after a datagram from another source, want 0", got)
	}
}

func mustUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	parsed, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return parsed
}
