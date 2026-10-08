// Package snarkauth is the zk-SNARK half of a private proxy's visitor auth: a
// visitor that knows the proxy's secret proves that knowledge with a Groth16
// proof instead of a Schnorr signature, and the proof says nothing about the
// secret while being bound to this one challenge.
//
// The circuit makes two public statements. MiMC(secret) equals the commitment
// the server derives from the secret it already holds, and MiMC(secret, H)
// equals the response, where H is a hash of the proof context — the proxy name
// and the server's nonce — so a proof made for one challenge is worthless for
// another. Everything else the visitor does is exactly what a nizk visitor
// does.
//
// The proving and verifying keys are the ones tools/snarksetup generated for
// this circuit, embedded in the binary. They carry the usual Groth16 trusted
// setup caveat: whoever ran the setup could have kept a trapdoor for that
// ceremony. Regenerating the pair with the same tool and rebuilding both ends
// replaces the ceremony with your own.
package snarkauth

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/hash"
	"github.com/consensys/gnark/backend/groth16"
	bn254groth16 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/hash/mimc"
)

//go:embed r1cs.bin
var r1csBin []byte

//go:embed proving.key
var provingKeyPEM []byte

//go:embed verifying.key
var verifyingKeyPEM []byte

// embed carries the generated keys; go:embed wants the files in this package.
var _ = embed.FS{}

// AuthCircuit is the visitor-auth circuit. Secret is the proxy's secret reduced
// into the scalar field; the three public values are the commitment the server
// derives, the challenge binding, and the response.
type AuthCircuit struct {
	Secret     frontend.Variable `gnark:",secret"`
	Commitment frontend.Variable `gnark:",public"`
	Context    frontend.Variable `gnark:",public"`
	Response   frontend.Variable `gnark:",public"`
}

// Define makes the two statements the proof stands for.
func (c *AuthCircuit) Define(api frontend.API) error {
	h, err := mimc.NewMiMC(api)
	if err != nil {
		return err
	}
	h.Write(c.Secret)
	api.AssertIsEqual(h.Sum(), c.Commitment)
	h.Reset()
	h.Write(c.Secret)
	h.Write(c.Context)
	api.AssertIsEqual(h.Sum(), c.Response)
	return nil
}

// NewCircuit is the compile-time shape of AuthCircuit, for tools/snarksetup,
// which must compile exactly what this package embeds keys for.
func NewCircuit() frontend.Circuit { return &AuthCircuit{} }

var (
	keysOnce  sync.Once
	system    constraint.ConstraintSystem
	proving   groth16.ProvingKey
	verifying groth16.VerifyingKey
	keysErr   error
)

func keys() (constraint.ConstraintSystem, groth16.ProvingKey, groth16.VerifyingKey, error) {
	keysOnce.Do(func() {
		cs := groth16.NewCS(ecc.BN254)
		if _, err := cs.ReadFrom(bytes.NewReader(r1csBin)); err != nil {
			keysErr = fmt.Errorf("snarkauth: decode the constraint system: %w", err)
			return
		}
		pk := groth16.NewProvingKey(ecc.BN254)
		if _, err := pk.ReadFrom(bytes.NewReader(provingKeyPEM)); err != nil {
			keysErr = fmt.Errorf("snarkauth: decode the proving key: %w", err)
			return
		}
		vk := groth16.NewVerifyingKey(ecc.BN254)
		if _, err := vk.ReadFrom(bytes.NewReader(verifyingKeyPEM)); err != nil {
			keysErr = fmt.Errorf("snarkauth: decode the verifying key: %w", err)
			return
		}
		system, proving, verifying = cs, pk, vk
	})
	return system, proving, verifying, keysErr
}

// hashContext is the out-of-circuit hash of the proof context. It is a public
// input, so any deterministic hash would do; SHA-256 keeps the circuit's MiMC
// for the two statements that need it in-circuit.
func hashContext(context []byte) []byte {
	sum := sha256.Sum256(context)
	return sum[:]
}

// mimcSum is MiMC over canonical field-element bytes, matching what the circuit
// computes on the same values.
func mimcSum(parts ...[]byte) []byte {
	h := hash.MIMC_BN254.New()
	for _, part := range parts {
		h.Write(part)
	}
	return h.Sum(nil)
}

// Prove answers a challenge: it returns the response and the Groth16 proof for
// the secret, bound to the proof context.
func Prove(secret []byte, context []byte) ([]byte, error) {
	system, pk, _, err := keys()
	if err != nil {
		return nil, err
	}

	x := new(fr.Element).SetBytes(secret)
	xb := canonical(x)
	hc := new(fr.Element).SetBytes(hashContext(context))
	commitment := mimcSum(xb)
	response := mimcSum(xb, canonical(hc))

	assignment := &AuthCircuit{
		Secret:     x,
		Commitment: new(fr.Element).SetBytes(commitment),
		Context:    hc,
		Response:   new(fr.Element).SetBytes(response),
	}
	witness, err := frontend.NewWitness(assignment, ecc.BN254.ScalarField())
	if err != nil {
		return nil, fmt.Errorf("snarkauth: build the witness: %w", err)
	}
	proof, err := groth16.Prove(system, pk, witness)
	if err != nil {
		return nil, fmt.Errorf("snarkauth: prove: %w", err)
	}
	var proofBytes bytes.Buffer
	if _, err := proof.WriteTo(&proofBytes); err != nil {
		return nil, fmt.Errorf("snarkauth: encode the proof: %w", err)
	}

	payload := make([]byte, 0, len(response)+proofBytes.Len())
	payload = append(payload, response...)
	payload = append(payload, proofBytes.Bytes()...)
	return payload, nil
}

