package vpn

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// errNilDevice is what AssignAddress returns when it is handed no device at all.
var errNilDevice = errors.New("vpn: a device is required")

// Addressable is implemented by devices that can be given an address after they
// are open. The address is not known when the device is created: the server's pool
// assigns it once the session is up.
type Addressable interface {
	SetAddress(address, netmask string) error
}

// AssignAddress gives a device the address the server handed out, together
// with the subnet mask of the pool. The mask has to be installed with the
// address: without it the kernel falls back to the legacy classful default —
// a /8 for any 10.x address — and every packet the host sends to another
// 10.x network is silently pushed into the tunnel.
//
// It returns an error rather than ignoring the case where the device cannot take an
// address, because a device without an address silently receives nothing.
func AssignAddress(device Device, address, netmask string) error {
	if device == nil {
		// The type assertion below calls Name() in its error path, and a nil
		// interface there is a panic rather than the error this function promises.
		return errNilDevice
	}
	addressable, ok := device.(Addressable)
	if !ok {
		return fmt.Errorf("vpn: device %s cannot be given an address", device.Name())
	}
	return addressable.SetAddress(address, netmask)
}

// Open opens the tun device named by name, using MTU when it is non-zero.
//
// An empty name lets the operating system choose. A non-empty name that is not a
// syntactically usable interface name is rejected before any system call, so a
// configuration typo does not depend on the kernel's error text to be understood.
func Open(name string, mtu int) (Device, error) {
	if mtu != 0 && (mtu < MinMTU || mtu > MaxMTU) {
		return nil, fmt.Errorf("vpn: MTU %d is outside %d-%d", mtu, MinMTU, MaxMTU)
	}
	if err := validateDeviceName(name); err != nil {
		return nil, err
	}
	return openPlatformDevice(name, mtu)
}

// validateDeviceName rejects names the kernel would refuse or that would be
// ambiguous in a log line. An empty name is allowed: it means "choose one".
func validateDeviceName(name string) error {
	if name == "" {
		return nil
	}
	if name == "." || name == ".." {
		// A dot entry is a directory reference, never an interface, and the kernel
		// reports it in a way that does not name the setting it came from.
		return fmt.Errorf("vpn: interface name %q is a directory reference, not an interface", name)
	}
	// A colon is the interface/address separator in the forms this name is pasted
	// into (ip link set dev X, netsh interface), and a carriage return would hide
	// the rest of a log line.
	if strings.ContainsAny(name, "/ \t\n\r\x00:") {
		return fmt.Errorf("vpn: interface name %q contains a character that is not allowed", name)
	}
	// Linux caps interface names at 15 bytes; a longer one is refused by the kernel
	// with a message that does not say why. The other platforms take longer names:
	// a Wintun adapter name is an arbitrary string, and a macOS utun keeps the
	// utunN shape this file checks for.
	if runtime.GOOS == "linux" && len(name) > 15 {
		return fmt.Errorf("vpn: interface name %q is %d bytes, longer than the 15 the kernel accepts", name, len(name))
	}
	return nil
}
