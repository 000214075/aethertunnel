package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// matchesSecret compares a presented secret with an expected one in constant time.
func matchesSecret(expected, presented string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// errServerDraining is returned instead of opening a stream while the server is
// winding down. It is the one stream failure that is not the client's fault.
var errServerDraining = errors.New("the server is shutting down and is not opening new streams")

// errProxyUnpublished is returned for a member that has already been removed from
// its group. A stream opened for it would be asked of a client that is no longer
// registered for the name, and counting it would recreate the metrics series the
// withdrawal dropped.
var errProxyUnpublished = errors.New("the proxy is no longer published")

// openStream asks the member's client for a fresh data connection and waits for
// it to dial back.
//
// The returned release function accounts for the stream's lifetime and must be
// called once the stream has ended.
func (t *Tunnel) openStream(visitor bool) (*dataConn, func(), error) {
	return t.openStreamFor(visitor, "")
}

// openStreamFor asks the client for a data connection, optionally naming the
// address the stream should reach. A target is only used by a socks5 proxy, whose
// client dials what the visitor asked for instead of its own local_addr.
func (t *Tunnel) openStreamFor(visitor bool, target string) (*dataConn, func(), error) {
	return t.openStreamWith(protocol.DataRequest{Proxy: t.Name, Visitor: visitor, Target: target,
		Compressed: t.Spec.UseCompression, Encrypted: t.Spec.UseEncryption && !t.Session.Encrypted})
}

// openSocksUDPStream asks the client for a data connection that carries socks5
// UDP ASSOCIATE datagrams. The target is named in each datagram rather than in
// the request, so the request only marks the stream's shape.
func (t *Tunnel) openSocksUDPStream() (*dataConn, func(), error) {
	return t.openStreamWith(protocol.DataRequest{Proxy: t.Name, SocksUDP: true})
}

// openStreamWith asks the client for the data connection described by request
// and waits for it to dial back. The request's stream identifier is assigned
// here.
func (t *Tunnel) openStreamWith(request protocol.DataRequest) (*dataConn, func(), error) {
	// A session that is draining still carries the streams it opened, but no new
	// one starts: the visitor is refused now instead of waiting for a dial that
	// would only be cut off when the grace period ends.
	if t.Session.isDraining() {
		return nil, nil, errServerDraining
	}
	// The dispatch that picked this member and this call are not atomic: the
	// member can be removed — replaced, unregistered, or dropped with its group —
	// in between. Refusing here keeps a stream from being asked of a client the
	// name no longer belongs to, and keeps streams_opened from recreating the
	// series forgetTunnel just dropped. It runs before AddPending, so there is no
	// pending request to clean up and no parked connection is consumed.
	if t.Closed() {
		return nil, nil, errProxyUnpublished
	}

	streamID := newID(8)
	request.StreamID = streamID

	pending, err := t.Session.AddPending(streamID)
	if err != nil {
		return nil, nil, err
	}

	// A connection the client parked in advance skips the dial: it is already
	// open and already acknowledged, so only the request has to cross it. Anything
	// that goes wrong here — no parked connection, one the client has since
	// dropped — falls back to asking over the control connection, which is what
	// every stream did before the pool existed.
	asked := false
	if parkedConn, parkedFramer, ok := t.Session.TakeParked(); ok {
		if err := parkedFramer.WriteJSON(protocol.TypeDataRequest, request); err == nil {
			// The parked connection carries this one stream, and the client parks
			// another when it is done with it.
			asked = true
			go t.Session.srv.awaitPooledData(t.Session, parkedConn, parkedFramer, request, t.streamWait())
		} else {
			t.metrics.poolMissed.Add(1)
			logging.Warnf(t.logger, "proxy %q: the parked connection could not be used (%v); asking over the control connection",
				t.Name, err)
			_ = parkedConn.Close()
		}
	}

	if !asked {
		if err := t.Session.Framer().WriteJSON(protocol.TypeDataRequest, request); err != nil {
			t.Session.DropPending(streamID)
			return nil, nil, fmt.Errorf("cannot ask the client for a stream: %w", err)
		}
	}

	t.Active.Add(1)
	t.Session.streamOpened()
	series := t.metrics.streamOpened(t.Name)
	// release is called both by the failure paths below and by whoever served the
	// stream, so it has to run its bookkeeping once: a double call used to be
	// invisible, and its absence left the active counters climbing for good.
	release := sync.OnceFunc(func() {
		t.Active.Add(-1)
		t.Session.streamClosed()
		t.metrics.streamClosed(series)
	})

	timer := time.NewTimer(t.streamWait())
	defer timer.Stop()

	select {
	case result := <-pending.ch:
		if result.err != nil {
			release()
			return nil, nil, result.err
		}
		if result.conn == nil {
			release()
			return nil, nil, errors.New("the client disconnected while the stream was pending")
		}
		return result.conn, release, nil
	case <-timer.C:
		t.Session.abandonPending(streamID, pending)
		release()
		closeLateStream(pending)
		return nil, nil, fmt.Errorf("the client did not provide a stream within %s", t.streamWait())
	case <-t.Session.Done():
		t.Session.abandonPending(streamID, pending)
		release()
		closeLateStream(pending)
		return nil, nil, errors.New("the session ended while the stream was pending")
	}
}

// sessionHoldsName reports whether the session already publishes a proxy with
// this name, which a registration would replace rather than add.
func sessionHoldsName(session *Session, name string) bool {
	for _, member := range session.Tunnels() {
		if member.Name == name {
			return true
		}
	}
	return false
}

// wrapDataStream presents a data connection as the net.Conn a relay reads and
// writes, applying the layers the DataRequest asked the client for: the
// per-proxy cipher when the session has none of its own, and compression
// outside it. The client applies the same two layers from the request's flags,
// so every relay that builds its own client-side view has to match or the bytes
// never decode.
func (t *Tunnel) wrapDataStream(dc *dataConn) (net.Conn, error) {
	// dc.cipher is the key derived for this stream: the configured cipher when
	// the session agreed no post-quantum key, and a per-stream key when it did.
	streamCipher := dc.cipher
	if streamCipher == nil && t.Spec.UseEncryption {
		// The client wrapped its side because the DataRequest said the proxy
		// asked for it, and this session has no cipher of its own; both ends
		// derive the same key from the shared secret and the proxy's name.
		proxyCipher, err := crypto.ProxyStreamCipher(t.Session.authSecret, t.Name)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: per-proxy encryption: %w", t.Name, err)
		}
		streamCipher = proxyCipher
	}
	var conn net.Conn = &cryptoStreamConn{Stream: crypto.NewStream(dc.conn, streamCipher), conn: dc.conn}
	if t.Spec.UseCompression {
		// The client wrapped its side because the DataRequest said so; this
		// side wraps to match, outside the encryption layer.
		conn = flynet.CompressConn(conn)
	}
	return conn, nil
}

