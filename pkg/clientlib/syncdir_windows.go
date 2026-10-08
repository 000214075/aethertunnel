//go:build windows

package clientlib

// syncParentDir is a no-op on Windows.
//
// Windows has no directory fsync: FlushFileBuffers on a handle opened on a
// directory (which os.Open uses, read-only) fails, and golang/go#26650 tracks
// that there is no portable replacement. The store's own file Sync, which
// writeStateLocked still performs, is everything Windows offers for durability
// here.
func syncParentDir(path string) error {
	return nil
}
