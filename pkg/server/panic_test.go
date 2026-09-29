package server

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// panicConn is a visitor connection whose Read panics, which is how this test stands
// in for a handler that trips over one visitor's bytes.
type panicConn struct{}

func (panicConn) Read([]byte) (int, error)    { panic("visitor read") }
func (panicConn) Write(p []byte) (int, error) { return len(p), nil }
func (panicConn) Close() error                { return nil }
func (panicConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
func (panicConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
}
func (panicConn) SetDeadline(time.Time) error      { return nil }
func (panicConn) SetReadDeadline(time.Time) error  { return nil }
func (panicConn) SetWriteDeadline(time.Time) error { return nil }

// TestVisitorHandlerSurvivesAPanic covers the guard on a proxy's public listener.
//
// acceptLoop serves each visitor on a goroutine of its own, which the control
// connection's guard (see Server.handleConn) does not cover, so a panic while serving
// one visitor used to end the process that was carrying every tunnel. The visitor's
// own connection is what is given up instead.
func TestVisitorHandlerSurvivesAPanic(t *testing.T) {
	group := &ProxyGroup{
		Name:   "exit",
		Type:   protocol.ProxyTypeSOCKS,
		logger: log.New(io.Discard, "", 0),
	}

	// serveVisit returns normally when the visitor's bytes panic inside it, which is
	// the whole assertion: without the guard this call takes the test process down.
	group.serveVisit(panicConn{})
}

// TestStreamVisitorHandlerSurvivesAPanic covers the same guard on the path every
// non-socks5 proxy type takes, which hands the connection straight to the relay.
func TestStreamVisitorHandlerSurvivesAPanic(t *testing.T) {
	group := &ProxyGroup{
		Name:   "web",
		Type:   protocol.ProxyTypeTCP,
		logger: log.New(io.Discard, "", 0),
	}

	group.serveVisit(panicConn{})
}
