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
}

// handleVisitor serves a visitor connection: a second client that wants to reach
// a private proxy of another client.
//
// A visitor connection is authenticated twice — the server auth token, then
// either the proxy's secret key or a proof of knowledge of it — because a private
// proxy is only as private as the key guarding it.
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

	tunnel, err := s.tunnels.Get(req.Proxy)
	if err != nil {
		s.rejectVisitor(conn, framer, fmt.Sprintf("no proxy named %q is registered", req.Proxy))
		return
	}
	if !tunnel.Private() {
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

	session := &visitorSession{conn: conn, framer: framer, cipher: visitorCipher, remote: remote}

	switch tunnel.AuthMethod {
	case config.AuthMethodNIZK:
		if err := s.challengeVisitor(session, tunnel, kexResponse); err != nil {
			s.metrics.authFailures.Add(1)
			s.auditor.Record(AuditEvent{
				Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
				Outcome: "denied", Detail: err.Error(),
			})
			s.logger.Printf("visitor from %s rejected for proxy %q: %v", remote, req.Proxy, err)
			return
		}
	default:
		if !tunnel.matchesSecret(req.Secret) {
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
		Detail:  fmt.Sprintf("type=%s auth=%s agreed_key=%v", tunnel.Type, tunnel.AuthMethod, len(kexResponse) > 0),
	})

	switch req.Type {
	case protocol.ProxyTypeXTCP:
		s.serveXTCPVisitor(session, tunnel)
	case protocol.ProxyTypeSUDP:
		s.relayDatagramVisitor(session, tunnel)
	default:
		s.relayStreamVisitor(session, tunnel)
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
func (s *Server) challengeVisitor(session *visitorSession, tunnel *Tunnel, kexResponse []byte) error {
	nonce, err := crypto.Nonce()
	if err != nil {
		s.rejectVisitor(session.conn, session.framer, "cannot generate a challenge")
		return err
	}

	if err := session.framer.WriteJSON(protocol.TypeVisitorChallenge, protocol.VisitorChallenge{
		Proxy: tunnel.Name,
		Nonce: nonce,
		KEX:   kexResponse,
	}); err != nil {
		return fmt.Errorf("send the challenge: %w", err)
	}
	session.framer = protocol.NewFramerWithOptions(session.conn, session.cipher, s.framerOptions())

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
		tunnel.SecretPublicKey,
		protocol.VisitorProofContext(tunnel.Name, nonce),
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

// relayStreamVisitor pairs a visitor connection with a data connection from the
// proxy's owner and copies bytes between them.
func (s *Server) relayStreamVisitor(session *visitorSession, tunnel *Tunnel) {
	dc, release, err := tunnel.openStream(true)
	if err != nil {
		s.logger.Printf("visitor from %s: %v", session.remote, err)
		s.rejectVisitor(session.conn, session.framer, err.Error())
		return
	}
	defer release()

	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
		_ = dc.Close()
		return
	}
	session.framer = protocol.NewFramerWithOptions(session.conn, session.cipher, s.framerOptions())

	visitorSide := &cryptoStreamConn{
		Stream: crypto.NewStream(session.conn, session.cipher),
		conn:   session.conn,
	}
	_ = tunnel.pipeStream(visitorSide, dc, "visitor "+session.remote)
}

// relayDatagramVisitor does the same for a sudp visitor, where each frame is one
// datagram rather than part of a byte stream.
func (s *Server) relayDatagramVisitor(session *visitorSession, tunnel *Tunnel) {
	dc, release, err := tunnel.openStream(true)
	if err != nil {
		s.logger.Printf("visitor from %s: %v", session.remote, err)
		s.rejectVisitor(session.conn, session.framer, err.Error())
		return
	}
	defer release()

	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
		_ = dc.Close()
		return
	}
	session.framer = protocol.NewFramerWithOptions(session.conn, session.cipher, s.framerOptions())

	_ = tunnel.pipeDatagrams(session.framer, dc, "visitor "+session.remote)
}

// serveXTCPVisitor offers the visitor a hole-punch token and waits either for the
// visitor to report that the punch failed or for it to go away.
func (s *Server) serveXTCPVisitor(session *visitorSession, tunnel *Tunnel) {
	if s.p2p == nil {
		// No rendezvous listener: the relayed path is the only path.
		s.relayStreamVisitor(session, tunnel)
		return
	}

	token, err := s.p2p.offer(tunnel)
	if err != nil {
		s.logger.Printf("visitor from %s: cannot start a punch for %q: %v", session.remote, tunnel.Name, err)
		s.relayStreamVisitor(session, tunnel)
		return
	}
	s.metrics.p2pPunches.Add(1)

	if err := session.framer.WriteJSON(protocol.TypeP2PPeer, protocol.P2PPeer{Token: token}); err != nil {
		s.p2p.forget(token)
		_ = session.conn.Close()
		return
	}
	session.framer = protocol.NewFramerWithOptions(session.conn, session.cipher, s.framerOptions())

	// Ask the owner to open its side of the punch.
	prepare := protocol.P2PPrepare{Proxy: tunnel.Name, Token: token}
	if err := tunnel.Session.Framer().WriteJSON(protocol.TypeP2PPrepare, prepare); err != nil {
		s.logger.Printf("visitor from %s: cannot ask the owner of %q to punch: %v", session.remote, tunnel.Name, err)
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
			Event: EventP2PDirect, Remote: session.remote, Proxy: tunnel.Name,
			Outcome: "ok", Detail: "visitor left the rendezvous without asking for a relay",
		})
		_ = session.conn.Close()
		return

	case msg.Type == protocol.TypeP2PFallback:
		s.metrics.p2pRelayed.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventP2PRelayed, Remote: session.remote, Proxy: tunnel.Name,
			Outcome: "ok", Detail: "hole punching failed, using the relayed path",
		})
		s.logger.Printf("visitor %s for %q fell back to the relayed path", session.remote, tunnel.Name)
		s.p2p.forget(token)
		s.relayStreamVisitor(session, tunnel)

	default:
		s.p2p.forget(token)
		s.rejectVisitor(session.conn, session.framer, fmt.Sprintf("expected %s", protocol.TypeP2PFallback))
	}
}
