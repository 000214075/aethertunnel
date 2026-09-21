package socks

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// serve runs ReadRequest against a scripted visitor and returns the request and
// the bytes the reader wrote back.
func serve(t *testing.T, script []byte) (Request, []byte, error) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	response := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		_ = client.SetDeadline(time.Now().Add(3 * time.Second))
		total := 0
		for total < 2 { // the method reply
			n, err := client.Read(buf[total:])
			if err != nil {
				break
			}
			total += n
		}
		response <- append([]byte(nil), buf[:total]...)
	}()

	go func() {
		_, _ = client.Write(script)
		// Keep the write side open until the reader has answered, so the reader
		// does not see a closed connection instead of the request.
		time.Sleep(200 * time.Millisecond)
		client.Close()
	}()

	request, err := ReadRequest(server, 2*time.Second)
	select {
	case written := <-response:
		return request, written, err
	case <-time.After(3 * time.Second):
		t.Fatal("the reader never answered the negotiation")
		return Request{}, nil, err
	}
}

func TestReadRequestAcceptsADomainTarget(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq}
	script = append(script, Version, CmdConnect, 0x00, AtypDomain, byte(len("example.com")))
	script = append(script, []byte("example.com")...)
	script = append(script, 0x01, 0xBB) // port 443

	request, written, err := serve(t, script)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if request.Target != "example.com:443" {
		t.Errorf("target is %q, want example.com:443", request.Target)
	}
	if !bytes.Equal(written, []byte{Version, MethodNoReq}) {
		t.Errorf("the negotiation answered %v, want 05 00", written)
	}
}

func TestReadRequestAcceptsAnIPv4Target(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x50}
	request, _, err := serve(t, script)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if request.Target != "127.0.0.1:80" {
		t.Errorf("target is %q, want 127.0.0.1:80", request.Target)
	}
}

func TestReadRequestAcceptsAnIPv6Target(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypIPv6}
	script = append(script, net.ParseIP("2001:db8::1").To16()...)
	script = append(script, 0x1F, 0x90) // port 8080
	request, _, err := serve(t, script)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if request.Target != "[2001:db8::1]:8080" {
		t.Errorf("target is %q, want [2001:db8::1]:8080", request.Target)
	}
}

func TestReadRequestRefusesAnotherVersion(t *testing.T) {
	_, written, err := serve(t, []byte{0x04, 1, MethodNoReq})
	if err == nil {
		t.Fatal("a SOCKS4 greeting was accepted")
	}
	if !bytes.Equal(written, []byte{Version, MethodNone}) {
		t.Errorf("the refusal is %v, want 05 ff", written)
	}
}

func TestReadRequestRefusesAnUnacceptableMethod(t *testing.T) {
	// The visitor offers only username/password authentication, which this server
	// does not implement.
	_, written, err := serve(t, []byte{Version, 1, 0x02})
	if err == nil {
		t.Fatal("a method this server does not offer was accepted")
	}
	if !bytes.Equal(written, []byte{Version, MethodNone}) {
		t.Errorf("the refusal is %v, want 05 ff", written)
	}
}

func TestReadRequestRefusesBindAndAssociate(t *testing.T) {
	for _, command := range []byte{CmdBind, CmdAssoc} {
		script := []byte{Version, 1, MethodNoReq, Version, command, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x50}
		_, _, err := serve(t, script)
		if err == nil {
			t.Fatalf("command %d was accepted", command)
		}
	}
}

func TestReadRequestRefusesAnUnknownAddressType(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, 0x09}
	if _, _, err := serve(t, script); err == nil {
		t.Fatal("an unknown address type was accepted")
	}
}

func TestReadRequestRefusesPortZero(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x00}
	if _, _, err := serve(t, script); err == nil {
		t.Fatal("a request for port 0 was accepted")
	}
}

func TestReplyForMapsTheFailure(t *testing.T) {
	cases := []struct {
		err  error
		want byte
	}{
		{nil, ReplySucceeded},
		{&net.DNSError{Err: "no such host", Name: "nope.invalid"}, ReplyHostUnreachable},
		{&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}, ReplyHostUnreachable},
		{errString("connect: connection refused"), ReplyConnectionRefused},
		{errString("socks: target 10.0.0.1:22 is not allowed"), ReplyNotAllowed},
		{errString("i/o timeout"), ReplyHostUnreachable},
		{errString("something else"), ReplyGeneralFailure},
	}
	for _, tc := range cases {
		if got := ReplyFor(tc.err); got != tc.want {
			t.Errorf("ReplyFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestWriteReply(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 10)
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.ReadFull(client, buf)
		done <- buf[:n]
	}()

	if err := WriteReply(server, ReplyConnectionRefused); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := <-done
	want := []byte{Version, ReplyConnectionRefused, 0x00, AtypIPv4, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Errorf("reply is %v, want %v", got, want)
	}
}

// --- target policy -------------------------------------------------------------

func TestTargetPolicyAllowsOnlyListedRanges(t *testing.T) {
	policy, err := NewTargetPolicy([]string{"10.0.0.0/8", "192.168.1.5"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	allowed := []string{"10.0.0.1", "10.255.255.254", "192.168.1.5"}
	for _, address := range allowed {
		if !policy.Allows(net.ParseIP(address)) {
			t.Errorf("%s was refused", address)
		}
	}
	refused := []string{"11.0.0.1", "192.168.1.6", "127.0.0.1", "::1"}
	for _, address := range refused {
		if policy.Allows(net.ParseIP(address)) {
			t.Errorf("%s was allowed", address)
		}
	}
}

func TestTargetPolicyRejectsAnEmptyOrUnusableList(t *testing.T) {
	for _, entries := range [][]string{nil, {}, {"", "  "}, {"not-an-address"}} {
		if policy, err := NewTargetPolicy(entries); err == nil {
			t.Errorf("allow_targets %v produced a policy: %v", entries, policy)
		}
	}
}

func TestTargetPolicyChecksEveryResolvedAddress(t *testing.T) {
	policy, err := NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	// localhost resolves to 127.0.0.1 and possibly ::1; the range covers only the
	// first, so the name is refused rather than dialled.
	if _, err := policy.Check("localhost:80", 2*time.Second); err == nil {
		t.Skip("localhost resolves to a loopback address only on this machine")
	}

	got, err := policy.Check("127.0.0.1:80", time.Second)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if got != "127.0.0.1:80" {
		t.Errorf("checked target is %q, want 127.0.0.1:80", got)
	}
}

func TestTargetPolicyRefusesAnAddressOutsideTheList(t *testing.T) {
	policy, err := NewTargetPolicy([]string{"10.1.0.0/16"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := policy.Check("10.2.0.1:22", time.Second); err == nil {
		t.Fatal("10.2.0.1 was allowed by a 10.1.0.0/16 policy")
	} else if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("the refusal reads %q", err)
	}
}

func TestTargetPolicyDialsAnAllowedTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- struct{}{}
		conn.Close()
	}()

	policy, err := NewTargetPolicy([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	conn, err := policy.Dial(listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the target never accepted the connection")
	}
}
