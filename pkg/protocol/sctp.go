package protocol

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-sctp"
)

// SCTPConn represents a SCTP connection
type SCTPConn struct {
	mu       sync.RWMutex
	conn     *sctp.Conn
	addr     net.Addr
	running  bool
	dataChan chan []byte
	errChan  chan error
}

// SCTPListener represents a SCTP listener
type SCTPListener struct {
	mu       sync.RWMutex
	listener *sctp.Listener
	addr     net.Addr
	running  bool
	connChan chan *SCTPConn
}

// NewSCTPConn creates a new SCTP connection
func NewSCTPConn(conn *sctp.Conn, addr net.Addr) *SCTPConn {
	return &SCTPConn{
		conn:     conn,
		addr:     addr,
		running:  true,
		dataChan: make(chan []byte, 1024),
		errChan:  make(chan error, 10),
	}
}

// NewSCTPListener creates a new SCTP listener
func NewSCTPListener(addr string) (*SCTPListener, error) {
	listener, err := sctp.Listen("sctp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on SCTP: %v", err)
	}

	return &SCTPListener{
		listener: listener,
		addr:     listener.Addr(),
		running:  true,
		connChan: make(chan *SCTPConn, 100),
	}, nil
}

// Accept accepts incoming SCTP connections
func (l *SCTPListener) Accept() (*SCTPConn, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.running {
		return nil, fmt.Errorf("listener is not running")
	}

	conn, err := l.listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("failed to accept SCTP connection: %v", err)
	}

	sctpConn := NewSCTPConn(conn.(net.Conn), conn.RemoteAddr())
	go sctpConn.handleIncomingData()

	return sctpConn, nil
}

// Read reads data from SCTP connection
func (c *SCTPConn) Read() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return nil, fmt.Errorf("connection is not running")
	}

	select {
	case data := <-c.dataChan:
		return data, nil
	case err := <-c.errChan:
		return nil, err
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("read timeout")
	}
}

// Write writes data to SCTP connection
func (c *SCTPConn) Write(data []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return fmt.Errorf("connection is not running")
	}

	_, err := c.conn.Write(data)
	return err
}

// Close closes the SCTP connection
func (c *SCTPConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	c.running = false
	close(c.dataChan)
	close(c.errChan)

	return c.conn.Close()
}

// handleIncomingData handles incoming SCTP data
func (c *SCTPConn) handleIncomingData() {
	for c.running {
		buffer := make([]byte, 65535)
		n, err := c.conn.Read(buffer)
		if err != nil {
			c.errChan <- fmt.Errorf("failed to read from SCTP: %v", err)
			return
		}

		if n > 0 {
			data := make([]byte, n)
			copy(data, buffer[:n])
			select {
			case c.dataChan <- data:
			case <-time.After(5 * time.Second):
				log.Printf("SCTP data channel full, dropping packet")
			}
		}
	}
}

// Addr returns the local address
func (c *SCTPConn) Addr() net.Addr {
	return c.addr
}

// RemoteAddr returns the remote address
func (c *SCTPConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// IsRunning checks if the connection is running
func (c *SCTPConn) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}