//go:build !unix

package ledger

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockLedgerFile takes an exclusive lock on the ledger's sidecar lock file.
// Windows releases the LockFileEx range when the handle closes, so Close
// unlocks it the same way flock does on the unix side. The whole lock file is
// locked: the byte range is the largest Overlapped expresses. Locking the
// sidecar keeps the mandatory-on-Windows lock off the ledger itself, whose
// readers must not be blocked while the writer runs.
func lockLedgerFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), ^uint32(0), overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLedgerAlreadyLocked
	}
	return err
}
