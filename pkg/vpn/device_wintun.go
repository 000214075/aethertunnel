//go:build windows

package vpn

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows tun device is a Wintun adapter. The driver ships as wintun.dll —
// the official one from wintun.net — and this file loads it and drives the ring
// session; the program itself never installs a driver. If the DLL is missing, the
// device refuses with a message that names the remedy, the same way the Linux
// device refuses without /dev/net/tun.
//
// Creating an adapter needs administrator rights, and so does giving it an
// address; both failures are reported as what they are.

const (
	// wintunCapacity is the ring size, the 0x400000 minimum the API documents.
	wintunCapacity = 0x400000

	// errnoNoMoreItems is ERROR_NO_MORE_ITEMS, which ReceivePacket returns when
	// the ring is empty.
	errnoNoMoreItems = syscall.Errno(259)
)

type tunDevice struct {
	api       *wintunAPI
	adapter   syscall.Handle
	session   syscall.Handle
	readEvent syscall.Handle
	name      string
	mtu       int
}

// wintunAPI holds the entry points this device uses, resolved once per process.
type wintunAPI struct {
	dll                  *windows.LazyDLL
	createAdapter        *windows.LazyProc
	closeAdapter         *windows.LazyProc
	deleteAdapter        *windows.LazyProc
	startSession         *windows.LazyProc
	endSession           *windows.LazyProc
	getReadWaitEvent     *windows.LazyProc
	receivePacket        *windows.LazyProc
	releaseReceivePacket *windows.LazyProc
	allocateSendPacket   *windows.LazyProc
	sendPacket           *windows.LazyProc
}

var (
	wintunOnce     sync.Once
	wintunAPIValue *wintunAPI
	wintunErr      error
)

// wintun resolves the DLL on first use: WINTUN_DLL names one explicitly, and
// otherwise the one beside this executable is used. The working directory is
// deliberately not searched, so a test run never picks up a DLL that happens to
// lie there.
func wintun() (*wintunAPI, error) {
	wintunOnce.Do(func() {
		path := os.Getenv("WINTUN_DLL")
		if path == "" {
			if exe, err := os.Executable(); err == nil {
				candidate := filepath.Join(filepath.Dir(exe), "wintun.dll")
				if _, err := os.Stat(candidate); err == nil {
					path = candidate
				}
			}
		}
		if path == "" {
			wintunErr = fmt.Errorf("vpn: this windows build opens a tun device through wintun.dll, which was not found; put the official one from wintun.net beside the executable or point WINTUN_DLL at it")
			return
		}
		dll := windows.NewLazyDLL(path)
		api := &wintunAPI{
			dll:                  dll,
			createAdapter:        dll.NewProc("WintunCreateAdapter"),
			closeAdapter:         dll.NewProc("WintunCloseAdapter"),
			deleteAdapter:        dll.NewProc("WintunDeleteAdapter"),
			startSession:         dll.NewProc("WintunStartSession"),
			endSession:           dll.NewProc("WintunEndSession"),
			getReadWaitEvent:     dll.NewProc("WintunGetReadWaitEvent"),
			receivePacket:        dll.NewProc("WintunReceivePacket"),
			releaseReceivePacket: dll.NewProc("WintunReleaseReceivePacket"),
			allocateSendPacket:   dll.NewProc("WintunAllocateSendPacket"),
			sendPacket:           dll.NewProc("WintunSendPacket"),
		}
		if err := dll.Load(); err != nil {
			wintunErr = fmt.Errorf("vpn: load %s: %w", path, err)
			return
		}
		for _, proc := range []*windows.LazyProc{
			api.createAdapter, api.closeAdapter, api.deleteAdapter, api.startSession,
			api.endSession, api.getReadWaitEvent, api.receivePacket,
			api.releaseReceivePacket, api.allocateSendPacket, api.sendPacket,
		} {
			if err := proc.Find(); err != nil {
				wintunErr = fmt.Errorf("vpn: %s does not export %s: %w", path, proc.Name, err)
				return
			}
		}
		wintunAPIValue = api
	})
	return wintunAPIValue, wintunErr
}

// pointerFromCall reinterprets the handle a Wintun call returned as a pointer
// into the driver's ring. The value crossed a syscall boundary, and go vet's
// unsafeptr check cannot see that provenance through a stored uintptr, so the
// conversion goes through the variable's own address.
func pointerFromCall(value uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&value))
}

