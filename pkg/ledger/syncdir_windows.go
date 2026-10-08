//go:build windows

package ledger

// syncParentDir is a no-op on Windows.
//
// Windows has no directory fsync: FlushFileBuffers on a handle opened on a
// directory (which os.Open uses, read-only) fails, and golang/go#26650 tracks
// that there is no portable replacement. Calling it made New treat every first
// creation of a ledger as a startup error, so the ledger could not be enabled at
// all. The file's own Sync, which New still performs, is everything Windows
// offers for durability here.
func syncParentDir(path string) error {
	return nil
}
