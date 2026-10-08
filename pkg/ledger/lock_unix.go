//go:build unix

package ledger

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockLedgerFile takes an exclusive advisory lock on the ledger's sidecar lock
// file. The lock belongs to the open file description, so it is released when
// the descriptor closes: the Ledger keeps the handle for its whole life and
// Close releases the lock with it. The lock is advisory, which is all this
// needs — the second instance is another copy of this program.
func lockLedgerFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errLedgerAlreadyLocked
	}
	return err
}
