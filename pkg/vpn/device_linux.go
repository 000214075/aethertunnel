//go:build linux

package vpn

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// tunDevice is a Linux tun interface opened from /dev/net/tun.
//
// The interface is created without the packet-information prefix (IFF_NO_PI), so
// every read returns exactly one IP packet and every write sends one. Routes are
// deliberately left alone: adding one is a decision about the host's whole network
// setup, and doing it here would silently take over the routing table.
type tunDevice struct {
	file *os.File
	name string
	mtu  int
}

// openPlatformDevice opens or creates the named tun interface. An empty name asks
// the kernel for the next available name (tun0, tun1, ...).
func openPlatformDevice(name string, mtu int) (Device, error) {
	return openTunDevice("/dev/net/tun", name, mtu)
}

// openTunDevice opens path and configures the named interface on it.
//
// The path is a parameter so that the failure a container produces can be exercised
// without a container: a machine that has /dev/net/tun cannot otherwise reach the
// branch that explains a device it was never given.
func openTunDevice(path, name string, mtu int) (Device, error) {
	// O_NONBLOCK is what makes the descriptor pollable to os.NewFile: a
	// blocking descriptor never enters the runtime poller, so a Read parked on it
	// is not woken by Close and the tunnel's teardown hangs forever.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, tunOpenError(path, os.Geteuid(), err)
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vpn: interface name %q is not usable: %w", name, err)
	}
	// IFF_TUN is a layer-3 interface; IFF_NO_PI drops the 4-byte header the kernel
	// would otherwise prepend to every packet.
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return nil, tunIoctlError(name, os.Geteuid(), err)
	}

	// The kernel chooses the name when the request was empty, so it is read back
	// out of the same request structure.
	actual := ifr.Name()
	if actual == "" {
		actual = name
	}

	if mtu > 0 {
		if err := setInterfaceMTU(actual, mtu); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}

	effective := mtu
	if readBack := interfaceMTU(actual); readBack > 0 {
		effective = readBack
	}
	return &tunDevice{file: os.NewFile(uintptr(fd), "/dev/net/tun"), name: actual, mtu: effective}, nil
}

// tunOpenError describes a failure to open /dev/net/tun.
//
// A container that was never given the device reports ENOENT, which reads like a
// missing kernel module, and one whose device cgroup refuses it reports EPERM. Both
// are the container's configuration, so the message names the setting to change
// rather than leaving an operator with the errno. An errno that says nothing about
// either gets no hint, which would send the operator to the wrong place.
func tunOpenError(path string, uid int, err error) error {
	var hint string
	switch {
	case errors.Is(err, os.ErrNotExist):
		hint = "; a container has to be given the device: --device /dev/net/tun in docker, a hostPath CharDevice volume in Kubernetes"
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		hint = tunCapabilityHint(uid)
	}
	return fmt.Errorf("vpn: open %s: %w%s", path, err, hint)
}

// tunIoctlError describes a TUNSETIFF the kernel refused.
func tunIoctlError(name string, uid int, err error) error {
	var hint string
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		hint = tunCapabilityHint(uid)
	}
	return fmt.Errorf("vpn: TUNSETIFF for %q: %w%s", name, err, hint)
}

// tunCapabilityHint names what grants the capability TUNSETIFF needs, and, for a
// process that is not uid 0, the reason adding it to a container is not enough.
//
// The kernel clears the permitted and effective sets when a process becomes a
// non-root user, and the container runtimes do not add the capability to the ambient
// set, so a capability the container was told to add never reaches such a process.
// Measured with docker 20.10.24, 28.4.0 and 29.2.1: `--user 65532:65532 --cap-add
// NET_ADMIN --device /dev/net/tun` answered EPERM, while the same run as uid 0 opened
// the device. Kubernetes reaches the same kernel through the same runc, so the hint
// names the uid it saw rather than only the capability.
func tunCapabilityHint(uid int) string {
	const remedy = "--cap-add NET_ADMIN in docker, securityContext.capabilities in Kubernetes"
	if uid == 0 {
		return "; opening a tun device needs CAP_NET_ADMIN (" + remedy + ")"
	}
	return fmt.Sprintf("; opening a tun device needs CAP_NET_ADMIN (%s), and a process whose uid is %d rather than 0 does not receive a capability the container adds, so run the tunnel as uid 0", remedy, uid)
}

// setInterfaceMTU sets the interface MTU with SIOCSIFMTU.
func setInterfaceMTU(name string, mtu int) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vpn: open a control socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("vpn: interface name %q is not usable: %w", name, err)
	}
	ifr.SetUint32(uint32(mtu))
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFMTU, ifr); err != nil {
		return fmt.Errorf("vpn: setting the MTU of %s to %d: %w", name, mtu, err)
	}
	return nil
}

// interfaceMTU reads the interface MTU back, so the tunnel uses what the kernel
// accepted rather than what was requested.
func interfaceMTU(name string) int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return 0
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFMTU, ifr); err != nil {
		return 0
	}
	return int(ifr.Uint32())
}

// Name is the interface name.
func (d *tunDevice) Name() string { return d.name }

// MTU is the interface MTU.
func (d *tunDevice) MTU() int { return d.mtu }

// Read returns one IP packet.
func (d *tunDevice) Read(packet []byte) (int, error) { return d.file.Read(packet) }

// Write sends one IP packet.
func (d *tunDevice) Write(packet []byte) (int, error) { return d.file.Write(packet) }

// Close closes the file, which destroys the interface the process created.
func (d *tunDevice) Close() error { return d.file.Close() }

// SetAddress assigns the address the server handed out together with the
// pool's netmask and brings the interface up. The mask goes on explicitly:
// SIOCSIFADDR alone leaves the kernel to pick the legacy classful default,
// and a 10.x address would then silently own a /8 of host routes.
func (d *tunDevice) SetAddress(address, netmask string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vpn: open a control socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("vpn: %q is not an IPv4 address", address)
	}
	mask := net.ParseIP(netmask)
	if mask == nil || mask.To4() == nil {
		return fmt.Errorf("vpn: %q is not an IPv4 netmask", netmask)
	}

	addr, err := unix.NewIfreq(d.name)
	if err != nil {
		return fmt.Errorf("vpn: interface name %q is not usable: %w", d.name, err)
	}
	// SetInet4Addr takes the four address bytes and builds the sockaddr_in around
	// them, family included. Handing it a whole sockaddr_in leaves the union
	// empty, and SIOCSIFADDR then rejects it with EINVAL.
	if err := addr.SetInet4Addr(ip.To4()); err != nil {
		return fmt.Errorf("vpn: %s is not a usable address: %w", address, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, addr); err != nil {
		return fmt.Errorf("vpn: assigning %s to %s: %w", address, d.name, err)
	}

	maskReq, err := unix.NewIfreq(d.name)
	if err != nil {
		return fmt.Errorf("vpn: interface name %q is not usable: %w", d.name, err)
	}
	if err := maskReq.SetInet4Addr(mask.To4()); err != nil {
		return fmt.Errorf("vpn: %s is not a usable netmask: %w", netmask, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, maskReq); err != nil {
		return fmt.Errorf("vpn: setting the netmask of %s to %s: %w", d.name, netmask, err)
	}

	flags, err := unix.NewIfreq(d.name)
	if err != nil {
		return fmt.Errorf("vpn: interface name %q is not usable: %w", d.name, err)
	}
	flags.SetUint16(unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, flags); err != nil {
		return fmt.Errorf("vpn: bringing %s up: %w", d.name, err)
	}
	return nil
}
