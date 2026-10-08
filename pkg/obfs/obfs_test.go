package obfs

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	wrappedLeft, err := Wrap(left, DisguiseTLSRecord, Dialer)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	wrappedRight, err := Wrap(right, DisguiseTLSRecord, Listener)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return wrappedLeft, wrappedRight
}

// --- tests --------------------------------------------------------------------

// CloseWrite has to reach the connection underneath the disguise. A relay ends each
// direction with CloseWrite when the connection supports it and closes the whole stream
// otherwise, so a wrapper without this method turns a half-close into a full close: the
// bytes the peer was about to send back never arrive.
func TestRecordConnHalfClosesTheConnectionUnderneath(t *testing.T) {
	left, right := recordPair(t)

	if _, err := left.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := left.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// The peer reads the request and then a clean end of stream, which is what tells a
	// service that reads until EOF that the request is complete.
	got, err := io.ReadAll(right)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "request" {
		t.Fatalf("the peer read %q, want request", got)
	}

	// The half-closed side can still read the reply.
	if _, err := right.Write([]byte("reply")); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	reply := make([]byte, len("reply"))
	_ = left.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(left, reply); err != nil {
		t.Fatalf("read the reply after the half-close: %v", err)
	}
	if string(reply) != "reply" {
		t.Fatalf("the reply came back as %q", reply)
	}
}

func TestWrapLeavesTheConnectionAloneWhenTheDisguiseIsNone(t *testing.T) {
	left, right := tcpPair(t)
	for _, disguise := range []string{"", DisguiseNone} {
		wrapped, err := Wrap(left, disguise, Dialer)
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

	_, err := Wrap(left, "make-it-look-like-http2", Dialer)
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
	wrapped, err := Wrap(left, DisguiseTLSRecord, Dialer)
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
	wrapped, err := Wrap(left, DisguiseTLSRecord, Dialer)
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
	wrapped, err := Wrap(left, DisguiseTLSRecord, Dialer)
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
	wrapped, err := Wrap(left, DisguiseTLSRecord, Dialer)
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

// --- the session disguise -----------------------------------------------------

// The session disguise is a real TLS session, so the checks are the ones a real
// session has to pass: bytes survive it, the handshake really happened, and the
// certificate is fresh per connection.

func sessionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	left, right := tcpPair(t)
	wrappedLeft, err := Wrap(left, DisguiseTLSSession, Dialer)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	wrappedRight, err := Wrap(right, DisguiseTLSSession, Listener)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	t.Cleanup(func() {
		_ = wrappedLeft.Close()
		_ = wrappedRight.Close()
	})
	return wrappedLeft, wrappedRight
}

func TestTheSessionDisguiseCarriesBytesThroughARealTLSHandshake(t *testing.T) {
	left, right := sessionPair(t)

	server := make(chan error, 1)
	go func() {
		got := make([]byte, 4)
		if _, err := io.ReadFull(right, got); err != nil {
			server <- err
			return
		}
		if string(got) != "ping" {
			server <- fmt.Errorf("the server read %q, want ping", got)
			return
		}
		_, err := right.Write([]byte("pong"))
		server <- err
	}()

	if _, err := left.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 4)
	_ = left.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(left, reply); err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if string(reply) != "pong" {
		t.Fatalf("the reply came back as %q", reply)
	}
	if err := <-server; err != nil {
		t.Fatalf("the server side: %v", err)
	}

	// The handshake really happened: the session reports it, the negotiated
	// protocol is this tunnel's, and the certificate is the fresh self-signed one.
	state := left.(*sessionConn).ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("the connection reports no completed handshake")
	}
	if state.NegotiatedProtocol != sessionALPN {
		t.Fatalf("the negotiated protocol is %q, want %q", state.NegotiatedProtocol, sessionALPN)
	}
	if got := len(state.PeerCertificates); got != 1 {
		t.Fatalf("the session presents %d certificates, want 1", got)
	}
	if cn := state.PeerCertificates[0].Subject.CommonName; cn != "aethertunnel" {
		t.Fatalf("the certificate's common name is %q, want aethertunnel", cn)
	}
}

func TestTheSessionDisguiseMintsAFreshCertificatePerConnection(t *testing.T) {
	certOf := func(t *testing.T) []byte {
		t.Helper()
		left, right := sessionPair(t)
		go func() {
			_, _ = right.Write([]byte("go"))
		}()
		_ = left.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := left.Read(make([]byte, 2)); err != nil {
			t.Fatalf("read through the session: %v", err)
		}
		state := left.(*sessionConn).ConnectionState()
		if !state.HandshakeComplete {
			t.Fatal("the handshake did not complete")
		}
		return state.PeerCertificates[0].Raw
	}

	first := certOf(t)
	second := certOf(t)
	if bytes.Equal(first, second) {
		t.Fatal("two connections were answered with the same certificate")
	}
}

func TestTheSessionDisguiseSurvivesAProbeThatModelsTLS(t *testing.T) {
	// The record disguise fails here by construction: it has no handshake. The
	// session disguise must give a TLS-speaking detector a session that is one.
	left, right := tcpPair(t)
	wrapped, err := Wrap(right, DisguiseTLSSession, Listener)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer wrapped.Close()
	defer left.Close()

	// The listener's handshake is lazy, so something has to touch the disguised
	// side for it to answer the probe's ClientHello.
	go func() {
		_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = wrapped.Read(make([]byte, 1))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := tls.Client(left, &tls.Config{InsecureSkipVerify: true, ServerName: "aethertunnel"})
	defer probe.Close()
	if err := probe.HandshakeContext(ctx); err != nil {
		t.Fatalf("a TLS probe could not complete the handshake: %v", err)
	}
	state := probe.ConnectionState()
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("the session negotiated %#04x, want TLS 1.2 or newer", state.Version)
	}
	if !wrapped.(*sessionConn).ConnectionState().HandshakeComplete {
		t.Fatal("the disguised side does not report a completed handshake")
	}
}

func TestTheSessionDisguiseRejectsAPeerThatIsNotTLS(t *testing.T) {
	left, right := tcpPair(t)
	wrapped, err := Wrap(right, DisguiseTLSSession, Listener)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer wrapped.Close()
	defer left.Close()

	// A peer that sends what the record disguise would have written is still not
	// TLS: the handshake must refuse it rather than let the tunnel start. The
	// garbage goes over the raw connection, the way an actual non-TLS peer's
	// bytes would arrive.
	if _, err := left.Write([]byte{0x17, 0x03, 0x03, 0x00, 0x01, 'x'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := wrapped.Read(make([]byte, 16)); err == nil {
		t.Fatal("the session disguise accepted a peer that never spoke TLS")
	}
}
