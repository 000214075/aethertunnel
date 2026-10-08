package server

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// newSession builds the internal server Session that owns the tunnels one SSH
// connection publishes. It is a Session like a real client's, registered in the
// same SessionManager and torn down by the same code, so the dashboard, the
// per-session counters, the bandwidth ledger and the load-balancing health check
// see SSH tunnels the way they see a client's.
//
// The control connection is an in-memory pipe: the tunnel machinery asks for a
// stream by writing a TypeDataRequest through the session's framer, and the
// responder reads it back and answers with a forwarded-tcpip channel to the SSH
// client.
func (g *sshTunnelGateway) newSession(sshConn *ssh.ServerConn, owner *sshGatewayConn) (*Session, error) {
	serverSide, clientSide := net.Pipe()
	conn := &sshSessionConn{Conn: clientSide, remote: sshConn.RemoteAddr()}
	framer := protocol.NewFramer(conn, nil, 0)

	req := &protocol.AuthRequest{ClientVersion: "ssh-gateway", Protocol: protocol.ProtocolVersion}
	// Zero, not the server's heartbeat interval: this session has no control
	// loop, so nothing would ever refresh its heartbeat, and reporting an
	// interval no one keeps made the dashboard show a healthy SSH tunnel as a
	// client that stopped heartbeating. The dashboard leaves the two heartbeat
	// fields out for a session with a zero interval.
	session := newSession(g.srv, conn, framer, req, false, 0)
	session.Metas = map[string]string{"ssh_user": sshConn.User()}

	if err := g.srv.sessions.Add(session); err != nil {
		_ = clientSide.Close()
		_ = serverSide.Close()
		return nil, err
	}

	responder := &sshDataResponder{
		session:  session,
		owner:    owner,
		sshConn:  sshConn,
		requests: protocol.NewFramer(serverSide, nil, 0),
	}
	go responder.loop()
	return session, nil
}

// sshSessionConn is the Session's view of the in-memory control connection: the
// pipe carries the frames, but the remote address reported to the dashboard and
// the audit log is the SSH client's.
type sshSessionConn struct {
	net.Conn
	remote net.Addr
}

func (c *sshSessionConn) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return c.Conn.RemoteAddr()
}

// sshDataResponder turns the server's request for a stream into a forwarded-tcpip
// channel back to the SSH client.
type sshDataResponder struct {
	session  *Session
	owner    *sshGatewayConn
	sshConn  *ssh.ServerConn
	requests *protocol.Framer
}

func (r *sshDataResponder) loop() {
	for {
		msg, err := r.requests.ReadFrame()
		if err != nil {
			// The session closed its own end of the pipe — a client that went
			// away, or the dashboard's "disconnect". Closing the SSH connection
			// is what ends the channel loop in handleConn, and its deferred
			// closeSession is the only place the session's tunnels are withdrawn
			// and its ledger settled. Without this the SSH client stayed
			// connected with its proxies still published, which is the opposite
			// of what the dashboard had just been asked to stop.
			_ = r.owner.conn.Close()
			return
		}
		if msg.Type != protocol.TypeDataRequest {
			continue
		}
		var request protocol.DataRequest
		if json.Unmarshal(msg.Payload, &request) != nil {
			continue
		}
		go r.serve(request)
	}
}

// serve answers one DataRequest. The response goes into the pending channel
// openStreamWith is waiting on, so a failure to open the SSH channel is reported
// to the visitor as a stream failure instead of making it wait for a timeout.
func (r *sshDataResponder) serve(request protocol.DataRequest) {
	pending, ok := r.session.TakePending(request.StreamID)
	if !ok {
		return
	}
	addr, port, err := r.owner.forward()
	if err != nil {
		r.session.resolveStream(pending, streamResult{err: err})
		return
	}
	// openForwardedChannel waits for the client's answer with no deadline of
	// its own: x/crypto's mux blocks until the peer confirms, refuses, or the
	// connection ends, and a peer that answers nothing pins this goroutine —
	// one per abandoned visitor stream, until the whole connection closes.
	// The gate bounds how many of those waits one connection may hold at the
	// same bound the session channels use; beyond it the stream fails fast
	// with the same outcome a refusal would produce, so a stalled or hostile
	// client cannot grow the process one goroutine per visitor request.
	if !r.owner.pendingForwards.reserve(maxSSHSessionChannels) {
		r.session.resolveStream(pending, streamResult{err: fmt.Errorf("too many forwarded connections are waiting for the ssh client to answer")})
		return
	}
	defer r.owner.pendingForwards.release()
	dc, err := r.openForwardedChannel(addr, port)
	if err != nil {
		r.session.resolveStream(pending, streamResult{err: fmt.Errorf("the ssh client did not accept the forwarded connection: %w", err)})
		return
	}
	// Opening the channel is a round trip, so the visitor can give up while it
	// runs; without this check the connection would sit in the buffered channel
	// with nobody to read it until the session ended.
	if !r.session.resolveStream(pending, streamResult{conn: dc}) {
		_ = dc.Close()
	}
}

