package net

import (
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// The path picker runs its counter in uint64; a 32-bit build that converted
// the counter to int before the modulo turned every counter value past 2^31
// negative, the modulo stayed negative, and pathAt silently dropped every
// datagram until the counter wrapped. Three paths make the divergence visible:
// 2^31 modulo 3 is 2, while the truncated negative value lands on -2.
func TestAPickPastTwoToTheThirtyOneStillFindsAPath(t *testing.T) {
	session := &datagramSession{
		addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65000},
		done: make(chan struct{}),
	}
	framers := make([]*protocol.Framer, 3)
	for i := range framers {
		framers[i] = pickTestFramer()
	}
	session.setFramers(framers, nil)
	session.next.Store(1<<31 - 1)

	for i := 0; i < 1000; i++ {
		if session.pick() == nil {
			t.Fatalf("pick %d after the 2^31 boundary returned no path", i)
		}
	}
}

// A first fragment of full frame size plus a full-size continuation used to
// push the assembly buffer to twice the limit before the check ran, and the
// over-limit buffer stayed for the connection's lifetime. The check has to
// refuse the append before it grows the buffer.
func TestAContinuationPastTheLimitIsRefusedBeforeItIsAppended(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// A client-side conn expects unmasked frames from its peer.
	c := &wsConn{Conn: client, reader: client, client: true}
	defer c.Close()

	// The peer writes two frames: a first binary fragment and a continuation,
	// each carrying the full message payload and neither carrying FIN.
	go func() {
		_ = writeRawFrame(server, opBinary, false, make([]byte, maxMessagePayload))
		_ = writeRawFrame(server, opContinuation, false, make([]byte, maxMessagePayload))
	}()

	buf := make([]byte, 4096)
	_, err := c.Read(buf)
	if err == nil {
		t.Fatal("a message past the payload limit was accepted")
	}
	if len(c.assembling) > maxMessagePayload {
		t.Fatalf("the assembly buffer holds %d bytes, more than the %d-byte limit", len(c.assembling), maxMessagePayload)
	}
}

// writeRawFrame writes one RFC 6455 frame without a mask, the shape a server
// side peer sends.
func writeRawFrame(w io.Writer, opcode byte, fin bool, payload []byte) error {
	var header [10]byte
	header[0] = opcode
	if fin {
		header[0] |= 0x80
	}
	length := len(payload)
	switch {
	case length < 126:
		header[1] = byte(length)
		if _, err := w.Write(header[:2]); err != nil {
			return err
		}
	case length <= 0xFFFF:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(length))
		if _, err := w.Write(header[:4]); err != nil {
			return err
		}
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(length))
		if _, err := w.Write(header[:10]); err != nil {
			return err
		}
	}
	_, err := w.Write(payload)
	return err
}
