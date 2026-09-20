package vpn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

// Router shares one layer-3 device between several peers, which is what a server
// needs: the host's routing table sends every packet for the tunnel subnet to the
// device, and the router looks at each packet's destination to decide which peer it
// belongs to.
//
// A peer's address is assigned by the caller, which is what makes the lookup a map
// access rather than a routing algorithm.
type Router struct {
	device Device
	mtu    int
	logger *log.Logger

	// writeMu serialises writes to the device. A tun device takes one packet per
	// write, so concurrent writes would interleave two packets into one.
	writeMu sync.Mutex

	mu    sync.RWMutex
	peers map[string]*Peer

	// counters
	fromDevice atomic.Int64
	toDevice   atomic.Int64
	delivered  atomic.Int64
	unroutable atomic.Int64
	dropped    atomic.Int64
	errors     atomic.Int64

	closeOnce sync.Once
	closed    atomic.Bool

	wg sync.WaitGroup
}

// RouterStats is a copy of a router's counters.
type RouterStats struct {
	// Peers is how many peers are attached.
	Peers int `json:"peers"`
	// FromDevice is packets read from the device.
	FromDevice int64 `json:"from_device"`
	// ToDevice is packets written to the device.
	ToDevice int64 `json:"to_device"`
	// Delivered is packets handed to a peer's buffer.
	Delivered int64 `json:"delivered"`
	// Unroutable is packets whose destination is no attached peer.
	Unroutable int64 `json:"unroutable"`
	// Dropped is packets rejected as malformed, above the MTU, or unbuffered.
	Dropped int64 `json:"dropped"`
	// Errors is read and write failures.
	Errors int64 `json:"errors"`
}

// NewRouter builds a router over a device.
func NewRouter(device Device, opts Options) (*Router, error) {
	if device == nil {
		return nil, errors.New("vpn: a device is required")
	}

	mtu := opts.MTU
	if mtu == 0 {
		mtu = device.MTU()
	}
	if mtu == 0 {
		mtu = DefaultMTU
	}
	if mtu < MinMTU || mtu > MaxMTU {
		return nil, fmt.Errorf("vpn: MTU %d is outside %d-%d", mtu, MinMTU, MaxMTU)
	}
	if deviceMTU := device.MTU(); deviceMTU > 0 && mtu > deviceMTU {
		return nil, fmt.Errorf("vpn: MTU %d is larger than the %d that device %s reports",
			mtu, deviceMTU, device.Name())
	}

	return &Router{device: device, mtu: mtu, logger: opts.Logger, peers: make(map[string]*Peer)}, nil
}

// MTU is the packet size limit in force.
func (r *Router) MTU() int { return r.mtu }

// Device is the shared interface.
func (r *Router) Device() Device { return r.device }

// Add attaches a peer that owns address and starts serving it.
//
// The address has to be inside the caller's subnet and unique: two peers with the
// same address would make routing ambiguous, so the second Add is refused.
func (r *Router) Add(transport Transport, address net.IP) (*Peer, error) {
	if transport == nil {
		return nil, errors.New("vpn: a transport is required")
	}
	if address == nil {
		return nil, errors.New("vpn: an address is required")
	}
	if r.closed.Load() {
		return nil, ErrClosed
	}

	key := address.String()
	r.mu.Lock()
	if _, taken := r.peers[key]; taken {
		r.mu.Unlock()
		return nil, fmt.Errorf("vpn: %s is already assigned to another peer", key)
	}
	peer := &Peer{
		router:    r,
		address:   address,
		key:       key,
		transport: transport,
		in:        make(chan []byte, DefaultReceiveBuffer),
		closedCh:  make(chan struct{}),
	}
	r.peers[key] = peer
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		peer.serve()
	}()
	return peer, nil
}

// Remove detaches the peer that owns address, if any.
func (r *Router) Remove(address net.IP) {
	r.remove(address.String())
}

func (r *Router) remove(key string) {
	r.mu.Lock()
	peer, ok := r.peers[key]
	if ok {
		delete(r.peers, key)
	}
	r.mu.Unlock()
	if ok {
		_ = peer.Close()
	}
}

// PeerCount is how many peers are attached.
func (r *Router) PeerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// PeerFor returns the peer that owns address, which is what the frame reader uses to
// find where a received packet goes.
func (r *Router) PeerFor(address net.IP) (*Peer, bool) {
	if address == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	peer, ok := r.peers[address.String()]
	return peer, ok
}

// Run reads the device and delivers each packet to the peer that owns its
// destination, until ctx is cancelled or the device fails.
//
// Peers are served by their own goroutines, so Run only owns the read side. It does
// not close the device: the caller owns it, and closing a shared interface would
// take down every peer at once.
func (r *Router) Run(ctx context.Context) error {
	if r.closed.Load() {
		return ErrClosed
	}

	buffer := make([]byte, MaxMTU)
	for {
		if ctx.Err() != nil {
			return nil
		}

		n, err := r.device.Read(buffer)
		if err != nil {
			if r.closed.Load() {
				return nil
			}
			r.errors.Add(1)
			return fmt.Errorf("vpn: reading from %s: %w", r.device.Name(), err)
		}
		if n == 0 {
			continue
		}

		packet := buffer[:n]
		r.fromDevice.Add(1)
		if err := Validate(packet, r.mtu); err != nil {
			r.dropped.Add(1)
			r.logf("dropping a packet from %s: %v", r.device.Name(), err)
			continue
		}

		destination := Destination(packet)
		peer, ok := r.PeerFor(destination)
		if !ok {
			r.unroutable.Add(1)
			continue
		}

		// The read buffer is reused by the next iteration, and the packet is queued
		// rather than sent here, so the bytes have to be copied before queueing.
		owned := append([]byte(nil), packet...)
		if !peer.Deliver(owned) {
			r.dropped.Add(1)
		} else {
			r.delivered.Add(1)
		}
	}
}

