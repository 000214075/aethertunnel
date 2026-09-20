//go:build !linux

package vpn

import (
	"fmt"
	"runtime"
)

// openPlatformDevice reports that this platform has no tun device support in this
// build.
//
// The message names what is actually missing rather than a generic failure, because
// the fix differs per platform: Linux uses /dev/net/tun and needs no extra software,
// Windows needs the Wintun driver or a TAP-Windows adapter plus an interface to
// drive it, and macOS has a utun interface whose kernel control socket is not
// implemented here yet.
func openPlatformDevice(name string, mtu int) (Device, error) {
	return nil, fmt.Errorf("%w: %s has no tun implementation in this build%s",
		ErrNoDevice, runtime.GOOS, platformHint(runtime.GOOS))
}

// platformHint explains what a platform would need.
func platformHint(goos string) string {
	switch goos {
	case "windows":
		return "; a tun device there needs the Wintun driver, and this program does not install drivers"
	case "darwin":
		return "; macOS uses a utun control socket, which this build does not open"
	default:
		return ""
	}
}
