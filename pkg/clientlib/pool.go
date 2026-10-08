package clientlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The connection pool is the client's half of frp's transport.poolCount: the
// client keeps [client].pool_count data connections parked at the server, so a
// visitor's stream does not wait for a dial and a key agreement — the connection
// is already open and already acknowledged when the server names a stream on it.
//
// Nothing here is required for a stream to work. The server asks over the control
// connection for any stream it has no parked connection for, and a parked
// connection that turns out to be gone costs one round trip rather than the
// stream, which is why a worker that cannot park simply keeps trying and a server
// that does not park at all just leaves the client dialling per stream.

const (
	// poolRetryDelay is how long a worker waits before offering a connection
	// again after a failure that looks transient, such as a dial that timed out.
	poolRetryDelay = 2 * time.Second
	// poolRetryMax bounds that wait. It is short enough that a client whose server
	// was restarted is pooling again soon after the session comes back.
	poolRetryMax = 30 * time.Second
)

// errPoolRefused carries the server's own refusal of a pooled connection.
var errPoolRefused = errors.New("the server refused a pooled connection")

// runPool keeps target connections parked for one session and returns when the
// session ends.
//
// cipher is the session's own static cipher, not the client's current one: the
// server seals every connection's first frame with the cipher it was built with,
// so a reload that rebuilds the client's key (a rotated auth_token when
// [encryption] derives from it) must not reach the data connections of a session
// that is already up. The session captured this value when it authenticated.
func (c *client) runPool(ctx context.Context, session string, target int, cipher *crypto.Cipher) {
	var workers sync.WaitGroup
	for i := 0; i < target; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			c.poolWorker(ctx, session, cipher)
		}()
	}
	workers.Wait()
}

// poolWorker keeps one connection parked, serves the single stream the server asks
// for on it, and parks another one afterwards.
func (c *client) poolWorker(ctx context.Context, session string, cipher *crypto.Cipher) {
	delay := poolRetryDelay
	for ctx.Err() == nil {
		err := c.parkAndServe(ctx, session, cipher)
		switch {
		case err == nil:
			delay = poolRetryDelay
		case errors.Is(err, errPoolRefused):
			// One line per session, not one per worker: a pool of sixty-four would
			// otherwise say the same thing sixty-four times. The message says what
			// really happens — this worker stops offering — because the server
			// answers the same refusal when the session already holds its maximum
			// parked connections, and there the ones that fit stay live and keep
			// serving.
			if c.poolRefused.CompareAndSwap(false, true) {
				c.logger.Printf("the server refused a pooled connection, so the workers it refuses stop offering on this session: %v", err)
			}
			return
		case ctx.Err() != nil:
			return
		default:
			c.logger.Printf("pool: %v; offering another connection in %s", err, delay)
			if !sleepContext(ctx, delay) {
				return
			}
			delay = min(2*delay, poolRetryMax)
		}
	}
}

// parkAndServe offers one connection for the pool and waits for the server to name
// a stream on it. It returns nil once a stream has been served on that connection,
// errNoPooling when the server does not park connections, and the connection's own
// failure otherwise.
func (c *client) parkAndServe(ctx context.Context, session string, cipher *crypto.Cipher) error {
	conn, err := c.dialServer(ctx)
	if err != nil {
		return fmt.Errorf("cannot reach the server: %w", err)
	}

	// The wait for a stream has no deadline — a pool is idle by design — so the
	// end of the session is what breaks it, by closing the connection.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	framer := protocol.NewFramerWithOptions(conn, cipher, c.framerOptions())
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{Session: session, Pool: true}); err != nil {
		_ = conn.Close()
		return fmt.Errorf("cannot offer a connection for the pool: %w", err)
	}
	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		_ = conn.Close()
		return fmt.Errorf("no answer to the pooled connection: %w", err)
	}
	if !ack.OK {
		_ = conn.Close()
		// Whatever the reason — a stale worker after a reconnect, a pool that
		// is already full, an old server — this worker stops here and says why:
		// the log carries the server's own refusal instead of a generic claim,
		// and the connections that did fit keep serving the session.
		return fmt.Errorf("%w: %s", errPoolRefused, ack.Error)
	}
	_ = conn.SetDeadline(time.Time{})

	for {
		msg, err := framer.ReadFrame()
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the parked connection ended before a stream arrived: %w", err)
		}
		if msg.Type != protocol.TypeDataRequest {
			// A parked connection has exactly one purpose, and the server answers
			// its offer before it uses it, so anything else here is ignored rather
			// than acted on.
			continue
		}
		var request protocol.DataRequest
		if err := json.Unmarshal(msg.Payload, &request); err != nil {
			logging.Warnf(c.logger, "pool: malformed data request: %v", err)
			continue
		}
		// The connection carries this one stream; serveParkedStream closes it.
		c.serveParkedStream(session, request, conn, framer, cipher)
		return nil
	}
}

// sleepContext waits for the duration, or returns false when the context ends
// first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
