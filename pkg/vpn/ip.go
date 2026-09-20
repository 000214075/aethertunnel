// Package vpn implements the layer-3 side of the tunnel: it moves whole IP
// packets between a tun device and a tunnel connection.
//
// The package is split so that everything except the operating-system device is
// portable and testable. Device is the only platform-specific piece: Linux opens
// /dev/net/tun, and every other platform returns ErrNoDevice with an explanation
// of what is missing, because a tun device needs a driver that this program cannot
// install.
package vpn

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// Device is a layer-3 network interface. Read returns exactly one IP packet,
// Write accepts exactly one; the boundaries are datagram boundaries, not a byte
// stream, which is what lets the tunnel forward packets without reassembling them.
type Device interface {
	io.ReadWriteCloser
	// Name is the interface name, for logs.
	Name() string
	// MTU is the largest packet the device will carry.
	MTU() int
}

var (
	// ErrNoDevice reports that this build has no working tun device on this
	// platform.
	ErrNoDevice = errors.New("vpn: no tun device is available on this platform")
	// ErrClosed reports an operation on a closed tunnel.
	ErrClosed = errors.New("vpn: the tunnel is closed")
	// ErrPacketTooLarge reports a packet that exceeds the configured MTU.
	ErrPacketTooLarge = errors.New("vpn: the packet exceeds the MTU")
	// ErrMalformedPacket reports a payload that is not an IP packet.
	ErrMalformedPacket = errors.New("vpn: the payload is not a valid IP packet")
)

// Limits that keep a misbehaving peer from making the tunnel allocate without
// bound. They are deliberately generous: a 9000-byte MTU is a common jumbo frame
// size, and IPv6 requires a 1280-byte minimum.
const (
	MinMTU = 576
	MaxMTU = 9000
	// MaxIPv4HeaderLen is the largest a header can be once options are counted.
	MaxIPv4HeaderLen = 60
	// IPv6HeaderLen is fixed: IPv6 extension headers are not walked.
	IPv6HeaderLen = 40
)

// DefaultMTU leaves room for the tunnel's own framing inside a 1500-byte path.
const DefaultMTU = 1420

// Version returns the IP version a packet declares, or 0 when the packet is too
// short to tell.
func Version(packet []byte) int {
	if len(packet) == 0 {
		return 0
	}
	switch packet[0] >> 4 {
	case 4:
		return 4
	case 6:
		return 6
	default:
		return 0
	}
}

// IPv4Header is the part of an IPv4 header the tunnel inspects.
type IPv4Header struct {
	// HeaderLen is the header length in bytes, counting options.
	HeaderLen int
	// TotalLen is the length the header claims for the whole packet.
	TotalLen int
	// Protocol is the payload protocol number.
	Protocol uint8
	// Source and Destination are the addresses in the header.
	Source      net.IP
	Destination net.IP
	// TTL is the remaining hop count.
	TTL uint8
}

// ParseIPv4 decodes an IPv4 header. It returns ErrMalformedPacket when the packet
// is too short, declares a header length below the 20-byte minimum, exceeds the
// maximum, or claims a total length larger than the bytes that are actually there.
//
// It deliberately does not reject a bad header checksum: a tunnel that drops those
// would hide a broken peer behind silence. Checksum is exposed so a caller can
// decide for itself.
func ParseIPv4(packet []byte) (IPv4Header, error) {
	if Version(packet) != 4 {
		return IPv4Header{}, fmt.Errorf("%w: not an IPv4 packet", ErrMalformedPacket)
	}
	if len(packet) < 20 {
		return IPv4Header{}, fmt.Errorf("%w: IPv4 header is %d bytes, want at least 20", ErrMalformedPacket, len(packet))
	}

	headerLen := int(packet[0]&0x0f) * 4
	switch {
	case headerLen < 20:
		return IPv4Header{}, fmt.Errorf("%w: IPv4 header length field says %d bytes, want at least 20", ErrMalformedPacket, headerLen)
	case headerLen > MaxIPv4HeaderLen:
		return IPv4Header{}, fmt.Errorf("%w: IPv4 header length field says %d bytes, want at most %d", ErrMalformedPacket, headerLen, MaxIPv4HeaderLen)
	case len(packet) < headerLen:
		return IPv4Header{}, fmt.Errorf("%w: IPv4 header claims %d bytes but the packet holds %d", ErrMalformedPacket, headerLen, len(packet))
	}

	totalLen := int(packet[2])<<8 | int(packet[3])
	if totalLen < headerLen {
		return IPv4Header{}, fmt.Errorf("%w: IPv4 total length %d is shorter than its %d-byte header", ErrMalformedPacket, totalLen, headerLen)
	}
	if totalLen > len(packet) {
		return IPv4Header{}, fmt.Errorf("%w: IPv4 total length claims %d bytes but the packet holds %d", ErrMalformedPacket, totalLen, len(packet))
	}

	source := make(net.IP, 4)
	copy(source, packet[12:16])
	destination := make(net.IP, 4)
	copy(destination, packet[16:20])

	return IPv4Header{
		HeaderLen:   headerLen,
		TotalLen:    totalLen,
		Protocol:    packet[9],
		TTL:         packet[8],
		Source:      source,
		Destination: destination,
	}, nil
}

