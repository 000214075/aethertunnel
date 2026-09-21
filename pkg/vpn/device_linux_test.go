//go:build linux

package vpn

import (
	"net"
	"os"
	"strings"
	"testing"
)

// TestTunDeviceTakesAnAddress opens a real interface and reads the address back
// from the kernel.
//
// This is the test a stub cannot replace: the address used to be handed to
// SetInet4Addr as a whole sockaddr_in, which that function rejects because it wants
// the four address bytes and nothing else. The error was discarded, the request went
// to the kernel empty, and SIOCSIFADDR answered EINVAL, so a server with [vpn]
// enabled refused to start on every machine. Only a real device shows that.
func TestTunDeviceTakesAnAddress(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("opening /dev/net/tun needs root")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("/dev/net/tun is not available here: %v", err)
	}

	device, err := Open("", 1400)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer device.Close()

	if err := AssignAddress(device, "10.77.0.1"); err != nil {
		t.Fatalf("AssignAddress: %v", err)
	}

	iface, err := net.InterfaceByName(device.Name())
	if err != nil {
		t.Fatalf("the interface the device reports is not visible to the kernel: %v", err)
	}
	if iface.Flags&net.FlagUp == 0 {
		t.Errorf("%s was given an address but was not brought up (flags %v)", iface.Name, iface.Flags)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		t.Fatalf("read the addresses of %s: %v", iface.Name, err)
	}
	assigned := false
	for _, addr := range addrs {
		if strings.HasPrefix(addr.String(), "10.77.0.1/") {
			assigned = true
		}
	}
	if !assigned {
		t.Errorf("%s carries %v, want 10.77.0.1", iface.Name, addrs)
	}
}

func TestTunDeviceRefusesAddressesItCannotUse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("opening /dev/net/tun needs root")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("/dev/net/tun is not available here: %v", err)
	}

	device, err := Open("", 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer device.Close()

	for _, address := range []string{"", "not-an-address", "2001:db8::1", "10.0.0.256"} {
		if err := AssignAddress(device, address); err == nil {
			t.Errorf("%q was accepted as an address", address)
		}
	}
}
