package reliable

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Token:            []byte("shared-test-token"),
		HandshakeTimeout: 2 * time.Second,
		IdleTimeout:      15 * time.Second,
	}
}

// pattern builds n bytes that encode their own offset, so a mismatch can be
// reported at the first byte that differs.
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
}

func firstDiff(got, want []byte) int {
	for i := range got {
		if i >= len(want) || got[i] != want[i] {
			return i
		}
	}
	return len(got)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// lossyConn drops a fraction of the datagrams written through it, using a fixed
// seed so a failing run can be repeated. drop can be changed while a connection
// is up, which is how the tests model a link that dies after the handshake.
type lossyConn struct {
	packetConn
	mu      sync.Mutex
	rnd     *rand.Rand
	drop    float64
	sent    int
	dropped int
}

func newLossy(pc packetConn, seed int64, drop float64) *lossyConn {
	return &lossyConn{packetConn: pc, rnd: rand.New(rand.NewSource(seed)), drop: drop}
}

func (l *lossyConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	l.mu.Lock()
	l.sent++
	drop := l.rnd.Float64() < l.drop
	if drop {
		l.dropped++
	}
	l.mu.Unlock()
	if drop {
		return len(b), nil
	}
	return l.packetConn.WriteTo(b, addr)
}

func (l *lossyConn) setDrop(drop float64) {
	l.mu.Lock()
	l.drop = drop
	l.mu.Unlock()
}

func (l *lossyConn) stats() (sent, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent, l.dropped
}

// dupConn sends every datagram twice, which exercises the duplicate handling in
// the receive path.
type dupConn struct {
	packetConn
	mu sync.Mutex
	n  int
}

func (d *dupConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := d.packetConn.WriteTo(b, addr)
	if err == nil {
		d.packetConn.WriteTo(b, addr)
		d.mu.Lock()
		d.n++
		d.mu.Unlock()
	}
	return n, err
}

func (d *dupConn) doubled() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

type wrapFunc func(packetConn) packetConn

func listenSocket(t *testing.T, cfg Config, wrap wrapFunc) *Socket {
	t.Helper()
	s, err := newSocket("127.0.0.1:0", cfg, wrap)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// punch runs Punch on both sockets towards each other's address, which is what
// two peers do after a rendezvous server has told them where to aim.
func punch(t *testing.T, a, b *Socket) (net.Conn, net.Conn) {
	t.Helper()
	ca, cb, ea, eb := punchErrors(t, a, b)
	if ea != nil || eb != nil {
		t.Fatalf("punch failed: a=%v b=%v", ea, eb)
	}
	return ca, cb
}

func punchErrors(t *testing.T, a, b *Socket) (ca, cb net.Conn, ea, eb error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ca, ea = a.Punch(ctx, b.LocalAddr())
	}()
	go func() {
		defer wg.Done()
		cb, eb = b.Punch(ctx, a.LocalAddr())
	}()
	wg.Wait()
	t.Cleanup(func() {
		if ca != nil {
			ca.Close()
		}
		if cb != nil {
			cb.Close()
		}
	})
	return ca, cb, ea, eb
}

func punchPair(t *testing.T, cfg Config, wrapA, wrapB wrapFunc) (net.Conn, net.Conn) {
	t.Helper()
	a := listenSocket(t, cfg, wrapA)
	b := listenSocket(t, cfg, wrapB)
	return punch(t, a, b)
}

// writeFrame and readFrame give the tests a stream boundary without relying on
// half-close, which net.Conn does not expose.
func writeFrame(w io.Writer, b []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func TestPunchEstablishesStream(t *testing.T) {
	// Listen carries no token, so it can bind but cannot authenticate a punch.
	plain, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if plain.LocalAddr() == nil {
		t.Fatal("Listen reported no local address")
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	a, err := ListenConfig("127.0.0.1:0", testConfig())
	if err != nil {
		t.Fatalf("ListenConfig: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := ListenConfig("127.0.0.1:0", testConfig())
	if err != nil {
		t.Fatalf("ListenConfig: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	ca, cb := punch(t, a, b)

	if _, ok := ca.LocalAddr().(*net.UDPAddr); !ok {
		t.Fatalf("LocalAddr is %T, want *net.UDPAddr", ca.LocalAddr())
	}
	if _, ok := ca.RemoteAddr().(*net.UDPAddr); !ok {
		t.Fatalf("RemoteAddr is %T, want *net.UDPAddr", ca.RemoteAddr())
	}
	if got, want := ca.LocalAddr().String(), a.LocalAddr().String(); got != want {
		t.Fatalf("conn local %s, socket local %s", got, want)
	}
	if got, want := ca.RemoteAddr().String(), b.LocalAddr().String(); got != want {
		t.Fatalf("conn remote %s, peer socket %s", got, want)
	}
	if got, want := cb.RemoteAddr().String(), a.LocalAddr().String(); got != want {
		t.Fatalf("peer conn remote %s, socket local %s", got, want)
	}

	msg := []byte("hello over the punched path")
	go ca.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cb, got); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("peer read %q, want %q", got, msg)
	}
}

// A half-close over a punched path has to behave like one over TCP: the peer reads the
// request and then end of stream, and the reply still comes back. A relay ends each
// direction with CloseWrite when the connection has one and closes the whole stream
// otherwise, so without CloseWrite the reply that a service only sends after it has seen the
// end of the request is lost on the direct path.
func TestCloseWriteHalfClosesTheStream(t *testing.T) {
	a, err := ListenConfig("127.0.0.1:0", testConfig())
	if err != nil {
		t.Fatalf("ListenConfig: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := ListenConfig("127.0.0.1:0", testConfig())
	if err != nil {
		t.Fatalf("ListenConfig: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	ca, cb := punch(t, a, b)

	request := []byte("a request that half-closes")
	if _, err := ca.Write(request); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	writer, ok := ca.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("the punched stream does not offer CloseWrite: %T", ca)
	}
	if err := writer.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// The peer sees the request and then a clean end of stream.
	got, err := io.ReadAll(cb)
	if err != nil {
		t.Fatalf("peer read to end of stream: %v", err)
	}
	if !bytes.Equal(got, request) {
		t.Fatalf("peer read %q, want %q", got, request)
	}

	// And the reply still reaches the half-closed side.
	reply := []byte("the reply after the half-close")
	go func() {
		_, _ = cb.Write(reply)
		_ = cb.Close()
	}()
	back, err := io.ReadAll(ca)
	if err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if !bytes.Equal(back, reply) {
		t.Fatalf("the reply came back as %q, want %q", back, reply)
	}
}

func TestAcceptLearnsPeerAddress(t *testing.T) {
	cfg := testConfig()
	// A wildcard bind, like a peer behind a NAT that only knows its own port.
	listener, err := newSocket(":0", cfg, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.LocalAddr().(*net.UDPAddr).Port
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}

	client := listenSocket(t, cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type result struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		accepted <- result{conn, err}
	}()

	dialed, err := client.Punch(ctx, peer)
	if err != nil {
		t.Fatalf("punch: %v", err)
	}
	t.Cleanup(func() { dialed.Close() })

	res := <-accepted
	if res.err != nil {
		t.Fatalf("accept: %v", res.err)
	}
	t.Cleanup(func() { res.conn.Close() })

	if got, want := res.conn.RemoteAddr().String(), client.LocalAddr().String(); got != want {
		t.Fatalf("accepted remote %s, want %s", got, want)
	}
	go res.conn.Write([]byte("accepted"))
	buf := make([]byte, 8)
	if _, err := io.ReadFull(dialed, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "accepted" {
		t.Fatalf("read %q", buf)
	}
}

func TestEmptyTokenRejected(t *testing.T) {
	s := listenSocket(t, Config{}, nil)
	if _, err := s.Punch(context.Background(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err == nil {
		t.Fatal("punch with no token succeeded")
	}
	if _, err := s.Accept(context.Background()); err == nil {
		t.Fatal("accept with no token succeeded")
	}
}

func TestMismatchedTokenFails(t *testing.T) {
	cfgA := testConfig()
	cfgA.HandshakeTimeout = 300 * time.Millisecond
	cfgB := testConfig()
	cfgB.HandshakeTimeout = 300 * time.Millisecond
	cfgB.Token = []byte("a different token")

	a := listenSocket(t, cfgA, nil)
	b := listenSocket(t, cfgB, nil)

	start := time.Now()
	ca, cb, ea, eb := punchErrors(t, a, b)
	elapsed := time.Since(start)

	if ca != nil || cb != nil {
		t.Fatalf("mismatched tokens connected: a=%v b=%v", ca != nil, cb != nil)
	}
	if ea == nil || eb == nil {
		t.Fatalf("want both punches to fail, got a=%v b=%v", ea, eb)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("handshake took %v, want it bounded by HandshakeTimeout", elapsed)
	}
}

func TestRoundTripMegabytes(t *testing.T) {
	a, b := punchPair(t, testConfig(), nil, nil)

	const size = 3 << 20
	up := pattern(0x11, size)
	down := pattern(0x22, size)

	errc := make(chan error, 1)
	go func() {
		got, err := readFrame(b)
		if err != nil {
			errc <- fmt.Errorf("peer read: %w", err)
			return
		}
		if !bytes.Equal(got, up) {
			errc <- fmt.Errorf("peer read %d bytes, first mismatch at %d", len(got), firstDiff(got, up))
			return
		}
		if err := writeFrame(b, down); err != nil {
			errc <- fmt.Errorf("peer write: %w", err)
			return
		}
		errc <- nil
	}()

	if err := writeFrame(a, up); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readFrame(a)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, down) {
		t.Fatalf("read %d bytes, first mismatch at %d", len(got), firstDiff(got, down))
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// msgBody is the payload the concurrency test checks byte for byte; its length
// is derived from the message so the reader can split the stream back up.
func msgBody(id byte, seq uint32) []byte {
	n := 1 + (int(id)+int(seq))%40
	body := make([]byte, n)
	for i := range body {
		body[i] = id ^ byte(seq) ^ byte(i)
	}
	return body
}

func TestConcurrentWritesArriveInOrder(t *testing.T) {
	a, b := punchPair(t, testConfig(), nil, nil)

	const (
		writers   = 8
		perWriter = 200
	)

	var wg sync.WaitGroup
	writeErr := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			for seq := uint32(0); seq < perWriter; seq++ {
				body := msgBody(id, seq)
				msg := make([]byte, 0, 6+len(body))
				msg = append(msg, id)
				msg = binary.BigEndian.AppendUint32(msg, seq)
				msg = append(msg, byte(len(body)))
				msg = append(msg, body...)
				if _, err := a.Write(msg); err != nil {
					writeErr <- fmt.Errorf("writer %d: %w", id, err)
					return
				}
			}
		}(byte(w))
	}
	go func() {
		wg.Wait()
		close(writeErr)
	}()

	last := make([]int, writers)
	hdr := make([]byte, 6)
	for seen := 0; seen < writers*perWriter; seen++ {
		if _, err := io.ReadFull(b, hdr); err != nil {
			t.Fatalf("read header after %d messages: %v", seen, err)
		}
		id := int(hdr[0])
		seq := binary.BigEndian.Uint32(hdr[1:5])
		size := int(hdr[5])
		if id >= writers {
			t.Fatalf("message %d has writer id %d", seen, id)
		}
		// Every writer's own messages must arrive in the order it sent them,
		// and no message may be interleaved with or corrupted by another.
		if int(seq) != last[id] {
			t.Fatalf("writer %d sent message %d, got %d", id, last[id], seq)
		}
		last[id]++
		body := make([]byte, size)
		if _, err := io.ReadFull(b, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Equal(body, msgBody(byte(id), seq)) {
			t.Fatalf("writer %d message %d arrived corrupted", id, seq)
		}
	}
	for err := range writeErr {
		t.Fatal(err)
	}
}

func TestLargeWriteIsSplitAndReassembled(t *testing.T) {
	var counter *lossyConn
	a, b := punchPair(t, testConfig(), func(pc packetConn) packetConn {
		counter = newLossy(pc, 1, 0)
		return counter
	}, nil)

	const size = 600 << 10
	payload := pattern(0x33, size)

	errc := make(chan error, 1)
	go func() {
		got := make([]byte, size)
		if _, err := io.ReadFull(b, got); err != nil {
			errc <- err
			return
		}
		if !bytes.Equal(got, payload) {
			errc <- fmt.Errorf("first mismatch at %d", firstDiff(got, payload))
			return
		}
		errc <- nil
	}()

	if _, err := a.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("read: %v", err)
	}

	sent, _ := counter.stats()
	if want := size / (DefaultMTU - headerSize - macSize); sent < want {
		t.Fatalf("one Write of %d bytes produced %d datagrams, want at least %d", size, sent, want)
	}
}

func TestDeadlines(t *testing.T) {
	a, b := punchPair(t, testConfig(), nil, nil)
	past := time.Now().Add(-time.Second)

	if err := a.SetReadDeadline(past); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := a.Read(make([]byte, 16)); !isTimeout(err) {
		t.Fatalf("read with an expired deadline returned %v", err)
	}

	if err := a.SetWriteDeadline(past); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if _, err := a.Write([]byte("x")); !isTimeout(err) {
		t.Fatalf("write with an expired deadline returned %v", err)
	}

	// Clearing the deadline restores normal operation.
	if err := a.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	msg := []byte("after the deadline")
	if _, err := a.Write(msg); err != nil {
		t.Fatalf("write after clearing the deadline: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("read %q, want %q", got, msg)
	}

	// A future read deadline expires too.
	if err := a.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	if _, err := a.Read(make([]byte, 16)); !isTimeout(err) {
		t.Fatalf("read returned %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("read deadline fired after %v", elapsed)
	}
	if err := a.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
}

func TestCloseUnblocksBlockedRead(t *testing.T) {
	t.Run("local close", func(t *testing.T) {
		a, b := punchPair(t, testConfig(), nil, nil)
		errc := make(chan error, 1)
		go func() {
			_, err := b.Read(make([]byte, 16))
			errc <- err
		}()
		time.Sleep(50 * time.Millisecond)

		start := time.Now()
		if err := b.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 1100*time.Millisecond {
			t.Fatalf("Close blocked for %v", elapsed)
		}
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("blocked read returned no error")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("close did not unblock the read")
		}
		if _, err := b.Read(make([]byte, 16)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after close returned %v", err)
		}
		// Close is idempotent.
		if err := b.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		_ = a
	})

	t.Run("peer close", func(t *testing.T) {
		a, b := punchPair(t, testConfig(), nil, nil)
		errc := make(chan error, 1)
		go func() {
			_, err := b.Read(make([]byte, 16))
			errc <- err
		}()
		time.Sleep(50 * time.Millisecond)

		if err := a.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-errc:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read after the peer closed returned %v, want io.EOF", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("peer close did not unblock the read")
		}
	})
}

func TestLossyLinkStillDelivers(t *testing.T) {
	cfg := testConfig()
	cfg.HandshakeTimeout = 5 * time.Second

	var la, lb *lossyConn
	a, b := punchPair(t, cfg,
		func(pc packetConn) packetConn { la = newLossy(pc, 11, 0.2); return la },
		func(pc packetConn) packetConn { lb = newLossy(pc, 22, 0.2); return lb })

	up := pattern(0x44, 256<<10)
	down := pattern(0x55, 256<<10)

	errc := make(chan error, 1)
	go func() {
		got, err := readFrame(b)
		if err != nil {
			errc <- fmt.Errorf("peer read: %w", err)
			return
		}
		if !bytes.Equal(got, up) {
			errc <- fmt.Errorf("peer read %d bytes, first mismatch at %d", len(got), firstDiff(got, up))
			return
		}
		if err := writeFrame(b, down); err != nil {
			errc <- fmt.Errorf("peer write: %w", err)
			return
		}
		errc <- nil
	}()

	if err := writeFrame(a, up); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readFrame(a)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, down) {
		t.Fatalf("read %d bytes, first mismatch at %d", len(got), firstDiff(got, down))
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	sentA, droppedA := la.stats()
	sentB, droppedB := lb.stats()
	if droppedA == 0 || droppedB == 0 {
		t.Fatalf("the lossy link dropped nothing: a=%d/%d b=%d/%d", droppedA, sentA, droppedB, sentB)
	}
	t.Logf("dropped %d of %d datagrams one way and %d of %d the other", droppedA, sentA, droppedB, sentB)
}

func TestDuplicateDatagramsAreIdempotent(t *testing.T) {
	var da, db *dupConn
	a, b := punchPair(t, testConfig(),
		func(pc packetConn) packetConn { da = &dupConn{packetConn: pc}; return da },
		func(pc packetConn) packetConn { db = &dupConn{packetConn: pc}; return db })

	payload := pattern(0x66, 256<<10)
	errc := make(chan error, 1)
	go func() {
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(b, got); err != nil {
			errc <- err
			return
		}
		if !bytes.Equal(got, payload) {
			errc <- fmt.Errorf("first mismatch at %d", firstDiff(got, payload))
			return
		}
		errc <- nil
	}()

	if _, err := a.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("read: %v", err)
	}
	if da.doubled() == 0 || db.doubled() == 0 {
		t.Fatal("no datagram was duplicated")
	}
}

// TestDeadLinkFailsKnownShort checks that a link that loses everything ends the
// connection instead of leaving Read and Write blocked forever.
func TestIdleTimeoutFails(t *testing.T) {
	cfg := testConfig()
	cfg.IdleTimeout = 300 * time.Millisecond

	a, b := punchPair(t, cfg, nil, nil)

	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 16))
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("idle timeout returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle connection never timed out")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("connection gave up after %v, before the idle timeout", elapsed)
	}

	// The idle timeout is measured from the last segment either way, so traffic
	// keeps the connection alive.
	a, b = punchPair(t, cfg, nil, nil)
	for i := 0; i < 4; i++ {
		if _, err := a.Write([]byte("ping")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(b, buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestDeadLinkFailsKnownShort(t *testing.T) {
	cfg := testConfig()
	cfg.RetransmitLimit = 2

	var la, lb *lossyConn
	a, b := punchPair(t, cfg,
		func(pc packetConn) packetConn { la = newLossy(pc, 33, 0); return la },
		func(pc packetConn) packetConn { lb = newLossy(pc, 44, 0); return lb })

	la.setDrop(1)
	lb.setDrop(1)

	start := time.Now()
	for _, conn := range []net.Conn{a, b} {
		if _, err := conn.Write(pattern(0x77, 32<<10)); err != nil {
			t.Fatalf("write into the dead link: %v", err)
		}
	}

	errc := make(chan error, 2)
	for _, conn := range []net.Conn{a, b} {
		go func(c net.Conn) {
			_, err := c.Read(make([]byte, 4096))
			errc <- err
		}(conn)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("read on a dead link returned no error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("connection hung instead of failing after the retransmit limit")
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("failure took %v", elapsed)
	}
}

func TestMalformedDatagramsIgnored(t *testing.T) {
	cfg := testConfig()
	cfg.HandshakeTimeout = 2 * time.Second

	a := listenSocket(t, cfg, nil)
	b := listenSocket(t, cfg, nil)

	junkTo := func(target net.Addr) *net.UDPConn {
		conn, err := net.DialUDP("udp", nil, target.(*net.UDPAddr))
		if err != nil {
			t.Fatalf("dial junk: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	junk := []*net.UDPConn{junkTo(a.LocalAddr()), junkTo(b.LocalAddr())}

	stop := make(chan struct{})
	var spam sync.WaitGroup
	spam.Add(1)
	go func() {
		defer spam.Done()
		rnd := rand.New(rand.NewSource(7))
		buf := make([]byte, 2048)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n := rnd.Intn(len(buf))
			rnd.Read(buf[:n])
			for _, conn := range junk {
				conn.Write(buf[:n])
			}
			// The spray is throttled so it cannot starve the connection under
			// test when the binary gets a single CPU.
			time.Sleep(2 * time.Millisecond)
		}
	}()

	ca, cb := punch(t, a, b)

	// Datagrams that authenticate but make no sense must be dropped as well.
	s := ca.(*stream)
	odd := [][]byte{
		marshalSegment(nil, cfg.Token, segment{typ: typeHello}),
		marshalSegment(nil, cfg.Token, segment{typ: typeHelloAck, session: s.session}),
		marshalSegment(nil, cfg.Token, segment{typ: typeData, session: s.session + 1, seq: 0, window: maxWindow, payload: make([]byte, 64)}),
		marshalSegment(nil, cfg.Token, segment{typ: typeData, session: s.session, seq: 1 << 31, window: maxWindow, payload: make([]byte, 64)}),
		marshalSegment(nil, cfg.Token, segment{typ: typeData, session: s.session, seq: 0xFFFFFF00, window: maxWindow, payload: make([]byte, 64)}),
		marshalSegment(nil, cfg.Token, segment{typ: typeAck, session: s.session, ack: 1 << 30}),
		{},
		{0, 1, 2},
	}
	for _, d := range odd {
		junk[0].Write(d)
		junk[1].Write(d)
	}

	// The connection still works.
	payload := pattern(0x88, 64<<10)
	errc := make(chan error, 1)
	go func() {
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(cb, got); err != nil {
			errc <- err
			return
		}
		if !bytes.Equal(got, payload) {
			errc <- fmt.Errorf("first mismatch at %d", firstDiff(got, payload))
			return
		}
		errc <- nil
	}()
	if _, err := ca.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("read: %v", err)
	}

	close(stop)
	spam.Wait()
}

func TestNoGoroutineLeak(t *testing.T) {
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		a, b := punchPair(t, testConfig(), nil, nil)
		if _, err := a.Write(pattern(byte(i), 4096)); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, 4096)
		if _, err := io.ReadFull(b, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := a.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= base+4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: %d before, %d after", base, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