// pipeStream copies raw bytes between a visitor-facing connection and a client
// data connection, recording the traffic. Both connections are closed when it
// returns.
func (t *Tunnel) pipeStream(public net.Conn, dc *dataConn, label string) error {
	// The pump states the contract its connection has with the session: a
	// shutdown's disconnectStreams closes what the session's set holds, so a
	// connection handed to the pump is put there. In every production path the
	// session already holds it — the relays receive stream.dc from
	// openStreamWith, which tracks the same pointer (group.go's open), and all
	// six call sites pass that field — so this is the pump's own guarantee for a
	// caller that drives it with a connection the session never saw. The set
	// dedups, so the overlap adds nothing to it.
	t.Session.trackStream(dc)
	defer t.Session.untrackStream(dc)
	defer dc.Close()

	clientSide, err := t.wrapDataStream(dc)
	if err != nil {
		return err
	}
	var visitor net.Conn = public
	if t.Spec.ProxyProtocol == "v1" {
		visitor = &proxyHeaderConn{Conn: public, header: []byte(ProxyHeaderV1(public.RemoteAddr(), public.LocalAddr()))}
	} else if t.Spec.ProxyProtocol == "v2" {
		visitor = &proxyHeaderConn{Conn: public, header: ProxyHeaderV2(public.RemoteAddr(), public.LocalAddr())}
	}
	toClient, fromClient := flynet.Pipe(visitor, clientSide, t.idleTimeout)

	t.addTraffic(toClient, fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)
	t.metrics.recordStream(t.Name, toClient, fromClient)

	t.logger.Printf("proxy %q: stream for %s finished (%d bytes out, %d bytes in)",
		t.Name, label, toClient, fromClient)
	return nil
}

