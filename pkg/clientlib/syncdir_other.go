//go:build !windows

package clientlib

import (
	"os"
	"path/filepath"
)

// syncParentDir flushes the directory entry of a file this process renamed into
// place, so the name that points at the inode survives a crash as the inode does.
func syncParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