// Validate checks that a packet is well formed and fits the configured MTU. It is
// what the tunnel calls on every packet it receives and before every packet it
// writes to a device.
func Validate(packet []byte, mtu int) error {
	switch Version(packet) {
	case 4:
		header, err := ParseIPv4(packet)
		if err != nil {
			return err
		}
		if mtu > 0 && header.TotalLen > mtu {
			return fmt.Errorf("%w: IPv4 packet is %d bytes, the MTU is %d", ErrPacketTooLarge, header.TotalLen, mtu)
		}
		return nil
	case 6:
		if len(packet) < IPv6HeaderLen {
			return fmt.Errorf("%w: IPv6 packet is %d bytes, want at least %d", ErrMalformedPacket, len(packet), IPv6HeaderLen)
		}
		// The length field of an IPv6 header counts the bytes after the header, not
		// the whole packet.
		payloadLen := int(packet[4])<<8 | int(packet[5])
		total := IPv6HeaderLen + payloadLen
		if len(packet) < total {
			return fmt.Errorf("%w: IPv6 payload length claims %d bytes but the packet holds %d", ErrMalformedPacket, total, len(packet))
		}
		if mtu > 0 && total > mtu {
			return fmt.Errorf("%w: IPv6 packet is %d bytes, the MTU is %d", ErrPacketTooLarge, total, mtu)
		}
		return nil
	default:
		return fmt.Errorf("%w: the first byte is %#02x, which is neither IPv4 nor IPv6", ErrMalformedPacket, packet[0])
	}
}

// Destination returns the destination address of an IPv4 or IPv6 packet, which is
// what a routing decision or an access rule needs. It returns nil for a packet it
// cannot parse.
func Destination(packet []byte) net.IP {
	switch Version(packet) {
	case 4:
		header, err := ParseIPv4(packet)
		if err != nil {
			return nil
		}
		return header.Destination
	case 6:
		if len(packet) < IPv6HeaderLen {
			return nil
		}
		destination := make(net.IP, 16)
		copy(destination, packet[24:40])
		return destination
	default:
		return nil
	}
}

// Source returns the source address of an IPv4 or IPv6 packet, or nil.
func Source(packet []byte) net.IP {
	switch Version(packet) {
	case 4:
		header, err := ParseIPv4(packet)
		if err != nil {
			return nil
		}
		return header.Source
	case 6:
		if len(packet) < IPv6HeaderLen {
			return nil
		}
		source := make(net.IP, 16)
		copy(source, packet[8:24])
		return source
	default:
		return nil
	}
}

// Checksum computes the internet checksum of a byte slice (RFC 1071), as the IPv4
// header and the transport pseudo-headers use it. An even-length buffer covers a
// whole header; an odd length is zero-padded on the right, which is how the
// pseudo-header sums are taken.
func Checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// SetIPv4Checksum writes the correct header checksum into an IPv4 packet. It is
// used whenever a packet's header is modified, since rewriting an address without
// this leaves a packet every receiver discards.
func SetIPv4Checksum(packet []byte) error {
	header, err := ParseIPv4(packet)
	if err != nil {
		return err
	}
	packet[10], packet[11] = 0, 0
	sum := Checksum(packet[:header.HeaderLen])
	packet[10] = byte(sum >> 8)
	packet[11] = byte(sum)
	return nil
}
