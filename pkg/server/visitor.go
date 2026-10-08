package server

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/snarkauth"
	"github.com/aethertunnel/aethertunnel/pkg/webrtcvisitor"
)

// punchWait is how long the server keeps a visitor connection open while the
// visitor attempts a direct path before falling back to relaying.
const punchWait = 15 * time.Second

// webrtcOfferWait bounds how long the server waits for an accepted WebRTC
// visitor to send its offer. It is deliberately much larger than the handshake
// timeout — gathering ICE candidates takes seconds on the client, whose own
// ceiling is 30 s — but it is finite: a visitor that never sends the offer
// otherwise held the connection, this goroutine and the session's key material
// until the process ended. A variable rather than a constant so a test can
// shorten it.
var webrtcOfferWait = 60 * time.Second

// punchReportWait is how long the server waits for a path report on the
// rendezvous socket after a visitor's control connection has gone away. The
// report is sent over UDP at the same moment the control connection closes, so a
// short wait is enough to tell a working punch from a visitor that gave up.
const punchReportWait = 2 * time.Second

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

	// sessionSalt is the random salt a visitor session without a key agreement
	// mixes into its key (crypto.SessionKey): without it the configured cipher
	// would seal the whole relayed stream under the long-lived static key.
	// saltSent keeps it to exactly one frame — the same one that would carry
	// kexResponse — so the two sides agree on where the switch happens.
	sessionSalt []byte
	saltSent    bool
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

