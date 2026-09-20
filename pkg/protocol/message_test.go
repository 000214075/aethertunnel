package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

func newFramer(t *testing.T, cipher *crypto.Cipher) (*Framer, *Framer) {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	return NewFramer(left, cipher, 0), NewFramer(right, cipher, 0)
}

func TestHeartbeatHasNoPayloadAndStillRoundTrips(t *testing.T) {
	// The v1 reader rejected payloadLen == 0 while the v1 writer used exactly
	// that for heartbeats, so every heartbeat killed the connection.
	sender, receiver := newFramer(t, nil)

	go func() {
		if err := sender.WriteFrame(&Message{Type: TypeHeartbeat}); err != nil {
			t.Errorf("write heartbeat: %v", err)
		}
	}()

	msg, err := receiver.ReadFrame()
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if msg.Type != TypeHeartbeat {
		t.Fatalf("type = %s, want heartbeat", msg.Type)
	}
	if len(msg.Payload) != 0 {
		t.Fatalf("payload = %q, want empty", msg.Payload)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	sender, receiver := newFramer(t, nil)
	request := DataOpen{Session: "abc123", Proxy: "ssh", StreamID: "deadbeef"}

	go func() {
		if err := sender.WriteJSON(TypeDataOpen, request); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	var got DataOpen
	if err := receiver.ReadJSON(TypeDataOpen, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != request {
		t.Fatalf("got %+v, want %+v", got, request)
	}
}

func TestReadJSONRejectsUnexpectedType(t *testing.T) {
	sender, receiver := newFramer(t, nil)

	go func() {
		_ = sender.WriteJSON(TypeError, ErrorPayload{Error: "boom"})
	}()

	var got DataOpen
	err := receiver.ReadJSON(TypeDataOpen, &got)
	if err == nil || !strings.Contains(err.Error(), "expected data-open") {
		t.Fatalf("expected a type mismatch error, got %v", err)
	}
}

func TestOversizedFrameIsRefusedBeforeAllocating(t *testing.T) {
	sender, receiver := newFramer(t, nil)

	// Hand-write a header that announces far more than the limit. It must be
	// rejected from the header alone, without allocating the announced size.
	header := make([]byte, 6)
	header[0] = byte(TypeDataOpen)
	binary.BigEndian.PutUint32(header[2:], 1<<30)
	go func() { _, _ = sender.conn.(net.Conn).Write(header) }()

	_, err := receiver.ReadFrame()
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestWriteRefusesOversizedFrame(t *testing.T) {
	sender, _ := newFramer(t, nil)
	big := &Message{Type: TypeDataOpen, Payload: make([]byte, DefaultMaxPayload+1)}
	if err := sender.WriteFrame(big); err == nil {
		t.Fatal("expected an error when sending more than the frame limit")
	}
}

func TestEncryptedRoundTrip(t *testing.T) {
	cipher, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "token", "salt")
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := newFramer(t, cipher)

	go func() {
		if err := sender.WriteJSON(TypeAuthRequest, AuthRequest{Token: "token", Protocol: ProtocolVersion}); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	var got AuthRequest
	if err := receiver.ReadJSON(TypeAuthRequest, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Token != "token" {
		t.Fatalf("token = %q", got.Token)
	}
	if got.Protocol != ProtocolVersion {
		t.Fatalf("protocol = %d", got.Protocol)
	}
}

func TestEncryptionMismatchIsReportedClearly(t *testing.T) {
	// A peer that sends cleartext to an encrypted listener must get a message
	// that says so, instead of an endless stream of authentication failures.
	cipher, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "token", "salt")
	if err != nil {
		t.Fatal(err)
	}

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	cleartextWriter := NewFramer(left, nil, 0)
	encryptedReader := NewFramer(right, cipher, 0)

	go func() { _ = cleartextWriter.WriteFrame(&Message{Type: TypeHeartbeat}) }()

	_, err = encryptedReader.ReadFrame()
	if err == nil || !strings.Contains(err.Error(), "encryption mismatch") {
		t.Fatalf("expected an encryption mismatch error, got %v", err)
	}
}

func TestFrameHeaderLayoutIsStable(t *testing.T) {
	// Pins the documented layout: type(1) flags(1) length(4 BE) payload.
	sender, receiver := newFramer(t, nil)
	payload := []byte("payload")

	go func() { _ = sender.WriteFrame(&Message{Type: TypeRegisterProxy, Payload: payload}) }()

	raw := make([]byte, 6+len(payload))
	if _, err := readFull(receiver.conn.(net.Conn), raw); err != nil {
		t.Fatalf("read raw frame: %v", err)
	}
	if raw[0] != byte(TypeRegisterProxy) {
		t.Fatalf("type byte = %d", raw[0])
	}
	if raw[1] != 0 {
		t.Fatalf("flags = %d, want 0 for cleartext", raw[1])
	}
	if got := binary.BigEndian.Uint32(raw[2:6]); got != uint32(len(payload)) {
		t.Fatalf("length = %d, want %d", got, len(payload))
	}
	if !bytes.Equal(raw[6:], payload) {
		t.Fatalf("payload = %q", raw[6:])
	}
}

func TestMessageTypeStrings(t *testing.T) {
	if got := TypeHeartbeatAck.String(); got != "heartbeat-ack" {
		t.Fatalf("String() = %q", got)
	}
	if got := MessageType(200).String(); !strings.HasPrefix(got, "unknown") {
		t.Fatalf("String() = %q", got)
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
