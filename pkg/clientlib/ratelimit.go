package clientlib

import (
	"context"
	"net"

	"golang.org/x/time/rate"
)

// newLimitedConn caps a connection's combined read and write throughput with a
// token bucket refilled at limit bytes per second; the burst is one second's
// worth, so short bursts pass and sustained traffic averages to the limit.
func newLimitedConn(limit int64, conn net.Conn) net.Conn {
	return &limitedConn{Conn: conn, limiter: rate.NewLimiter(rate.Limit(limit), int(limit))}
}

type limitedConn struct {
	net.Conn
	limiter *rate.Limiter
}

// wait reserves n tokens, splitting the reservation into pieces the limiter's
// burst can grant at once.
func (c *limitedConn) wait(ctx context.Context, n int) error {
	burst := c.limiter.Burst()
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

func (c *limitedConn) Read(p []byte) (int, error) {
	if err := c.wait(context.Background(), len(p)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *limitedConn) Write(p []byte) (int, error) {
	if err := c.wait(context.Background(), len(p)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}