// takeSessionSalt returns the per-session salt when this is the first frame
// the server sends, and nil afterwards — the same frame that carries the key
// agreement response, so both ends agree on where the switch happens whether
// the session agreed a post-quantum key or derived one from a salt.
func (v *visitorSession) takeSessionSalt() []byte {
	if v.saltSent || len(v.sessionSalt) == 0 {
		return nil
	}
	v.saltSent = true
	return v.sessionSalt
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
		// Counted on the series that names this reason too, exactly as a malformed
		// AuthRequest is: the frame type is dispatched before anything is
		// authenticated, so a payload that does not parse is a garbage frame aimed at
		// this port rather than a policy decision.
		s.metrics.unusableFrames.Add(1)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Detail: "malformed visitor-connect",
		}, "malformed visitor-connect")
		return
	}

	remote := conn.RemoteAddr().String()
	// The same credentials the control connection accepts are accepted here:
	// an [oidc] server also verifies an access token, which is what an [oidc]
	// client presents on its visitor connections when its auth_token is a
	// placeholder.
	tokenAccepted := crypto.EqualTokens(req.AuthToken, s.cfg.Server.AuthToken)
	if !tokenAccepted && s.oidc != nil {
		if err := s.oidc.Verify(req.AuthToken); err == nil {
			tokenAccepted = true
			s.logger.Printf("visitor from %s accepted with an oidc token", remote)
		}
	}
	if !tokenAccepted {
		s.metrics.authFailures.Add(1)
		s.recordAuthFailure(conn)
		logging.Warnf(s.logger, "visitor from %s rejected: invalid auth token", remote)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventAuthFailed, Proxy: req.Proxy,
			Detail: "visitor connection with an invalid auth token",
		}, "invalid auth token")
		return
	}

	if err := s.checkIdentity(identityAssertion{
		PublicKey: req.Identity,
		Nonce:     req.IdentityNonce,
		Timestamp: req.IdentityTime,
		Signature: req.IdentitySignature,
	}); err != nil {
		s.metrics.authFailures.Add(1)
		s.recordAuthFailure(conn)
		logging.Warnf(s.logger, "visitor from %s rejected: %v", remote, err)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventAuthFailed, Proxy: req.Proxy, Detail: err.Error(),
		}, err.Error())
		return
	}

	group, err := s.tunnels.Get(req.Proxy)
	if err != nil {
		reason := fmt.Sprintf("no proxy named %q is registered", req.Proxy)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
		}, reason)
		return
	}
	if !group.Private {
		reason := fmt.Sprintf("proxy %q is not a private proxy", req.Proxy)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
		}, reason)
		return
	}

	// The proxy's visitor rules apply to pairing with a private proxy exactly as
	// they do to a public endpoint: the server's [[proxies]] lists bound which
	// sources may pair with the name, the proxy's own allow_cidrs/deny_cidrs bound
	// it further, and the newUserConn webhooks are the operator's gate. This path
	// used to run neither, so a visitor the policy denied still reached the
	// tunnel — and the denial never reached the metrics or the audit log.
	//
	// The address is checked before the secret: a visitor that is not allowed to
	// pair learns nothing about the proxy beyond the refusal, and no proof
	// exchange is spent on it. The audit entry and the counter come from
	// refuseVisitor, so this refusal is visible where every other one is.
	if reason := group.admitVisitor(conn.RemoteAddr()); reason != "" {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		s.rejectVisitor(conn, framer, reason)
		return
	}
	if allowed, reason := group.visitorAllowed(conn.RemoteAddr()); !allowed {
		group.refuseVisitor(conn.RemoteAddr(), reason)
		s.rejectVisitor(conn, framer, reason)
		return
	}

	// The visitor's transport has to match the shape of the proxy it names. A datagram
	// visitor on a byte-stream proxy makes the server relay TypeUDPPacket frames while the
	// client — which decides by its own proxy type — pipes raw bytes: the framing bytes are
	// written into the local service and its answer comes back unparsable, so the visitor
	// gets nothing at all, and only a service that echoes the frame bytes untouched hides
	// it.
	//
	// xtcp is a byte-stream visitor even though its name suggests otherwise: the client
	// handles it with the stream path and a punch carries a byte stream, so an xtcp visitor
	// on a datagram proxy disagrees in exactly the same way (measured: no answer at all,
	// with or without a punch port).
	visitorWantsDatagrams := req.Type == protocol.ProxyTypeSUDP
	proxyIsDatagram := group.Type == protocol.ProxyTypeSUDP
	if visitorWantsDatagrams != proxyIsDatagram {
		carries, asked := "a byte stream", "a byte stream"
		want := string(protocol.ProxyTypeSTCP)
		if proxyIsDatagram {
			carries, want = "datagrams", string(protocol.ProxyTypeSUDP)
		}
		if visitorWantsDatagrams {
			asked = "datagrams"
		}
		reason := fmt.Sprintf(
			"proxy %q carries %s, and a %q visitor carries %s: use a %s visitor",
			req.Proxy, carries, req.Type, asked, want)
		logging.Warnf(s.logger, "visitor from %s rejected for proxy %q: %s", remote, req.Proxy, reason)
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
		}, reason)
		return
	}

	// The key agreement runs before the proxy is authorised so that the
	// challenge, the proof and everything after it are protected by the agreed
	// key rather than by the configured cipher alone.
	visitorCipher := s.cipher
	var kexResponse, sessionSalt []byte
	if s.cfg.PostQuantum() {
		if len(req.KEX) == 0 {
			reason := "this server requires a post-quantum key exchange (encryption.post_quantum)"
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
			}, reason)
			return
		}
		response, key, err := crypto.HybridServerFinish(req.KEX)
		if err != nil {
			s.metrics.authFailures.Add(1)
			s.recordAuthFailure(conn)
			logging.Warnf(s.logger, "visitor from %s: post-quantum key agreement failed: %v", remote, err)
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy,
				Detail: fmt.Sprintf("post-quantum key agreement failed: %v", err),
			}, "post-quantum key agreement failed")
			return
		}
		visitorCipher, err = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if err != nil {
			logging.Warnf(s.logger, "visitor from %s: the agreed key is unusable: %v", remote, err)
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy,
				Detail: fmt.Sprintf("the agreed key is unusable: %v", err),
			}, "post-quantum key agreement failed")
			return
		}
		kexResponse = response
	} else if s.cipher.Enabled() {
		// Without a key agreement the configured cipher would seal the whole
		// relayed stream under the long-lived static key — the same AES-GCM
		// random-IV bound (NIST SP 800-38D: 2^32 invocations per key) the
		// control sessions escaped with a per-session key. The salt rides the
		// challenge, the frame the configured cipher still protects, and both
		// ends switch right after it: a visitor that never sees one keeps the
		// configured cipher, so new visitors still talk to old servers, and an
		// old visitor against this server fails after the challenge — both
		// ends must move together.
		salt, saltErr := crypto.SessionSalt()
		if saltErr != nil {
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy,
				Detail: fmt.Sprintf("cannot draw a session salt: %v", saltErr),
			}, "the server cannot draw a session salt")
			return
		}
		key, keyErr := crypto.SessionKey(req.AuthToken, salt)
		if keyErr != nil {
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy,
				Detail: fmt.Sprintf("the session key is unusable: %v", keyErr),
			}, "the session key is unusable")
			return
		}
		visitorCipher, keyErr = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if keyErr != nil {
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy,
				Detail: fmt.Sprintf("the session key is unusable: %v", keyErr),
			}, "the session key is unusable")
			return
		}
		sessionSalt = salt
	}

	session := &visitorSession{
		conn: conn, framer: framer, cipher: visitorCipher,
		remote: remote, kexResponse: kexResponse, sessionSalt: sessionSalt,
	}

	// Feature gates are the server's own policy: a client whose configuration has
	// no gates still meets them here. Both refusals keep the session's teardown
	// path, so the accounting and the audit record match every other rejection.
	if req.Transport == config.TransportWebRTC && !s.cfg.FeatureEnabled(config.FeatureWebRTC) {
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
			Outcome: "denied", Detail: "the WebRTC transport is turned off on this server (feature_gates.WebRTC)",
		}, "the WebRTC transport is turned off on this server (feature_gates.WebRTC)")
		return
	}
	if group.AuthMethod == config.AuthMethodSNARK && !s.cfg.FeatureEnabled(config.FeatureSnark) {
		s.refuseVisitor(conn, framer, AuditEvent{
			Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
			Outcome: "denied", Detail: "the Groth16 visitor proof is turned off on this server (feature_gates.Snark)",
		}, "the Groth16 visitor proof is turned off on this server (feature_gates.Snark)")
		return
	}

	switch group.AuthMethod {
	case config.AuthMethodNIZK, config.AuthMethodSNARK:
		if req.Secret != "" {
			// The visitor sent the proxy's secret, which clientlib does only
			// when its own auth_method is "secret", while this proxy is
			// registered for a proof method. The challenge is skipped so the
			// mismatch is refused instead of being answered with a Schnorr
			// proof the server would accept for a nizk proxy — the silent
			// downgrade the visitor documentation promises not to happen.
			//
			// The secret is already on this connection: whether to send it is
			// the visitor's local decision, made before the server can see the
			// proxy it names, so this refusal stops the acceptance, not the
			// exposure.
			reason := "the visitor's auth_method does not match the proxy's"
			s.metrics.authFailures.Add(1)
			s.recordAuthFailure(conn)
			logging.Warnf(s.logger, "visitor from %s rejected for proxy %q: %s", remote, req.Proxy, reason)
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
			}, reason)
			return
		}
		if err := s.challengeVisitor(session, group, kexResponse); err != nil {
			s.metrics.authFailures.Add(1)
			s.recordAuthFailure(conn)
			// challengeVisitor has already written the refusal on the wire, so this is
			// only the accounting half of what refuseVisitor does for the other paths.
			s.metrics.controlRejected.Add(1)
			s.auditor.Record(AuditEvent{
				Event: EventVisitorRejected, Remote: remote, Proxy: req.Proxy,
				Outcome: "denied", Detail: err.Error(),
			})
			logging.Warnf(s.logger, "visitor from %s rejected for proxy %q: %v", remote, req.Proxy, err)
			return
		}
	default:
		if !group.matchesSecret(req.Secret) {
			s.metrics.authFailures.Add(1)
			s.recordAuthFailure(conn)
			logging.Warnf(s.logger, "visitor from %s rejected for proxy %q: invalid secret key", remote, req.Proxy)
			s.refuseVisitor(conn, framer, AuditEvent{
				Event: EventVisitorRejected, Proxy: req.Proxy, Detail: "invalid secret key",
			}, "invalid secret key")
			return
		}
	}

	// The secret key is proven before the identity is checked: a visitor that
	// does not hold the key learns nothing about who may use the proxy. frp
	// checks allow_users first, which turns the proxy into an oracle for the
	// identities it allows.
	if allowed, reason := group.visitorIdentityAllow(req.User); !allowed {
		s.metrics.authFailures.Add(1)
		s.recordAuthFailure(conn)
		logging.Warnf(s.logger, "visitor from %s rejected for proxy %q: %s", remote, req.Proxy, reason)
		// session.framer, not the framer argument: an auth_method of "nizk"
		// or "snark" has already run challengeVisitor, which moved the
		// connection onto the agreed session key. A refusal written through
		// the configured cipher would not decrypt on the visitor's side, so
		// the visitor would see a decryption failure instead of the reason.
		s.refuseVisitor(conn, session.framer, AuditEvent{
			Event: EventVisitorRejected, Proxy: req.Proxy, Detail: reason,
		}, reason)
		return
	}

	s.metrics.controlAccepted.Add(1)
	s.recordAuthSuccess(conn)
	s.auditor.Record(AuditEvent{
		Event: EventVisitorAccepted, Remote: remote, Proxy: req.Proxy,
		Outcome: "ok",
		Detail:  fmt.Sprintf("type=%s auth=%s agreed_key=%v session_key=%v", group.Type, group.AuthMethod, len(kexResponse) > 0, len(sessionSalt) > 0),
	})

	// The transport decides the data path before the type does. An xtcp visitor
	// that asks for WebRTC used to take the punch path, which sends a punch offer
	// the client answers by punching; the WebRTC offer it sends after a failed
	// punch was then relayed to the local service as data, and the visitor waited
	// out the answer timeout. The client only offers WebRTC when no punch offer
	// arrives, so dispatching on the transport here is what it expects.
	//
	// A datagram visitor is left to the datagram relay: WebRTC carries a byte
	// stream, the client's own configuration check refuses that combination, and a
	// hand-written one is served by the shape its type names.
	if req.Transport == config.TransportWebRTC && !visitorWantsDatagrams {
		s.serveWebRTCVisitor(session, group, req)
		return
	}

	switch req.Type {
	case protocol.ProxyTypeXTCP:
		s.serveXTCPVisitor(session, group)
	case protocol.ProxyTypeSUDP:
		s.relayDatagramVisitor(session, group)
	default:
		s.relayStreamVisitor(session, group)
	}
}

