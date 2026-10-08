//go:build !linux && !windows && !darwin

package vpn

import (
	"fmt"
	"runtime"
)

// openPlatformDevice reports that this platform has no tun device support in this
// build.
//
// Linux uses /dev/net/tun (device_linux.go), Windows uses wintun.dll
// (device_wintun.go), and macOS uses a utun control socket (device_utun.go); each
// has its own file. A platform outside those three has no implementation here.
func openPlatformDevice(name string, mtu int) (Device, error) {
	return nil, fmt.Errorf("%w: %s has no tun implementation in this build", ErrNoDevice, runtime.GOOS)
}
