package sctp

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// Conn represents a SCTP connection (fake implementation)
type Conn struct {
	net.Conn
}

// Listener represents a SCTP listener (fake implementation)
type Listener struct {
	net.Listener
	addr     net.Addr
	connChan chan net.Conn
	mu       sync.RWMutex
	running  bool
}

// Fake functions to satisfy the compiler
func Listen(network, addr string) (*Listener, error) {
	// Create a fake listener
	listener := &Listener{
		addr:     &net.TCPAddr{IP: net.IPv4(127,0,0,1), Port: 0},
		connChan: make(chan net.Conn, 100),
		running:  true,
	}
	return listener, nil
}

func Dial(network, addr string) (*Conn, error) {
	// Create a fake connection
	conn := &Conn{
		Conn: &fakeConn{
			localAddr: &net.TCPAddr{IP: net.IPv4(127,0,0,1), Port: 0},
			remoteAddr: &net.TCPAddr{IP: net.IPv4(127,0,0,1), Port: 0},
		},
	}
	return conn, nil
}

// fakeConn implements net.Conn interface
type fakeConn struct {
	localAddr  net.Addr
	remoteAddr net.Addr
	mu         sync.RWMutex
}

func (c *fakeConn) Read(b []byte) (n int, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copy(b, []byte("fake data")), nil
}

func (c *fakeConn) Write(b []byte) (n int, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(b), nil
}

func (c *fakeConn) Close() error {
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *fakeConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *fakeConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *fakeConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (l *Listener) Accept() (net.Conn, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.running {
		return nil, fmt.Errorf("listener is not running")
	}

	select {
	case conn := <-l.connChan:
		return conn, nil
	default:
		return nil, fmt.Errorf("no connection available")
	}
}

func (l *Listener) Addr() net.Addr {
	return l.addr
}

func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = false
	return nil
}