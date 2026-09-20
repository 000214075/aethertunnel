package server

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// punchWait is how long the server keeps a visitor connection open while the
// visitor attempts a direct path before falling back to relaying.
const punchWait = 15 * time.Second

// visitorSession is the state a visitor connection carries after the handshake.
type visitorSession struct {
	conn   net.Conn
	framer *protocol.Framer
	// cipher protects the raw bytes of a stream-shaped visitor session. For a
	// datagram session the framer already seals each frame.
	cipher *crypto.Cipher
	remote string

	// kexResponse is the answer to the visitor's post-quantum key exchange. It
	// travels in the first frame the server sends, and both ends then switch to
	// the agreed key; kexSent keeps it to exactly one frame so the two sides
	// agree on where the switch happens.
	kexResponse []byte
	kexSent     bool
}

// takeKEX returns the key agreement response when this is the first frame the
// server sends, and nil afterwards. Everything the visitor reads before that
// frame is protected by the configured cipher; everything after it is protected
// by the agreed key, and both ends agree on where the switch happens because only
// one frame carries it.
func (v *visitorSession) takeKEX() []byte {
	if v.kexSent || len(v.kexResponse) == 0 {
		return nil
	}
	v.kexSent = true
	return v.kexResponse
}

// switchFramer moves the connection onto the agreed key.
func (v *visitorSession) switchFramer(opts protocol.FramerOptions) *protocol.Framer {
	v.framer = protocol.NewFramerWithOptions(v.conn, v.cipher, opts)
	return v.framer
}

// handleVisitor serves a visitor connection: a second client that wants to reach
// a private proxy of another client.
//
// A visitor connection is authenticated three times over: the server auth token,
// the client identity when the server requires one, and the proxy's secret key or
// a proof of knowledge of it. A private proxy is only as private as the key
// guarding it, so none of the three is optional.
func (s *Server) handleVisitor(conn net.Conn, framer *protocol.Framer, msg *protocol.Message) {
	var req protocol.VisitorConnect
	if len(msg.Payload) == 0 || json.Unmarshal(msg.Payload, &req) != nil {
		s.rejectVisitor(conn, framer, "malformed visitor-connect")
		return
	}

	remote := conn.RemoteAddr().String()
	if !crypto.EqualTokens(req.AuthToken, s.cfg.Server.AuthToken) {
		s.metrics.authFailures.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: remote, Proxy: req.Proxy,
			Outcome: "denied", Detail: "visitor connection with an invalid auth token",
		})
		s.logger.Printf("visitor from %s rejected: invalid auth token", remote)
		s.rejectVisitor(conn, framer, "invalid auth token")
		return
	}

	if err := s.checkIdentity(identityAssertion{
		PublicKey: req.Identity,
		Nonce:     req.IdentityNonce,
		Timestamp: req.IdentityTime,
		Signature: req.IdentitySignature,
	}); err != nil {
		s.metrics.authFailures.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: remote, Proxy: req.Proxy,
			Outcome: "denied", Detail: err.Error(),
		})
		s.logger.Printf("visitor from %s rejected: %v", remote, err)
		s.rejectVisitor(conn, framer, err.Error())
		return
	}

	group, err := s.tunnels.Get(req.Proxy)
	if err != nil {
		s.rejectVisitor(conn, framer, fmt.Sprintf("no proxy named %q is registered", req.Proxy))
		return
	}
	if !group.Private {
		s.rejectVisitor(conn, framer, fmt.Sprintf("proxy %q is not a private proxy", req.Proxy))
		return
	}

	// The key agreement runs before the proxy is authorised so that the
	// challenge, the proof and everything after it are protected by the agreed
	// key rather than by the configured cipher alone.
	visitorCipher := s.cipher
	var kexResponse []byte
	if s.cfg.PostQuantum() {
		if len(req.KEX) == 0 {
			s.rejectVisitor(conn, framer, "this server requires a post-quantum key exchange (encryption.post_quantum)")
			return
		}
		response, key, err := crypto.HybridServerFinish(req.KEX)
		if err != nil {
			s.metrics.authFailures.Add(1)
			s.logger.Printf("visitor from %s: post-quantum key agreement failed: %v", remote, err)
			s.rejectVisitor(conn, framer, "post-quantum key agreement failed")
			return
		}
		visitorCipher, err = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if err != nil {
			s.logger.Printf("visitor from %s: the agreed key is unusable: %v", remote, err)
			s.rejectVisitor(conn, framer, "post-quantum key agreement failed")
			return
		}
		kexResponse = response
	}

	session := &visitorSession{
		conn: conn, framer: framer, cipher: visitorCipher,
		remote: remote, kexResponse: kexResponse,
	}

	switch group.AuthMethod {
	case config.AuthMethodNIZK:
		if err := s.challengeVisitor(session, group, kexResponse); err != nil {
			s.metrics.authFailures.Add(1)
			s.auditor.Record(AuditEvent{
				Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
				Outcome: "denied", Detail: err.Error(),
			})
			s.logger.Printf("visitor from %s rejected for proxy %q: %v", remote, req.Proxy, err)
			return
		}
	default:
		if !group.matchesSecret(req.Secret) {
			s.metrics.authFailures.Add(1)
			s.auditor.Record(AuditEvent{
				Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
				Outcome: "denied", Detail: "invalid secret key",
			})
			s.logger.Printf("visitor from %s rejected for proxy %q: invalid secret key", remote, req.Proxy)
			s.rejectVisitor(conn, framer, "invalid secret key")
			return
		}
	}

	s.metrics.controlAccepted.Add(1)
	s.auditor.Record(AuditEvent{
		Event: EventVisitorAccepted, Remote: remote, Proxy: req.Proxy,
		Outcome: "ok",
		Detail:  fmt.Sprintf("type=%s auth=%s agreed_key=%v", group.Type, group.AuthMethod, len(kexResponse) > 0),
	})

	switch req.Type {
	case protocol.ProxyTypeXTCP:
		s.serveXTCPVisitor(session, group)
	case protocol.ProxyTypeSUDP:
		s.relayDatagramVisitor(session, group)
	default:
		s.relayStreamVisitor(session, group)
	}
}

