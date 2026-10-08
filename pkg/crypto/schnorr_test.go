package crypto

import (
	"bytes"
	"testing"
)

func TestSchnorrProofRoundTrip(t *testing.T) {
	secret := []byte("correct horse battery staple")
	context := []byte("proxy=private-web|nonce=0123456789abcdef")

	public, err := SchnorrPublicKey(secret)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	if len(public) != SchnorrPublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(public), SchnorrPublicKeySize)
	}

	proof, err := SchnorrProve(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if len(proof) != SchnorrProofSize {
		t.Fatalf("proof is %d bytes, want %d", len(proof), SchnorrProofSize)
	}
	if err := SchnorrVerify(public, context, proof); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSchnorrProofDoesNotRevealTheSecret(t *testing.T) {
	secret := []byte("s3cret-proxy-key")
	context := []byte("context")

	public, err := SchnorrPublicKey(secret)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	proof, err := SchnorrProve(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if bytes.Contains(public, secret) || bytes.Contains(proof, secret) {
		t.Fatal("the secret appears verbatim in the public key or the proof")
	}
}

func TestSchnorrProofIsBoundToItsContext(t *testing.T) {
	secret := []byte("shared-secret")
	public, err := SchnorrPublicKey(secret)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}

	proof, err := SchnorrProve(secret, []byte("proxy=web|nonce=aaaaaaaa"))
	if err != nil {
		t.Fatalf("prove: %v", err)
	}

	cases := map[string][]byte{
		"different nonce": []byte("proxy=web|nonce=bbbbbbbb"),
		"different proxy": []byte("proxy=other|nonce=aaaaaaaa"),
		"empty context":   nil,
	}
	for name, context := range cases {
		if err := SchnorrVerify(public, context, proof); err == nil {
			t.Fatalf("the proof verified under the %s context", name)
		}
	}
}

func TestSchnorrRejectsWrongSecretAndBrokenProofs(t *testing.T) {
	secret := []byte("the-real-secret")
	context := []byte("context")

	public, err := SchnorrPublicKey(secret)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	proof, err := SchnorrProve(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}

	otherPublic, err := SchnorrPublicKey([]byte("a-different-secret"))
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	if err := SchnorrVerify(otherPublic, context, proof); err == nil {
		t.Fatal("a proof for one secret verified against another secret's public key")
	}

	for i := range proof {
		broken := append([]byte(nil), proof...)
		broken[i] ^= 0x80
		if err := SchnorrVerify(public, context, broken); err == nil {
			t.Fatalf("a proof with byte %d flipped verified", i)
		}
	}

	if err := SchnorrVerify(public, context, proof[:10]); err == nil {
		t.Fatal("a truncated proof verified")
	}
	if err := SchnorrVerify(public[:10], context, proof); err == nil {
		t.Fatal("a truncated public key verified")
	}
	notAPoint := append([]byte(nil), public...)
	notAPoint[1] ^= 0xff
	if err := SchnorrVerify(notAPoint, context, proof); err == nil {
		t.Fatal("a public key that is not on the curve verified")
	}
}

func TestSchnorrRejectsAnEmptySecret(t *testing.T) {
	if _, err := SchnorrPublicKey(nil); err == nil {
		t.Fatal("an empty secret produced a public key")
	}
	if _, err := SchnorrProve(nil, []byte("context")); err == nil {
		t.Fatal("an empty secret produced a proof")
	}
}

func TestSchnorrIsNonDeterministic(t *testing.T) {
	secret := []byte("secret")
	context := []byte("context")

	first, err := SchnorrProve(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	second, err := SchnorrProve(secret, context)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two proofs for the same statement are identical, so the nonce is not random")
	}

	public, err := SchnorrPublicKey(secret)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	if err := SchnorrVerify(public, context, second); err != nil {
		t.Fatalf("the second proof did not verify: %v", err)
	}
}
