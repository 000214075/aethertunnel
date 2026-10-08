package socks

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// The three refusals below are the ones an operator reads out of a client's
// log, so each has to say what actually happened.

// A greeting that does not start with the SOCKS5 version used to be answered
// with nothing at all: the connection was closed and the visitor could not tell
// "refused" from "the server died" — the one thing ServeConn's documentation
// promises. ReadRequest, the socks5 proxy type's reader, answers the same case
// with the same 0xFF.
func TestANonSOCKS5GreetingIsAnsweredBeforeTheConnectionCloses(t *testing.T) {
	server, visitor := tcpPair(t)

	go func() {
		_, _ = ServeConn(server, ServerOptions{
			Dial: func(string) (net.Conn, error) {
				return nil, errors.New("nothing should be dialed")
			},
		})
	}()

	_ = visitor.SetDeadline(time.Now().Add(5 * time.Second))
	// Only the two-byte header: the version check happens before the method
	// list is read, and leaving no unread byte behind keeps the refusal a
	// delivered reply rather than a reset.
	if _, err := visitor.Write([]byte{0x04, 0x01}); err != nil {
		t.Fatalf("write the greeting: %v", err)
	}
	answer := make([]byte, 2)
	if _, err := io.ReadFull(visitor, answer); err != nil {
		t.Fatalf("the refusal was not answered on the wire: %v", err)
	}
	if answer[0] != Version || answer[1] != MethodNone {
		t.Fatalf("the refusal answered % x, want %02x %02x", answer, Version, MethodNone)
	}
}

// ServeConn serves the socks5 plugin, whose tunneled form answers CONNECT only.
// The refusal used to carry ErrUnsupported, whose text names UDP ASSOCIATE as
// supported — true of ReadRequest, the socks5 proxy type, and the opposite of
// what had just happened to this visitor.
func TestABindRequestIsRefusedWithAnErrorThatNamesThisEndpoint(t *testing.T) {
	server, visitor := tcpPair(t)

	done := make(chan error, 1)
	go func() {
		_, err := ServeConn(server, ServerOptions{
			Dial: func(string) (net.Conn, error) {
				return nil, errors.New("nothing should be dialed")
			},
		})
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
	if answer[1] != MethodNoReq {
		t.Fatalf("the server chose method 0x%02x, want 0x%02x", answer[1], MethodNoReq)
	}
	request := []byte{Version, CmdBind, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := visitor.Write(request); err != nil {
		t.Fatalf("write the bind request: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(visitor, reply); err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if reply[1] != ReplyCommandNotSupported {
		t.Fatalf("BIND was answered 0x%02x, want 0x%02x", reply[1], ReplyCommandNotSupported)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrConnectOnly) {
			t.Fatalf("ServeConn returned %v, want ErrConnectOnly", err)
		}
		if err.Error() != ErrConnectOnly.Error() {
			t.Fatalf("the refusal reads %q", err.Error())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return")
	}
}

// A caller that configures only a password means to require credentials. The
// check used to be `Username != ""`, so that caller got no authentication at
// all and every visitor was admitted — a configured credential has to fail
// closed, not open.
func TestAConfiguredPasswordAloneStillRequiresAuthentication(t *testing.T) {
	server, visitor := tcpPair(t)

	go func() {
		_, _ = ServeConn(server, ServerOptions{
			Password: "s3cret",
			Dial: func(string) (net.Conn, error) {
				return nil, errors.New("nothing should be dialed")
			},
		})
	}()

	_ = visitor.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := visitor.Write([]byte{Version, 1, MethodNoReq}); err != nil {
		t.Fatalf("write the greeting: %v", err)
	}
	answer := make([]byte, 2)
	if _, err := io.ReadFull(visitor, answer); err != nil {
		t.Fatalf("read the method: %v", err)
	}
	if answer[1] != MethodNone {
		t.Fatalf("a visitor offering no credentials was offered method 0x%02x, want the 0x%02x refusal",
			answer[1], MethodNone)
	}
}
