package vpn

import (
	"errors"
	"testing"
)

// The type assertion in AssignAddress calls Name() in its error path, and a nil
// interface there is a panic rather than the error the function promises a caller.
func TestAssignAddressRefusesANilDevice(t *testing.T) {
	err := AssignAddress(nil, "10.0.0.2", "255.255.255.0")
	if err == nil {
		t.Fatal("AssignAddress accepted a nil device")
	}
	if !errors.Is(err, errNilDevice) {
		t.Fatalf("AssignAddress(nil) = %v, want the nil-device error", err)
	}
}

// A name the kernel refuses, or that reads as something else in a log line or in
// a command the operator pastes it into, is rejected before any system call.
func TestDeviceNamesThatWouldBeAmbiguousAreRefused(t *testing.T) {
	for _, name := range []string{".", "..", "tun0:1", "tun\r0"} {
		if err := validateDeviceName(name); err == nil {
			t.Fatalf("validateDeviceName(%q) accepted the name", name)
		}
	}
}
