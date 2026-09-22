package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// testClient builds the smallest client the heartbeat decision needs: a config and a
// logger whose output the test can read.
func testClient(t *testing.T, clientHeartbeatSeconds int) (*client, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	cfg := &config.Config{}
	cfg.Client.HeartbeatSeconds = clientHeartbeatSeconds
	return &client{cfg: cfg, logger: log.New(buf, "", 0)}, buf
}

// The server decides how often a client heartbeats, because the server is what drops
// a client that has been quiet for three intervals.
func TestHeartbeatIntervalFollowsTheServer(t *testing.T) {
	c, logs := testClient(t, 60)

	if got := c.heartbeatInterval(15); got != 15*time.Second {
		t.Fatalf("interval is %s, want the server's 15s", got)
	}
	if !strings.Contains(logs.String(), "client.heartbeat_seconds") {
		t.Errorf("a configured value that is overridden was not reported: %q", logs.String())
	}
}

// A server that does not state an interval leaves the configured value in charge:
// that is what makes [client].heartbeat_seconds a setting rather than a decoration.
func TestHeartbeatIntervalFallsBackToTheConfiguredValue(t *testing.T) {
	c, logs := testClient(t, 45)

	if got := c.heartbeatInterval(0); got != 45*time.Second {
		t.Fatalf("interval is %s, want the configured 45s", got)
	}
	if logs.Len() != 0 {
		t.Errorf("the fallback logged something: %q", logs.String())
	}
	// A negative value cannot mean an interval either; treat it like a missing one.
	if got := c.heartbeatInterval(-1); got != 45*time.Second {
		t.Fatalf("interval for a negative server value is %s, want the configured 45s", got)
	}
}

// A config built without applyDefaults has zero everywhere, and a ticker needs a
// positive period, so there is a floor under it.
func TestHeartbeatIntervalHasADefaultWhenNothingIsSet(t *testing.T) {
	c, _ := testClient(t, 0)

	if got := c.heartbeatInterval(0); got != 30*time.Second {
		t.Fatalf("interval is %s, want the 30s default", got)
	}
}

// The same interval is agreed on, so nothing claims an override happened.
func TestHeartbeatIntervalIsSilentWhenBothAgree(t *testing.T) {
	c, logs := testClient(t, 30)

	if got := c.heartbeatInterval(30); got != 30*time.Second {
		t.Fatalf("interval is %s, want 30s", got)
	}
	if logs.Len() != 0 {
		t.Errorf("an agreeing pair logged something: %q", logs.String())
	}
}
