package protocol

import (
	"strings"
	"testing"
)

// EncodePunchRequest accepted any role byte, so a caller that passed the wrong one
// built a datagram its own decoder drops: a silent rendezvous timeout instead of an
// error at the call.
func TestEncodePunchRequestRefusesAnUnknownRole(t *testing.T) {
	token := strings.Repeat("t", PunchTokenLen)
	if got := EncodePunchRequest('X', token); got != nil {
		t.Fatalf("an unknown role produced %d bytes, want nil", len(got))
	}
	if got := EncodePunchRequest(PunchRoleVisitor, "too-short"); got != nil {
		t.Fatalf("a short token produced %d bytes, want nil", len(got))
	}
	for _, role := range []byte{PunchRoleVisitor, PunchRoleOwner} {
		req := EncodePunchRequest(role, token)
		if len(req) != PunchRequestLen {
			t.Fatalf("role %q produced %d bytes, want %d", role, len(req), PunchRequestLen)
		}
		gotRole, gotToken, ok := DecodePunchRequest(req)
		if !ok || gotRole != role || gotToken != token {
			t.Fatalf("round trip of role %q gave (%q, %q, %v)", role, gotRole, gotToken, ok)
		}
	}
}
