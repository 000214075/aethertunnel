// Command snarksetup regenerates the Groth16 keys and constraint system the
// visitor-auth circuit uses, writing them where pkg/snarkauth embeds them.
// Rebuild both ends after running it: the ceremony's trapdoor belongs to
// whoever runs this, which is exactly the trusted-setup caveat the docs carry.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/aethertunnel/aethertunnel/pkg/snarkauth"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
)

func main() {
	ccs, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, snarkauth.NewCircuit())
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}
	pk, vk, err := groth16.Setup(ccs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	write := func(name string, blob interface {
		WriteTo(io.Writer) (int64, error)
	}) {
		f, err := os.Create(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		if _, err := blob.WriteTo(f); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", name)
	}
	write("pkg/snarkauth/r1cs.bin", ccs)
	write("pkg/snarkauth/proving.key", pk)
	write("pkg/snarkauth/verifying.key", vk)
}