// Stats copies the counters.
func (r *Router) Stats() RouterStats {
	return RouterStats{
		Peers:      r.PeerCount(),
		FromDevice: r.fromDevice.Load(),
		ToDevice:   r.toDevice.Load(),
		Delivered:  r.delivered.Load(),
		Unroutable: r.unroutable.Load(),
		Dropped:    r.dropped.Load(),
		Errors:     r.errors.Load(),
	}
}

// Close detaches every peer and closes the device. It is idempotent.
func (r *Router) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.closed.Store(true)

		r.mu.Lock()
		peers := make([]*Peer, 0, len(r.peers))
		for _, peer := range r.peers {
			peers = append(peers, peer)
		}
		r.peers = make(map[string]*Peer)
		r.mu.Unlock()

		for _, peer := range peers {
			_ = peer.Close()
		}
		err = r.device.Close()
		r.wg.Wait()
	})
	return err
}

// writeToDevice serialises writes to the shared device and counts them.
func (r *Router) writeToDevice(packet []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	n, err := r.device.Write(packet)
	if err != nil {
		return err
	}
	if n != len(packet) {
		return fmt.Errorf("vpn: the device wrote %d of %d bytes", n, len(packet))
	}
	r.toDevice.Add(1)
	return nil
}

func (r *Router) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Printf(format, args...)
	}
}

// Peer is one attached link: it owns a transport, writes what it receives to the
// router's device, and queues what the router delivers to it.
type Peer struct {
	router    *Router
	address   net.IP
	key       string
	transport Transport

	// in queues packets going out to the peer, so one slow peer cannot hold up the
	// router's read loop.
	in chan []byte

	toPeer      atomic.Int64
	fromPeer    atomic.Int64
	bytesToPeer atomic.Int64
	bytesFromP  atomic.Int64
	dropped     atomic.Int64
	errors      atomic.Int64

	closeOnce sync.Once
	closed    atomic.Bool
	closedCh  chan struct{}
}

// Address is the tunnel address this peer owns.
func (p *Peer) Address() net.IP { return p.address }

// Stats copies the peer's counters. ToDevice and FromDevice are counted from the
// device's point of view, as everywhere else in this package.
func (p *Peer) Stats() Snapshot {
	return Snapshot{
		ToDevice:        p.fromPeer.Load(),
		FromDevice:      p.toPeer.Load(),
		BytesToDevice:   p.bytesFromP.Load(),
		BytesFromDevice: p.bytesToPeer.Load(),
		Dropped:         p.dropped.Load(),
		Errors:          p.errors.Load(),
	}
}

// Buffered is how many outbound packets are waiting for this peer.
func (p *Peer) Buffered() int { return len(p.in) }

// Deliver queues a packet for this peer. It never blocks: it reports false when the
// queue is full or the peer is closed, which is what a router does with a link that
// cannot keep up.
func (p *Peer) Deliver(packet []byte) bool {
	if p == nil || p.closed.Load() {
		return false
	}
	select {
	case p.in <- packet:
		p.toPeer.Add(1)
		p.bytesToPeer.Add(int64(len(packet)))
		return true
	case <-p.closedCh:
		return false
	default:
		p.dropped.Add(1)
		return false
	}
}

// serve runs both directions of the peer until one of them fails, then detaches it.
func (p *Peer) serve() {
	finished := make(chan struct{}, 2)
	go func() {
		defer func() { finished <- struct{}{} }()
		p.sendLoop()
	}()
	go func() {
		defer func() { finished <- struct{}{} }()
		p.receiveLoop()
	}()

	<-finished
	_ = p.Close()
	<-finished

	// A peer whose transport failed is detached, so the router stops trying to
	// deliver to it and its address becomes free.
	p.router.remove(p.key)
}

// sendLoop drains the outbound queue to the peer.
func (p *Peer) sendLoop() {
	for {
		select {
		case <-p.closedCh:
			return
		case packet := <-p.in:
			if err := p.transport.Send(packet); err != nil {
				if !p.closed.Load() && !p.router.closed.Load() {
					p.errors.Add(1)
					p.router.logf("vpn: peer %s: sending a packet: %v", p.key, err)
				}
				return
			}
		}
	}
}

// receiveLoop writes everything the peer sends into the device.
func (p *Peer) receiveLoop() {
	for {
		packet, err := p.transport.Receive()
		if err != nil {
			if !p.closed.Load() && !p.router.closed.Load() {
				p.errors.Add(1)
				p.router.logf("vpn: peer %s: receiving a packet: %v", p.key, err)
			}
			return
		}

		if err := Validate(packet, p.router.mtu); err != nil {
			p.dropped.Add(1)
			p.router.logf("vpn: peer %s sent a packet that is not forwardable: %v", p.key, err)
			continue
		}
		// A peer may only send as itself. Without this, any client could forge
		// another client's address and receive its traffic.
		if source := Source(packet); source != nil && !source.Equal(p.address) {
			p.dropped.Add(1)
			p.router.logf("vpn: peer %s sent a packet claiming to come from %s", p.key, source)
			continue
		}

		if err := p.router.writeToDevice(packet); err != nil {
			if p.closed.Load() || p.router.closed.Load() {
				return
			}
			p.errors.Add(1)
			p.router.logf("vpn: peer %s: writing to %s: %v", p.key, p.router.device.Name(), err)
			return
		}

		p.fromPeer.Add(1)
		p.bytesFromP.Add(int64(len(packet)))
	}
}

// Close detaches the peer and closes its transport. It is idempotent.
func (p *Peer) Close() error {
	var err error
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.closedCh)
		err = p.transport.Close()
	})
	return err
}
