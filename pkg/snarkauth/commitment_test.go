package snarkauth

import (
	"encoding/binary"
	"strings"
	"testing"
)

const (
	commitmentSecret  = "s3cret"
	commitmentContext = "aethertunnel/v3/visitor-proof\x00commitment"
)

// commitmentProof returns a payload whose proof declares one commitment,
// with real bytes around the count: the fixed fields and the 32-byte points the
// decoder reads come from a proof the embedded keys produced, so a decoder that
// is allowed to proceed gets past the encoding.
//
// The proof is longer than minProofBytes by exactly the commitment and the
// count it declares, which is the case the size bound admits and the one this
// test is about: the bound alone would let the decoder read the point and hand
// the proof to the verifier.
func commitmentProof(t *testing.T) []byte {
	t.Helper()
	payload, err := Prove([]byte(commitmentSecret), []byte(commitmentContext))
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	proof := payload[32:]
	if len(proof) != minProofBytes {
		t.Fatalf("the embedded keys produced a %d-byte proof, want the %d-byte encoding of a proof without commitments",
			len(proof), minProofBytes)
	}
	// Ar is a real G1 point of the proof; it stands in for the commitment and
	// for the commitment proof's element, both of which the decoder reads as
	// points. Any point decodes here — the test is about the count, not the
	// values.
	point := proof[:32]
	crafted := make([]byte, 0, 32+len(proof)+32)
	crafted = append(crafted, payload[:32]...)
	crafted = append(crafted, proof[:proofCommitmentCount]...)
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], 1)
	crafted = append(crafted, count[:]...)
	crafted = append(crafted, point...)
	crafted = append(crafted, point...)
	return crafted
}

// This circuit's verifying key has no commitment keys, so a proof that declares
// a commitment is not one it could have produced. gnark's verifier adds every
// decoded commitment to its accumulator before it looks at the key, and it only
// checks the commitment proof itself when the key has commitment keys, so the
// decode the size bound allows is work an authenticated caller can buy. The
// count check in Verify refuses it before the decode.
func TestAProofThatDeclaresACommitmentIsRefusedOnACircuitThatHasNone(t *testing.T) {
	err := Verify(commitmentProof(t), []byte(commitmentSecret), []byte(commitmentContext))
	if err == nil {
		t.Fatal("a proof declaring a commitment was accepted on a circuit whose key has no commitment keys")
	}
	if strings.Contains(err.Error(), "more than its") {
		t.Fatalf("the refusal came from the size bound, which this payload is built to pass: %v", err)
	}
	if !strings.Contains(err.Error(), "carries none") {
		t.Fatalf("the proof was not refused by the commitment check, so it reached the decoder: %v", err)
	}
}
