package socks

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair hands one real loopback connection split into its two ends: kernel
// buffering makes the half-written requests a refusal produces behave the way
// they do on a real socket, which a synchronous net.Pipe cannot.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	dialed := make(chan net.Conn, 1)
	go func() {
		client, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			return
		}
		dialed <- client
	}()
	server, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	select {
	case client := <-dialed:
		t.Cleanup(func() { server.Close(); client.Close() })
		return server, client
	case <-time.After(5 * time.Second):
		t.Fatal("the loopback pair did not connect")
		return nil, nil
	}
}

// socksVisitor drives the client half of the protocol against ServeConn: it
// speaks the greeting (and RFC 1929 when told to), one CONNECT request, and
// reports the reply code it got.
func socksVisitor(t *testing.T, conn net.Conn, user, password, target string) byte {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	methods := []byte{MethodNoReq}
	if user != "" {
		methods = []byte{methodUserPass}
	}
	greeting := append([]byte{Version, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		t.Fatalf("write the greeting: %v", err)
	}
	answer := make([]byte, 2)
	if _, err := io.ReadFull(conn, answer); err != nil {
		t.Fatalf("read the method: %v", err)
	}
	if answer[1] != methods[0] {
		t.Fatalf("the server chose method 0x%x, want 0x%x", answer[1], methods[0])
	}
	if user != "" {
		auth := append([]byte{1, byte(len(user))}, user...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if _, err := conn.Write(auth); err != nil {
			t.Fatalf("write the credentials: %v", err)
		}
		verdict := make([]byte, 2)
		if _, err := io.ReadFull(conn, verdict); err != nil {
			t.Fatalf("read the auth verdict: %v", err)
		}
		if verdict[1] != 0x00 {
			t.Fatalf("the credentials were refused (0x%02x)", verdict[1])
		}
	}

	request := []byte{Version, CmdConnect, 0x00, AtypDomain, byte(len(target))}
	request = append(request, target...)
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], 80)
	request = append(request, port[:]...)
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if reply[3] == AtypIPv4 {
		bound := make([]byte, 6)
		if _, err := io.ReadFull(conn, bound); err != nil {
			t.Fatalf("read the bound address: %v", err)
		}
	}
	return reply[1]
}

func TestServeConnConnectsWithoutCredentials(t *testing.T) {
	server, visitor := tcpPair(t)

	type exchange struct {
		target net.Conn
		err    error
	}
	dialed := make(chan exchange, 1)
	go func() {
		target, err := ServeConn(server, ServerOptions{
			Dial: func(target string) (net.Conn, error) {
				if target != "origin.example:80" {
					return nil, errors.New("the dial saw " + target)
				}
				one, other := net.Pipe()
				go func() { _, _ = io.Copy(io.Discard, other) }()
				return one, nil
			},
		})
		dialed <- exchange{target, err}
	}()

	if code := socksVisitor(t, visitor, "", "", "origin.example"); code != ReplySucceeded {
		t.Fatalf("the reply code is 0x%02x, want success", code)
	}
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatalf("ServeConn: %v", result.err)
		}
		_ = result.target.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return")
	}
}

func TestServeConnRequiresCredentials(t *testing.T) {
	server, visitor := tcpPair(t)

	dialed := make(chan string, 1)
	go func() {
		_, err := ServeConn(server, ServerOptions{
			Username: "ops",
			Password: "s3cret",
			Dial: func(target string) (net.Conn, error) {
				dialed <- target
				one, other := net.Pipe()
				go func() { _, _ = io.Copy(io.Discard, other) }()
				return one, nil
			},
		})
		if err != nil {
			dialed <- "error: " + err.Error()
		}
	}()

	if code := socksVisitor(t, visitor, "ops", "s3cret", "origin.example"); code != ReplySucceeded {
		t.Fatalf("the reply code is 0x%02x, want success", code)
	}
	select {
	case seen := <-dialed:
		if seen != "origin.example:80" {
			t.Fatalf("the dial saw %q", seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial never happened")
	}
}

func TestServeConnRefusesWrongCredentialsAndUDP(t *testing.T) {
	t.Run("wrong password", func(t *testing.T) {
		server, visitor := tcpPair(t)

		done := make(chan error, 1)
		go func() {
			_, err := ServeConn(server, ServerOptions{Username: "ops", Password: "s3cret"})
			done <- err
		}()

		_ = visitor.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := visitor.Write([]byte{Version, 1, methodUserPass}); err != nil {
			t.Fatalf("write the greeting: %v", err)
		}
		answer := make([]byte, 2)
		if _, err := io.ReadFull(visitor, answer); err != nil {
			t.Fatalf("read the method: %v", err)
		}
		credentials := append([]byte{1, 3}, 'a', 'b', 'c')
		credentials = append(credentials, 5, 'x', 'x', 'x', 'x', 'x')
		if _, err := visitor.Write(credentials); err != nil {
			t.Fatalf("write the credentials: %v", err)
		}
		verdict := make([]byte, 2)
		if _, err := io.ReadFull(visitor, verdict); err != nil {
			t.Fatalf("read the auth verdict: %v", err)
		}
		if verdict[1] == 0x00 {
			t.Fatal("a wrong password was accepted")
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrNoAuthMethod) {
				t.Fatalf("ServeConn = %v, want ErrNoAuthMethod", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ServeConn did not return")
		}
	})

	t.Run("udp associate", func(t *testing.T) {
		server, visitor := tcpPair(t)

		done := make(chan error, 1)
		go func() {
			_, err := ServeConn(server, ServerOptions{})
			done <- err
		}()

		_ = visitor.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := visitor.Write([]byte{Version, 1, MethodNoReq}); err != nil {
			t.Fatalf("write the greeting: %v", err)
		}
		answer := make([]byte, 2)
		if _, err := io.ReadFull(visitor, answer); err != nil {
			t.Fatalf("read the method: %v", err)
		}
		// The refusal answers after the command byte, before the address is
		// read; the kernel buffers the unread tail on a real socket.
		request := []byte{Version, CmdAssoc, 0x00, AtypIPv4, 127, 0, 0, 1, 0, 0}
		if _, err := visitor.Write(request); err != nil {
			t.Fatalf("write the request: %v", err)
		}
		reply := make([]byte, 4)
		if _, err := io.ReadFull(visitor, reply); err != nil {
			t.Fatalf("read the reply: %v", err)
		}
		if reply[1] != ReplyCommandNotSupported {
			t.Fatalf("UDP ASSOCIATE was answered 0x%02x, want 0x07", reply[1])
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("ServeConn = %v, want ErrUnsupported", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ServeConn did not return")
		}
	})
}
