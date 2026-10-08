package net

import (
	"io"
	"log"
	"net"
	"testing"
	"time"
)

// A pump can be driven by calling Run directly, without Start, and a caller that
// does that has no Shutdown of its own to make: closing the socket ends the read
// loop, and the teardown has to happen there too. Without it the reaper kept
// ticking on a pump that would never read again, and every session it was going
// to release stayed in the table until its idle timeout. datagramSession.finish
// is once-guarded, so a caller that already called Shutdown sees nothing extra.
func TestAPumpRunDirectlyEndsItsSessionsWhenTheSocketCloses(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	released := 0
	pump := &DatagramPump{
		Socket:      socket,
		Logger:      log.New(io.Discard, "", 0),
		IdleTimeout: time.Hour,
		OnSession: func(delta int) {
			released += delta
		},
	}
	pump.prepare()
	t.Cleanup(pump.Shutdown)

	// One session in the table, exactly as a datagram from that address leaves it.
	session := &datagramSession{
		pump: pump,
		addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40404},
		done: make(chan struct{}),
	}
	pump.mu.Lock()
	pump.sessions[session.addr.String()] = session
	pump.mu.Unlock()

	if err := socket.Close(); err != nil {
		t.Fatalf("close the socket: %v", err)
	}
	pump.Run()

	if got := pump.Sessions(); got != 0 {
		t.Fatalf("%d session(s) left in the table after Run returned on a closed socket", got)
	}
	if released != -1 {
		t.Fatalf("the session's release was reported %d time(s), want once", -released)
	}
}