// pipeDatagrams relays datagrams between a visitor connection and a client data
// connection. Both connections are closed when it returns. visitorConn is the
// connection behind visitor, handed over so the relay can hold the association
// to the idle timeout the tunnel carries.
func (t *Tunnel) pipeDatagrams(visitor *protocol.Framer, visitorConn net.Conn, dc *dataConn, label string) error {
	// Tracked like pipeStream's, and for the same reason: the session's set is
	// what the shutdown's disconnect closes, and on every production path
	// openStreamWith has already put this connection in it. The line is the
	// pump's own statement of the contract, not what makes the shutdown reach
	// it.
	t.Session.trackStream(dc)
	defer t.Session.untrackStream(dc)
	defer dc.Close()

	// A sudp proxy moves datagrams exactly as a udp proxy's pump does, and the
	// per-datagram counter is the only thing that shows the path is carrying them, so
	// it is fed here too. It counts each direction once, which is what the pump does
	// for a udp proxy.
	toClient, fromClient := relayDatagrams(visitor, dc.framer, visitorConn, dc.conn, t.idleTimeout, func() {
		t.metrics.udpDatagrams.Add(1)
	})

	t.addTraffic(toClient, fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)
	t.metrics.recordDatagramSession(t.Name, toClient, fromClient)

	t.logger.Printf("proxy %q: datagram session for %s finished (%d bytes out, %d bytes in)",
		t.Name, label, toClient, fromClient)
	return nil
}

// relayDatagrams copies TypeUDPPacket frames between two framers until one of
// them fails. The frame boundary is what preserves the datagram boundary.
//
// aConn and bConn are the connections behind the two framers, used to apply
// idle as a shared read deadline: an association that moves no datagram for
// that long is torn down, the same bound the socks5 UDP relay applies.
// Without it a visitor that stops reading while keeping the connection open —
// no FIN, which is what a NAT that forgot the mapping looks like — pins the
// client data connection and both relay goroutines until the process ends.
// onDatagram, when set, is called once for every datagram forwarded, in whichever
// direction it went.
func relayDatagrams(a, b *protocol.Framer, aConn, bConn net.Conn, idle time.Duration, onDatagram func()) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	var deadlineMu sync.Mutex
	touch := func() {
		if idle <= 0 {
			return
		}
		deadline := time.Now().Add(idle)
		deadlineMu.Lock()
		_ = aConn.SetReadDeadline(deadline)
		_ = bConn.SetReadDeadline(deadline)
		deadlineMu.Unlock()
	}
	touch()

	// The write side carries its own bound, taken per write rather than from
	// touch: a peer that keeps sending but never reading would have the
	// shared deadline refreshed forever by its own incoming traffic while
	// the frame write parked on its full socket — on the ssh gateway's
	// forwarded channels nothing else would ever end that stall, because
	// the client's own idle watchdog does not exist there. Taking the
	// deadline here is what flynet.Pipe does for the same reason.
	copyFrames := func(dst, src *protocol.Framer, dstConn, srcConn net.Conn) int64 {
		var total int64
		for {
			msg, err := src.ReadFrame()
			if err != nil {
				return total
			}
			if msg.Type != protocol.TypeUDPPacket {
				continue
			}
			if idle > 0 {
				_ = dstConn.SetWriteDeadline(time.Now().Add(idle))
			}
			if err := dst.WriteFrame(&protocol.Message{
				Type:    protocol.TypeUDPPacket,
				Payload: msg.Payload,
			}); err != nil {
				return total
			}
			total += int64(len(msg.Payload))
			touch()
			if onDatagram != nil {
				onDatagram()
			}
		}
	}

	go func() {
		defer wg.Done()
		aToB = copyFrames(b, a, bConn, aConn)
		_ = a.Close()
		_ = b.Close()
	}()
	go func() {
		defer wg.Done()
		bToA = copyFrames(a, b, aConn, bConn)
		_ = a.Close()
		_ = b.Close()
	}()

	wg.Wait()
	return aToB, bToA
}

// pipeSocksUDP relays wrapped socks5 UDP datagrams between a visitor's relay
// socket and a member's data connection. The association ends when the visitor's
// control connection goes away or either side fails.
func (t *Tunnel) pipeSocksUDP(relay net.PacketConn, dc *dataConn, expectedIP net.IP, control net.Conn, label string) error {
	// Tracked like pipeStream's, and for the same reason: the session's set is
	// what the shutdown's disconnect closes, and on every production path
	// openStreamWith has already put this connection in it. The line is the
	// pump's own statement of the contract, not what makes the shutdown reach
	// it.
	t.Session.trackStream(dc)
	defer t.Session.untrackStream(dc)
	defer dc.Close()

	toClient, fromClient := t.relaySocksUDP(relay, dc, expectedIP, control)

	t.addTraffic(toClient, fromClient)
	t.Total.Add(1)
	t.Session.RecordTraffic(toClient, fromClient)
	t.metrics.recordDatagramSession(t.Name, toClient, fromClient)

	t.logger.Printf("proxy %q: socks5 udp association for %s finished (%d bytes out, %d bytes in)",
		t.Name, label, toClient, fromClient)
	return nil
}

