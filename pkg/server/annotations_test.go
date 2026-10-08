package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The labels a publisher attaches to a proxy reach the dashboard, which is what
// they are for: an operator reads "env" or a ticket number off the tunnel
// without the name having to carry it.
func TestTheDashboardShowsProxyAnnotations(t *testing.T) {
	echo := startEcho(t)
	rs := startServer(t, testConfig(t, false))

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"labelled": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "labelled", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
		Annotations: map[string]string{"env": "prod", "owner": "platform"},
	})

	dashboard, err := NewDashboard(rs.server, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if err := dashboard.Start(); err != nil {
		t.Fatalf("start the dashboard: %v", err)
	}
	defer dashboard.Stop()

	response, err := http.Get(fmt.Sprintf("http://%s/api/proxies", dashboard.Listener().Addr()))
	if err != nil {
		t.Fatalf("fetch the proxy list: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	var payload struct {
		Proxies []struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode the dashboard response: %v (body %s)", err, body)
	}
	if len(payload.Proxies) != 1 {
		t.Fatalf("the dashboard lists %d proxies, want 1: %s", len(payload.Proxies), body)
	}
	got := payload.Proxies[0].Annotations
	if got["env"] != "prod" || got["owner"] != "platform" || len(got) != 2 {
		t.Fatalf("the dashboard shows %v, want the two labels that were published", got)
	}
}

// A proxy without labels keeps the response shape it had before the field
// existed: no key, rather than an empty object.
func TestTheDashboardOmitsAnnotationsWhenThereAreNone(t *testing.T) {
	echo := startEcho(t)
	rs := startServer(t, testConfig(t, false))

	agent := startAgent(t, rs.addr, false, map[string]dataHandler{"plain": streamHandler(echo)})
	agent.register(protocol.ProxySpec{
		Name: "plain", Type: protocol.ProxyTypeTCP, LocalAddr: echo, RemotePort: freePort(t),
	})

	dashboard, err := NewDashboard(rs.server, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if err := dashboard.Start(); err != nil {
		t.Fatalf("start the dashboard: %v", err)
	}
	defer dashboard.Stop()

	response, err := http.Get(fmt.Sprintf("http://%s/api/proxies", dashboard.Listener().Addr()))
	if err != nil {
		t.Fatalf("fetch the proxy list: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	var payload struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode the dashboard response: %v (body %s)", err, body)
	}
	if len(payload.Proxies) != 1 {
		t.Fatalf("the dashboard lists %d proxies, want 1: %s", len(payload.Proxies), body)
	}
	if _, present := payload.Proxies[0]["annotations"]; present {
		t.Fatalf("a proxy without labels still carries an annotations key: %s", body)
	}
}