// openPlatformDevice creates (or reopens) the named Wintun adapter and starts its
// ring session. An empty name becomes "AetherTunnel"; two processes on one machine
// need two names, which is what [vpn].device is for.
func openPlatformDevice(name string, mtu int) (Device, error) {
	api, err := wintun()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDevice, err)
	}

	if name == "" {
		name = "AetherTunnel"
	}
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("vpn: adapter name %q is not usable: %w", name, err)
	}
	typePtr, err := syscall.UTF16PtrFromString("AetherTunnel")
	if err != nil {
		return nil, fmt.Errorf("vpn: tunnel type is not usable: %w", err)
	}

	adapter, _, callErr := api.createAdapter.Call(
		uintptr(unsafe.Pointer(namePtr)), uintptr(unsafe.Pointer(typePtr)), 0)
	if adapter == 0 {
		return nil, fmt.Errorf("vpn: create the Wintun adapter %q: %v (creating one needs administrator rights)",
			name, callErr)
	}

	session, _, callErr := api.startSession.Call(adapter, wintunCapacity)
	if session == 0 {
		_, _, _ = api.closeAdapter.Call(adapter)
		return nil, fmt.Errorf("vpn: start the Wintun session on %q: %v", name, callErr)
	}
	event, _, _ := api.getReadWaitEvent.Call(session)

	device := &tunDevice{
		api:       api,
		adapter:   syscall.Handle(adapter),
		session:   syscall.Handle(session),
		readEvent: syscall.Handle(event),
		name:      name,
		mtu:       mtu,
	}
	if mtu > 0 {
		if err := device.setMTU(mtu); err != nil {
			_ = device.Close()
			return nil, err
		}
	}
	return device, nil
}

// setMTU sets the interface MTU through netsh, which like adapter creation needs
// administrator rights.
func (d *tunDevice) setMTU(mtu int) error {
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "subinterface",
		d.name, "mtu="+fmt.Sprint(mtu), "store=active").CombinedOutput()
	if err != nil {
		return fmt.Errorf("vpn: setting the MTU of %s to %d: %w: %s", d.name, mtu, err, out)
	}
	return nil
}

// Name is the adapter name.
func (d *tunDevice) Name() string { return d.name }

// MTU is the configured MTU.
func (d *tunDevice) MTU() int { return d.mtu }

// Read returns one IP packet from the ring, waiting on the read event when the
// ring is empty. Closing the session unblocks a waiting read.
func (d *tunDevice) Read(packet []byte) (int, error) {
	for {
		var size uint32
		head, _, callErr := d.api.receivePacket.Call(
			uintptr(d.session), uintptr(unsafe.Pointer(&size)))
		if head == 0 {
			if errno, ok := callErr.(syscall.Errno); ok && errno == errnoNoMoreItems {
				_, _ = syscall.WaitForSingleObject(d.readEvent, 1000)
				continue
			}
			return 0, fmt.Errorf("vpn: wintun receive on %s: %w", d.name, callErr)
		}
		n := int(size)
		if n > len(packet) {
			_, _, _ = d.api.releaseReceivePacket.Call(uintptr(d.session), head)
			return 0, fmt.Errorf("vpn: the packet exceeds the %d-byte read buffer", len(packet))
		}
		copy(packet[:n], unsafe.Slice((*byte)(pointerFromCall(head)), n))
		_, _, _ = d.api.releaseReceivePacket.Call(uintptr(d.session), head)
		return n, nil
	}
}

// Write copies one IP packet into the ring and hands it to the driver.
func (d *tunDevice) Write(packet []byte) (int, error) {
	head, _, callErr := d.api.allocateSendPacket.Call(uintptr(d.session), uintptr(len(packet)))
	if head == 0 {
		return 0, fmt.Errorf("vpn: wintun send on %s: %w", d.name, callErr)
	}
	copy(unsafe.Slice((*byte)(pointerFromCall(head)), len(packet)), packet)
	_, _, _ = d.api.sendPacket.Call(uintptr(d.session), head)
	return len(packet), nil
}

// Close ends the session and removes the adapter, the way closing the Linux
// device file destroys the interface it created.
func (d *tunDevice) Close() error {
	_, _, _ = d.api.endSession.Call(uintptr(d.session))
	force := 1
	_, _, _ = d.api.deleteAdapter.Call(uintptr(d.adapter), uintptr(force), 0)
	_, _, _ = d.api.closeAdapter.Call(uintptr(d.adapter))
	return nil
}

// SetAddress assigns the address the server handed out, through netsh — the
// third administrator-only step after creating the adapter and starting the
// session.
func (d *tunDevice) SetAddress(address string) error {
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("vpn: %q is not an IPv4 address", address)
	}
	out, err := exec.Command("netsh", "interface", "ip", "set", "address",
		"name="+d.name, "source=static", "addr="+address, "mask=255.255.255.0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("vpn: assigning %s to %s: %w: %s (giving an adapter an address needs administrator rights)",
			address, d.name, err, out)
	}
	return nil
}