// openForwardedChannel opens the RFC 4254 forwarded-tcpip channel that carries
// one stream. The connected address and port are the ones the client asked for
// in tcpip-forward, which is what its -R rule is keyed on.
func (r *sshDataResponder) openForwardedChannel(addr string, port uint32) (*dataConn, error) {
	payload := ssh.Marshal(&sshForwardedTCPIP{
		Addr:       addr,
		Port:       port,
		OriginAddr: sshGatewayVisitorAddress,
		OriginPort: 1,
	})
	channel, requests, err := r.sshConn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(requests)

	conn := &sshChannelConn{
		Channel: channel,
		local:   r.owner.conn.LocalAddr(),
		remote:  r.owner.conn.RemoteAddr(),
		onIO:    r.owner.touch,
	}
	// The ssh channel is already encrypted, so the tunnel's own cipher stays
	// off: a nil cipher makes crypto.NewStream a passthrough. A datagram proxy
	// reads dc.framer instead of dc.conn, and that framer writes its packet
	// frames on the same channel.
	return &dataConn{
		conn:   conn,
		framer: protocol.NewFramer(conn, nil, 0),
		cipher: nil,
	}, nil
}

// sshForwardedTCPIP is the payload of a forwarded-tcpip channel open, in the
// field order RFC 4254 section 7.2 defines. ssh.Marshal serializes the fields in
// order, so an unexported type in the ssh package is not needed.
type sshForwardedTCPIP struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// sshChannelConn adapts an SSH channel to net.Conn. Deadlines are enforced by
// closing the channel when one fires: x/crypto's mux has no per-channel
// deadline, and without this a forwarded channel whose peer stops reading or
// sending parks the relay goroutines on it until the whole connection ends —
// the relays apply the tunnel's idle timeout as a deadline on this conn, and a
// deadline that did nothing would leave every stalled stream pinned. Closing
// ends only this channel; the connection's other channels keep running.
type sshChannelConn struct {
	ssh.Channel
	local, remote net.Addr
	onIO          func()

	mu         sync.Mutex
	readTimer  *time.Timer
	writeTimer *time.Timer
}

func (c *sshChannelConn) LocalAddr() net.Addr  { return c.local }
func (c *sshChannelConn) RemoteAddr() net.Addr { return c.remote }

// armTimer arms (or, for the zero time, disarms) the timer that closes the
// channel when the deadline passes.
func (c *sshChannelConn) armTimer(slot **time.Timer, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *slot != nil {
		(*slot).Stop()
		*slot = nil
	}
	if t.IsZero() {
		return
	}
	ch := c.Channel
	*slot = time.AfterFunc(time.Until(t), func() { _ = ch.Close() })
}

func (c *sshChannelConn) SetDeadline(t time.Time) error {
	c.SetWriteDeadline(t)
	return c.SetReadDeadline(t)
}

func (c *sshChannelConn) SetReadDeadline(t time.Time) error {
	c.armTimer(&c.readTimer, t)
	return nil
}

func (c *sshChannelConn) SetWriteDeadline(t time.Time) error {
	c.armTimer(&c.writeTimer, t)
	return nil
}

func (c *sshChannelConn) Close() error {
	c.mu.Lock()
	readTimer, writeTimer := c.readTimer, c.writeTimer
	c.readTimer, c.writeTimer = nil, nil
	c.mu.Unlock()
	if readTimer != nil {
		readTimer.Stop()
	}
	if writeTimer != nil {
		writeTimer.Stop()
	}
	return c.Channel.Close()
}

func (c *sshChannelConn) Read(p []byte) (int, error) {
	n, err := c.Channel.Read(p)
	if n > 0 && c.onIO != nil {
		c.onIO()
	}
	return n, err
}

func (c *sshChannelConn) Write(p []byte) (int, error) {
	n, err := c.Channel.Write(p)
	if n > 0 && c.onIO != nil {
		c.onIO()
	}
	return n, err
}