// relaySocksUDP copies wrapped datagrams between a packet socket and a framed
// data connection until the control connection closes or either side fails.
// Replies are sent to the source of the most recent accepted datagram, and a
// datagram from any other address is dropped when expectedIP is set.
func (t *Tunnel) relaySocksUDP(relay net.PacketConn, dc *dataConn, expectedIP net.IP, control net.Conn) (toClient, fromClient int64) {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		source net.Addr
	)

	// Any exit path tears both sides down, so the two relay directions and the
	// control watcher always unblock each other.
	teardown := sync.OnceFunc(func() {
		_ = relay.Close()
		_ = dc.Close()
	})

	// An association that moves no datagram for idleTimeout is torn down: the
	// socks5 port is unauthenticated, and without this a visitor can open any
	// number of associations and let them sit, each pinning a relay socket, a
	// client data connection and a goroutine until the process ends. Both
	// directions share the deadline, so traffic either way keeps it alive.
	var deadlineMu sync.Mutex
	touch := func() {
		if t.idleTimeout <= 0 {
			return
		}
		deadline := time.Now().Add(t.idleTimeout)
		deadlineMu.Lock()
		_ = relay.SetReadDeadline(deadline)
		_ = dc.conn.SetReadDeadline(deadline)
		deadlineMu.Unlock()
	}
	touch()

	// The association lives as long as the visitor's control connection does.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := control.Read(buf); err != nil {
				teardown()
				return
			}
		}
	}()

	wg.Add(2)

	// relay -> client: accept wrapped datagrams from the visitor and forward them.
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			touch()
			n, addr, err := relay.ReadFrom(buf)
			if err != nil {
				teardown()
				return
			}
			if expectedIP != nil {
				if ip := ipOfAddr(addr); ip == nil || !ip.Equal(expectedIP) {
					logging.Warnf(t.logger, "proxy %q: dropping a socks5 udp datagram from %s, which is not the association's source",
						t.Name, addr)
					continue
				}
			}
			mu.Lock()
			source = addr
			mu.Unlock()
			datagram := append([]byte(nil), buf[:n]...)
			// The write carries its own bound, taken here rather than from
			// touch: a client that keeps sending datagrams but never reads
			// them would have the shared deadline refreshed forever by its
			// own incoming traffic while the frame write parked on its full
			// socket, pinning the relay socket, the data connection and the
			// goroutines until its control connection ends — one association
			// per attempt on an unauthenticated port. The relay socket
			// itself needs no write bound: a UDP WriteTo does not wait on
			// the peer.
			if t.idleTimeout > 0 {
				_ = dc.conn.SetWriteDeadline(time.Now().Add(t.idleTimeout))
			}
			if err := dc.framer.WriteFrame(&protocol.Message{Type: protocol.TypeUDPPacket, Payload: datagram}); err != nil {
				teardown()
				return
			}
			toClient += int64(n)
			t.metrics.socksUDPDatagrams.Add(1)
		}
	}()

	// client -> relay: read replies from the client and send them back to the visitor.
	go func() {
		defer wg.Done()
		for {
			touch()
			msg, err := dc.framer.ReadFrame()
			if err != nil {
				teardown()
				return
			}
			if msg.Type != protocol.TypeUDPPacket {
				continue
			}
			mu.Lock()
			addr := source
			mu.Unlock()
			if addr == nil {
				continue
			}
			if _, err := relay.WriteTo(msg.Payload, addr); err != nil {
				teardown()
				return
			}
			fromClient += int64(len(msg.Payload))
			t.metrics.socksUDPDatagrams.Add(1)
		}
	}()

	wg.Wait()
	return toClient, fromClient
}

// cryptoStreamConn adapts crypto.Stream (an io.ReadWriteCloser) to net.Conn so the
// pipe helper can use it interchangeably with a raw socket.
type cryptoStreamConn struct {
	*crypto.Stream
	conn net.Conn
}