// serveWebRTCVisitor moves a stream visitor's data path onto a WebRTC
// DataChannel. The signaling is this control connection: the answer frame
// goes out after the acceptance, and everything the visitor sends from then
// on arrives through DTLS over ICE instead of through the relay.
func (s *Server) serveWebRTCVisitor(session *visitorSession, group *ProxyGroup, req protocol.VisitorConnect) {
	// The control connection is this function's to close: every error path
	// below used to return without it — and without an answer frame — which
	// left the visitor blocked on a read that no deadline bounds, and left
	// the socket half-open here after a success. The client reads EOF as the
	// refusal.
	defer func() { _ = session.conn.Close() }()

	ack := protocol.DataOpenAck{OK: true, KEX: session.takeKEX(), SessionSalt: session.takeSessionSalt()}
	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, ack); err != nil {
		return
	}
	session.switchFramer(s.framerOptions())

	// The handshake deadline handleConn set bounds the first frame only, and ICE
	// gathering on the client legitimately takes longer than it — but it does
	// end. This wait is generous rather than absent, and it is the only bound the
	// visitor path was missing: without it an authenticated visitor that never
	// sent its offer held this goroutine, the socket and the session's key
	// material for as long as the process lived.
	_ = session.conn.SetReadDeadline(time.Now().Add(webrtcOfferWait))

	var offer protocol.VisitorWebRTCOffer
	readErr := session.framer.ReadJSON(protocol.TypeVisitorWebRTCOffer, &offer)
	// Only this read is bounded: the session that follows is not.
	_ = session.conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		s.logger.Printf("webrtc visitor for proxy %q: %v", req.Proxy, readErr)
		return
	}
	answerSDP, wait, cleanup, err := webrtcvisitor.ServerAccept(offer.SDP)
	if err != nil {
		s.logger.Printf("webrtc visitor for proxy %q: %v", req.Proxy, err)
		return
	}
	defer cleanup()
	if err := session.framer.WriteJSON(protocol.TypeVisitorWebRTCAnswer, protocol.VisitorWebRTCAnswer{SDP: answerSDP}); err != nil {
		s.logger.Printf("webrtc visitor for proxy %q: %v", req.Proxy, err)
		return
	}
	dataPath, err := wait()
	if err != nil {
		s.logger.Printf("webrtc visitor for proxy %q: %v", req.Proxy, err)
		return
	}
	defer dataPath.Close()
	// The visitor's bytes travel over this channel instead of the relay, and
	// nothing else says so: a failure here is logged, a success was not, which
	// left an operator unable to tell a visitor that moved onto its data channel
	// from one that quietly took the relayed path.
	s.logger.Printf("webrtc visitor for proxy %q from %s: data channel established", req.Proxy, session.remote)

	member, stream, err := group.openForVisitor()
	if err != nil {
		s.logger.Printf("webrtc visitor from %s: %v", session.remote, err)
		return
	}
	defer stream.release()
	_ = member.pipeStream(dataPath, stream.dc, "webrtc visitor "+session.remote)
}