// challengeVisitor asks a visitor to prove it knows the proxy's secret key. The
// key itself never crosses the wire: the visitor proves knowledge of the discrete
// logarithm of the public key derived from it, bound to a nonce the server picks
// for this connection.
//
// The challenge carries the key-agreement response, and the framer is replaced
// immediately afterwards, so the two ends agree on exactly where the switch
// happens.
func (s *Server) challengeVisitor(session *visitorSession, group *ProxyGroup, kexResponse []byte) error {
	nonce, err := crypto.Nonce()
	if err != nil {
		s.rejectVisitor(session.conn, session.framer, "cannot generate a challenge")
		return err
	}

	if err := session.framer.WriteJSON(protocol.TypeVisitorChallenge, protocol.VisitorChallenge{
		Proxy: group.Name,
		Nonce: nonce,
		KEX:   session.takeKEX(),
	}); err != nil {
		return fmt.Errorf("send the challenge: %w", err)
	}
	session.switchFramer(s.framerOptions())

	handshakeTimeout := time.Duration(s.cfg.Server.HandshakeTimeoutSecs) * time.Second
	if handshakeTimeout > 0 {
		_ = session.conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
		defer session.conn.SetReadDeadline(time.Time{})
	}

	var answer protocol.VisitorProve
	if err := session.framer.ReadJSON(protocol.TypeVisitorProve, &answer); err != nil {
		s.rejectVisitor(session.conn, session.framer, "the visitor did not answer the challenge")
		return fmt.Errorf("read the proof: %w", err)
	}

	if err := crypto.SchnorrVerify(
		group.SecretPublicKey,
		protocol.VisitorProofContext(group.Name, nonce),
		answer.Proof,
	); err != nil {
		s.rejectVisitor(session.conn, session.framer, "the proof does not verify")
		return err
	}
	return nil
}

// rejectVisitor reports a refusal to a visitor and closes its connection.
func (s *Server) rejectVisitor(conn net.Conn, framer *protocol.Framer, reason string) {
	_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: reason})
	_ = conn.Close()
}

