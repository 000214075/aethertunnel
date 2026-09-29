package server

import (
	"io"
	"net"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// TestAVisitorRefusalBeforeAcceptanceMovesTheRejectionCounter covers the aggregate
// rejection counter on the visitor entry point.
//
// A visitor arrives on the control port, so refusing one is the same event as refusing
// an ordinary client and has to reach the same counter: that counter is the one an
// alert watches when the reason does not matter, and only one of the visitor's refusal
// paths used to move it. A refusal that leaves the counter untouched is a connection
// the server answered that no series accounts for.
func TestAVisitorRefusalBeforeAcceptanceMovesTheRejectionCounter(t *testing.T) {
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
			name: "a proxy that is not registered",
			connect: protocol.VisitorConnect{
				Proxy: "missing", Type: "tcp", AuthToken: testToken,
			},
		},
		{
			name: "a wrong secret key",
			connect: protocol.VisitorConnect{
				Proxy: "private", Type: "tcp", AuthToken: testToken, Secret: "wrong",
			},
		},
		{
			name: "a transport that does not match the proxy",
			connect: protocol.VisitorConnect{
				// private is a byte-stream proxy, so a datagram visitor cannot be
				// paired with it.
				Proxy: "private", Type: protocol.ProxyTypeSUDP, AuthToken: testToken, Secret: "s3cret",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echo := startEcho(t)
			cfg := testConfig(t, false)
			if tc.configure != nil {
				tc.configure(cfg)
			}
			if err := cfg.Validate(config.RoleServer); err != nil {
				t.Fatalf("config: %v", err)
			}
			rs := startServer(t, cfg)

			agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
			agent.register(protocol.ProxySpec{
				Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
			})

			metrics := rs.server.metrics
			rejectedBefore := metrics.controlRejected.Load()
			acceptedBefore := metrics.controlAccepted.Load()

			visitorCredentialFailure(t, rs.addr, tc.connect)

			if got := metrics.controlRejected.Load(); got != rejectedBefore+1 {
				t.Errorf("the rejection counter is %d after one visitor refusal, want %d", got, rejectedBefore+1)
			}
			if got := metrics.controlAccepted.Load(); got != acceptedBefore {
				t.Errorf("the refusal was counted as an accepted connection (%d, was %d)", got, acceptedBefore)
			}
		})
	}
}

// TestAnAcceptedVisitorIsNotCountedAsRejected is the other half: a visitor that
// authenticates is an accepted connection and nothing else, so the pair of counters
// keeps meaning "answered, and how" rather than "tried, and how".
func TestAnAcceptedVisitorIsNotCountedAsRejected(t *testing.T) {
	echo := startEcho(t)
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"private": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "private", Type: protocol.ProxyTypeSTCP, LocalAddr: echo, SecretKey: "s3cret",
	})

	metrics := rs.server.metrics
	rejectedBefore := metrics.controlRejected.Load()
	acceptedBefore := metrics.controlAccepted.Load()

	conn, _ := visitorHandshake(t, rs.addr, "private", "s3cret", protocol.ProxyTypeSTCP)
	conn.Close()

	if got := metrics.controlRejected.Load(); got != rejectedBefore {
		t.Errorf("the rejection counter moved for a visitor that authenticated: %d, was %d", got, rejectedBefore)
	}
	if got := metrics.controlAccepted.Load(); got != acceptedBefore+1 {
		t.Errorf("the accepted visitor moved the acceptance counter to %d, want %d",
			got, acceptedBefore+1)
	}
}

// TestOnlyThePreAcceptanceRefusalIsBooked pins the split between the two refusal
// helpers. The counter has to move for a visitor the server turned away before it was
// accepted, and must not move for a relay failure after it was — that connection is
// already counted as accepted, so booking it again would make accepted plus rejected
// exceed the connections the server answered. Folding the counter into rejectVisitor,
// which reads like a tidy-up, is what breaks this.
func TestOnlyThePreAcceptanceRefusalIsBooked(t *testing.T) {
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)
	metrics := rs.server.metrics
	before := metrics.controlRejected.Load()

	for _, tc := range []struct {
		name  string
		book  func(conn net.Conn, framer *protocol.Framer)
		moved bool
	}{
		{
			name: "a relay that could not be set up, after acceptance",
			book: func(conn net.Conn, framer *protocol.Framer) {
				rs.server.rejectVisitor(conn, framer, "no client is publishing this proxy")
			},
			moved: false,
		},
		{
			name: "credentials that do not check out, before acceptance",
			book: func(conn net.Conn, framer *protocol.Framer) {
				rs.server.refuseVisitor(conn, framer, AuditEvent{
					Event: EventVisitorRejected, Detail: "invalid secret key",
				}, "invalid secret key")
			},
			moved: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			// net.Pipe is unbuffered, so a write completes only once the peer reads it.
			go io.Copy(io.Discard, client)

			was := metrics.controlRejected.Load()
			tc.book(server, protocol.NewFramer(server, nil, 0))

			got := metrics.controlRejected.Load()
			if tc.moved && got != was+1 {
				t.Errorf("the rejection counter is %d after the refusal, want %d", got, was+1)
			}
			if !tc.moved && got != was {
				t.Errorf("the rejection counter moved to %d for a refusal after acceptance, was %d", got, was)
			}
		})
	}

	if got := metrics.controlRejected.Load(); got != before+1 {
		t.Errorf("the two refusals moved the counter to %d, want %d", got, before+1)
	}
}