// Verify checks a Prove payload against the secret the server holds and the
// context the challenge carried. A proof made for another proxy, another
// challenge or another secret fails here.
func Verify(payload, secret []byte, context []byte) error {
	_, _, vk, err := keys()
	if err != nil {
		return err
	}
	if len(payload) < 32 {
		return errors.New("snarkauth: the proof payload is too short for a response")
	}
	response, proofBytes := payload[:32], payload[32:]

	// A proof's encoding is the fixed fields followed by a list whose length the
	// decoder reads before it allocates anything: Ar (32 bytes), Bs (64), Krs
	// (32), a 4-byte commitment count, that many 32-byte commitments, then the
	// 32-byte commitment proof. gnark-crypto allocates the slice from the count
	// first, so a count the encoder never wrote is an allocation the sender
	// picks: 0xFFFFFFFF asks for 2^32 G1Affine points — 274 GB — and the runtime
	// answers with a fatal "out of memory" that no recover catches, taking every
	// tunnel of the process down with it. The frame is 196 bytes either way.
	//
	// The count therefore may not exceed what the bytes left in the proof can
	// describe. A proof the embedded keys produced carries no commitments at all
	// (the circuit has none), and a legal one of any generation has exactly as
	// many as its length allows, so this bound refuses no honest proof. Who can
	// send it: a client that holds the server's shared auth_token, or a valid
	// OIDC token, reaches this decoder without the private proxy's secret_key.
	if len(proofBytes) < minProofBytes {
		return fmt.Errorf("snarkauth: the proof is %d bytes, shorter than the %d the encoding needs",
			len(proofBytes), minProofBytes)
	}
	count := binary.BigEndian.Uint32(proofBytes[proofCommitmentCount:])
	if count > maxProofCommitments(len(proofBytes)) {
		return fmt.Errorf("snarkauth: the proof declares %d commitment(s), more than its %d bytes can hold",
			count, len(proofBytes))
	}
	// The size bound above still lets a frame name a count that its bytes can
	// describe, and decoding that many points is what costs: gnark's verifier
	// adds every decoded commitment to its accumulator before it looks at the
	// key, and it only checks the commitment proof itself when the key has
	// commitment keys (backend/groth16/bn254/verify.go). This circuit's key has
	// none, so a proof that declares even one commitment is not one this
	// circuit could have produced; refusing it before ReadFrom keeps the
	// decode off the path a caller with an auth token can buy.
	if count != 0 && !verifyingKeyHasCommitments(vk) {
		return fmt.Errorf("snarkauth: the proof declares %d commitment(s), but this circuit's proof carries none",
			count)
	}

	x := new(fr.Element).SetBytes(secret)
	hc := new(fr.Element).SetBytes(hashContext(context))
	public := &AuthCircuit{
		Commitment: new(fr.Element).SetBytes(mimcSum(canonical(x))),
		Context:    hc,
		Response:   new(fr.Element).SetBytes(response),
	}
	publicWitness, err := frontend.NewWitness(public, ecc.BN254.ScalarField(), frontend.PublicOnly())
	if err != nil {
		return fmt.Errorf("snarkauth: build the public witness: %w", err)
	}
	proof := groth16.NewProof(ecc.BN254)
	if _, err := proof.ReadFrom(bytes.NewReader(proofBytes)); err != nil {
		return fmt.Errorf("snarkauth: decode the proof: %w", err)
	}
	if err := groth16.Verify(proof, vk, publicWitness); err != nil {
		return fmt.Errorf("snarkauth: the proof does not verify: %w", err)
	}
	return nil
}

// The Groth16 proof encoding, as gnark writes it: Ar (32 bytes), Bs (64), Krs
// (32), the big-endian commitment count, that many 32-byte commitments, then
// the commitment proof (32). The fixed part plus the count and the trailing
// proof element is minProofBytes.
const (
	proofCommitmentCount = 128
	minProofBytes        = 164
)

// maxProofCommitments is the largest commitment count a proof of len bytes can
// describe: the remaining space after the fixed part, one 32-byte point each.
// It is computed from the bytes present rather than from a compile-time
// constant so a future circuit that really commits still decodes, while the
// allocation stays within a constant factor of the message.
func maxProofCommitments(proofLen int) uint32 {
	if proofLen < minProofBytes {
		return 0
	}
	return uint32((proofLen - minProofBytes) / 32)
}

// verifyingKeyHasCommitments reports whether a verifying key carries commitment
// keys. The groth16.VerifyingKey interface does not expose them, so the check is
// made on the concrete BN254 key this package embeds; a key that is not that
// type is reported as carrying commitments, which leaves the count check above
// as the only bound rather than refusing a proof on a guess.
func verifyingKeyHasCommitments(vk groth16.VerifyingKey) bool {
	bn, ok := vk.(*bn254groth16.VerifyingKey)
	if !ok {
		return true
	}
	return len(bn.CommitmentKeys) > 0
}

// canonical is the fixed-width byte form of a field element, the input shape
// the circuit's MiMC sees.
func canonical(x *fr.Element) []byte {
	b := x.Bytes()
	return b[:]
}
