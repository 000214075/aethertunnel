package server

import (
	"errors"
	"strings"
	"testing"
)

// The client is closed out without an answer when the first frame cannot be read, so the
// server's log line is the only record of why. An authentication failure on that frame is
// what a passphrase, salt or algorithm that differs between the two ends looks like from
// here, and the line has to say so: an operator seeing "message authentication failed"
// should not have to guess which setting it refers to.
func TestAHandshakeFailureNamesTheSettingsThatCouldCauseIt(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("crypto: message authentication failed"), "[encryption]"},
		{errors.New("protocol: encryption mismatch: frame says encrypted=false but this side has encryption=true"), "[encryption]"},
		// A disguise or transport problem has its own message, and pointing at [encryption]
		// there would send the operator to the wrong setting.
		{errors.New("obfs: the stream is not carrying record-framed data"), ""},
		{errors.New("EOF"), ""},
		{errors.New("protocol: frame carries 70000 bytes, limit 65536"), ""},
		{nil, ""},
	}
	for _, c := range cases {
		got := handshakeFailureHint(c.err)
		if c.want == "" {
			if got != "" {
				t.Errorf("hint for %v is %q, want none", c.err, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("hint for %v is %q, want it to mention %s", c.err, got, c.want)
		}
	}
}
