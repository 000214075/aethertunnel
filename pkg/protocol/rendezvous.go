package protocol

// Rendezvous wire format for xtcp hole punching.
//
// Two peers that have been told to punch each other both send a request datagram
// to the server's rendezvous port. The server observes the source address of each
// request — that is the peer's NAT-mapped address — and answers with the other
// peer's address as soon as both have been seen:
//
//	request   "ATP1" | role:1 | token:32
//	response  "ATP2" | token:32 | length:1 | "ip:port"
//	result    "ATP3" | token:32 | path:1
//
// The token is 32 ASCII characters (16 random bytes in hex) handed out over the
// authenticated control connection, so a third party cannot join an attempt.
//
// The result datagram is how a peer reports which path it ended up using. A
// visitor that punches a direct path stops using its control connection without
// saying anything, so the control connection alone cannot distinguish a working
// punch from a visitor that gave up; the result datagram is sent on the same
// socket the peers already use for the rendezvous. A server that predates it
// ignores the datagram, because the magic differs from every request it knows.
const (
	PunchRequestMagic  = "ATP1"
	PunchResponseMagic = "ATP2"
	PunchResultMagic   = "ATP3"
	PunchTokenLen      = 32
	PunchRequestLen    = len(PunchRequestMagic) + 1 + PunchTokenLen
	PunchResultLen     = len(PunchResultMagic) + PunchTokenLen + 1

	PunchRoleVisitor = 'V'
	PunchRoleOwner   = 'O'

	// PunchPathDirect is the path byte a visitor reports after punching a direct
	// path to the owner.
	PunchPathDirect = 'D'
	// PunchPathRelayed is the path byte a peer reports when it fell back to the
	// relayed path.
	PunchPathRelayed = 'R'
)

// EncodePunchRequest builds the datagram a peer sends to the rendezvous port.
func EncodePunchRequest(role byte, token string) []byte {
	if len(token) != PunchTokenLen {
		return nil
	}
	out := make([]byte, 0, PunchRequestLen)
	out = append(out, PunchRequestMagic...)
	out = append(out, role)
	out = append(out, token...)
	return out
}

// DecodePunchRequest parses a rendezvous request.
func DecodePunchRequest(data []byte) (role byte, token string, ok bool) {
	if len(data) != PunchRequestLen || string(data[:len(PunchRequestMagic)]) != PunchRequestMagic {
		return 0, "", false
	}
	role = data[len(PunchRequestMagic)]
	if role != PunchRoleVisitor && role != PunchRoleOwner {
		return 0, "", false
	}
	return role, string(data[len(PunchRequestMagic)+1:]), true
}

// EncodePunchResponse builds the reply carrying the other peer's address.
func EncodePunchResponse(token, peer string) []byte {
	if len(token) != PunchTokenLen || len(peer) > 255 {
		return nil
	}
	out := make([]byte, 0, len(PunchResponseMagic)+PunchTokenLen+1+len(peer))
	out = append(out, PunchResponseMagic...)
	out = append(out, token...)
	out = append(out, byte(len(peer)))
	out = append(out, peer...)
	return out
}

// DecodePunchResponse parses a rendezvous reply.
func DecodePunchResponse(data []byte) (token, peer string, ok bool) {
	header := len(PunchResponseMagic) + PunchTokenLen + 1
	if len(data) < header || string(data[:len(PunchResponseMagic)]) != PunchResponseMagic {
		return "", "", false
	}
	token = string(data[len(PunchResponseMagic) : len(PunchResponseMagic)+PunchTokenLen])
	length := int(data[len(PunchResponseMagic)+PunchTokenLen])
	rest := data[header:]
	if length > len(rest) {
		return "", "", false
	}
	return token, string(rest[:length]), true
}

// EncodePunchResult builds the datagram with which a peer reports the path it
// took after punching.
func EncodePunchResult(token string, path byte) []byte {
	if len(token) != PunchTokenLen || (path != PunchPathDirect && path != PunchPathRelayed) {
		return nil
	}
	out := make([]byte, 0, PunchResultLen)
	out = append(out, PunchResultMagic...)
	out = append(out, token...)
	out = append(out, path)
	return out
}

// DecodePunchResult parses a path report.
func DecodePunchResult(data []byte) (token string, path byte, ok bool) {
	if len(data) != PunchResultLen || string(data[:len(PunchResultMagic)]) != PunchResultMagic {
		return "", 0, false
	}
	token = string(data[len(PunchResultMagic) : len(PunchResultMagic)+PunchTokenLen])
	path = data[len(PunchResultMagic)+PunchTokenLen]
	if path != PunchPathDirect && path != PunchPathRelayed {
		return "", 0, false
	}
	return token, path, true
}
