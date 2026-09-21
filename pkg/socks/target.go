package socks

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// TargetPolicy decides which addresses a socks5 client may dial.
//
// A socks5 tunnel exists so that a machine which cannot create a tun device can
// still route TCP traffic through the tunnel, which means the client behind it
// dials addresses that the visitor chooses. The allow list is what keeps that
// from turning the client into an open exit for its whole network: without an
// entry that matches, the request is refused.
type TargetPolicy struct {
	allow []*net.IPNet
}

// NewTargetPolicy compiles the configured address ranges. An empty list is
// rejected: it would allow nothing, which makes the proxy pointless, and the
// distinction between "nothing" and "everything" is too easy to get wrong to
// leave implicit.
func NewTargetPolicy(entries []string) (*TargetPolicy, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("socks: allow_targets is empty, so no target would ever be reachable")
	}

	allow := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(trimmed); err == nil {
			allow = append(allow, network)
			continue
		}
		// A bare address is accepted as a single-host range.
		if ip := net.ParseIP(trimmed); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			allow = append(allow, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		return nil, fmt.Errorf("socks: allow_targets entry %q is neither an address nor a CIDR", entry)
	}
	if len(allow) == 0 {
		return nil, fmt.Errorf("socks: allow_targets holds no usable entry")
	}
	return &TargetPolicy{allow: allow}, nil
}

// Allows reports whether an address may be dialed.
func (p *TargetPolicy) Allows(ip net.IP) bool {
	if p == nil || ip == nil {
		return false
	}
	for _, network := range p.allow {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// Check resolves a target and returns the address to dial.
//
// Every address the name resolves to has to be allowed, and the dial then goes to
// one of the allowed addresses rather than to the name: resolving after the check
// would let a name that resolves differently the second time reach a target that
// was never allowed.
func (p *TargetPolicy) Check(target string, timeout time.Duration) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", fmt.Errorf("socks: target %q is not host:port: %w", target, err)
	}
	if port == "" {
		return "", fmt.Errorf("socks: target %q has no port", target)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !p.Allows(ip) {
			return "", fmt.Errorf("socks: target %s is not allowed", target)
		}
		return net.JoinHostPort(ip.String(), port), nil
	}

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("socks: cannot resolve %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return "", fmt.Errorf("socks: %q resolves to no address", host)
	}

	allowed := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if !p.Allows(address.IP) {
			return "", fmt.Errorf("socks: %s resolves to %s, which is not allowed", host, address.IP)
		}
		allowed = append(allowed, address.IP)
	}
	return net.JoinHostPort(allowed[0].String(), port), nil
}

// Dial checks a target and connects to it.
func (p *TargetPolicy) Dial(target string, timeout time.Duration) (net.Conn, error) {
	address, err := p.Check(target, timeout)
	if err != nil {
		return nil, err
	}
	return net.DialTimeout("tcp", address, timeout)
}
