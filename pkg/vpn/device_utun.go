//go:build darwin

package vpn

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The macOS tun device is a utun control socket: opening one is unprivileged, and
// the interface it creates disappears when the socket closes. Giving the interface
// an address runs ifconfig, which needs root — a failure there is reported as
// what it is rather than silently skipped.
//
// Every packet on the socket is prefixed with four bytes of address family in
// host byte order, and reads return exactly one prefix-plus-packet per call,
// because the socket is a datagram socket.

const utunControlName = "com.apple.net.utun_control"

type tunDevice struct {
	file *os.File
	name string
	mtu  int

	// readBuf holds one prefix-plus-packet; the router is the only reader, so
	// the buffer is not guarded.
	readBuf []byte
}

// openPlatformDevice opens the named utun interface. The name has the utunN shape
// (utun0, utun1, ...); an empty name becomes utun9, which collides rarely. The
// unit the control socket takes is one above the number in the name, which is how
// the kext numbers its interfaces.
func openPlatformDevice(name string, mtu int) (Device, error) {
	if name == "" {
		name = "utun9"
	}
	if !strings.HasPrefix(name, "utun") {
		return nil, fmt.Errorf("vpn: %q is not a usable utun name on macOS (they are utun0, utun1, ...)", name)
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(name, "utun"), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("vpn: %q is not a usable utun name on macOS: %w", name, err)
	}

	// SYSPROTO_CONTROL is 2 on darwin; x/sys/unix does not name it.
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		return nil, fmt.Errorf("vpn: open a system control socket: %w", err)
	}
	var info unix.CtlInfo
	copy(info.Name[:], utunControlName)
	if err := unix.IoctlCtlInfo(fd, &info); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vpn: resolve the %s control socket: %w", utunControlName, err)
	}
	if err := unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id, Unit: uint32(number) + 1}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vpn: open %s (it may already exist): %w", name, err)
	}

	if mtu == 0 {
		mtu = 1500
	}
	return &tunDevice{
		file:    os.NewFile(uintptr(fd), name),
		name:    name,
		mtu:     mtu,
		readBuf: make([]byte, mtu+4),
	}, nil
}

// Name is the interface name.
func (d *tunDevice) Name() string { return d.name }

// MTU is the interface MTU.
func (d *tunDevice) MTU() int { return d.mtu }

// Read returns one IP packet, stripping the address-family prefix.
func (d *tunDevice) Read(packet []byte) (int, error) {
	n, err := d.file.Read(d.readBuf)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, fmt.Errorf("vpn: utun read on %s returned %d bytes, shorter than the 4-byte prefix", d.name, n)
	}
	// The family is the prefix; IPv6 arrives as AF_INET6 and is dropped here,
	// because the tunnel assigns IPv4 addresses only.
	if family := binary.LittleEndian.Uint32(d.readBuf[:4]); family != unix.AF_INET {
		return 0, nil
	}
	n -= 4
	if n > len(packet) {
		n = len(packet)
	}
	copy(packet[:n], d.readBuf[4:n+4])
	return n, nil
}

// Write sends one IP packet with the address-family prefix.
func (d *tunDevice) Write(packet []byte) (int, error) {
	buf := make([]byte, len(packet)+4)
	binary.LittleEndian.PutUint32(buf[:4], unix.AF_INET)
	copy(buf[4:], packet)
	if _, err := d.file.Write(buf); err != nil {
		return 0, err
	}
	return len(packet), nil
}

// Close closes the socket, which destroys the interface.
func (d *tunDevice) Close() error { return d.file.Close() }

// SetAddress assigns the address the server handed out and brings the interface
// up, through ifconfig — which needs root, and says so when it fails.
func (d *tunDevice) SetAddress(address string) error {
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("vpn: %q is not an IPv4 address", address)
	}
	out, err := exec.Command("ifconfig", d.name, address, address, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("vpn: assigning %s to %s: %w: %s (giving a utun interface an address needs root)",
			address, d.name, err, out)
	}
	if d.mtu != 1500 {
		out, err := exec.Command("ifconfig", d.name, "mtu", strconv.Itoa(d.mtu)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("vpn: setting the MTU of %s to %d: %w: %s", d.name, d.mtu, err, out)
		}
	}
	return nil
}