func (c *cryptoStreamConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *cryptoStreamConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *cryptoStreamConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *cryptoStreamConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *cryptoStreamConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// CloseWrite half-closes the underlying connection and leaves the read side open, which is
// what a half-closed stream needs.
//
// flynet.Pipe finishes each direction with CloseWrite when the type has one and closes the
// whole connection otherwise, so a wrapper without this method turns every half-close into
// a full close: the direction that was still carrying bytes — the reply that a service only
// sends after it has seen the end of the request, as HTTP/1.0 and several database
// protocols do — is cut off before it arrives. The record layer holds no state that needs
// flushing (see crypto.Stream.Close).
func (c *cryptoStreamConn) CloseWrite() error {
	if hc, ok := c.conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.conn.Close()
}

// --- the registry -------------------------------------------------------------

// TunnelManager owns every published proxy on the server.
type TunnelManager struct {
	mu     sync.RWMutex
	groups map[string]*ProxyGroup

	cfg      *config.Config
	logger   *log.Logger
	cipher   *crypto.Cipher
	sessions *SessionManager
	metrics  *Metrics
	auditor  *Auditor

	// allowPorts is [server].allow_ports in parsed form. The caller checks it
	// before registering, and Register checks it again because the newProxy
	// plugin runs in between and may rewrite remote_port: a plugin that names a
	// port the operator excluded must not be able to publish it.
	allowPorts *config.PortSet

	// policies holds the server's own rule per published name, from the
	// [[proxies]] list of the server configuration.
	policies *proxyPolicies

	vhost       *vhostSet
	tcpmux      *tcpmuxSet
	sni         *sniSet
	p2p         *p2pRendezvous
	directory   *directory
	httpPlugins *httpPluginManager
}

func newTunnelManager(cfg *config.Config, logger *log.Logger, cipher *crypto.Cipher, sessions *SessionManager, metrics *Metrics, auditor *Auditor, allowPorts *config.PortSet) *TunnelManager {
	return &TunnelManager{
		groups:     make(map[string]*ProxyGroup),
		cfg:        cfg,
		logger:     logger,
		cipher:     cipher,
		sessions:   sessions,
		metrics:    metrics,
		auditor:    auditor,
		allowPorts: allowPorts,
		policies:   newProxyPolicies(cfg.Proxies),
	}
}

var (
	errTunnelExists = errors.New("a tunnel with that name is already registered")
	errNoSuchTunnel = errors.New("no such tunnel")
)

// Register publishes a tunnel for a session. When the spec asks for a remote port
// the port is bound here, so a failure (port in use, out of range) is reported to
// the client immediately instead of failing on the first visit.
func (m *TunnelManager) Register(session *Session, spec protocol.ProxySpec) (*Tunnel, error) {
	if spec.Name == "" {
		return nil, errors.New("proxy name is required")
	}
	// The webhooks see the registration first: a reject is the refusal the
	// client is told, and a rewrite becomes the spec this call proceeds with.
	if err := m.httpPlugins.runNewProxy(session, &spec); err != nil {
		return nil, err
	}
	// A plugin's rewrite is the spec this call proceeds with, so it is held to
	// the same rules the client's own spec passed above: a name rewritten to
	// empty would wedge an unnameable group into the registry, and domains the
	// name-routing tables cannot match would publish keys no Host header can
	// reach. Type, remote_port and the policy checks below run again on their
	// own; these two have no later checkpoint.
	if spec.Name == "" {
		return nil, errors.New("proxy name is required")
	}
	if bad := firstBadDomain(spec); bad != "" {
		return nil, fmt.Errorf("proxy %q: domain %q is not a usable hostname or \"*.suffix\" wildcard", spec.Name, bad)
	}
	if spec.Type == "" {
		spec.Type = protocol.ProxyTypeTCP
	}
	if !config.IsProxyType(spec.Type) {
		return nil, fmt.Errorf("proxy type %q is not supported (use one of %s)",
			spec.Type, joinTypes(config.ProxyTypes))
	}
	if spec.RemotePort < 0 || spec.RemotePort > 65535 {
		return nil, fmt.Errorf("remote_port %d is out of range", spec.RemotePort)
	}
	if spec.RemotePort != 0 && !m.allowPorts.Contains(spec.RemotePort) {
		// The plugin above may have rewritten remote_port, and the rule the
		// operator wrote it into apply only before this call. The same list has
		// to judge the value the registration proceeds with, or a rewrite
		// publishes a port allow_ports exists to keep out.
		return nil, fmt.Errorf("remote port %d is outside allow_ports", spec.RemotePort)
	}
	if spec.TLSPassthrough && spec.Type != protocol.ProxyTypeHTTPS {
		// tls_passthrough relays a TLS session chosen by its ClientHello, and the
		// passthrough listener serves that shape only: an http or tcp registration
		// that sets the flag would be published somewhere the flag cannot act on,
		// and the address the DHT announces for it would name the wrong port. The
		// client's own validator refuses the combination, and a hand-written
		// registration is refused here.
		return nil, fmt.Errorf("proxy %q: tls_passthrough routes an https tunnel, not a %s tunnel", spec.Name, spec.Type)
	}
	if spec.Multipath < 0 || spec.Multipath > config.MaxMultipath {
		return nil, fmt.Errorf("multipath %d is out of range (0-%d)", spec.Multipath, config.MaxMultipath)
	}
	if spec.Type == protocol.ProxyTypeSOCKS && len(spec.AllowTargets) == 0 {
		// The list is what bounds the endpoint, so a registration that leaves it
		// out is refused rather than published as an exit for everything.
		return nil, errors.New("a socks5 tunnel needs allow_targets: it names the ranges the client may dial")
	}
	if spec.Type == protocol.ProxyTypeTCPMux {
		if spec.Multiplexer != config.MultiplexerHTTPConnect {
			return nil, fmt.Errorf("proxy %q: multiplexer %q is not supported (the only multiplexer is %q)",
				spec.Name, spec.Multiplexer, config.MultiplexerHTTPConnect)
		}
		if spec.RemotePort != 0 {
			return nil, fmt.Errorf("proxy %q: tcpmux is routed by hostname and must not open a public port", spec.Name)
		}
		if len(spec.Domains) == 0 {
			return nil, fmt.Errorf("proxy %q: a tcpmux tunnel needs domains (or subdomain on a server with subdomain_host)", spec.Name)
		}
	}
	for _, location := range spec.Locations {
		// A relative prefix never matches: selectByRoute compares it against the
		// request path, which always begins with a slash, so the proxy would be
		// published and then be unreachable.
		if !strings.HasPrefix(location, "/") {
			return nil, fmt.Errorf("proxy %q: location %q must start with a slash: locations are path prefixes",
				spec.Name, location)
		}
	}
	for _, cidr := range append(append([]string{}, spec.AllowCIDRs...), spec.DenyCIDRs...) {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return nil, fmt.Errorf("visitor CIDR %q is not valid: %w", cidr, err)
		}
	}
	if m.cfg.Server.MaxPortsPerClient > 0 && spec.RemotePort > 0 {
		// The cap counts what this session already holds, so a client that
		// registers and closes stays well under it; one that keeps every
		// port it opens hits the refusal by name. A re-registration of a name
		// this session already holds replaces that member rather than adding a
		// port, so it is not counted twice: otherwise a session at the cap could
		// not update one of its own proxies. The name being registered is left
		// out of the count rather than subtracted afterwards, because the member
		// it replaces may hold no port at all — subtracting then removed a port
		// that was never counted and let a session at the cap open one more.
		used := m.portsForSessionExcept(session, spec.Name)
		if used+1 > m.cfg.Server.MaxPortsPerClient {
			return nil, fmt.Errorf("proxy %q: registering another public port would exceed the server's max_ports_per_client (%d)",
				spec.Name, m.cfg.Server.MaxPortsPerClient)
		}
	}
	// The server's own rule for this name comes before the type-specific checks: a
	// registration the operator's policy refuses should not be told about anything
	// else, and the refusal names what the server expects.
	if err := m.policies.admits(spec.Name, spec.Type, spec.RemotePort); err != nil {
		return nil, err
	}
	if config.IsPrivateProxyType(spec.Type) {
		if spec.SecretKey == "" {
			return nil, fmt.Errorf("proxy type %q is private and needs a secret_key", spec.Type)
		}
		if spec.RemotePort != 0 {
			return nil, fmt.Errorf("proxy type %q is private and must not open a public port", spec.Type)
		}
		if spec.AuthMethod == "" {
			spec.AuthMethod = config.AuthMethodSecret
		}
		// The method decides how a visitor's answer is checked, so a value this
		// server does not know is refused by name. It used to fall through to
		// comparing the secret itself — the weakest of the three — which would
		// answer a client built against a newer method by quietly weakening the
		// proxy it just registered.
		switch spec.AuthMethod {
		case config.AuthMethodSecret, config.AuthMethodNIZK, config.AuthMethodSNARK:
		default:
			return nil, fmt.Errorf("proxy type %q: auth_method %q is not supported", spec.Type, spec.AuthMethod)
		}
	}

	m.mu.Lock()
	group, exists := m.groups[spec.Name]
	if exists && group.isClosed() {
		// The endpoint was closed for good — an empty-unregister or a failed
		// first bind — and the entry is only still here because whichever
		// close ran is still finishing its bookkeeping. Drop it so this
		// registration builds a fresh group instead of being refused by a
		// bind that can never run again.
		m.dropGroup(spec.Name)
		exists = false
	}
	if !exists {
		group = newProxyGroup(spec, m)
		// A visitor's NIZK proof is checked against the public key of the secret, and
		// the key has to be in place before the name is in the map: a visitor reaches
		// the group the moment it is published, so a key derived later by bind was a
		// data race against verifyVisitorProof and a window in which a correct proof
		// was refused.
		if group.AuthMethod == config.AuthMethodNIZK {
			public, err := crypto.SchnorrPublicKey([]byte(group.SecretKey))
			if err != nil {
				m.mu.Unlock()
				return nil, fmt.Errorf("proxy %q: cannot derive the proof key: %w", spec.Name, err)
			}
			group.SecretPublicKey = public
		}
		m.groups[spec.Name] = group
		// The name is live from here on, so its series is created here too. It
		// used to be created lazily by the first stream, and the http path
		// counts a request before it opens the stream the request rides — so
		// the first request through a fresh name landed on a series that did
		// not exist yet and went uncounted (the smoke suite's per-tunnel http
		// counter caught exactly that on the first request after a restart).
		m.metrics.tunnel(spec.Name)
	} else if group.Type != spec.Type {
		m.mu.Unlock()
		return nil, fmt.Errorf("proxy %q is already published as type %q", spec.Name, group.Type)
	}
	m.mu.Unlock()

	member, err := group.add(session, spec)
	if err != nil {
		// The join travels with its member so the counter its registration
		// created can be judged by the same gate the re-check below applies:
		// one that was never reachable is forgotten, and one whose endpoint
		// came up keeps it — a stream may be holding it. forgetUsage is a map
		// delete, so this is harmless where add's bind-failure path already
		// forgot the name on its way out.
		if member != nil && member.createdUsage && !member.reachable.Load() {
			session.forgetUsage(spec.Name)
		}
		if group.memberCount() == 0 {
			m.mu.Lock()
			if current, ok := m.groups[spec.Name]; ok && current == group {
				m.dropGroup(spec.Name)
			}
			m.mu.Unlock()
		}
		return nil, err
	}
	// Re-check under the manager lock: an Unregister that ran between the map
	// lookup above and this member joining may have seen an empty group,
	// deleted the entry and closed the group's endpoint for good — its
	// sync.Once cannot ever rebind. Without this check the just-added member
	// would be orphaned: unreachable by visitors, unremovable on disconnect,
	// and blocking the name from being published again. A closed group is the
	// same outcome from the other side (a first bind that failed while this
	// session was joining).
	m.mu.Lock()
	current, ok := m.groups[spec.Name]
	if ok && current == group && group.isClosed() {
		m.dropGroup(spec.Name)
		ok = false
	}
	if !ok || current != group {
		m.mu.Unlock()
		group.remove(member, "the proxy was unregistered while it was being published")
		if member.createdUsage && !member.reachable.Load() {
			// This registration created the counter and its endpoint never came
			// up, so no visitor can have been served and leaving it behind would
			// have the ledger bill an empty entry for a name nothing served. A
			// counter an earlier registration left is kept, and so is one whose
			// endpoint did come up: a stream may be holding it and writing to it,
			// and the ledger bills the name, not the registration.
			session.forgetUsage(spec.Name)
		}
		return nil, fmt.Errorf("proxy %q was unregistered while it was being published; publish it again", spec.Name)
	}
	m.mu.Unlock()

	if members := group.memberCount(); members > 1 {
		m.logger.Printf("proxy %q: session %s joined the pool (%d members, strategy %s)",
			spec.Name, session.ID, members, group.strategy)
	} else {
		// The record names the address a visitor dials, so it is written when the
		// proxy first appears rather than once per pool member. A DHT failure is
		// logged by the directory and is not fatal: the proxy still works for a
		// visitor that knows the address.
		_ = m.directory.publish(recordForPublish(group, spec))
	}
	return member, nil
}

