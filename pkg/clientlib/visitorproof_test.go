package clientlib

import (
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/snarkauth"
)

// visitorProof picks the proof the visitor's auth_method calls for, and the server
// picks the verifier the same way. Both directions are asserted here because the
// wire carries an opaque byte string: a visitor that sent the wrong one would look
// to the server exactly like a visitor with the wrong secret, which is a bug report
// nobody can act on.
func TestVisitorProofFollowsTheAuthMethod(t *testing.T) {
	nonce := []byte("nonce-one")
	context := protocol.VisitorProofContext("private", nonce)

	nizk, err := visitorProof(config.VisitorConfig{
		ServerName: "private", SecretKey: "s3cret", AuthMethod: config.AuthMethodNIZK,
	}, nonce)
	if err != nil {
		t.Fatalf("nizk proof: %v", err)
	}
	public, err := crypto.SchnorrPublicKey([]byte("s3cret"))
	if err != nil {
		t.Fatalf("derive the public key: %v", err)
	}
	if err := crypto.SchnorrVerify(public, context, nizk); err != nil {
		t.Errorf("the nizk proof does not verify as a Schnorr proof: %v", err)
	}
	if err := snarkauth.Verify(nizk, []byte("s3cret"), context); err == nil {
		t.Error("a Schnorr proof verified as a Groth16 one")
	}

	snark, err := visitorProof(config.VisitorConfig{
		ServerName: "private", SecretKey: "s3cret", AuthMethod: config.AuthMethodSNARK,
	}, nonce)
	if err != nil {
		t.Fatalf("snark proof: %v", err)
	}
	if err := snarkauth.Verify(snark, []byte("s3cret"), context); err != nil {
		t.Errorf("the snark proof does not verify as a Groth16 one: %v", err)
	}
	if err := crypto.SchnorrVerify(public, context, snark); err == nil {
		t.Error("a Groth16 proof verified as a Schnorr one")
	}
}

// The context is the proxy name and the server's nonce, so a proof answers this
// challenge for this proxy and nothing else. The client is the end that builds it,
// so this is where a context that left the nonce out would show up.
func TestVisitorProofIsBoundToTheProxyAndTheChallenge(t *testing.T) {
	cfg := config.VisitorConfig{
		ServerName: "private", SecretKey: "s3cret", AuthMethod: config.AuthMethodSNARK,
	}
	proof, err := visitorProof(cfg, []byte("nonce-one"))
	if err != nil {
		t.Fatalf("snark proof: %v", err)
	}

	other := map[string][]byte{
		"another challenge": protocol.VisitorProofContext("private", []byte("nonce-two")),
		"another proxy":     protocol.VisitorProofContext("elsewhere", []byte("nonce-one")),
	}
	for what, context := range other {
		if err := snarkauth.Verify(proof, []byte("s3cret"), context); err == nil {
			t.Errorf("a proof made for one challenge verified against %s", what)
		}
	}
	if err := snarkauth.Verify(proof, []byte("other-secret"), protocol.VisitorProofContext("private", []byte("nonce-one"))); err == nil {
		t.Error("a proof verified against a secret the visitor does not hold")
	}
}
