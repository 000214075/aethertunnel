package server

import (
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// visitorCredentialFailure dials the control port, presents a visitor-connect that
// cannot authenticate, and waits for the answer before returning.
func visitorCredentialFailure(t *testing.T, serverAddr string, connect protocol.VisitorConnect) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	framer := protocol.NewFramer(conn, nil, 0)
	if err := framer.WriteJSON(protocol.TypeVisitorConnect, connect); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The server answers a refusal before closing, so reading it makes what follows
	// independent of how the connection is torn down.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = framer.ReadFrame()
}

// TestAVisitorThatFailsACredentialCheckIsBanned covers the ban list on the visitor
// entry point.
//
// A visitor connection carries the same server auth token a control connection does,
// and a `TypeVisitorConnect` first frame is dispatched before anything is
// authenticated, so the two frame types are two doors to the same credential.
// recordAuthFailure is the only place a ban is imposed and says every way of failing
// to authenticate counts; the visitor door skipped it, so an address could guess the
// token through TypeVisitorConnect for as long as it liked and never be refused.
func TestAVisitorThatFailsACredentialCheckIsBanned(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*config.Config)
		connect   protocol.VisitorConnect
	}{
		{
			name: "an invalid auth token",
			connect: protocol.VisitorConnect{
				Proxy: "private", Type: "tcp", AuthToken: "wrong",
			},
		},
		{
			name:      "a failed identity assertion",
			configure: func(cfg *config.Config) { cfg.Identity.Enabled = true },
			connect: protocol.VisitorConnect{
				// The token is right, so the run reaches the identity check, which
				// cannot verify a signature this short.
				Proxy: "private", Type: "tcp", AuthToken: testToken,
				Identity: []byte("not-a-key"), IdentityNonce: []byte("short"),
				IdentityTime: 1, IdentitySignature: []byte("nope"),
			},
		},
		{
			name: "an invalid proxy secret key",
			// Reaching the secret check needs the proxy to exist; the token check comes
			// first and is satisfied, so this is the first thing that can fail.
			configure: nil,
			connect: protocol.VisitorConnect{
				Proxy: "private", Type: "tcp", AuthToken: testToken, Secret: "wrong",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, false)
			cfg.Server.BanAfterFailures = 1
			cfg.Server.BanSeconds = 60
			if tc.configure != nil {
				tc.configure(cfg)
			}
			if err := cfg.Validate(config.RoleServer); err != nil {
				t.Fatalf("config: %v", err)
			}
			rs := startServer(t, cfg)

			// A private proxy has to exist for the secret check to be the thing that
			// fails, and a member has to be registered to publish it.
			agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(startEcho(t))})
			agent.register(protocol.ProxySpec{
				Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: "127.0.0.1:1", SecretKey: "s3cret",
			})

			visitorCredentialFailure(t, rs.addr, tc.connect)

			if banned := rs.server.bans.banned(); banned != 1 {
				t.Fatalf("the list reports %d banned sources after a visitor credential failure, want 1", banned)
			}
			if got := rs.server.metrics.authFailures.Load(); got == 0 {
				t.Error("the failure was not counted as an authentication failure")
			}
			// The ban is on the source address, so this address is now refused before any
			// frame of its next connection is read.
			if banned, _ := rs.server.bans.blocked(net.ParseIP("127.0.0.1")); !banned {
				t.Error("the source is not refused after the ban it earned")
			}
		})
	}
}

// TestAVisitorThatAuthenticatesClearsItsFailureCount covers the other half: a source
// that eventually presents good credentials must not carry its earlier typos towards a
// ban, which is what a control connection already guarantees.
func TestAVisitorThatAuthenticatesClearsItsFailureCount(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 3
	cfg.Server.BanSeconds = 60
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	// Two failures, then a success: without the clear, one more failure would ban.
	for i := 0; i < 2; i++ {
		visitorCredentialFailure(t, rs.addr, protocol.VisitorConnect{
			Proxy: "private", Type: "tcp", AuthToken: "wrong",
		})
	}

	conn, _ := visitorHandshake(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP)
	conn.Close()

	visitorCredentialFailure(t, rs.addr, protocol.VisitorConnect{
		Proxy: "private", Type: "tcp", AuthToken: "wrong",
	})

	if banned := rs.server.bans.banned(); banned != 0 {
		t.Errorf("a source that authenticated once and then failed once was banned (%d)", banned)
	}
}
