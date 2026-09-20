package server

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Options carries the build metadata the server reports to clients, to the
// dashboard and on its own startup line.
type Options struct {
	Version   string
	BuildTime string
	GitCommit string
	Logger    *log.Logger
}

// Server is the AetherTunnel server.
//
// One listening socket carries both control connections (a client authenticates
// and keeps the session alive) and data connections (a client dials back to carry
// one stream of a published tunnel). Keeping a single port means a firewall only
// needs one rule, and it removes the v1 bug where the same address was bound twice
// and the second bind aborted startup.
type Server struct {
	cfg    *config.Config
	cipher *crypto.Cipher
	logger *log.Logger

	version   string
	buildTime string
	gitCommit string

	startedAt time.Time

	sessions *SessionManager
	tunnels  *TunnelManager
	metrics  *Metrics
	acl      *AccessControl
	auditor  *Auditor
	vhost    *vhostSet
	p2p      *p2pRendezvous

	tlsConfig  *tls.Config
	identities []ed25519.PublicKey
	nonces     *crypto.NonceCache

	listener net.Listener

	totalConnections atomic.Int64

	closing atomic.Bool
	wg      sync.WaitGroup
}

// New builds a server from a validated configuration.
func New(cfg *config.Config, opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	cipher, err := cfg.Cipher(config.RoleServer)
	if err != nil {
		return nil, err
	}
	acl, err := NewAccessControl(cfg.Server.AllowCIDRs, cfg.Server.DenyCIDRs,
		cfg.Server.RateLimitPerSecond, cfg.Server.RateLimitBurst)
	if err != nil {
		return nil, err
	}
	auditor, err := NewAuditor(cfg.Audit.Enabled, cfg.Audit.Path, cfg.Audit.MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	tlsConfig, err := cfg.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	identities, err := cfg.AllowedIdentities()
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:        cfg,
		cipher:     cipher,
		logger:     logger,
		version:    opts.Version,
		buildTime:  opts.BuildTime,
		gitCommit:  opts.GitCommit,
		startedAt:  time.Now(),
		metrics:    newMetrics(),
		acl:        acl,
		auditor:    auditor,
		tlsConfig:  tlsConfig,
		identities: identities,
		nonces:     crypto.NewNonceCache(0),
	}
	sessions := newSessionManager(cfg.Server.MaxConnections)
	s.sessions = sessions
	s.tunnels = newTunnelManager(cfg, logger, cipher, sessions, s.metrics)

	s.vhost = newVhostSet(cfg, logger, s.metrics)
	s.tunnels.vhost = s.vhost
	if cfg.Server.P2PPort > 0 {
		s.p2p = newP2PRendezvous(logger)
		s.tunnels.p2p = s.p2p
	}
	return s, nil
}

// Metrics exposes the counter set for the dashboard endpoint.
func (s *Server) Metrics() *Metrics { return s.metrics }

// Auditor exposes the audit log for call sites outside the accept path.
func (s *Server) Auditor() *Auditor { return s.auditor }

// framerOptions maps the [obfuscation] section onto frame-level padding and
// jitter. The receiver unpads from the frame flag alone, so the two ends do not
// have to agree on these values.
func (s *Server) framerOptions() protocol.FramerOptions {
	return protocol.FramerOptions{
		MaxPayload: protocol.DefaultMaxPayload,
		PadTo:      s.cfg.Obfuscation.PadTo,
		Jitter:     time.Duration(s.cfg.Obfuscation.JitterMillis) * time.Millisecond,
	}
}

// Cipher reports the encryption algorithm in use ("none" when disabled).
func (s *Server) Cipher() string { return s.cipher.Algorithm() }