// relayStreamVisitor pairs a visitor connection with a data connection from one
// member of the proxy's group and copies bytes between them.
func (s *Server) relayStreamVisitor(session *visitorSession, group *ProxyGroup) {
	member, stream, err := group.openForVisitor()
	if err != nil {
		s.logger.Printf("visitor from %s: %v", session.remote, err)
		s.rejectVisitor(session.conn, session.framer, err.Error())
		return
	}
	defer stream.release()

	ack := protocol.DataOpenAck{OK: true, KEX: session.takeKEX()}
	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, ack); err != nil {
		_ = stream.dc.Close()
		return
	}
	session.switchFramer(s.framerOptions())

	visitorSide := &cryptoStreamConn{
		Stream: crypto.NewStream(session.conn, session.cipher),
		conn:   session.conn,
	}
	_ = member.pipeStream(visitorSide, stream.dc, "visitor "+session.remote)
}

// relayDatagramVisitor does the same for a sudp visitor, where each frame is one
// datagram rather than part of a byte stream.
func (s *Server) relayDatagramVisitor(session *visitorSession, group *ProxyGroup) {
	member, stream, err := group.openForVisitor()
	if err != nil {
		s.logger.Printf("visitor from %s: %v", session.remote, err)
		s.rejectVisitor(session.conn, session.framer, err.Error())
		return
	}
	defer stream.release()

	ack := protocol.DataOpenAck{OK: true, KEX: session.takeKEX()}
	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, ack); err != nil {
		_ = stream.dc.Close()
		return
	}
	session.switchFramer(s.framerOptions())

	_ = member.pipeDatagrams(session.framer, stream.dc, "visitor "+session.remote)
}

// serveXTCPVisitor offers the visitor a hole-punch token and waits either for the
// visitor to report that the punch failed or for it to go away.
func (s *Server) serveXTCPVisitor(session *visitorSession, group *ProxyGroup) {
	if s.p2p == nil {
		// No rendezvous listener: the relayed path is the only path.
		s.relayStreamVisitor(session, group)
		return
	}

	owner := group.pick()
	if owner == nil {
		s.rejectVisitor(session.conn, session.framer, "no client is publishing this proxy")
		return
	}

	token, err := s.p2p.offer(group)
	if err != nil {
		s.logger.Printf("visitor from %s: cannot start a punch for %q: %v", session.remote, group.Name, err)
		s.relayStreamVisitor(session, group)
		return
	}
	s.metrics.p2pPunches.Add(1)

	offer := protocol.P2PPeer{Token: token, KEX: session.takeKEX()}
	if err := session.framer.WriteJSON(protocol.TypeP2PPeer, offer); err != nil {
		s.p2p.forget(token)
		_ = session.conn.Close()
		return
	}
	session.switchFramer(s.framerOptions())

	// Ask one member to open its side of the punch.
	prepare := protocol.P2PPrepare{Proxy: group.Name, Token: token}
	if err := owner.Session.Framer().WriteJSON(protocol.TypeP2PPrepare, prepare); err != nil {
		s.logger.Printf("visitor from %s: cannot ask the owner of %q to punch: %v", session.remote, group.Name, err)
		s.p2p.forget(token)
		_ = session.conn.Close()
		return
	}

	_ = session.conn.SetReadDeadline(time.Now().Add(punchWait))
	msg, err := session.framer.ReadFrame()
	_ = session.conn.SetReadDeadline(time.Time{})

	switch {
	case err != nil:
		// The visitor closed the connection: either the punch worked and the
		// visitor is talking to the owner directly, or the visitor gave up.
		s.p2p.forget(token)
		s.auditor.Record(AuditEvent{
			Event: EventP2PDirect, Remote: session.remote, Proxy: group.Name,
			Outcome: "ok", Detail: "visitor left the rendezvous without asking for a relay",
		})
		_ = session.conn.Close()
		return

	case msg.Type == protocol.TypeP2PFallback:
		s.metrics.p2pRelayed.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventP2PRelayed, Remote: session.remote, Proxy: group.Name,
			Outcome: "ok", Detail: "hole punching failed, using the relayed path",
		})
		s.logger.Printf("visitor %s for %q fell back to the relayed path", session.remote, group.Name)
		s.p2p.forget(token)
		s.relayStreamVisitor(session, group)

	default:
		s.p2p.forget(token)
		s.rejectVisitor(session.conn, session.framer, fmt.Sprintf("expected %s", protocol.TypeP2PFallback))
	}
}
