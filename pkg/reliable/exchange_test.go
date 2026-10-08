package reliable

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// respondFunc is a trivial rendezvous service: it answers every request with the
// bytes the test wants it to send.
func startRendezvous(t *testing.T, respond func(request []byte, from net.Addr) [][]byte) net.Addr {
	t.Helper()

	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("rendezvous socket: %v", err)
	}
	t.Cleanup(func() { socket.Close() })

	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := socket.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, reply := range respond(append([]byte(nil), buf[:n]...), from) {
				_, _ = socket.WriteTo(reply, from)
			}
		}
	}()
	return socket.LocalAddr()
}

func TestExchangeReturnsTheAcceptedReply(t *testing.T) {
	server := startRendezvous(t, func(request []byte, from net.Addr) [][]byte {
		return [][]byte{[]byte("reply:" + string(request))}
	})

	socket, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, from, err := socket.Exchange(ctx, server, []byte("ping"), func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("reply:"))
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if string(reply) != "reply:ping" {
		t.Fatalf("reply is %q", reply)
	}
	if from.String() != server.String() {
		t.Fatalf("reply came from %q, want %q", from, server)
	}
}

func TestExchangeIgnoresDatagramsThePredicateRejects(t *testing.T) {
	// The first datagrams are noise, as a peer's punch hellos would be on a real
	// punch socket; the accepted reply arrives afterwards. The service answers on
	// its own goroutine, so the counter it keeps has to be atomic.
	var noise atomic.Int64
	server := startRendezvous(t, func(request []byte, from net.Addr) [][]byte {
		if noise.Add(1) < 4 {
			return [][]byte{[]byte("noise")}
		}
		return [][]byte{[]byte("ATP2-wanted")}
	})

	socket, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, _, err := socket.Exchange(ctx, server, []byte("hello"), func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("ATP2"))
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if string(reply) != "ATP2-wanted" {
		t.Fatalf("reply is %q", reply)
	}
	if asked := noise.Load(); asked < 4 {
		t.Fatalf("the rendezvous was asked only %d times", asked)
	}
}

func TestExchangeRejectsEmptyInputs(t *testing.T) {
	socket, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	server, err := net.ResolveUDPAddr("udp", "127.0.0.1:9")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ctx := context.Background()
	accept := func([]byte) bool { return true }

	if _, _, err := socket.Exchange(ctx, nil, []byte("x"), accept); err == nil {
		t.Fatal("a nil server was accepted")
	}
	if _, _, err := socket.Exchange(ctx, server, nil, accept); err == nil {
		t.Fatal("an empty payload was accepted")
	}
	if _, _, err := socket.Exchange(ctx, server, []byte("x"), nil); err == nil {
		t.Fatal("a nil accept predicate was accepted")
	}
}

func TestExchangeGivesUpWhenTheDeadlinePasses(t *testing.T) {
	socket, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	// A socket that is bound but never answers.
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("silent socket: %v", err)
	}
	defer silent.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, _, err = socket.Exchange(ctx, silent.LocalAddr(), []byte("ping"), func([]byte) bool { return true })
	if err == nil {
		t.Fatal("exchange returned without an answer")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange returned %v, want a deadline error", err)
	}
}

func TestExchangeLeavesTheSocketUsableForPunching(t *testing.T) {
	server := startRendezvous(t, func(request []byte, from net.Addr) [][]byte {
		// Answer with this peer's own observed address, so both ends punch
		// towards each other exactly as they would after a real rendezvous.
		return [][]byte{[]byte(from.String())}
	})

	token := []byte("a-shared-punch-token")

	left, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen left: %v", err)
	}
	defer left.Close()
	right, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen right: %v", err)
	}
	defer right.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leftReply, _, err := left.Exchange(ctx, server, []byte("whoami"), func([]byte) bool { return true })
	if err != nil {
		t.Fatalf("left exchange: %v", err)
	}
	rightReply, _, err := right.Exchange(ctx, server, []byte("whoami"), func([]byte) bool { return true })
	if err != nil {
		t.Fatalf("right exchange: %v", err)
	}

	leftPeer, err := net.ResolveUDPAddr("udp", string(rightReply))
	if err != nil {
		t.Fatalf("left peer: %v", err)
	}
	rightPeer, err := net.ResolveUDPAddr("udp", string(leftReply))
	if err != nil {
		t.Fatalf("right peer: %v", err)
	}

	// The token is only known after the exchange, exactly as it is in the tunnel.
	left.SetToken(token)
	right.SetToken(token)

	type outcome struct {
		conn net.Conn
		err  error
	}
	leftDone := make(chan outcome, 1)
	rightDone := make(chan outcome, 1)

	go func() {
		conn, err := left.Punch(ctx, leftPeer)
		leftDone <- outcome{conn, err}
	}()
	go func() {
		conn, err := right.Punch(ctx, rightPeer)
		rightDone <- outcome{conn, err}
	}()

	leftResult := <-leftDone
	rightResult := <-rightDone
	if leftResult.err != nil {
		t.Fatalf("left punch: %v", leftResult.err)
	}
	if rightResult.err != nil {
		t.Fatalf("right punch: %v", rightResult.err)
	}
	defer leftResult.conn.Close()
	defer rightResult.conn.Close()

	payload := []byte("through the punched path")
	if _, err := leftResult.conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = rightResult.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, len(payload))
	read := 0
	for read < len(payload) {
		n, err := rightResult.conn.Read(buf[read:])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		read += n
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("received %q, want %q", buf, payload)
	}
}

func TestSetTokenIsIgnoredOnceTheSocketIsInUse(t *testing.T) {
	socket, err := Listen(":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socket.Close()

	// No token: the handshake must refuse to start.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	peer, err := net.ResolveUDPAddr("udp", "127.0.0.1:9")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := socket.Punch(ctx, peer); err == nil {
		t.Fatal("a punch without a token was accepted")
	}

	socket.SetToken([]byte("now-we-have-one"))
	if _, err := socket.Punch(ctx, peer); err == nil {
		t.Fatal("a punch with a token but no peer was accepted")
	}
}
