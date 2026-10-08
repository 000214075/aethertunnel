//go:build unix

package vpn

import "golang.org/x/sys/unix"

// setNonblockFD makes a descriptor non-blocking, which is what puts it in the Go
// runtime's poller: os.NewFile only registers a descriptor that is already
// non-blocking, and a descriptor outside the poller has a Read that Close cannot
// interrupt. A platform shell hands over the tun descriptor as it created it,
// which is normally blocking.
func setNonblockFD(fd uintptr) error {
	return unix.SetNonblock(int(fd), true)
}
