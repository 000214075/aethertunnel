package vpn

import (
	"encoding/binary"
	"testing"
)

// FuzzValidate feeds arbitrary bytes to the check the tunnel runs on every packet
// it reads from a device and before every packet it writes to one. A packet that
// panicked here would take the tunnel down, and the bytes come from the peer.
func FuzzValidate(f *testing.F) {
	f.Add([]byte{}, 1500)
	f.Add([]byte{0x45, 0, 0, 20}, 1500)
	f.Add([]byte{0x60, 0, 0, 0}, 1500)
	// An IPv4 header whose total length is smaller than its own header length.
	f.Add([]byte{0x4f, 0, 0, 20}, 1500)
	// An IPv6 header that claims a payload far larger than the packet.
	f.Add([]byte{0x60, 0, 0, 0, 0xff, 0xff}, 1500)

	// A well-formed IPv4 packet and a well-formed IPv6 one, so both branches start
	// from something valid.
	v4 := make([]byte, 40)
	v4[0] = 0x45
	binary.BigEndian.PutUint16(v4[2:], uint16(len(v4)))
	binary.BigEndian.PutUint16(v4[10:], Checksum(v4[:20]))
	f.Add(v4, 1500)
	v6 := make([]byte, 40)
	v6[0] = 0x60
	f.Add(v6, 1500)

	f.Fuzz(func(t *testing.T, packet []byte, mtu int) {
		// The MTU is clamped the way the configuration clamps it, so the fuzzer
		// explores the length comparisons rather than negative MTUs.
		if mtu < 0 {
			mtu = -mtu
		}
		if mtu > MaxMTU+64 {
			mtu = MaxMTU + 64
		}

		err := Validate(packet, mtu)
		// The helpers below walk the same bytes and are called by the router and by
		// the access rules, so they get the same treatment: they may return nil,
		// and they may not panic.
		version := Version(packet)
		destination := Destination(packet)
		source := Source(packet)
		_ = Checksum(packet)

		if err == nil {
			// A packet that passed the check has to be one the version reader
			// agrees with, and its addresses have to have been readable.
			if version != 4 && version != 6 {
				t.Fatalf("Validate accepted a packet Version reads as %d", version)
			}
			if destination == nil || source == nil {
				t.Fatalf("Validate accepted a %d byte IPv%d packet whose addresses are unreadable", len(packet), version)
			}
			if version == 4 {
				if destination.To4() == nil || source.To4() == nil {
					t.Fatalf("a valid IPv4 packet reported non-IPv4 addresses %v/%v", source, destination)
				}
			} else if len(destination) != 16 || len(source) != 16 {
				t.Fatalf("a valid IPv6 packet reported %d/%d byte addresses", len(source), len(destination))
			}
		}
		// A packet too short to hold even a version nibble is version 0, which is
		// neither of the two the tunnel carries.
		if len(packet) == 0 && version != 0 {
			t.Fatalf("an empty packet reports version %d", version)
		}
	})
}

// FuzzSetIPv4Checksum covers the writer that fixes up a header after the tunnel
// rewrites its addresses. A checksum that did not verify would make every
// receiver drop the packet, which looks like a silent black hole on the far side.
func FuzzSetIPv4Checksum(f *testing.F) {
	base := make([]byte, 40)
	base[0] = 0x45
	binary.BigEndian.PutUint16(base[2:], uint16(len(base)))
	f.Add(base)
	// A header with options, so HeaderLen is not always 20.
	long := make([]byte, 60)
	long[0] = 0x4f
	binary.BigEndian.PutUint16(long[2:], uint16(len(long)))
	f.Add(long)

	f.Fuzz(func(t *testing.T, packet []byte) {
		if Version(packet) != 4 {
			return
		}
		header, err := ParseIPv4(packet)
		if err != nil {
			return
		}
		// SetIPv4Checksum rewrites the packet in place, so it gets a copy: the
		// fuzzer's own slice is not the test's to modify.
		packet = append([]byte(nil), packet...)
		if err := SetIPv4Checksum(packet); err != nil {
			t.Fatalf("a packet ParseIPv4 accepted was refused by SetIPv4Checksum: %v", err)
		}
		// The internet checksum of a header that already carries the right value is
		// zero, which is how every receiver tests it.
		if got := Checksum(packet[:header.HeaderLen]); got != 0 {
			t.Fatalf("the header checksum of a %d byte header does not verify: %#04x", header.HeaderLen, got)
		}
	})
}
