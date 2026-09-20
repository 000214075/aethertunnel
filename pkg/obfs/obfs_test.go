package obfs

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// --- test harness -------------------------------------------------------------

// tcpPair returns two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server := <-accepted
	if server == nil {
		t.Fatal("the listener stopped before accepting")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// recordPair returns two ends wrapped in the record disguise.
func recordPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	left, right := tcpPair(t)
	wrappedLeft, err := Wrap(left, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	wrappedRight, err := Wrap(right, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return wrappedLeft, wrappedRight
}

// --- tests --------------------------------------------------------------------

func TestWrapLeavesTheConnectionAloneWhenTheDisguiseIsNone(t *testing.T) {
	left, right := tcpPair(t)
	for _, disguise := range []string{"", DisguiseNone} {
		wrapped, err := Wrap(left, disguise)
		if err != nil {
			t.Fatalf("Wrap(%q): %v", disguise, err)
		}
		if wrapped != net.Conn(left) {
			t.Fatalf("Wrap(%q) returned a different connection", disguise)
		}
	}
	_ = right.Close()
}

func TestWrapRejectsAnUnknownDisguise(t *testing.T) {
	left, right := tcpPair(t)
	defer right.Close()

	_, err := Wrap(left, "make-it-look-like-http2")
	if err == nil {
		t.Fatal("an unknown disguise was accepted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("not a disguise")) {
		t.Errorf("the error is %v, which does not say what is wrong", err)
	}
}

func TestIsDisguise(t *testing.T) {
	for _, name := range Disguises {
		if !IsDisguise(name) {
			t.Errorf("IsDisguise(%q) is false for a disguise this package implements", name)
		}
	}
	if IsDisguise("noise") {
		t.Error("IsDisguise accepted a name that is not implemented")
	}
}

func TestRecordConnCarriesDataBothWays(t *testing.T) {
	left, right := recordPair(t)

	payload := []byte("a frame carrying a control message")
	if _, err := left.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read %q, want %q", got, payload)
	}

	reply := []byte("and an answer")
	if _, err := right.Write(reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	got = make([]byte, len(reply))
	if _, err := io.ReadFull(left, got); err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("read %q, want %q", got, reply)
	}
}

func TestRecordConnPutsARecordHeaderOnTheWire(t *testing.T) {
	left, right := tcpPair(t)
	wrapped, err := Wrap(left, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer right.Close()

	payload := []byte("observable")
	if _, err := wrapped.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	onWire := make([]byte, recordHeaderLen+len(payload))
	_ = right.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(right, onWire); err != nil {
		t.Fatalf("read the wire bytes: %v", err)
	}
	if onWire[0] != tlsRecordContentTypeApplicationData {
		t.Errorf("the first byte is %#02x, want %#02x", onWire[0], tlsRecordContentTypeApplicationData)
	}
	if onWire[1] != 0x03 || onWire[2] != 0x03 {
		t.Errorf("the version is %#02x%02x, want 0303", onWire[1], onWire[2])
	}
	length := int(onWire[3])<<8 | int(onWire[4])
	if length != len(payload) {
		t.Errorf("the record announces %d bytes, want %d", length, len(payload))
	}
	if !bytes.Equal(onWire[recordHeaderLen:], payload) {
		t.Errorf("the record carries %q, want %q", onWire[recordHeaderLen:], payload)
	}
}

func TestRecordConnSplitsAndReassemblesLargeWrites(t *testing.T) {
	left, right := recordPair(t)

	// Larger than one record, and not a multiple of the record size, so the last
	// record is partial.
	size := maxRecordPayload*2 + 777
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	go func() {
		if _, err := left.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	got := make([]byte, size)
	_ = right.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the reassembled stream differs from what was written")
	}
}

func TestRecordConnKeepsRecordBoundariesOutOfTheStream(t *testing.T) {
	left, right := recordPair(t)

	// Several small writes have to come out as one continuous stream: the reader
	// has no way to see where the writer split them, which is what makes the
	// disguise safe for a length-delimited protocol on top of it.
	parts := []string{"one", "two", "three"}
	var expected []byte
	for _, part := range parts {
		if _, err := left.Write([]byte(part)); err != nil {
			t.Fatalf("write %q: %v", part, err)
		}
		expected = append(expected, part...)
	}

	got := make([]byte, len(expected))
	_ = right.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("read %q, want %q", got, expected)
	}
}

func TestRecordConnSerialisesConcurrentWriters(t *testing.T) {
	left, right := recordPair(t)

	const writers = 4
	const size = 5000

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('a' + id)}, size)
			if _, err := left.Write(payload); err != nil {
				t.Errorf("writer %d: %v", id, err)
			}
		}(i)
	}

	got := make([]byte, writers*size)
	_ = right.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	wg.Wait()

	// Every writer's block has to arrive whole: a header written in the middle of
	// another writer's payload would break the stream, and the read above would
	// have failed.
	for i := 0; i < writers; i++ {
		block := bytes.Repeat([]byte{byte('a' + i)}, size)
		if !bytes.Contains(got, block) {
			t.Fatalf("writer %d's block did not arrive whole", i)
		}
	}
}

