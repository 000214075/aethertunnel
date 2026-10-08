package socks

import (
	"bytes"
	"errors"
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

func TestReadRequestRefusesBind(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdBind, 0x00, AtypIPv4, 127, 0, 0, 1, 0x00, 0x50}
	if _, _, err := serve(t, script); err == nil {
		t.Fatal("a BIND command was accepted")
	}
}

func TestReadRequestAcceptsAUDPAssociateRequest(t *testing.T) {
	script := []byte{Version, 1, MethodNoReq, Version, CmdAssoc, 0x00, AtypIPv4, 0, 0, 0, 0, 0x00, 0x00}
	request, written, err := serve(t, script)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if request.Command != CmdAssoc {
		t.Errorf("the command is %d, want %d", request.Command, CmdAssoc)
	}
	if request.Target != "" {
		t.Errorf("a UDP ASSOCIATE request must not set the CONNECT target: %q", request.Target)
	}
	if request.ClientAddr != "0.0.0.0:0" {
		t.Errorf("the client address is %q, want 0.0.0.0:0", request.ClientAddr)
	}
	if !bytes.Equal(written, []byte{Version, MethodNoReq}) {
		t.Errorf("the negotiation answered %v, want 05 00", written)
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

// TestReadRequestRefusesADomainThatCannotBeHostPort covers a name a visitor can
// put in the domain address type that no consumer of a target can take apart:
// net.JoinHostPort leaves "a[" alone because it only brackets a host holding a
// colon, and net.SplitHostPort then refuses the stray bracket. Every target is
// read with SplitHostPort — the allow list, the client's dial, the reply — so the
// request is refused here rather than handed on as a string only the client's own
// parser can reject.
func TestReadRequestRefusesADomainThatCannotBeHostPort(t *testing.T) {
	for _, command := range []byte{CmdConnect, CmdAssoc} {
		for _, name := range []string{"a[", "[a", "a]b", "ho[st.example"} {
			script := []byte{Version, 1, MethodNoReq, Version, command, 0x00, AtypDomain, byte(len(name))}
			script = append(script, []byte(name)...)
			script = append(script, 0x01, 0xBB)

			_, _, err := serve(t, script)
			if !errors.Is(err, ErrBadAddress) {
				t.Errorf("command %d with domain %q: err = %v, want %v", command, name, err, ErrBadAddress)
			}
		}
	}
}

// TestReadRequestResolvesTheAmbiguousDomainForms covers the forms that look
// suspicious and are not: a colon is bracketed by net.JoinHostPort and comes back
// out of net.SplitHostPort unchanged, so a target that names one is still a usable
// host:port and is not refused for its shape. Whether such a name resolves is the
// client's business, not the parser's.
func TestReadRequestResolvesTheAmbiguousDomainForms(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"a:b", "[a:b]:443"},
		{"example.com", "example.com:443"},
		{"xn--bcher-kva.example", "xn--bcher-kva.example:443"},
	}
	for _, tc := range cases {
		script := []byte{Version, 1, MethodNoReq, Version, CmdConnect, 0x00, AtypDomain, byte(len(tc.name))}
		script = append(script, []byte(tc.name)...)
		script = append(script, 0x01, 0xBB)

		request, _, err := serve(t, script)
		if err != nil {
			t.Fatalf("domain %q: %v", tc.name, err)
		}
		if request.Target != tc.want {
			t.Errorf("domain %q became %q, want %q", tc.name, request.Target, tc.want)
		}
		if _, _, err := net.SplitHostPort(request.Target); err != nil {
			t.Errorf("domain %q became %q, which is not host:port: %v", tc.name, request.Target, err)
		}
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

func TestWriteReplyBoundNamesTheRelayAddress(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 10)
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.ReadFull(client, buf)
		done <- buf[:n]
	}()

	if err := WriteReplyBound(server, ReplySucceeded, net.IPv4(127, 0, 0, 1), 4100); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := <-done
	want := []byte{Version, ReplySucceeded, 0x00, AtypIPv4, 127, 0, 0, 1, 0x10, 0x04}
	if !bytes.Equal(got, want) {
		t.Errorf("reply is %v, want %v", got, want)
	}
}

// --- UDP datagrams ------------------------------------------------------------

func TestUDPDatagramRoundTripsForEveryAddressType(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:53", "[2001:db8::1]:53", "example.com:53"} {
		packet, err := WrapUDPDatagram(addr, []byte("payload"))
		if err != nil {
			t.Fatalf("%s: wrap: %v", addr, err)
		}
		got, data, err := ParseUDPDatagram(packet)
		if err != nil {
			t.Fatalf("%s: parse: %v", addr, err)
		}
		if got != addr {
			t.Errorf("parsed address is %q, want %q", got, addr)
		}
		if string(data) != "payload" {
			t.Errorf("parsed data is %q, want payload", data)
		}
	}
}

func TestParseUDPDatagramRefusesFragmentsAndGarbage(t *testing.T) {
	packet, err := WrapUDPDatagram("127.0.0.1:53", []byte("data"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	// A non-zero fragment byte is refused: this relay does not reassemble.
	fragmented := append([]byte(nil), packet...)
	fragmented[2] = 1
	if _, _, err := ParseUDPDatagram(fragmented); err != ErrFragment {
		t.Errorf("a fragmented datagram failed with %v, want %v", err, ErrFragment)
	}

	// A non-zero reserved byte is refused too.
	reserved := append([]byte(nil), packet...)
	reserved[0] = 1
	if _, _, err := ParseUDPDatagram(reserved); err == nil {
		t.Error("a datagram with a non-zero reserved byte was accepted")
	}

	// A truncated datagram has no address or port to read.
	if _, _, err := ParseUDPDatagram(packet[:3]); err == nil {
		t.Error("a truncated datagram was accepted")
	}
}

func TestWrapUDPDatagramRejectsAnUnusableAddress(t *testing.T) {
	if _, err := WrapUDPDatagram("no-port", []byte("x")); err == nil {
		t.Error("an address without a port was accepted")
	}
	if _, err := WrapUDPDatagram("127.0.0.1:not-a-number", []byte("x")); err == nil {
		t.Error("an address with a non-numeric port was accepted")
	}
}

// TestParseUDPDatagramRefusesADomainThatCannotBeHostPort covers the datagram form
// of the same gap: the target a relayed datagram names is dialled by the client
// behind the tunnel, which takes it apart with net.SplitHostPort, so the parser
// refuses a domain that would not survive that join.
func TestParseUDPDatagramRefusesADomainThatCannotBeHostPort(t *testing.T) {
	for _, name := range []string{"a[", "[a", "a]b"} {
		packet := []byte{0, 0, 0, AtypDomain, byte(len(name))}
		packet = append(packet, []byte(name)...)
		packet = append(packet, 0x00, 0x50)
		packet = append(packet, []byte("data")...)

		address, _, err := ParseUDPDatagram(packet)
		if !errors.Is(err, ErrBadAddress) {
			t.Errorf("domain %q: address %q, err = %v, want %v", name, address, err, ErrBadAddress)
		}
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
	// first, so on a machine whose localhost includes ::1 the name is refused
	// rather than dialled. Where it resolves to 127.0.0.1 alone there is nothing
	// to refuse, and the check below still has to run: skipping the whole test
	// there left the numeric target untested.
	if _, err := policy.Check("localhost:80", 2*time.Second); err != nil {
		if !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("localhost was refused for the wrong reason: %v", err)
		}
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
