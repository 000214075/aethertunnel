package vpn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrPoolExhausted reports that every usable address in the pool is handed out.
var ErrPoolExhausted = errors.New("vpn: the address pool is exhausted")

// Pool hands out IPv4 addresses from a subnet for as long as a client session
// lasts.
//
// The network and broadcast addresses are never handed out, and the first usable
// address is reserved for the server itself, which is the convention every point to
// point link follows: the server keeps .1 and clients get the rest.
type Pool struct {
	network   *net.IPNet
	first     uint32 // first address after the reserved one
	last      uint32 // last usable address, excluding broadcast
	reserved  uint32 // the address held back for the server
	total     int
	next      uint32
	mu        sync.Mutex
	inUse     map[uint32]bool
	leaseNote string
}

// NewPool builds a pool for an IPv4 subnet given as a CIDR, for example
// "10.7.0.0/24".
func NewPool(cidr string) (*Pool, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("vpn: %q is not a CIDR: %w", cidr, err)
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("vpn: %q is not an IPv4 subnet; the address pool only assigns IPv4", cidr)
	}

	ones, bits := network.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("vpn: %q has a %d-bit mask, want a 32-bit IPv4 mask", cidr, bits)
	}
	hosts := 1 << (32 - ones)
	if hosts < 4 {
		return nil, fmt.Errorf("vpn: %q leaves no address for a client: a /%d has %d addresses, and the network, server and broadcast addresses are reserved",
			cidr, ones, hosts)
	}

	networkAddr := binary.BigEndian.Uint32(network.IP.To4())
	first := networkAddr + 2 // .0 is the network, .1 is the server
	last := networkAddr + uint32(hosts) - 2
	if last < first {
		return nil, fmt.Errorf("vpn: %q leaves no address for a client", cidr)
	}

	return &Pool{
		network:  network,
		first:    first,
		last:     last,
		reserved: networkAddr + 1,
		total:    int(last-first) + 1,
		next:     first,
		inUse:    make(map[uint32]bool),
	}, nil
}

// Network is the subnet the pool allocates from.
func (p *Pool) Network() *net.IPNet { return p.network }

// Server is the address reserved for the server end of the link.
func (p *Pool) Server() net.IP { return uint32ToIP(p.reserved) }

// Mask is the subnet mask, in the form a client needs to configure its device.
func (p *Pool) Mask() net.IPMask { return p.network.Mask }

// Size is how many addresses can be handed out at once.
func (p *Pool) Size() int { return p.total }

// InUse is how many addresses are currently handed out.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}

// Contains reports whether an address belongs to the subnet and is not the
// reserved or broadcast address. It is what a server checks before accepting a
// packet that claims to come from a client.
func (p *Pool) Contains(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	value := binary.BigEndian.Uint32(v4)
	if !p.network.Contains(v4) {
		return false
	}
	return value >= p.first && value <= p.last
}

// Acquire hands out the lowest free address, so a reconnecting client tends to get
// the address it had before. It returns ErrPoolExhausted when every address is out.
func (p *Pool) Acquire() (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inUse) >= p.total {
		return nil, fmt.Errorf("%w: all %d addresses in %s are handed out", ErrPoolExhausted, p.total, p.network)
	}

	// Start where the last search stopped rather than at the beginning, which keeps
	// allocation constant time when many clients are connected.
	if p.next < p.first || p.next > p.last || p.inUse[p.next] {
		p.next = p.first
	}
	for p.inUse[p.next] {
		p.next++
		if p.next > p.last {
			p.next = p.first
		}
	}

	addr := p.next
	p.inUse[addr] = true
	p.next++
	return uint32ToIP(addr), nil
}

// Release returns an address to the pool. Releasing an address that was not handed
// out, or that is outside the subnet, is ignored: a client that disconnects twice
// must not be able to free an address it does not hold.
func (p *Pool) Release(ip net.IP) {
	v4 := ip.To4()
	if v4 == nil {
		return
	}
	value := binary.BigEndian.Uint32(v4)
	if value < p.first || value > p.last {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, value)
	if value < p.next {
		p.next = value
	}
}

// uint32ToIP renders an address in the 4-byte form.
func uint32ToIP(value uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, value)
	return ip
}
