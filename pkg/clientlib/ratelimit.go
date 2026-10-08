package clientlib

import (
	"context"
	"math"
	"net"

	"golang.org/x/time/rate"
)

// newLimitedConn caps a connection's combined read and write throughput with a
// token bucket refilled at limit bytes per second; the burst is one second's
// worth, so short bursts pass and sustained traffic averages to the limit.
//
// A limit below one byte per second leaves the connection unwrapped: a limiter
// with a burst of zero can grant nothing, so wait would loop over the same count
// forever and burn a core. The configuration refuses such a rate, and this is the
// second line of defence for a value that arrives from anywhere else.
func newLimitedConn(limit int64, conn net.Conn) net.Conn {
	return newSharedLimitedConn(newBandwidthLimiter(limit), conn)
}

// newBandwidthLimiter builds the token bucket a connection — or every connection
// of one relay — draws from. A limit below one byte per second yields no bucket at
// all, which the callers read as "not throttled" for the reason newLimitedConn
// gives.
func newBandwidthLimiter(limit int64) *rate.Limiter {
	if limit < 1 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(limit), burstFor(limit))
}

// newSharedLimitedConn caps a connection with a token bucket the caller owns, so
// several connections can share one rate. The socks5 UDP relay holds one bucket
// for its whole association: a bucket per target socket made the association's
// rate grow with the number of addresses its visitor probed, and one association
// could carry up to maxSocksUDPTargets times the configured bandwidth.
func newSharedLimitedConn(limiter *rate.Limiter, conn net.Conn) net.Conn {
	if limiter == nil {
		return conn
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &limitedConn{
		Conn:    conn,
		limiter: limiter,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// burstFor caps the bucket's burst at what an int can hold. The burst is one
// second's worth of the rate, and on a 32-bit build a rate past 2^31 bytes per
// second wraps int(limit) negative; a limiter whose burst is below one grants
// nothing, so wait would let the connection pass unthrottled. The rate itself
// is still enforced by rate.Limit, which is a float64 on every platform.
func burstFor(limit int64) int {
	if limit > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(limit)
}

// Close ends a throttle wait as well as the connection.
func (c *limitedConn) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	return c.Conn.Close()
}

// waitCtx is the context a throttle wait runs under: the connection's own, so Close
// during a wait ends it instead of leaving the caller to wait out the tokens.
func (c *limitedConn) waitCtx() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

type limitedConn struct {
	net.Conn
	limiter *rate.Limiter
	// ctx is cancelled by Close. The limiter's WaitN is the only wait in this
	// wrapper that nothing else can end: with a low bandwidth and a 32 KiB read, a
	// limit of one byte per second holds the goroutine — and the stream teardown
	// that waits for it — for hours.
	ctx    context.Context
	cancel context.CancelFunc
}

// wait reserves n tokens, splitting the reservation into pieces the limiter's
// burst can grant at once.
func (c *limitedConn) wait(ctx context.Context, n int) error {
	burst := c.limiter.Burst()
	if burst < 1 {
		// Nothing can be granted, and the loop below would not make progress:
		// newLimitedConn does not build one of these without a burst, so this is
		// only reachable if a limiter is ever built some other way.
		return nil
	}
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := c.limiter.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// CloseWrite forwards the half-close the wrapped connection may support. net.Conn
// does not carry CloseWrite, so without this the relay falls back to a full close on
// exactly the proxies an operator capped, and the reply a service sends only after it
// has seen the end of the request never arrives.
func (c *limitedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}

// Read and Write are charged for the bytes that actually moved. The relay reads
// into a 32 KiB buffer, so reserving len(p) up front charged every small read as
// if it were full: a stream of small records was throttled to a fraction of the
// configured rate, and a low rate turned one read into a long stall. The wait
// still happens before the next bytes are handed over, so the sustained rate is
// the limit.
func (c *limitedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if werr := c.wait(c.waitCtx(), n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *limitedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		if werr := c.wait(c.waitCtx(), n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