// challengeVisitor asks a visitor to prove it knows the proxy's secret key. The
// key itself never crosses the wire: with auth_method = "nizk" the visitor proves
// knowledge of the discrete logarithm of the public key derived from it, and with
// "snark" it proves the same knowledge as a Groth16 proof. Either way the proof is
// bound to a nonce the server picks for this connection, so a proof recorded from
// one connection is worthless on another.
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
		Proxy:       group.Name,
		Nonce:       nonce,
		KEX:         session.takeKEX(),
		SessionSalt: session.takeSessionSalt(),
	}); err != nil {
		// Nothing else owns this connection on this path: the probe loop below never
		// ran, so without this the socket stays open until the process exits.
		_ = session.conn.Close()
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

	if err := s.verifyVisitorProof(group, nonce, answer.Proof); err != nil {
		s.rejectVisitor(session.conn, session.framer, "the proof does not verify")
		return err
	}
	return nil
}

// verifyVisitorProof checks the answer to the challenge with the method the proxy
// registered. The context is built the same way for both methods, so a proof made
// for one proxy or one nonce fails here whichever method produced it.
func (s *Server) verifyVisitorProof(group *ProxyGroup, nonce, proof []byte) error {
	context := protocol.VisitorProofContext(group.Name, nonce)
	if group.AuthMethod == config.AuthMethodSNARK {
		return snarkauth.Verify(proof, []byte(group.SecretKey), context)
	}
	return crypto.SchnorrVerify(group.SecretPublicKey, context, proof)
}