// recordForPublish names what the directory announces for a single-member
// group. The endpoint a visitor dials is the group's — the founding member
// bound it and the group keeps it for its whole life — while the registration
// that happens to be the last one standing may name another port: a joiner's
// own remote_port was never adopted, and its replacement is only free to name
// its own port while the founding member is still in the list, because that is
// the single-member case the port rule covers. A replacement accepted on that
// rule whose founder then leaves — before Register's publish branch runs —
// would have the record point at the joiner's port, a port nothing listens on,
// until the group's lifecycle withdraws the name. The group's own port is the
// truthful answer in every case: the founder's registration carries it, and a
// hostname-routed group's port is inert (its visitors dial a shared listener).
func recordForPublish(g *ProxyGroup, spec protocol.ProxySpec) protocol.ProxySpec {
	record := spec
	record.RemotePort = g.RemotePort
	return record
}

func joinTypes(types []string) string {
	out := ""
	for i, t := range types {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(t)
	}
	return out
}

// portsForSessionExcept counts the public ports a session holds, ignoring the
// group named by except: a re-registration replaces that group's member, so the
// port it may hold must not count against the new one. An empty except counts
// every group.
func (m *TunnelManager) portsForSessionExcept(session *Session, except string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ports := 0
	for name, group := range m.groups {
		if name == except {
			continue
		}
		// A hostname-routed group binds a virtual host and never opens a port of
		// its own, so the remote_port a client wrote beside it buys nothing: the
		// budget counts public ports, and charging one for a port that is never
		// bound spent a session's allowance on nothing.
		if routesByHostname(group.Type) {
			continue
		}
		// One port per group, and only for the session whose member holds it: bind
		// took the group's endpoint from its first member, and a session that merely
		// joins the pool opens no port of its own — its remote_port is not honoured,
		// so charging it refused a client a port it never held. The oldest surviving
		// member is the one the group's port belongs to once the first one leaves.
		members := group.Members()
		for index, member := range members {
			if member.Session != session {
				continue
			}
			if group.RemotePort > 0 && index == 0 {
				ports++
			}
			break
		}
	}
	return ports
}