// Run listens and serves until the listener is closed or ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenAddr(), err)
	}
	if s.tlsConfig != nil {
		listener = tls.NewListener(listener, s.tlsConfig)
	}
	s.listener = listener

	s.logger.Printf("AetherTunnel server %s (protocol %d) listening on %s", s.version, protocol.ProtocolVersion, listener.Addr())
	s.logger.Printf("encryption: %s", s.Cipher())
	if s.cfg.PostQuantum() {
		s.logger.Printf("post-quantum key agreement: X25519 with ML-KEM-768, one key per data connection")
	}
	if s.tlsConfig != nil {
		s.logger.Printf("the control port is wrapped in TLS")
	}
	if s.cfg.Identity.Enabled {
		s.logger.Printf("client identities: %d allowed key(s), require_identity=%v",
			len(s.identities), s.cfg.Identity.RequireIdentity)
	}
	s.logger.Printf("max connections: %d, heartbeat: %ds, idle timeout: %ds",
		s.cfg.Server.MaxConnections, s.cfg.Server.HeartbeatSeconds, s.cfg.Server.ReadTimeoutSecs)

	if err := s.vhost.start(); err != nil {
		_ = listener.Close()
		return err
	}
	if s.p2p != nil {
		if err := s.p2p.Start(s.cfg.P2PAddr()); err != nil {
			s.vhost.stop()
			_ = listener.Close()
			return err
		}
	}

	go func() {
		<-ctx.Done()
		s.Shutdown("server shutting down")
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if s.closing.Load() {
				s.wg.Wait()
				return nil
			}
			s.logger.Printf("accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.totalConnections.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// admit applies the access-control rules to a freshly accepted connection. It
// returns false when the connection has already been closed.
func (s *Server) admit(conn net.Conn) bool {
	switch s.acl.Check(conn) {
	case DenyCIDR:
		s.metrics.aclDenied.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventACLDenied, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "source address rejected by allow/deny lists",
		})
		s.logger.Printf("connection from %s denied by access control", conn.RemoteAddr())
		_ = conn.Close()
		return false
	case DenyRate:
		s.metrics.rateLimited.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventRateLimited, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "source address exceeded its connection rate",
		})
		s.logger.Printf("connection from %s denied by rate limit", conn.RemoteAddr())
		_ = conn.Close()
		return false
	default:
		return true
	}
}

// Shutdown stops the listener and disconnects every client.
func (s *Server) Shutdown(reason string) {
	if s.closing.Swap(true) {
		return
	}
	s.logger.Printf("shutting down: %s", reason)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.vhost.stop()
	if s.p2p != nil {
		_ = s.p2p.Close()
	}
	s.sessions.CloseAll(reason)
	if err := s.auditor.Close(); err != nil {
		s.logger.Printf("closing the audit log: %v", err)
	}
}

// checkIdentity verifies the Ed25519 assertion a client may attach to its auth
// request. It returns nil when no assertion was offered and none is required.
func (s *Server) checkIdentity(req *protocol.AuthRequest) error {
	if !s.cfg.Identity.Enabled {
		return nil
	}
	if len(req.Identity) == 0 {
		if s.cfg.Identity.RequireIdentity {
			return errors.New("this server requires a client identity; set [identity].enabled and [identity].key_file")
		}
		return nil
	}

	publicKey, err := crypto.ParseIdentityKey(hex.EncodeToString(req.Identity))
	if err != nil {
		return err
	}
	if err := crypto.VerifyIdentity(crypto.IdentityCheckInput{
		PublicKey: publicKey,
		Nonce:     req.IdentityNonce,
		Timestamp: req.IdentityTime,
		Signature: req.IdentitySignature,
		Allowed:   s.identities,
		Seen:      s.nonces,
	}); err != nil {
		return err
	}
	return nil
}

