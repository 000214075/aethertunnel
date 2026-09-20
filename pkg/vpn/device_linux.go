//go:build linux

package vpn

import (
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
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vpn: open /dev/net/tun: %w", err)
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
		return nil, fmt.Errorf("vpn: TUNSETIFF for %q: %w", name, err)
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

// SetAddress assigns the address the server handed out and brings the interface up.
func (d *tunDevice) SetAddress(address string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vpn: open a control socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("vpn: %q is not an IPv4 address", address)
	}

	addr, err := unix.NewIfreq(d.name)
	if err != nil {
		return fmt.Errorf("vpn: interface name %q is not usable: %w", d.name, err)
	}
	var raw [16]byte
	raw[0] = unix.AF_INET
	copy(raw[2:6], ip.To4())
	addr.SetInet4Addr(raw[:])
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, addr); err != nil {
		return fmt.Errorf("vpn: assigning %s to %s: %w", address, d.name, err)
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