// refuseVisitor answers a visitor the server has not accepted with a refusal and books
// it. A visitor arrives on the control port and is answered the way an ordinary client
// is, so its refusals have to reach the same two places a control connection's do: the
// aggregate rejection counter — the one an alert watches when the reason does not
// matter — and the audit log, which is what makes the refusal attributable afterwards.
// Refusing the connection and the visitor being told are the same event, so the reason
// on the wire and in the log is one argument.
//
// A refusal after acceptance, which is a relay that cannot be set up rather than a
// connection that was turned away, goes through rejectVisitor alone: the connection was
// already accepted, and counting it again would make accepted plus rejected exceed the
// connections the server answered.
func (s *Server) refuseVisitor(conn net.Conn, framer *protocol.Framer, event AuditEvent, reason string) {
	event.Remote = conn.RemoteAddr().String()
	event.Outcome = "denied"
	s.metrics.controlRejected.Add(1)
	s.auditor.Record(event)
	s.rejectVisitor(conn, framer, reason)
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

	ack := protocol.DataOpenAck{OK: true, KEX: session.takeKEX(), SessionSalt: session.takeSessionSalt()}
	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, ack); err != nil {
		_ = stream.dc.Close()
		// Nothing else closes this connection on this path — the pipe that would
		// have owned it never started — so a visitor that reset as its stream was
		// being established left a socket in CLOSE_WAIT for the life of the
		// process.
		_ = session.conn.Close()
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

	ack := protocol.DataOpenAck{OK: true, KEX: session.takeKEX(), SessionSalt: session.takeSessionSalt()}
	if err := session.framer.WriteJSON(protocol.TypeDataOpenAck, ack); err != nil {
		_ = stream.dc.Close()
		_ = session.conn.Close()
		return
	}
	session.switchFramer(s.framerOptions())

	_ = member.pipeDatagrams(session.framer, session.conn, stream.dc, "visitor "+session.remote)
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
		logging.Warnf(s.logger, "visitor from %s: cannot start a punch for %q: %v", session.remote, group.Name, err)
		s.relayStreamVisitor(session, group)
		return
	}
	s.metrics.p2pPunches.Add(1)

	offer := protocol.P2PPeer{Token: token, KEX: session.takeKEX(), SessionSalt: session.takeSessionSalt()}
	if err := session.framer.WriteJSON(protocol.TypeP2PPeer, offer); err != nil {
		s.p2p.forget(token)
		_ = session.conn.Close()
		return
	}
	session.switchFramer(s.framerOptions())

	// Ask one member to open its side of the punch.
	prepare := protocol.P2PPrepare{Proxy: group.Name, Token: token}
	if err := owner.Session.Framer().WriteJSON(protocol.TypeP2PPrepare, prepare); err != nil {
		logging.Warnf(s.logger, "visitor from %s: cannot ask the owner of %q to punch: %v", session.remote, group.Name, err)
		s.p2p.forget(token)
		_ = session.conn.Close()
		return
	}

	_ = session.conn.SetReadDeadline(time.Now().Add(punchWait))
	msg, err := session.framer.ReadFrame()
	_ = session.conn.SetReadDeadline(time.Time{})

	switch {
	case err != nil:
		// The visitor closed the control connection. A visitor that punched a
		// direct path stops using this connection without saying so, which is why
		// it reports the path over the rendezvous socket instead. That datagram
		// can land just after the connection is gone, so it is given a moment.
		path, reported := s.p2p.awaitPath(token, punchReportWait)
		s.p2p.forget(token)

		if reported && path == protocol.PunchPathDirect {
			s.metrics.p2pDirect.Add(1)
			s.auditor.Record(AuditEvent{
				Event: EventP2PDirect, Remote: session.remote, Proxy: group.Name,
				Outcome: "ok", Detail: "the visitor reported a direct path to the owner",
			})
			s.logger.Printf("visitor %s for %q reports a direct path", session.remote, group.Name)
			_ = session.conn.Close()
			return
		}

		// Nothing was reported: the visitor gave up, was killed, or lost the
		// report on the way. It is not known to have reached the owner, so it is
		// not counted as a direct path.
		detail := "the visitor left the rendezvous without asking for a relay or reporting a direct path"
		if reported {
			detail = fmt.Sprintf("the visitor reported path %q, which this server does not act on", string(path))
		}
		s.auditor.Record(AuditEvent{
			Event: EventP2PAbandoned, Remote: session.remote, Proxy: group.Name,
			Outcome: "unknown", Detail: detail,
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
