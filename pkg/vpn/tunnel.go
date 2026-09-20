package vpn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// Transport carries IP packets over an established connection. The tunnel does not
// care how they travel: the client supplies one that writes frames on a data
// connection, and a test supplies one that hands packets over a channel.
//
// Send and Receive are each called from one goroutine, which is how the tunnel uses
// them: one goroutine per direction. Close must unblock a Receive that is already
// in progress.
type Transport interface {
	Send(packet []byte) error
	Receive() ([]byte, error)
	Close() error
}

// Stats counts what the tunnel has moved. Every field is written with atomic adds,
// so a status endpoint can read them while the tunnel runs.
type Stats struct {
	// ToDevice is packets that arrived from the peer and were written to the device.
	ToDevice atomic.Int64
	// FromDevice is packets read from the device and sent to the peer.
	FromDevice atomic.Int64
	// BytesToDevice and BytesFromDevice are the payload bytes of those packets.
	BytesToDevice   atomic.Int64
	BytesFromDevice atomic.Int64
	// Dropped counts packets rejected for being malformed or above the MTU.
	Dropped atomic.Int64
	// Errors counts read and write failures on either side.
	Errors atomic.Int64
}

// Snapshot is a copy of the counters, for reporting.
type Snapshot struct {
	ToDevice        int64 `json:"to_device"`
	FromDevice      int64 `json:"from_device"`
	BytesToDevice   int64 `json:"bytes_to_device"`
	BytesFromDevice int64 `json:"bytes_from_device"`
	Dropped         int64 `json:"dropped"`
	Errors          int64 `json:"errors"`
}

// Snapshot copies the counters.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		ToDevice:        s.ToDevice.Load(),
		FromDevice:      s.FromDevice.Load(),
		BytesToDevice:   s.BytesToDevice.Load(),
		BytesFromDevice: s.BytesFromDevice.Load(),
		Dropped:         s.Dropped.Load(),
		Errors:          s.Errors.Load(),
	}
}

// Tunnel forwards IP packets between a device and a transport.
type Tunnel struct {
	device    Device
	transport Transport
	mtu       int
	logger    *log.Logger
	stats     Stats

	closeOnce sync.Once
	closed    atomic.Bool
}

// Options configures a tunnel.
type Options struct {
	// MTU bounds the packets the tunnel will forward. Zero uses the device's own
	// MTU, and DefaultMTU when the device does not report one.
	MTU int
	// Logger receives diagnostic lines. nil disables logging.
	Logger *log.Logger
}

// New builds a tunnel. It checks that the device is usable and that the MTU is
// within the range the package supports, so a misconfiguration is reported at
// startup rather than as silent packet loss.
func New(device Device, transport Transport, opts Options) (*Tunnel, error) {
	if device == nil {
		return nil, errors.New("vpn: a device is required")
	}
	if transport == nil {
		return nil, errors.New("vpn: a transport is required")
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

	return &Tunnel{device: device, transport: transport, mtu: mtu, logger: opts.Logger}, nil
}

// MTU is the packet size limit in force.
func (t *Tunnel) MTU() int { return t.mtu }

// Device is the interface the tunnel reads from and writes to.
func (t *Tunnel) Device() Device { return t.device }

// Stats exposes the counters.
func (t *Tunnel) Stats() *Stats { return &t.stats }

// Run forwards packets in both directions until ctx is cancelled or one of the
// sides fails.
//
// The tunnel owns the device and the transport for the duration of the call:
// returning closes both, which is also what unblocks a read that is in progress.
// Cancellation is a normal stop and is reported as a nil error; any other failure
// is returned.
func (t *Tunnel) Run(ctx context.Context) error {
	results := make(chan error, 2)
	go func() { results <- t.deviceToTransport(ctx) }()
	go func() { results <- t.transportToDevice() }()

	var first error
	select {
	case <-ctx.Done():
	case first = <-results:
	}

	// Closing both ends unblocks whichever direction is still reading.
	_ = t.close()
	second := <-results

	err := first
	if err == nil {
		err = second
	}
	switch {
	case errors.Is(err, ErrClosed):
		return nil
	case ctx.Err() != nil:
		return nil
	default:
		return err
	}
}

// deviceToTransport reads packets from the device and sends them to the peer.
func (t *Tunnel) deviceToTransport(ctx context.Context) error {
	buffer := make([]byte, MaxMTU)
	for {
		if ctx.Err() != nil {
			return ErrClosed
		}

		n, err := t.device.Read(buffer)
		if err != nil {
			if t.closed.Load() {
				return ErrClosed
			}
			t.stats.Errors.Add(1)
			return fmt.Errorf("vpn: reading from %s: %w", t.device.Name(), err)
		}
		if n == 0 {
			continue
		}

		packet := buffer[:n]
		if err := Validate(packet, t.mtu); err != nil {
			t.stats.Dropped.Add(1)
			t.logf("dropping a packet from %s: %v", t.device.Name(), err)
			continue
		}
		if err := t.transport.Send(packet); err != nil {
			if t.closed.Load() {
				return ErrClosed
			}
			t.stats.Errors.Add(1)
			return fmt.Errorf("vpn: sending a packet to the peer: %w", err)
		}

		t.stats.FromDevice.Add(1)
		t.stats.BytesFromDevice.Add(int64(n))
	}
}

// transportToDevice receives packets from the peer and writes them to the device.
func (t *Tunnel) transportToDevice() error {
	for {
		packet, err := t.transport.Receive()
		if err != nil {
			if t.closed.Load() {
				return ErrClosed
			}
			t.stats.Errors.Add(1)
			return fmt.Errorf("vpn: receiving a packet from the peer: %w", err)
		}

		if err := Validate(packet, t.mtu); err != nil {
			t.stats.Dropped.Add(1)
			t.logf("dropping a packet from the peer: %v", err)
			continue
		}
		if _, err := t.device.Write(packet); err != nil {
			if t.closed.Load() {
				return ErrClosed
			}
			t.stats.Errors.Add(1)
			return fmt.Errorf("vpn: writing to %s: %w", t.device.Name(), err)
		}

		t.stats.ToDevice.Add(1)
		t.stats.BytesToDevice.Add(int64(len(packet)))
	}
}

// Close closes the device and the transport. It is idempotent.
func (t *Tunnel) Close() error { return t.close() }

func (t *Tunnel) close() error {
	var err error
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		deviceErr := t.device.Close()
		transportErr := t.transport.Close()
		err = errors.Join(deviceErr, transportErr)
	})
	return err
}

func (t *Tunnel) logf(format string, args ...any) {
	if t.logger != nil {
		t.logger.Printf(format, args...)
	}
}
