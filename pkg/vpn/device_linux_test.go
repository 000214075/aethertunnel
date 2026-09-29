//go:build linux

package vpn

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// TestAMissingDeviceIsNamedForTheOperator drives the real open path against a path
// that does not exist, which is what a container without the device hits.
func TestAMissingDeviceIsNamedForTheOperator(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "tun")
	_, err := openTunDevice(absent, "aetvpn0", 0)
	if err == nil {
		t.Fatal("opening a path that does not exist succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failure does not wrap the errno: %v", err)
	}
	if !strings.Contains(err.Error(), "--device /dev/net/tun") {
		t.Errorf("a container that was never given the device is not told how to pass it: %v", err)
	}
}

// TestADeviceTheProcessMayNotOpenIsNamedForTheOperator covers the other errno the
// open path explains: a device that is present but that this process may not open.
func TestADeviceTheProcessMayNotOpenIsNamedForTheOperator(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a file that carries no permissions, so this case needs a non-root uid")
	}
	path := filepath.Join(t.TempDir(), "tun")
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	_, err := openTunDevice(path, "aetvpn0", 0)
	if err == nil {
		t.Fatal("opening a file with no permissions succeeded")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("the failure does not wrap the errno: %v", err)
	}
	if !strings.Contains(err.Error(), "CAP_NET_ADMIN") {
		t.Errorf("a device this process may not open does not name the capability: %v", err)
	}
}

// The failures a container produces reach the operator as a bare errno, and an
// operator looking at "operation not permitted" from a pod has nothing to act on.
// These pin the setting each failure names.
func TestTunOpenErrorNamesWhatAContainerHasToChange(t *testing.T) {
	missing := tunOpenError("/dev/net/tun", 1000, fs.ErrNotExist).Error()
	if !strings.Contains(missing, "--device /dev/net/tun") {
		t.Errorf("a missing device does not name the flag that passes it: %s", missing)
	}

	denied := tunOpenError("/dev/net/tun", 1000, unix.EPERM).Error()
	if !strings.Contains(denied, "CAP_NET_ADMIN") {
		t.Errorf("a device the process may not open does not name the capability: %s", denied)
	}
	if !strings.Contains(denied, "1000") {
		t.Errorf("the hint does not name the uid it saw: %s", denied)
	}

	// An errno that is not about the device or the capability must not be answered
	// with either hint, which would point the operator at the wrong setting.
	other := tunOpenError("/dev/net/tun", 1000, unix.EMFILE).Error()
	if strings.Contains(other, "CAP_NET_ADMIN") || strings.Contains(other, "--device") {
		t.Errorf("a failure that is not a container setting was given a container hint: %s", other)
	}
}

func TestTunIoctlErrorNamesTheCapabilityAndTheUidItSaw(t *testing.T) {
	asRoot := tunIoctlError("aetvpn0", 0, unix.EPERM).Error()
	if !strings.Contains(asRoot, "CAP_NET_ADMIN") {
		t.Errorf("a refused TUNSETIFF does not name the capability: %s", asRoot)
	}

	asUser := tunIoctlError("aetvpn0", 1000, unix.EPERM).Error()
	if !strings.Contains(asUser, "CAP_NET_ADMIN") || !strings.Contains(asUser, "1000") {
		t.Errorf("the hint does not name the uid it saw, which is the case a container hits: %s", asUser)
	}
	if asRoot == asUser {
		t.Error("uid 0 and a non-root uid get the same hint, so it cannot tell an operator which of the two they are in")
	}

	// EINVAL is a malformed request rather than a permission failure.
	if got := tunIoctlError("aetvpn0", 1000, unix.EINVAL).Error(); strings.Contains(got, "CAP_NET_ADMIN") {
		t.Errorf("a request the kernel rejected for its contents was given a capability hint: %s", got)
	}
}

// TestARefusedDeviceReachesTheOperatorWithTheHint goes through the real call. On an
// unprivileged machine that is the same failure a container without the capability
// produces, which is the one this message exists for.
func TestARefusedDeviceReachesTheOperatorWithTheHint(t *testing.T) {
	device, err := Open("", 0)
	if err == nil {
		_ = device.Close()
		t.Skip("this process can open a tun device, so it cannot see the permission path")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Skipf("the device failed for a reason other than permission: %v", err)
	}
	if !strings.Contains(err.Error(), "CAP_NET_ADMIN") {
		t.Errorf("a process that cannot open a tun device is not told what it is missing: %v", err)
	}
}
