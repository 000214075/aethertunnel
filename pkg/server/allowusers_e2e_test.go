package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// visitorAckAs performs the visitor handshake as a named identity and returns
// what the server answered, refusals included.
func visitorAckAs(t *testing.T, serverAddr, proxy, secret, kind, user string) protocol.DataOpenAck {
	t.Helper()

	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	framer := protocol.NewFramer(conn, nil, 0)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := framer.WriteJSON(protocol.TypeVisitorConnect, protocol.VisitorConnect{
		Proxy: proxy, Secret: secret, Type: kind, AuthToken: testToken, User: user,
	}); err != nil {
		t.Fatalf("send visitor-connect: %v", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	return ack
}

// A private proxy's allow_users decides which identities may visit it, and the
// secret key alone is not enough.
func TestAVisitorIsRefusedWhenTheProxyDoesNotAllowItsIdentity(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
		User: "ada", AllowUsers: []string{"grace"},
	})

	if ack := visitorAckAs(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP, "grace"); !ack.OK {
		t.Fatalf("the listed identity was refused: %s", ack.Error)
	}
	ack := visitorAckAs(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP, "linus")
	if ack.OK {
		t.Fatal("an identity that is not on the list was served")
	}
	if !strings.Contains(ack.Error, "grace") {
		t.Errorf("the refusal does not name the identity that is allowed: %s", ack.Error)
	}
}

// With no allow_users the publisher's own identity is the list, so a proxy
// published by one operator's client is not shared with another's.
func TestAVisitorIsRefusedWhenThePublisherIsAnotherIdentity(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
		User: "ada",
	})

	if ack := visitorAckAs(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP, "ada"); !ack.OK {
		t.Fatalf("the publisher's own identity was refused: %s", ack.Error)
	}
	ack := visitorAckAs(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP, "grace")
	if ack.OK {
		t.Fatal("another identity was served by a proxy that lists nobody")
	}
	if !strings.Contains(ack.Error, "ada") {
		t.Errorf("the refusal does not name the publisher's identity: %s", ack.Error)
	}
}

// The proxy's secret is proven first, so a visitor that does not hold it learns
// nothing about who may use the proxy: the refusal it gets is about the key.
func TestTheIdentityIsCheckedAfterTheSecret(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
		User: "ada", AllowUsers: []string{"grace"},
	})

	ack := visitorAckAs(t, rs.addr, "private", "wrong", protocol.ProxyTypeSTCP, "grace")
	if ack.OK {
		t.Fatal("a visitor with the wrong secret was served")
	}
	if strings.Contains(ack.Error, "grace") {
		t.Errorf("the refusal leaked the allow_users list before the secret was proven: %s", ack.Error)
	}
}