// Unregister removes the given session's registration of a proxy. Other members
// of the same pool keep the endpoint published.
func (m *TunnelManager) Unregister(name string, session *Session, reason string) {
	m.mu.RLock()
	group, ok := m.groups[name]
	m.mu.RUnlock()
	if !ok {
		return
	}

	for _, member := range group.Members() {
		if session != nil && member.Session != session {
			continue
		}
		// The name stays published while another member holds it, so the audit
		// trail follows every member while the map entry and the DHT record follow
		// the group. The record itself is written by the removal.
		if removed, empty := group.remove(member, reason); removed {
			m.mu.Lock()
			// Re-check emptiness under the manager lock: a Register that
			// joined this group between the removal and this lock would
			// otherwise be orphaned by the delete — its listener unreachable,
			// its member never removed, the group's close already spent.
			if current, still := m.groups[name]; still && current == group && group.memberCount() == 0 {
				m.dropGroup(name)
			} else {
				empty = false
			}
			m.mu.Unlock()
			if empty {
				// The last member is gone, so the name is no longer served
				// here. Copies already replicated to DHT peers lapse within
				// one TTL.
				m.directory.withdraw(name)
			}
		}
		if session != nil {
			return
		}
	}

	// A session closed from somewhere other than its control loop — the
	// dashboard's DELETE /api/clients/{id} — has already detached its members by
	// the time the control loop tears the session down. The loop above then finds
	// nothing, and this call used to leave the empty group in the map: a phantom
	// proxy in /api/proxies and a DHT record that stayed published until it
	// lapsed. The check is under the manager lock for the same reason the one
	// inside the loop is: a Register that joined between the two must not be
	// orphaned.
	m.mu.Lock()
	current, still := m.groups[name]
	if !still || current != group || group.memberCount() != 0 {
		m.mu.Unlock()
		return
	}
	m.dropGroup(name)
	m.mu.Unlock()
	m.directory.withdraw(name)
}

