package server

import (
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A private proxy with no allow_users admits the publisher's own identity,
// which is frp's rule: the secret key is shared, and the identity decides whose
// client may use it.
func TestPrivateProxyWithoutAllowUsersAdmitsThePublisherAlone(t *testing.T) {
	cfg := &config.Config{}
	manager := &TunnelManager{cfg: cfg, policies: &proxyPolicies{}}
	group := newProxyGroup(protocol.ProxySpec{
		Name: "ssh", Type: protocol.ProxyTypeSTCP, SecretKey: "s", User: "ada",
	}, manager)

	allowed, reason := group.visitorIdentityAllow("ada")
	if !allowed {
		t.Fatalf("the publisher's own identity was refused: %s", reason)
	}
	if allowed, reason := group.visitorIdentityAllow("grace"); allowed {
		t.Fatal("a stranger's identity was admitted by a proxy that lists no allow_users")
	} else if !strings.Contains(reason, "ada") || !strings.Contains(reason, "grace") {
		t.Errorf("the refusal names neither identity: %s", reason)
	}
}

// The default case is also the one every deployment had before the key existed:
// clients that name no identity all present "", so they keep visiting each
// other's private proxies.
func TestPrivateProxiesWithoutIdentitiesStillAdmitEachOther(t *testing.T) {
	manager := &TunnelManager{cfg: &config.Config{}, policies: &proxyPolicies{}}
	group := newProxyGroup(protocol.ProxySpec{
		Name: "ssh", Type: protocol.ProxyTypeSTCP, SecretKey: "s", AllowUsers: []string{"alice"},
	}, manager)

	if allowed, _ := group.visitorIdentityAllow("alice"); !allowed {
		t.Fatal("a listed identity was refused")
	}
	if allowed, reason := group.visitorIdentityAllow(""); allowed {
		t.Fatal("an unnamed visitor was admitted by a proxy that lists allow_users")
	} else if !strings.Contains(reason, "alice") {
		t.Errorf("the refusal does not name the allowed identity: %s", reason)
	}
}

// An empty allow_users entry is an identity like any other: a proxy published
// by an unnamed client admits unnamed visitors.
func TestAnUnnamedPublisherAdmitsUnnamedVisitors(t *testing.T) {
	manager := &TunnelManager{cfg: &config.Config{}, policies: &proxyPolicies{}}
	group := newProxyGroup(protocol.ProxySpec{Name: "ssh", Type: protocol.ProxyTypeSTCP, SecretKey: "s"}, manager)

	if allowed, reason := group.visitorIdentityAllow(""); !allowed {
		t.Fatalf("two unnamed clients were kept apart: %s", reason)
	}
}
