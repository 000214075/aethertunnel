package vpn

import (
	"fmt"
	"os"
)

// fdDevice is a tun interface that already exists: the platform shell opened it
// (Android's VpnService.establish, iOS's packet flow) and hands over the file
// descriptor. The address, the netmask and the routes were configured on it
// before this code saw it, because a platform interface is configured at
// creation time — which is why SetAddress validates nothing and reports success
// whatever it is handed: there is no ioctl to reach from here, and nothing left
// to configure. The arguments are the platform shell's, and it has already
// applied them.
type fdDevice struct {
	file *os.File
	name string
	mtu  int
}

// NewFromFD wraps an already-open tun file descriptor as a Device.
//
// The descriptor must really be a tun device — reads return exactly one IP
// packet per call only then, which is what the tunnel's forwarding relies on.
// name and mtu are what the shell reports for the interface; an empty name
// becomes "tun" in the logs, and a zero mtu keeps DefaultMTU.
func NewFromFD(fd uintptr, name string, mtu int) (Device, error) {
	if mtu != 0 && (mtu < MinMTU || mtu > MaxMTU) {
		return nil, fmt.Errorf("vpn: MTU %d is outside %d-%d", mtu, MinMTU, MaxMTU)
	}
	if name == "" {
		name = "tun"
	}
	// The platform shell usually hands over a blocking descriptor; without this
	// a Read parked on it is not woken by Close, so the client never finishes
	// tearing the tunnel down.
	if err := setNonblockFD(fd); err != nil {
		return nil, fmt.Errorf("vpn: cannot make the tun descriptor %d pollable: %w", fd, err)
	}
	file := os.NewFile(fd, "tun:"+name)
	if file == nil {
		return nil, fmt.Errorf("vpn: file descriptor %d is not usable", fd)
	}
	return &fdDevice{file: file, name: name, mtu: mtu}, nil
}

func (d *fdDevice) Read(p []byte) (int, error)  { return d.file.Read(p) }
func (d *fdDevice) Write(p []byte) (int, error) { return d.file.Write(p) }
func (d *fdDevice) Close() error                { return d.file.Close() }

// Name is the interface name the shell reported, for the logs.
func (d *fdDevice) Name() string { return d.name }

// MTU is the size the shell configured; zero means this device keeps DefaultMTU.
func (d *fdDevice) MTU() int {
	if d.mtu == 0 {
		return DefaultMTU
	}
	return d.mtu
}

// SetAddress accepts the address and netmask the server assigned. The shell
// configured them on the interface before handing the descriptor over, so
// there is nothing to apply; the arguments are not kept, because nothing
// reads them back.
func (d *fdDevice) SetAddress(address, netmask string) error {
	return nil
}