// handleConn dispatches the first frame: an auth request opens a control session,
// a data-open claims a stream that a public connection is waiting for.
func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			// A panic in one connection must not take the server down.
			s.logger.Printf("panic while handling %s: %v", conn.RemoteAddr(), r)
			_ = conn.Close()
		}
	}()

	if !s.admit(conn) {
		return
	}

	handshakeTimeout := time.Duration(s.cfg.Server.HandshakeTimeoutSecs) * time.Second
	if handshakeTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	}

	framer := protocol.NewFramerWithOptions(conn, s.cipher, s.framerOptions())
	msg, err := framer.ReadFrame()
	if err != nil {
		s.logger.Printf("handshake from %s failed: %v", conn.RemoteAddr(), err)
		_ = conn.Close()
		return
	}

	switch msg.Type {
	case protocol.TypeAuthRequest:
		s.handleControl(conn, framer, msg)
	case protocol.TypeDataOpen:
		s.handleData(conn, framer, msg)
	case protocol.TypeVisitorConnect:
		s.handleVisitor(conn, framer, msg)
	default:
		s.logger.Printf("unexpected first frame %s from %s", msg.Type, conn.RemoteAddr())
		_ = framer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{
			Error: fmt.Sprintf("first frame must be %s, %s or %s, got %s",
				protocol.TypeAuthRequest, protocol.TypeDataOpen, protocol.TypeVisitorConnect, msg.Type),
		})
		_ = conn.Close()
	}
}

// handleControl authenticates a client and then serves its session loop.
func (s *Server) handleControl(conn net.Conn, framer *protocol.Framer, msg *protocol.Message) {
	var req protocol.AuthRequest
	if len(msg.Payload) == 0 || json.Unmarshal(msg.Payload, &req) != nil {
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "malformed auth request",
		})
		_ = conn.Close()
		return
	}

	// Constant-time comparison so the token cannot be recovered byte by byte from
	// response timing, and never log the offered token.
	if !crypto.EqualTokens(req.Token, s.cfg.Server.AuthToken) {
		s.metrics.authFailures.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: "invalid auth token",
		})
		s.logger.Printf("authentication failed for %s (client %s)", conn.RemoteAddr(), req.ClientVersion)
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "invalid auth token",
		})
		_ = conn.Close()
		return
	}

	reject := func(reason string, identityRequired bool) {
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: reason, IdentityRequired: identityRequired,
		})
		_ = conn.Close()
	}

	if err := s.checkIdentity(&req); err != nil {
		s.metrics.authFailures.Add(1)
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventAuthFailed, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: err.Error(),
		})
		s.logger.Printf("identity check failed for %s: %v", conn.RemoteAddr(), err)
		reject(err.Error(), s.cfg.Identity.RequireIdentity)
		return
	}

	// The post-quantum handshake runs before the session exists, because the
	// session's key is what protects every stream it later opens.
	sessionCipher := s.cipher
	var sessionKey, kexResponse []byte
	if s.cfg.PostQuantum() {
		if len(req.KEX) == 0 {
			reject("this server requires a post-quantum key exchange (encryption.post_quantum)", false)
			return
		}
		response, key, err := crypto.HybridServerFinish(req.KEX)
		if err != nil {
			s.metrics.authFailures.Add(1)
			s.metrics.controlRejected.Add(1)
			s.logger.Printf("post-quantum key agreement with %s failed: %v", conn.RemoteAddr(), err)
			reject("post-quantum key agreement failed", false)
			return
		}
		sessionCipher, err = crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, key)
		if err != nil {
			s.logger.Printf("post-quantum session key for %s is unusable: %v", conn.RemoteAddr(), err)
			reject("post-quantum key agreement failed", false)
			return
		}
		sessionKey, kexResponse = key, response
	}

	if req.Protocol != protocol.ProtocolVersion {
		s.logger.Printf("client %s speaks protocol %d, this server speaks %d: continuing, but upgrade the client if traffic misbehaves",
			conn.RemoteAddr(), req.Protocol, protocol.ProtocolVersion)
	}

	session := newSession(conn, framer, &req, sessionCipher.Enabled(), s.cfg.Server.HeartbeatSeconds)
	session.streamKey = sessionKey
	if err := s.sessions.Add(session); err != nil {
		s.metrics.controlRejected.Add(1)
		s.auditor.Record(AuditEvent{
			Event: EventControlRejected, Remote: conn.RemoteAddr().String(),
			Outcome: "denied", Detail: err.Error(),
		})
		s.logger.Printf("rejecting %s: %v", conn.RemoteAddr(), err)
		reject(err.Error(), false)
		return
	}
	s.metrics.controlAccepted.Add(1)

	response := protocol.AuthResponse{
		OK:               true,
		Session:          session.ID,
		ServerVersion:    s.version,
		Protocol:         protocol.ProtocolVersion,
		Encryption:       s.Cipher(),
		HeartbeatSecs:    s.cfg.Server.HeartbeatSeconds,
		ProtocolMismatch: req.Protocol != protocol.ProtocolVersion,
		P2PPort:          s.cfg.Server.P2PPort,
		KEX:              kexResponse,
	}
	// The response itself is still protected by the configured cipher; both
	// sides switch to the agreed key immediately afterwards.
	if err := framer.WriteJSON(protocol.TypeAuthResponse, response); err != nil {
		s.sessions.Remove(session.ID)
		session.Close("failed to send auth response")
		return
	}
	if len(sessionKey) > 0 {
		session.framer = protocol.NewFramerWithOptions(conn, sessionCipher, s.framerOptions())
		s.logger.Printf("post-quantum session key %s agreed with %s", crypto.SessionKeyID(sessionKey), conn.RemoteAddr())
	}

	s.logger.Printf("client %s connected as %s (protocol %d, encryption %s, version %s)",
		conn.RemoteAddr(), session.ID, req.Protocol, s.Cipher(), req.ClientVersion)

	defer func() {
		s.tunnels.RemoveSessionTunnels(session, "client disconnected")
		s.sessions.Remove(session.ID)
		session.Close("control connection ended")
		s.auditor.Record(AuditEvent{
			Event: EventClientGone, ClientID: session.ID, Remote: session.RemoteAddr,
			Outcome: "closed",
			Detail:  fmt.Sprintf("connected for %s", session.Uptime().Round(time.Second)),
		})
		s.logger.Printf("client %s (%s) disconnected after %s",
			session.ID, session.RemoteAddr, session.Uptime().Round(time.Second))
	}()

	s.sessionLoop(session)
}

