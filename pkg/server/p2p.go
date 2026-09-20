package server

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

const (
	// punchAttemptTTL bounds how long an unused attempt occupies memory.
	punchAttemptTTL = 30 * time.Second
	// maxPunchAttempts caps concurrent attempts.
	maxPunchAttempts = 4096
)

// punchAttempt is one in-flight hole-punch rendezvous.
type punchAttempt struct {
	token   string
	proxy   string
	created time.Time

	visitor net.Addr
	owner   net.Addr
}

// p2pRendezvous lets two NATed peers learn each other's public UDP address so
// that they can punch a path through their NATs and talk directly, without the
// server carrying their traffic.
type p2pRendezvous struct {
	conn   net.PacketConn
	logger *log.Logger

	mu       sync.Mutex
	attempts map[string]*punchAttempt

	done chan struct{}
	once sync.Once
}

func newP2PRendezvous(logger *log.Logger) *p2pRendezvous {
	return &p2pRendezvous{
		logger:   logger,
		attempts: make(map[string]*punchAttempt),
		done:     make(chan struct{}),
	}
}

// Start binds the rendezvous socket.
func (p *p2pRendezvous) Start(addr string) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s/udp: %w", addr, err)
	}
	p.conn = conn
	p.logger.Printf("xtcp rendezvous listening on %s/udp", conn.LocalAddr())

	go p.serve()
	go p.reap()
	return nil
}

// Addr reports the bound address, or nil before Start.
func (p *p2pRendezvous) Addr() net.Addr {
	if p.conn == nil {
		return nil
	}
	return p.conn.LocalAddr()
}

// Close stops the rendezvous service.
func (p *p2pRendezvous) Close() error {
	var err error
	p.once.Do(func() {
		close(p.done)
		if p.conn != nil {
			err = p.conn.Close()
		}
	})
	return err
}

// offer reserves a token for one hole-punch attempt.
func (p *p2pRendezvous) offer(owner *Tunnel) (string, error) {
	buf := make([]byte, 16)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.attempts) >= maxPunchAttempts {
		return "", errors.New("too many hole-punch attempts are in flight")
	}
	p.attempts[token] = &punchAttempt{token: token, proxy: owner.Name, created: time.Now()}
	return token, nil
}

// forget ends an attempt.
func (p *p2pRendezvous) forget(token string) {
	p.mu.Lock()
	delete(p.attempts, token)
	p.mu.Unlock()
}

// serve answers rendezvous requests.
func (p *p2pRendezvous) serve() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := p.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		p.handle(buf[:n], addr)
	}
}

// handle records one peer's address and, once both ends are known, tells each
// end where the other one is.
func (p *p2pRendezvous) handle(data []byte, addr net.Addr) {
	role, token, ok := protocol.DecodePunchRequest(data)
	if !ok {
		return
	}

	p.mu.Lock()
	attempt, known := p.attempts[token]
	var other net.Addr
	if known {
		if role == protocol.PunchRoleVisitor {
			attempt.visitor = addr
			other = attempt.owner
		} else {
			attempt.owner = addr
			other = attempt.visitor
		}
	}
	p.mu.Unlock()

	if !known || other == nil {
		// The other end has not reported yet; the peer retries, which also keeps
		// its NAT mapping open.
		return
	}
	reply := protocol.EncodePunchResponse(token, other.String())
	if len(reply) == 0 {
		return
	}
	if _, err := p.conn.WriteTo(reply, addr); err != nil {
		p.logger.Printf("rendezvous: cannot answer %s: %v", addr, err)
	}
}

// reap drops attempts that were never completed.
func (p *p2pRendezvous) reap() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			now := time.Now()
			p.mu.Lock()
			for token, attempt := range p.attempts {
				if now.Sub(attempt.created) > punchAttemptTTL {
					delete(p.attempts, token)
				}
			}
			p.mu.Unlock()
		}
	}
}
