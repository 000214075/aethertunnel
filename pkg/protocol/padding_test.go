package protocol

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

func TestPaddingHidesTheExactPayloadLength(t *testing.T) {
	// Payload sizes that fall into the same bucket must produce frames of
	// identical length on the wire. Each case writes exactly one frame and
	// inspects the raw bytes.
	const padTo = 64

	var observed []int
	for _, size := range []int{1, 5, 30, 55} {
		sender, receiver := newFramerWithOptions(t, nil, FramerOptions{PadTo: padTo})
		payload := bytes.Repeat([]byte("x"), size)

		go func() { _ = sender.WriteFrame(&Message{Type: TypeDataOpen, Payload: payload}) }()

		raw := make([]byte, 6+nearestMultiple(4+size, padTo))
		if _, err := readFull(receiver.conn.(net.Conn), raw); err != nil {
			t.Fatalf("read raw frame: %v", err)
		}
		length := int(binary.BigEndian.Uint32(raw[2:6]))
		if length%padTo != 0 {
			t.Fatalf("a %d byte payload produced a %d byte frame, not a multiple of %d",
				size, length, padTo)
		}
		if raw[1]&flagPadded == 0 {
			t.Fatalf("the padded flag is not set for a %d byte payload", size)
		}
		if declared := int(binary.BigEndian.Uint32(raw[6:10])); declared != size {
			t.Fatalf("the inner length prefix says %d, want %d", declared, size)
		}
		observed = append(observed, length)
	}

	for i := 1; i < len(observed); i++ {
		if observed[i] != observed[0] {
			t.Fatalf("payloads in the same bucket produced different frame lengths: %v", observed)
		}
	}
}

func TestPaddingBucketsGrowWithThePayload(t *testing.T) {
	// A payload that crosses a bucket boundary must still be padded up, never
	// down: the declared frame length may grow, never shrink below the data.
	const padTo = 64

	for _, size := range []int{1, 60, 61, 124, 125} {
		sender, receiver := newFramerWithOptions(t, nil, FramerOptions{PadTo: padTo})
		payload := bytes.Repeat([]byte("x"), size)

		go func() { _ = sender.WriteFrame(&Message{Type: TypeDataOpen, Payload: payload}) }()

		raw := make([]byte, 6+nearestMultiple(4+size, padTo))
		if _, err := readFull(receiver.conn.(net.Conn), raw); err != nil {
			t.Fatalf("read raw frame for %d bytes: %v", size, err)
		}
		length := int(binary.BigEndian.Uint32(raw[2:6]))
		if length < 4+size {
			t.Fatalf("a %d byte payload was padded down to %d bytes", size, length)
		}
		if length%padTo != 0 {
			t.Fatalf("a %d byte payload produced a %d byte frame", size, length)
		}
	}
}

func TestPaddedFrameRoundTrips(t *testing.T) {
	for _, size := range []int{1, 5, 30, 55, 300} {
		sender, receiver := newFramerWithOptions(t, nil, FramerOptions{PadTo: 64})
		payload := bytes.Repeat([]byte("y"), size)

		go func() { _ = sender.WriteFrame(&Message{Type: TypeDataOpen, Payload: payload}) }()

		msg, err := receiver.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame for a %d byte payload: %v", size, err)
		}
		if !bytes.Equal(msg.Payload, payload) {
			t.Fatalf("a %d byte payload round-tripped as %d bytes", size, len(msg.Payload))
		}
	}
}

func TestPaddingCombinesWithEncryption(t *testing.T) {
	cipher, err := crypto.NewCipher(crypto.AlgorithmXChaCha20Poly1305, "token", "salt")
	if err != nil {
		t.Fatal(err)
	}

	sender, receiver := newFramerWithOptions(t, cipher, FramerOptions{PadTo: 128})
	payload := []byte("secret payload that must survive padding and encryption")

	go func() { _ = sender.WriteFrame(&Message{Type: TypeDataOpen, Payload: payload}) }()

	msg, err := receiver.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Fatalf("payload = %q", msg.Payload)
	}
	if !msg.Encrypted {
		t.Fatal("the encrypted flag is not set")
	}
}

func TestUnpaddedPeerReadsPaddedFrames(t *testing.T) {
	// Padding is a sender-side choice: the receiver strips it from the flag alone.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	padded := NewFramerWithOptions(left, nil, FramerOptions{PadTo: 32})
	unpadded := NewFramerWithOptions(right, nil, FramerOptions{})

	go func() { _ = padded.WriteFrame(&Message{Type: TypeHeartbeat, Payload: []byte("hi")}) }()

	msg, err := unpadded.ReadFrame()
	if err != nil {
		t.Fatalf("an unpadded peer must accept a padded frame: %v", err)
	}
	if string(msg.Payload) != "hi" {
		t.Fatalf("payload = %q", msg.Payload)
	}
}

func TestMalformedPaddedFrameIsRejected(t *testing.T) {
	sender, receiver := newFramer(t, nil)

	// A frame that claims the padded flag but has no room for the length prefix.
	header := make([]byte, 6)
	header[0] = byte(TypeHeartbeat)
	header[1] = flagPadded
	binary.BigEndian.PutUint32(header[2:], 2)
	frame := append(header, 0x01, 0x02)

	go func() { _, _ = sender.conn.(net.Conn).Write(frame) }()

	if _, err := receiver.ReadFrame(); err == nil {
		t.Fatal("expected an error for a padded frame without a length prefix")
	}
}

func TestPaddedFrameDeclaringTooMuchIsRejected(t *testing.T) {
	sender, receiver := newFramer(t, nil)

	body := make([]byte, 4+8)
	binary.BigEndian.PutUint32(body, 4096) // claims more than it carries
	header := make([]byte, 6)
	header[0] = byte(TypeHeartbeat)
	header[1] = flagPadded
	binary.BigEndian.PutUint32(header[2:], uint32(len(body)))

	go func() { _, _ = sender.conn.(net.Conn).Write(append(header, body...)) }()

	if _, err := receiver.ReadFrame(); err == nil {
		t.Fatal("expected an error for a padded frame that overstates its payload")
	}
}

func TestJitterDelaysWritesWithoutReordering(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	sender := NewFramerWithOptions(left, nil, FramerOptions{Jitter: 5 * time.Millisecond})
	receiver := NewFramerWithOptions(right, nil, FramerOptions{})

	go func() {
		for _, body := range []string{"first", "second", "third"} {
			_ = sender.WriteFrame(&Message{Type: TypeHeartbeat, Payload: []byte(body)})
		}
	}()

	started := time.Now()
	for _, want := range []string{"first", "second", "third"} {
		msg, err := receiver.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if string(msg.Payload) != want {
			t.Fatalf("out of order: got %q, want %q", msg.Payload, want)
		}
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("jitter of 5 ms per frame took %s for three frames", elapsed)
	}
}

func newFramerWithOptions(t *testing.T, cipher *crypto.Cipher, opts FramerOptions) (*Framer, *Framer) {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	return NewFramerWithOptions(left, cipher, opts), NewFramerWithOptions(right, cipher, opts)
}

func nearestMultiple(value, multiple int) int {
	if value%multiple == 0 {
		return value
	}
	return value + multiple - value%multiple
}
