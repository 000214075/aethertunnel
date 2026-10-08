package server

import (
	"errors"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// A client-supplied string that reaches the log cannot carry control
// characters — a newline above all, which would forge the next log line — and
// cannot run past the cap.
func TestSanitizedLogTextDropsControlCharactersAndTruncates(t *testing.T) {
	if got := sanitizeLogText("client\nERROR injected\r\x00line"); got != "clientERROR injectedline" {
		t.Fatalf("sanitizeLogText kept control characters: %q", got)
	}
	if got := sanitizeLogText("kept"); got != "kept" {
		t.Fatalf("sanitizeLogText changed printable text: %q", got)
	}
	long := strings.Repeat("x", maxSanitizedLogText+50)
	if got := sanitizeLogText(long); len(got) != maxSanitizedLogText {
		t.Fatalf("sanitizeLogText returned %d bytes, want %d", len(got), maxSanitizedLogText)
	}
}

// A domain that normalizes to empty is skipped, the way the tcpmux and sni
// tables skip it: installing it under the empty key would let a request
// without a Host header match the binding.
func TestVhostSkipsWhatNormalizesToEmpty(t *testing.T) {
	v := newVhostRouter("http", &config.Config{}, log.New(io.Discard, "", 0), &Metrics{})
	group := &ProxyGroup{Name: "web", Type: "http", Domains: []string{"", "web.example"}}
	if _, err := v.add(group); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, ok := v.exact[""]; ok {
		t.Fatal("the empty domain was installed under the empty key")
	}
	if _, ok := v.exact["web.example"]; !ok {
		t.Fatal("the usable domain was not installed")
	}
}

// A deadline on an SSH gateway channel closes the channel when it fires: the
// mux has no per-channel deadline, so closing is how a stalled forwarded
// stream is freed. Clearing the deadline stops the timer instead.
func TestAnSSHChannelDeadlineClosesTheChannel(t *testing.T) {
	var closed atomic.Int32
	channel := &fakeSSHChannel{onClose: func() { closed.Add(1) }}
	conn := &sshChannelConn{Channel: channel}

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline = %v, want nil", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && closed.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("the read deadline fired but the channel was never closed")
	}
}

func TestAnSSHChannelClearedDeadlineClosesNothing(t *testing.T) {
	var closed atomic.Int32
	channel := &fakeSSHChannel{onClose: func() { closed.Add(1) }}
	conn := &sshChannelConn{Channel: channel}

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline = %v, want nil", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing SetReadDeadline = %v, want nil", err)
	}
	time.Sleep(80 * time.Millisecond)
	if closed.Load() != 0 {
		t.Fatal("a cleared deadline still closed the channel")
	}
}

type fakeSSHChannel struct {
	onClose func()
}

func (f *fakeSSHChannel) Read([]byte) (int, error)    { return 0, io.EOF }
func (f *fakeSSHChannel) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeSSHChannel) Close() error                { f.onClose(); return nil }
func (f *fakeSSHChannel) CloseWrite() error           { return nil }
func (f *fakeSSHChannel) SendRequest(string, bool, []byte) (bool, error) {
	return false, errors.New("no requests on a fake channel")
}
func (f *fakeSSHChannel) Stderr() io.ReadWriter { return nil }
