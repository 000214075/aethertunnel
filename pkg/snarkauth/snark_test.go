package snarkauth

import (
	"bytes"
	"strings"
	"testing"
)

// The tests are the statements the circuit makes: the right secret verifies, a
// wrong secret does not, the proof is bound to its challenge, and a fiddled
// response is caught.

func TestAProofFromTheRightSecretVerifies(t *testing.T) {
	secret := []byte("s3cret")
	context := []byte("aethertunnel/v3/visitor-proof\x00\x06private\x20\x01\x02...")

	payload, err := Prove(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := Verify(payload, secret, context); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestAProofFromAnotherSecretDoesNotVerify(t *testing.T) {
	context := []byte("aethertunnel/v3/visitor-proof\x00\x06private\x20\x01\x02...")
	payload, err := Prove([]byte("guessing"), context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := Verify(payload, []byte("s3cret"), context); err == nil {
		t.Fatal("a proof of the wrong secret was accepted")
	} else if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestAProofIsBoundToItsChallenge(t *testing.T) {
	secret := []byte("s3cret")
	payload, err := Prove(secret, []byte("context of the first challenge"))
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := Verify(payload, secret, []byte("context of the second challenge")); err == nil {
		t.Fatal("a proof replayed against another challenge was accepted")
	}
}

func TestATamperedResponseIsRefused(t *testing.T) {
	secret := []byte("s3cret")
	context := []byte("aethertunnel/v3/visitor-proof\x00\x06private\x20\x01\x02...")
	payload, err := Prove(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	tampered := bytes.Clone(payload)
	tampered[0] ^= 1
	if err := Verify(tampered, secret, context); err == nil {
		t.Fatal("a tampered response was accepted")
	}
}
