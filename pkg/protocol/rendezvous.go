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
//
// The token is 32 ASCII characters (16 random bytes in hex) handed out over the
// authenticated control connection, so a third party cannot join an attempt.
const (
	PunchRequestMagic  = "ATP1"
	PunchResponseMagic = "ATP2"
	PunchTokenLen      = 32
	PunchRequestLen    = len(PunchRequestMagic) + 1 + PunchTokenLen

	PunchRoleVisitor = 'V'
	PunchRoleOwner   = 'O'
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