// sessionLoop serves control frames until the client goes away or stops sending
// heartbeats.
func (s *Server) sessionLoop(session *Session) {
	heartbeat := time.Duration(session.HeartbeatSeconds) * time.Second
	deadline := 3 * heartbeat

	for {
		if deadline > 0 {
			_ = session.conn.SetReadDeadline(time.Now().Add(deadline))
		}

		msg, err := session.framer.ReadFrame()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				s.logger.Printf("client %s missed its heartbeat window (%s), closing", session.ID, deadline)
			} else {
				s.logger.Printf("client %s control read ended: %v", session.ID, err)
			}
			return
		}

		switch msg.Type {
		case protocol.TypeHeartbeat:
			session.touchHeartbeat()
			if err := session.framer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeatAck}); err != nil {
				return
			}

		case protocol.TypeRegisterProxy:
			var spec protocol.ProxySpec
			if err := json.Unmarshal(msg.Payload, &spec); err != nil {
				s.replyError(session, fmt.Sprintf("malformed register-proxy: %v", err))
				continue
			}
			tunnel, err := s.tunnels.Register(session, spec)
			if err != nil {
				s.logger.Printf("client %s: cannot register proxy %q: %v", session.ID, spec.Name, err)
				s.auditor.Record(AuditEvent{
					Event: EventProxyRejected, ClientID: session.ID, Proxy: spec.Name,
					Outcome: "denied", Detail: err.Error(),
				})
				s.replyError(session, err.Error())
				continue
			}
			if err := session.RegisterProxy(tunnel); err != nil {
				s.tunnels.Unregister(spec.Name, "session closed during registration")
				s.replyError(session, err.Error())
				continue
			}
			s.auditor.Record(AuditEvent{
				Event: EventProxyRegistered, ClientID: session.ID, Proxy: spec.Name,
				Outcome: "ok",
				Detail:  fmt.Sprintf("type=%s local=%s remote_port=%d", spec.Type, spec.LocalAddr, spec.RemotePort),
			})
			s.sendProxyList(session)

		case protocol.TypeError:
			s.logger.Printf("client %s reported an error: %s", session.ID, string(msg.Payload))

		default:
			s.logger.Printf("client %s sent %s on the control connection; ignoring", session.ID, msg.Type)
		}
	}
}

