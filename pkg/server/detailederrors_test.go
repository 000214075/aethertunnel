package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// refusalText registers a proxy that the server has to refuse and returns the
// text it answered with.
func refusalText(t *testing.T, client *testClient, spec protocol.ProxySpec) string {
	t.Helper()
	if err := client.register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	msg, err := client.framer.ReadFrame()
	if err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if msg.Type != protocol.TypeError {
		t.Fatalf("expected a refusal, got %s", msg.Type)
	}
	var payload protocol.ErrorPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	return payload.Error
}

// A refusal that names what the server has or lacks reaches the client by
// default, which is frp's detailedErrorsToClient = true and the behaviour this
// server had before the key existed.
func TestARefusalNamesTheReasonByDefault(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Server.AllowPorts = []string{"10000-10010"}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	got := refusalText(t, client, protocol.ProxySpec{Name: "outside", Type: "tcp", LocalAddr: "127.0.0.1:9", RemotePort: 12345})
	if !strings.Contains(got, "allow_ports") || !strings.Contains(got, "12345") {
		t.Fatalf("the refusal did not name the reason: %q", got)
	}
}

// With the key off, the same refusal is a summary: a client that has not proven
// itself learns that the registration failed, not which of the server's ports
// are free or which address a subdomain would need. The server's log keeps the
// detail, so the operator does not lose it.
func TestARefusalIsSummarisedWhenDetailedErrorsAreOff(t *testing.T) {
	off := false
	cfg := testConfig(t, false)
	cfg.Server.AllowPorts = []string{"10000-10010"}
	cfg.Server.DetailedErrorsToClient = &off
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	var logs lockedBuffer
	rs := startServerLoggingTo(t, cfg, &logs)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	got := refusalText(t, client, protocol.ProxySpec{Name: "outside", Type: "tcp", LocalAddr: "127.0.0.1:9", RemotePort: 12345})
	if got != `cannot register the proxy "outside"` {
		t.Fatalf("the refusal was %q, want the summary", got)
	}
	if !strings.Contains(logs.String(), "allow_ports") {
		t.Fatalf("the server's log lost the reason: %q", logs.String())
	}
}

// The withdrawal path keeps its answer: "no such proxy" names a proxy the
// client itself asked about, so it describes nothing of the server's setup.
func TestAWithdrawalOfAnUnknownProxyStillSaysSoWithShortRefusals(t *testing.T) {
	off := false
	cfg := testConfig(t, false)
	cfg.Server.DetailedErrorsToClient = &off
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("c1", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if err := client.framer.WriteJSON(protocol.TypeProxyWithdraw, protocol.ProxyWithdraw{Name: "never-published"}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	var ack protocol.ProxyWithdrawAck
	if err := client.framer.ReadJSON(protocol.TypeProxyWithdrawAck, &ack); err != nil {
		t.Fatalf("read the ack: %v", err)
	}
	if ack.OK || ack.Error != "no such proxy" {
		t.Fatalf("the withdrawal answer was ok=%v error=%q", ack.OK, ack.Error)
	}
}

// The identity refusal carries the actionable flag whatever the wording is, so
// a shortened text still tells the client what to configure.
func TestTheIdentityRefusalStaysActionableWithShortRefusals(t *testing.T) {
	off := false
	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	cfg.Server.DetailedErrorsToClient = &off
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("anonymous", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if response.OK {
		t.Fatal("a client without an identity was accepted")
	}
	if response.Error != "identity check failed" {
		t.Fatalf("the refusal was %q, want the summary", response.Error)
	}
	if !response.IdentityRequired {
		t.Fatal("the shortened refusal dropped the identity flag")
	}
}

// The identity refusal names the rule when detailed errors are on, which is what
// an operator debugging a client sees by default.
func TestTheIdentityRefusalNamesTheRuleByDefault(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Identity.Enabled = true
	cfg.Identity.RequireIdentity = true
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()

	response, err := client.authenticate("anonymous", testToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !response.IdentityRequired {
		t.Fatal("the refusal did not say that an identity is required")
	}
	if !strings.Contains(response.Error, "[identity]") {
		t.Fatalf("the refusal did not name the rule: %q", response.Error)
	}
}

// A wrong first frame is answered with the accepted frame types only while
// detailed errors are on: the list describes this server's protocol.
func TestTheFirstFrameRefusalIsShortenedToo(t *testing.T) {
	for _, detailed := range []bool{true, false} {
		name := "detailed"
		if !detailed {
			name = "summary"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t, false)
			if !detailed {
				off := false
				cfg.Server.DetailedErrorsToClient = &off
			}
			if err := cfg.Validate(config.RoleServer); err != nil {
				t.Fatalf("config: %v", err)
			}
			rs := startServer(t, cfg)

			conn, err := newTestClient(t, rs.addr, false)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer conn.close()

			if err := conn.framer.WriteJSON(protocol.TypeHeartbeat, struct{}{}); err != nil {
				t.Fatalf("write: %v", err)
			}
			var payload protocol.ErrorPayload
			if err := conn.framer.ReadJSON(protocol.TypeError, &payload); err != nil {
				t.Fatalf("read the refusal: %v", err)
			}
			named := strings.Contains(payload.Error, "must be")
			if named != detailed {
				t.Fatalf("detailed=%v but the refusal was %q", detailed, payload.Error)
			}
		})
	}
}

// The key has to survive a reload of the client's configuration, where the two
// sides are compared field by field: a pointer is what makes "absent" and
// "false" distinguishable, so this pins the accessor's meaning.
func TestDetailedErrorsDefaultsToOn(t *testing.T) {
	cfg := &config.Config{}
	if !cfg.DetailedErrors() {
		t.Fatal("an absent detailed_errors_to_client must mean detailed")
	}
	off := false
	cfg.Server.DetailedErrorsToClient = &off
	if cfg.DetailedErrors() {
		t.Fatal("an explicit false must mean short refusals")
	}
	on := true
	cfg.Server.DetailedErrorsToClient = &on
	if !cfg.DetailedErrors() {
		t.Fatal("an explicit true must mean detailed refusals")
	}
}