func TestRecordConnRejectsAPeerThatIsNotWrapped(t *testing.T) {
	left, right := tcpPair(t)
	wrapped, err := Wrap(left, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	// The other end writes plain bytes, which is what a peer without the disguise
	// sends.
	if _, err := right.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := wrapped.Read(make([]byte, 16)); !errors.Is(err, ErrNotRecord) {
		t.Fatalf("reading an unwrapped stream returned %v, want ErrNotRecord", err)
	}
}

func TestRecordConnReportsATruncatedRecord(t *testing.T) {
	left, right := tcpPair(t)
	wrapped, err := Wrap(left, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	// A header that promises ten bytes followed by two.
	header := []byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x0a}
	if _, err := right.Write(append(header, 'a', 'b')); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = right.Close()

	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(wrapped, make([]byte, 10)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading a truncated record returned %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestRecordConnSkipsEmptyRecords(t *testing.T) {
	left, right := tcpPair(t)
	wrapped, err := Wrap(left, DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer right.Close()

	empty := []byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x00}
	header := []byte{tlsRecordContentTypeApplicationData, 0x03, 0x03, 0x00, 0x02}
	if _, err := right.Write(append(append(empty, header...), 'h', 'i')); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 2)
	if _, err := io.ReadFull(wrapped, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("read %q, want hi", got)
	}
}

func TestRecordConnReadsAnEmptyBufferAsNothing(t *testing.T) {
	left, right := recordPair(t)
	n, err := left.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) returned %d, %v; want 0, nil", n, err)
	}
	_ = right.Close()
}

func TestRecordConnKeepsDeadlines(t *testing.T) {
	left, right := recordPair(t)
	defer right.Close()

	if err := left.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	if _, err := left.Read(make([]byte, 8)); err == nil {
		t.Fatal("Read returned nil although nothing was sent")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the deadline took %s to fire, which means it was not passed through", elapsed)
	}
}

func TestParseHeaderValidation(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   uint16
		fails  bool
	}{
		{"application data over TLS 1.2", []byte{0x17, 0x03, 0x03, 0x00, 0x2a}, 42, false},
		{"handshake record", []byte{0x16, 0x03, 0x03, 0x00, 0x2a}, 0, true},
		{"TLS 1.0 version", []byte{0x17, 0x03, 0x01, 0x00, 0x2a}, 0, true},
		{"not a record at all", []byte{0x47, 0x45, 0x54, 0x20, 0x2f}, 0, true},
		{"too short", []byte{0x17, 0x03}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHeader(tc.header)
			if tc.fails {
				if !errors.Is(err, ErrNotRecord) {
					t.Fatalf("parseHeader returned %v, want ErrNotRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHeader: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseHeader returned %d, want %d", got, tc.want)
			}
		})
	}
}
