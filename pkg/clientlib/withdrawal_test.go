package clientlib

import (
	"errors"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// A proxy the client has forgotten locally must not keep a mirrored spec behind
// it: the spec map is what a health check uses to register the same proxy again,
// and re-registering a proxy the operator removed would republish it. Both
// refusal paths — a server that does not support withdrawal, and no control
// connection to withdraw on — used to leave the spec in place.
func TestAWithdrawalThatCannotReachTheServerForgetsTheSpec(t *testing.T) {
	c := &client{}
	c.rememberSpec(protocol.ProxySpec{Name: "gone"})
	c.withdrawUnsupported = true

	if err := c.withdrawProxy(config.ProxyConfig{Name: "gone"}); !errors.Is(err, errWithdrawUnsupported) {
		t.Fatalf("withdrawProxy returned %v, want errWithdrawUnsupported", err)
	}
	if _, ok := c.specFor("gone"); ok {
		t.Fatal("the mirrored spec survived a withdrawal the server does not support")
	}
}

func TestAWithdrawalWithoutAControlConnectionForgetsTheSpec(t *testing.T) {
	c := &client{}
	c.rememberSpec(protocol.ProxySpec{Name: "gone"})

	if err := c.withdrawProxy(config.ProxyConfig{Name: "gone"}); err == nil {
		t.Fatal("withdrawProxy succeeded without a control connection")
	}
	if _, ok := c.specFor("gone"); ok {
		t.Fatal("the mirrored spec survived a withdrawal with no control connection")
	}
}
