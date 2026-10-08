package reliable

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Exchange sends payload to server repeatedly and returns the addresses reply it
// selects, together with the address it came from.
//
// It runs on the very socket that will later carry the stream. That is the whole
// point: a NAT maps a socket's traffic by port, so the rendezvous server only
// observes the mapping the peer will have to send back to if the address exchange
// and the stream leave through the same socket. Exchanging addresses from a
// throwaway socket produces a mapping that no incoming packet will ever reach.
//
// accept decides which datagrams count as the reply. It must not be nil, because
// the peer starts punching as soon as the server has told it where this socket
// is, so hello datagrams and the reply can arrive interleaved; anything accept
// rejects is discarded, and the peer keeps retrying.
//
// Exchange must be called before Punch or Accept. It leaves the socket usable and
// does not consume the handshake token, so the caller may set the token with
// SetToken before punching.
func (s *Socket) Exchange(ctx context.Context, server net.Addr, payload []byte, accept func([]byte) bool) ([]byte, net.Addr, error) {
	if server == nil {
		return nil, nil, errors.New("reliable: exchange needs a server address")
	}
	if len(payload) == 0 {
		return nil, nil, errors.New("reliable: exchange needs a payload")
	}
	if accept == nil {
		return nil, nil, errors.New("reliable: exchange needs an accept predicate")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	closed, used := s.dead, s.used
	s.mu.Unlock()
	// A dead socket is not a busy one: establish reports net.ErrClosed for it, and
	// a caller that checks with errors.Is has to get the same answer from here.
	if closed {
		return nil, nil, net.ErrClosed
	}
	if used {
		return nil, nil, errors.New("reliable: the socket is already carrying a connection")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		ticker := time.NewTicker(punchInterval)
		defer ticker.Stop()
		for {
			s.tx.writeTo(payload, server)
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	buf := make([]byte, s.cfg.MTU)
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		_ = s.tx.pc.SetReadDeadline(time.Now().Add(punchInterval))

		n, addr, err := s.tx.pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if s.isDead() {
				return nil, nil, net.ErrClosed
			}
			// A platform can report a stray oversized datagram or an ICMP error as
			// a read error; one of those during the address exchange must not abort
			// the whole rendezvous. The handshake and the data read loop keep the
			// same tolerance.
			if failures++; failures <= readFailLimit {
				continue
			}
			return nil, nil, fmt.Errorf("reliable: exchange read: %w", err)
		}
		failures = 0
		if n == 0 {
			continue
		}
		if !accept(buf[:n]) {
			continue
		}

		reply := append([]byte(nil), buf[:n]...)
		_ = s.tx.pc.SetReadDeadline(time.Time{})
		return reply, addr, nil
	}
}

// SetToken replaces the handshake token. It must be called before Punch or
// Accept, which is what a peer does after the rendezvous server has told it the
// token to use.
func (s *Socket) SetToken(token []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used {
		return
	}
	s.cfg.Token = append([]byte(nil), token...)
}