// dropGroup drops the manager's entry for name and the metric series that goes
// with it, so a name that is gone does not keep a family in every scrape. The
// caller holds m.mu and has checked that the entry is still the group it means
// to drop.
//
// The series is dropped here, at the map entry, rather than in ProxyGroup.close:
// close runs while a same-name registration can already be building the group
// that takes the name over, and a same-name group's first stream creates its
// series lazily — forgetting inside close deleted the series of the group that
// had just taken the name, leaving it missing until its next stream.
func (m *TunnelManager) dropGroup(name string) {
	delete(m.groups, name)
	m.metrics.forgetTunnel(name)
}

// Get looks a proxy up by name.
func (m *TunnelManager) Get(name string) (*ProxyGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	group, ok := m.groups[name]
	if !ok {
		return nil, errNoSuchTunnel
	}
	return group, nil
}

// List returns every member of every group, ordered by proxy name and then by
// registration order.
func (m *TunnelManager) List() []*Tunnel {
	m.mu.RLock()
	groups := make([]*ProxyGroup, 0, len(m.groups))
	for _, group := range m.groups {
		groups = append(groups, group)
	}
	m.mu.RUnlock()

	out := make([]*Tunnel, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.Members()...)
	}
	return out
}

// Groups returns a snapshot of the published proxies, ordered by name.
func (m *TunnelManager) Groups() []*ProxyGroup {
	m.mu.RLock()
	out := make([]*ProxyGroup, 0, len(m.groups))
	for _, group := range m.groups {
		out = append(out, group)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RemoveSessionTunnels stops everything a disconnecting session published. Other
// members of a pool keep the endpoint alive.
func (m *TunnelManager) RemoveSessionTunnels(s *Session, reason string) {
	for _, name := range s.Proxies() {
		m.Unregister(name, s, reason)
	}
}

// streamWait is how long a published connection waits for the client to hand
// back the data connection it was asked for: server.user_conn_timeout_seconds
// when the operator set one, the dial timeout otherwise — which is the wait
// this server had before that key existed.
func (t *Tunnel) streamWait() time.Duration {
	if t.userConnTimeout > 0 {
		return t.userConnTimeout
	}
	return t.dialTimeout
}
