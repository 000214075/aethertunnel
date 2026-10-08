package clientlib

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A reload whose new configuration deleted the token altogether — the
// auth_token_file line removed and no inline auth_token in its place — is not
// announced as restart-only (the [client] comparison drops the token fields)
// and has nothing to adopt. What it must not be is silent: the running client
// keeps authenticating with the token the configuration no longer carries, and
// the operator is the one who has to hear that.
func TestAReloadWithoutAnAuthTokenSaysThePreviousOneIsKept(t *testing.T) {
	buf := &bytes.Buffer{}
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()
	c.cfg.Client.AuthToken = "kept-token-0123456789"

	reloaded := &config.Config{}
	c.applyReload(reloaded)

	if c.cfg.Client.AuthToken != "kept-token-0123456789" {
		t.Fatalf("the reload replaced the token the configuration deleted: %q", c.cfg.Client.AuthToken)
	}
	if want := "reload: the new configuration carries no auth token; the running client keeps the previous one"; !strings.Contains(buf.String(), want) {
		t.Fatalf("the reload stayed silent about the deleted token; it says:\n%s", buf.String())
	}
}
