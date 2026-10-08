// Command snarksetup regenerates the Groth16 keys and constraint system the
// visitor-auth circuit uses, writing them where pkg/snarkauth embeds them.
// Rebuild both ends after running it: the ceremony's trapdoor belongs to
// whoever runs this, which is exactly the trusted-setup caveat the docs carry.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aethertunnel/aethertunnel/pkg/snarkauth"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
)

// staged is one of the three files of a generation: where it belongs, the
// value to write, and the temporary name it is staged under.
type staged struct {
	name string
	blob interface {
		WriteTo(io.Writer) (int64, error)
	}
	temp string
}

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
	// The three files are one generation: an interrupted run must leave the
	// previous set whole, because r1cs.bin from one setup and keys from
	// another still decode cleanly but proofs made against the new circuit
	// never verify against the old verifying key. Everything is staged under
	// temporary names and renamed into place only after all three writes and
	// their syncs succeeded.
	files := []staged{
		{"pkg/snarkauth/r1cs.bin", ccs, ""},
		{"pkg/snarkauth/proving.key", pk, ""},
		{"pkg/snarkauth/verifying.key", vk, ""},
	}
	// A failed run must not leave its half-written staging files behind:
	// os.Exit skips every deferred cleanup, so each error path removes what
	// has been staged so far before it exits.
	cleanup := func() {
		for _, f := range files {
			if f.temp != "" {
				_ = os.Remove(f.temp)
			}
		}
	}
	fail := func(what string, err error) {
		cleanup()
		fmt.Fprintln(os.Stderr, what+":", err)
		os.Exit(1)
	}
	for i := range files {
		temp, err := os.CreateTemp(filepath.Dir(files[i].name), ".snarksetup-*")
		if err != nil {
			fail("create", err)
		}
		files[i].temp = temp.Name()
		if _, err := files[i].blob.WriteTo(temp); err != nil {
			_ = temp.Close()
			fail("write", err)
		}
		if err := temp.Sync(); err != nil {
			_ = temp.Close()
			fail("sync", err)
		}
		if err := temp.Close(); err != nil {
			fail("close", err)
		}
	}
	if err := commit(files); err != nil {
		// fail's cleanup removes the staging files that are left; the ones
		// already renamed into place were put back by commit's rollback.
		fail("commit", err)
	}
	for _, f := range files {
		fmt.Println("wrote", f.name)
	}
}

// commit renames the staged files into place. Renames are individually atomic
// but the set is not: if the second one fails after the first succeeded, the
// tree would hold r1cs.bin from one generation with keys from another — the
// mixed generation the staging exists to prevent. So each file's previous
// content is moved aside first, and put back whole when any rename fails.
func commit(files []staged) error {
	backups := make([]string, len(files))
	placed := make([]bool, len(files))
	for i := range files {
		if _, err := os.Lstat(files[i].name); err != nil {
			if !os.IsNotExist(err) {
				return rollback(files, backups, placed, fmt.Errorf("look at %s: %w", files[i].name, err))
			}
			continue
		}
		backup := files[i].temp + ".prev"
		if err := os.Rename(files[i].name, backup); err != nil {
			return rollback(files, backups, placed, fmt.Errorf("back up %s: %w", files[i].name, err))
		}
		backups[i] = backup
	}
	for i := range files {
		if err := os.Rename(files[i].temp, files[i].name); err != nil {
			return rollback(files, backups, placed, fmt.Errorf("rename %s: %w", files[i].name, err))
		}
		files[i].temp = ""
		placed[i] = true
	}
	for _, backup := range backups {
		if backup != "" {
			_ = os.Remove(backup)
		}
	}
	return nil
}

// rollback puts the previous generation back and removes the staging files,
// then hands back the cause. placed marks the files the second loop already
// renamed into place: an index whose file did not exist before this run has no
// backup to restore, so the new file that rename left there is removed instead —
// keeping it is what mixes one generation's r1cs.bin with another's keys.
// Restores are best effort: a rollback that cannot run leaves the leftover under
// the ".prev" name rather than throwing away a file it could not move.
func rollback(files []staged, backups []string, placed []bool, cause error) error {
	for i := range backups {
		if !placed[i] {
			// The file was moved aside but never replaced, so the old content
			// is still in the backup and only has to go back.
			if backups[i] != "" {
				if err := os.Rename(backups[i], files[i].name); err == nil {
					backups[i] = ""
				}
			}
			continue
		}
		if backups[i] == "" {
			_ = os.Remove(files[i].name)
			continue
		}
		if err := os.Rename(backups[i], files[i].name); err == nil {
			backups[i] = ""
		}
	}
	for _, f := range files {
		if f.temp != "" {
			_ = os.Remove(f.temp)
		}
	}
	return cause
}
