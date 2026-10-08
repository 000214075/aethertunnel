package clientlib

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A section the reload cannot apply is announced, not ignored: an operator who
// edits [oidc] and reloads has to learn that the running token fetch is
// unchanged, or the change looks applied and is not.
func TestReloadAnnouncesASectionItCannotApply(t *testing.T) {
	buf := &bytes.Buffer{}
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()

	// Everything else stays as it is; only the OIDC section differs.
	newCfg := &config.Config{}
	newCfg.OIDC = &config.OIDCConfig{
		TokenEndpointURL: "https://issuer.example/token",
		ClientID:         "aethertunnel",
		ClientSecret:     "s3cret",
	}
	c.applyReload(newCfg)

	if !strings.Contains(buf.String(), "[oidc] changed; it applies after a restart") {
		t.Fatalf("the reload said nothing about [oidc]: %q", buf.String())
	}
}

// [log] is read once, when the process builds its logger, so a reload that
// changes where the log goes has to say that the running writer is unchanged.
func TestReloadAnnouncesAChangedLogSection(t *testing.T) {
	buf := &bytes.Buffer{}
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()

	newCfg := &config.Config{}
	newCfg.Log.To = "aethertunnel.log"
	newCfg.Log.MaxDays = 7
	c.applyReload(newCfg)

	if !strings.Contains(buf.String(), "[log] changed; it applies after a restart") {
		t.Fatalf("the reload said nothing about [log]: %q", buf.String())
	}
}

// A reload that changes nothing announces nothing: the restart notice is worth
// reading only if it appears when something really changed.
func TestReloadStaysQuietWhenNothingChanged(t *testing.T) {
	buf := &bytes.Buffer{}
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()

	c.applyReload(&config.Config{})

	if strings.Contains(buf.String(), "applies after a restart") {
		t.Fatalf("an unchanged configuration announced a restart: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "0 proxy(es) added, 0 changed, 0 removed, 0 unchanged") {
		t.Fatalf("the reload did not report the empty diff: %q", buf.String())
	}
}

// The live half of the reload is the dispatch lists, and a start list that
// selects a subset is what they are narrowed by.
func TestReloadAppliesTheStartSelection(t *testing.T) {
	buf := &bytes.Buffer{}
	off := false
	c := &client{cfg: &config.Config{}, logger: log.New(buf, "", 0)}
	c.baseCtx = context.Background()

	newCfg := &config.Config{}
	newCfg.Client.Start = []string{"web"}
	newCfg.Proxies = []config.ProxyConfig{
		{Name: "web", Type: config.ProxyTypeTCP},
		{Name: "ssh", Type: config.ProxyTypeTCP},
		{Name: "files", Type: config.ProxyTypeHTTP, Enabled: &off},
	}
	c.applyReload(newCfg)

	if got := c.proxyList(); len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("the reload left %d proxies, want just web", len(got))
	}
	if !strings.Contains(buf.String(), "client.start selects 1 of 3 tunnel(s)") {
		t.Errorf("the reload did not report the selection: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "1 entry(s) carry enabled = false") {
		t.Errorf("the reload did not report the disabled entry: %q", buf.String())
	}
}
