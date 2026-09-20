package vpn

import (
	"fmt"
	"strings"
)

// Addressable is implemented by devices that can be given an address after they
// are open. The address is not known when the device is created: the server's pool
// assigns it once the session is up.
type Addressable interface {
	SetAddress(address string) error
}

// AssignAddress gives a device the address the server handed out.
//
// It returns an error rather than ignoring the case where the device cannot take an
// address, because a device without an address silently receives nothing.
func AssignAddress(device Device, address string) error {
	addressable, ok := device.(Addressable)
	if !ok {
		return fmt.Errorf("vpn: device %s cannot be given an address", device.Name())
	}
	return addressable.SetAddress(address)
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
	if strings.ContainsAny(name, "/ \t\n\x00") {
		return fmt.Errorf("vpn: interface name %q contains a character that is not allowed", name)
	}
	// Linux caps interface names at 15 bytes; a longer one is refused by the kernel
	// with a message that does not say why.
	if len(name) > 15 {
		return fmt.Errorf("vpn: interface name %q is %d bytes, longer than the 15 the kernel accepts", name, len(name))
	}
	return nil
}
