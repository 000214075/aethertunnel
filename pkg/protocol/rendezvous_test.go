package protocol

import (
	"bytes"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestPunchResultRoundTrips(t *testing.T) {
	for _, path := range []byte{PunchPathDirect, PunchPathRelayed} {
		encoded := EncodePunchResult(testToken, path)
		if len(encoded) != PunchResultLen {
			t.Fatalf("a path report is %d bytes, want %d", len(encoded), PunchResultLen)
		}

		token, got, ok := DecodePunchResult(encoded)
		if !ok {
			t.Fatalf("a report built for path %q did not decode", string(path))
		}
		if token != testToken || got != path {
			t.Fatalf("decoded token %q path %q, want %q and %q", token, string(got), testToken, string(path))
		}
	}
}

func TestAPunchRequestIsNotReadAsAPathReport(t *testing.T) {
	// Both datagrams are the same length, so only the magic keeps them apart. A
	// server that mixed them up would answer the peer's own report with the
	// peer's address and end the attempt early.
	request := EncodePunchRequest(PunchRoleVisitor, testToken)
	if _, _, ok := DecodePunchResult(request); ok {
		t.Fatal("a punch request decoded as a path report")
	}

	report := EncodePunchResult(testToken, PunchPathDirect)
	if _, _, ok := DecodePunchRequest(report); ok {
		t.Fatal("a path report decoded as a punch request")
	}
}

func TestMalformedPunchResultsAreRejected(t *testing.T) {
	good := EncodePunchResult(testToken, PunchPathDirect)

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short token", EncodePunchResult("too-short", PunchPathDirect)},
		{"truncated", good[:len(good)-1]},
		{"trailing byte", append(append([]byte{}, good...), 'x')},
		{"unknown path", func() []byte {
			bad := append([]byte{}, good...)
			bad[len(bad)-1] = 'Z'
			return bad
		}()},
		{"flipped magic", func() []byte {
			bad := append([]byte{}, good...)
			bad[0] = 'X'
			return bad
		}()},
	}
	for _, tc := range cases {
		if _, _, ok := DecodePunchResult(tc.data); ok {
			t.Errorf("%s decoded as a valid path report", tc.name)
		}
	}
}

func TestEncodePunchResultRefusesUnusableInput(t *testing.T) {
	if EncodePunchResult("short", PunchPathDirect) != nil {
		t.Error("a report was built for a token of the wrong length")
	}
	if EncodePunchResult(testToken, 'Z') != nil {
		t.Error("a report was built for an unknown path")
	}
	if !bytes.Equal(EncodePunchResult(testToken, PunchPathDirect)[:4], []byte(PunchResultMagic)) {
		t.Error("the report does not start with its magic")
	}
}
