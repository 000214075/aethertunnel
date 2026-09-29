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
	"errors"
	"fmt"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/hash"
	"github.com/consensys/gnark/backend/groth16"
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

// contextField reduces the proof context into a scalar, the value the circuit
// hashes the secret against.
func contextField(context []byte) *fr.Element {
	return new(fr.Element).SetBytes(hashContext(context))
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

// canonical is the fixed-width byte form of a field element, the input shape
// the circuit's MiMC sees.
func canonical(x *fr.Element) []byte {
	b := x.Bytes()
	return b[:]
}
