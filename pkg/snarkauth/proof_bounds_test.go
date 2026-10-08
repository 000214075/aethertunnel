package snarkauth

import (
	"encoding/binary"
	"strings"
	"testing"
)

// The proof encoding carries one length a decoder reads before it allocates
// anything: the big-endian commitment count at proofCommitmentCount. That makes
// it the only field an attacker can turn into an allocation of their choosing,
// and the assertions below are about the bound that keeps it within the bytes
// that are actually there.

const (
	proofSecret  = "s3cret"
	proofContext = "aethertunnel/v3/visitor-proof\x00proof"
)

// craftedProof returns a proof the embedded keys really produced, with its
// commitment count replaced. The bytes around the count stay the ones an honest
// prover wrote, so nothing else about the encoding is malformed.
func craftedProof(t *testing.T, count uint32) []byte {
	t.Helper()
	payload, err := Prove([]byte(proofSecret), []byte(proofContext))
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	proofBytes := payload[32:]
	if len(proofBytes) != minProofBytes {
		t.Fatalf("the embedded keys produced a %d-byte proof, want the %d-byte encoding of a proof without commitments",
			len(proofBytes), minProofBytes)
	}
	binary.BigEndian.PutUint32(proofBytes[proofCommitmentCount:], count)
	return payload
}

// A proof whose count is one more than its bytes can carry is refused by the
// bound. The count is deliberately small: with the bound reverted the decoder
// allocates that one point and then fails on the bytes that are not there, so
// this test can run in the test process either way.
func TestAProofThatDeclaresMoreCommitmentsThanItHoldsIsRefusedByTheBound(t *testing.T) {
	err := Verify(craftedProof(t, 1), []byte(proofSecret), []byte(proofContext))
	if err == nil {
		t.Fatal("a proof declaring a commitment its bytes do not carry was accepted")
	}
	if !strings.Contains(err.Error(), "declares 1 commitment") {
		t.Fatalf("the refusal does not name the declared count, so it came from the decoder: %v", err)
	}
}
