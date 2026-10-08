package net

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *lockedSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *lockedSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// erringSocket is a packet socket whose reads fail immediately and forever,
// which is what a socket on an interface that went down reads like.
type erringSocket struct{}

func (erringSocket) ReadFrom(p []byte) (int, net.Addr, error) {
	return 0, nil, errors.New("network is down")
}
func (erringSocket) WriteTo(p []byte, addr net.Addr) (int, error) { return len(p), nil }
func (erringSocket) Close() error                                 { return nil }
func (erringSocket) LocalAddr() net.Addr                          { return nil }
func (erringSocket) SetDeadline(time.Time) error                  { return nil }
func (erringSocket) SetReadDeadline(time.Time) error              { return nil }
func (erringSocket) SetWriteDeadline(time.Time) error             { return nil }

// A pump whose socket fails continuously used to write one log line every
// 50 ms for as long as the failure lasted. The first socketErrorLogLimit errors
// are logged one by one; past that the pump sums them up once a minute, so a
// network that is down for hours cannot flood the log.
func TestADatagramPumpStopsLoggingReadErrorsOneByOne(t *testing.T) {
	var sink lockedSink
	p := &DatagramPump{Socket: erringSocket{}, Logger: log.New(&sink, "", 0)}
	p.prepare()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run()
	}()

	// The 50 ms retry spreads the first socketErrorLogLimit lines over a few
	// seconds; the summary follows the last of them at once.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if strings.Contains(sink.String(), "read error(s) so far") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no summary line appeared; the log holds %d lines:\n%s",
				strings.Count(sink.String(), "\n"), sink.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.closed.Store(true)
	<-done

	if got := strings.Count(sink.String(), "datagram pump: read error:"); got != socketErrorLogLimit {
		t.Fatalf("the pump logged %d individual read errors, want %d", got, socketErrorLogLimit)
	}
	if got := strings.Count(sink.String(), "read error(s) so far"); got != 1 {
		t.Fatalf("the pump wrote %d summary lines in the first minute, want 1", got)
	}
}
