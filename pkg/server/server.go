package server

import (
	"context"
	"encoding/json"
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

	s := &Server{
		cfg:       cfg,
		cipher:    cipher,
		logger:    logger,
		version:   opts.Version,
		buildTime: opts.BuildTime,
		gitCommit: opts.GitCommit,
		startedAt: time.Now(),
	}
	sessions := newSessionManager(cfg.Server.MaxConnections)
	s.sessions = sessions
	s.tunnels = newTunnelManager(cfg, logger, cipher, sessions)
	return s, nil
}

// Cipher reports the encryption algorithm in use ("none" when disabled).
func (s *Server) Cipher() string { return s.cipher.Algorithm() }

// Run listens and serves until the listener is closed or ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenAddr(), err)
	}
	s.listener = listener

	s.logger.Printf("AetherTunnel server %s (protocol %d) listening on %s", s.version, protocol.ProtocolVersion, listener.Addr())
	s.logger.Printf("encryption: %s", s.Cipher())
	s.logger.Printf("max connections: %d, heartbeat: %ds, idle timeout: %ds",
		s.cfg.Server.MaxConnections, s.cfg.Server.HeartbeatSeconds, s.cfg.Server.ReadTimeoutSecs)

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

// Shutdown stops the listener and disconnects every client.
func (s *Server) Shutdown(reason string) {
	if s.closing.Swap(true) {
		return
	}
	s.logger.Printf("shutting down: %s", reason)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.sessions.CloseAll(reason)
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

	handshakeTimeout := time.Duration(s.cfg.Server.HandshakeTimeoutSecs) * time.Second
	if handshakeTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	}

	framer := protocol.NewFramer(conn, s.cipher, protocol.DefaultMaxPayload)
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
	default:
		s.logger.Printf("unexpected first frame %s from %s", msg.Type, conn.RemoteAddr())
		_ = framer.WriteJSON(protocol.TypeError, protocol.ErrorPayload{
			Error: fmt.Sprintf("first frame must be %s or %s, got %s",
				protocol.TypeAuthRequest, protocol.TypeDataOpen, msg.Type),
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
		s.logger.Printf("authentication failed for %s (client %s)", conn.RemoteAddr(), req.ClientVersion)
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: "invalid auth token",
		})
		_ = conn.Close()
		return
	}

	if req.Protocol != protocol.ProtocolVersion {
		s.logger.Printf("client %s speaks protocol %d, this server speaks %d: continuing, but upgrade the client if traffic misbehaves",
			conn.RemoteAddr(), req.Protocol, protocol.ProtocolVersion)
	}

	session := newSession(conn, framer, &req, s.cipher.Enabled(), s.cfg.Server.HeartbeatSeconds)
	if err := s.sessions.Add(session); err != nil {
		s.logger.Printf("rejecting %s: %v", conn.RemoteAddr(), err)
		_ = framer.WriteJSON(protocol.TypeAuthResponse, protocol.AuthResponse{
			OK: false, ServerVersion: s.version, Protocol: protocol.ProtocolVersion,
			Encryption: s.Cipher(), Error: err.Error(),
		})
		_ = conn.Close()
		return
	}

	response := protocol.AuthResponse{
		OK:               true,
		Session:          session.ID,
		ServerVersion:    s.version,
		Protocol:         protocol.ProtocolVersion,
		Encryption:       s.Cipher(),
		HeartbeatSecs:    s.cfg.Server.HeartbeatSeconds,
		ProtocolMismatch: req.Protocol != protocol.ProtocolVersion,
	}
	if err := framer.WriteJSON(protocol.TypeAuthResponse, response); err != nil {
		s.sessions.Remove(session.ID)
		session.Close("failed to send auth response")
		return
	}

	s.logger.Printf("client %s connected as %s (protocol %d, encryption %s, version %s)",
		conn.RemoteAddr(), session.ID, req.Protocol, s.Cipher(), req.ClientVersion)

	defer func() {
		s.tunnels.RemoveSessionTunnels(session, "client disconnected")
		s.sessions.Remove(session.ID)
		session.Close("control connection ended")
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
				s.replyError(session, err.Error())
				continue
			}
			if err := session.RegisterProxy(tunnel); err != nil {
				s.tunnels.Unregister(spec.Name, "session closed during registration")
				s.replyError(session, err.Error())
				continue
			}
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

	// The ack has been written; from here the stream is raw bytes. Handing the
	// connection over is a non-blocking send because waiting is buffered with
	// capacity 1 and only ever used once.
	waiting <- conn
}

// totalBytes reports aggregate tunnel traffic.
func (s *Server) totalBytes() (in, out int64) {
	for _, tunnel := range s.tunnels.List() {
		in += tunnel.BytesIn.Load()
		out += tunnel.BytesOut.Load()
	}
	return in, out
}