func (s *Server) replyError(session *Session, message string) {
	if err := session.framer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{Error: message}); err != nil {
		s.logger.Printf("client %s: could not deliver error %q: %v", session.ID, message, err)
	}
}

func (s *Server) sendProxyList(session *Session) {
	statuses := make([]protocol.ProxyStatus, 0)

	s.tunnels.mu.RLock()
	for _, tunnel := range s.tunnels.tunnels {
		if tunnel.Session != session {
			continue
		}
		statuses = append(statuses, protocol.ProxyStatus{
			Name:        tunnel.Name,
			Type:        tunnel.Spec.Type,
			LocalAddr:   tunnel.Spec.LocalAddr,
			RemotePort:  tunnel.RemotePort,
			ClientID:    session.ID,
			Active:      tunnel.Active.Load(),
			TotalOpened: tunnel.Total.Load(),
			BytesIn:     tunnel.BytesIn.Load(),
			BytesOut:    tunnel.BytesOut.Load(),
		})
	}
	s.tunnels.mu.RUnlock()

	if err := session.framer.WriteJSON(protocol.TypeProxyList, statuses); err != nil {
		s.logger.Printf("client %s: could not send proxy list: %v", session.ID, err)
	}
}

// handleData accepts a data connection from a client and hands it to the public
// connection that is waiting for that stream id.
func (s *Server) handleData(conn net.Conn, framer *protocol.Framer, msg *protocol.Message) {
	var open protocol.DataOpen
	if len(msg.Payload) == 0 || json.Unmarshal(msg.Payload, &open) != nil {
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: "malformed data-open"})
		_ = conn.Close()
		return
	}
	s.metrics.dataConnections.Add(1)

	fail := func(reason string) {
		s.logger.Printf("data connection from %s rejected: %s", conn.RemoteAddr(), reason)
		_ = framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: false, Error: reason})
		_ = conn.Close()
	}

	session, ok := s.sessions.Get(open.Session)
	if !ok {
		fail("unknown or expired session")
		return
	}

	waiting, ok := session.TakePending(open.StreamID)
	if !ok {
		fail("no public connection is waiting for that stream")
		return
	}

	if err := framer.WriteJSON(protocol.TypeDataOpenAck, protocol.DataOpenAck{OK: true}); err != nil {
		_ = conn.Close()
		return
	}

	// The ack has been written, and both ends now switch to the key derived for
	// this stream, so the bytes that follow never share a keystream with another
	// stream of the same session.
	dc := &dataConn{conn: conn, framer: framer, cipher: s.cipher}
	if len(session.streamKey) > 0 {
		streamKey, keyErr := crypto.StreamKey(session.streamKey, open.StreamID)
		streamCipher, cipherErr := crypto.NewCipherFromKey(s.cfg.Encryption.Algorithm, streamKey)
		switch {
		case keyErr != nil || cipherErr != nil:
			s.logger.Printf("data connection from %s: cannot derive a stream key: %v%v",
				conn.RemoteAddr(), keyErr, cipherErr)
		default:
			dc.cipher = streamCipher
			dc.framer = protocol.NewFramerWithOptions(conn, streamCipher, s.framerOptions())
		}
	}

	// From here the connection carries the stream — raw bytes for a stream
	// proxy, TypeUDPPacket frames for a datagram proxy. Handing it over is a
	// non-blocking send because waiting is buffered with capacity 1 and only
	// ever used once.
	waiting <- dc
}

// totalBytes reports aggregate tunnel traffic.
func (s *Server) totalBytes() (in, out int64) {
	for _, tunnel := range s.tunnels.List() {
		in += tunnel.BytesIn.Load()
		out += tunnel.BytesOut.Load()
	}
	return in, out
}
