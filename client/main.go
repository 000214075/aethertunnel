// Command aethertunnel-client connects to an AetherTunnel server, publishes the
// tunnels listed in its configuration and forwards each visiting connection to the
// matching local service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Stamped at build time; see the server's main.go for the ldflags form.
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

type client struct {
	cfg    *config.Config
	cipher *crypto.Cipher
	logger *log.Logger

	mu              sync.Mutex
	session         string
	activeStreams   sync.WaitGroup
	registeredNames []string
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		configPath  = flag.String("config", "", "path to the client configuration file (default client.toml)")
		checkConfig = flag.Bool("check", false, "validate the configuration and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [config-file]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("aethertunnel-client %s (protocol %d, built %s, commit %s)\n",
			version, protocol.ProtocolVersion, buildTime, gitCommit)
		return
	}

	path := *configPath
	if path == "" {
		if flag.NArg() > 0 {
			path = flag.Arg(0)
		} else {
			path = "client.toml"
		}
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient})
	if err != nil {
		logger.Fatalf("%v", err)
	}
	for _, warning := range cfg.Warnings {
		logger.Printf("warning: %s", warning)
	}
	if *checkConfig {
		fmt.Printf("%s is valid\n", path)
		return
	}

	cipher, err := cfg.Cipher(config.RoleClient)
	if err != nil {
		logger.Fatalf("encryption configuration: %v", err)
	}

	c := &client{cfg: cfg, cipher: cipher, logger: logger}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("AetherTunnel client %s (protocol %d) -> %s, encryption %s, %d tunnel(s) configured",
		version, protocol.ProtocolVersion, cfg.Client.ServerAddr, cipher.Algorithm(), len(cfg.Proxies))

	c.run(ctx)
	c.logger.Printf("client stopped")
}

// run keeps a session alive, reconnecting with exponential backoff.
func (c *client) run(ctx context.Context) {
	backoff := time.Duration(c.cfg.Client.ReconnectSeconds) * time.Second
	maxBackoff := time.Duration(c.cfg.Client.MaxReconnectSeconds) * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		startedAt := time.Now()
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}

		// A session that lasted a while is a success as far as backoff is
		// concerned: the next failure is a new incident, not a retry storm.
		if time.Since(startedAt) > 60*time.Second {
			backoff = time.Duration(c.cfg.Client.ReconnectSeconds) * time.Second
		}

		wait := jitter(backoff)
		if err != nil {
			c.logger.Printf("session ended: %v; reconnecting in %s", err, wait.Round(time.Second))
		} else {
			c.logger.Printf("session ended; reconnecting in %s", wait.Round(time.Second))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// jitter spreads reconnects out by ±20% so a fleet of clients does not retry in
// lockstep after a server restart.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

// session runs one control connection until it fails or ctx is cancelled.
func (c *client) runSession(ctx context.Context) error {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second
	conn, err := net.DialTimeout("tcp", c.cfg.Client.ServerAddr, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.Client.ServerAddr, err)
	}
	defer conn.Close()

	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	request := protocol.AuthRequest{
		Token:         c.cfg.Client.AuthToken,
		ClientVersion: version,
		Protocol:      protocol.ProtocolVersion,
		Encryption:    c.cipher.Algorithm(),
	}
	if err := framer.WriteJSON(protocol.TypeAuthRequest, request); err != nil {
		return fmt.Errorf("send auth request: %w", err)
	}

	var response protocol.AuthResponse
	if err := framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if !response.OK {
		return fmt.Errorf("authentication rejected: %s", response.Error)
	}
	if response.ProtocolMismatch {
		c.logger.Printf("warning: server speaks protocol %d, this client speaks %d", response.Protocol, protocol.ProtocolVersion)
	}
	if response.Encryption != c.cipher.Algorithm() {
		return fmt.Errorf("encryption mismatch: server uses %q, this client uses %q", response.Encryption, c.cipher.Algorithm())
	}
	_ = conn.SetDeadline(time.Time{})

	c.mu.Lock()
	c.session = response.Session
	c.mu.Unlock()

	c.logger.Printf("connected to %s as session %s (server %s)", c.cfg.Client.ServerAddr, response.Session, response.ServerVersion)

	if err := c.registerProxies(framer); err != nil {
		return err
	}

	heartbeatDone := make(chan struct{})
	go c.heartbeatLoop(heartbeatDone, framer, response.HeartbeatSecs)

	defer close(heartbeatDone)

	// One reader: this loop owns the control framer for the life of the session.
	for {
		if ctx.Err() != nil {
			return nil
		}
		msg, err := framer.ReadFrame()
		if err != nil {
			return err
		}

		switch msg.Type {
		case protocol.TypeHeartbeatAck:
			// nothing to do; the ack proves the link is alive
		case protocol.TypeDataRequest:
			var request protocol.DataRequest
			if err := json.Unmarshal(msg.Payload, &request); err != nil {
				c.logger.Printf("malformed data request: %v", err)
				continue
			}
			go c.serveStream(response.Session, request)
		case protocol.TypeProxyList:
			c.logProxyList(msg.Payload)
		case protocol.TypeError:
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(msg.Payload, &payload)
			c.logger.Printf("server reported: %s", payload.Error)
		default:
			c.logger.Printf("ignoring unexpected control frame %s", msg.Type)
		}
	}
}

// registerProxies publishes every configured tunnel on the current session.
func (c *client) registerProxies(framer *protocol.Framer) error {
	if len(c.cfg.Proxies) == 0 {
		c.logger.Printf("no [[proxies]] configured: the connection is up but publishes nothing")
		return nil
	}
	for _, proxy := range c.cfg.Proxies {
		spec := protocol.ProxySpec{
			Name:       proxy.Name,
			Type:       proxy.Type,
			LocalAddr:  proxy.LocalAddr(),
			RemotePort: proxy.RemotePort,
		}
		if err := framer.WriteJSON(protocol.TypeRegisterProxy, spec); err != nil {
			return fmt.Errorf("register proxy %q: %w", proxy.Name, err)
		}
		c.logger.Printf("requested tunnel %q -> %s (public port %d)", proxy.Name, spec.LocalAddr, proxy.RemotePort)
	}
	return nil
}

func (c *client) logProxyList(payload []byte) {
	var statuses []protocol.ProxyStatus
	if err := json.Unmarshal(payload, &statuses); err != nil {
		c.logger.Printf("malformed proxy list: %v", err)
		return
	}
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, status.Name)
	}
	c.mu.Lock()
	c.registeredNames = names
	c.mu.Unlock()
	c.logger.Printf("server confirms %d tunnel(s): %v", len(names), names)
}

// framerOptions maps the client's [obfuscation] section onto frame padding and
// jitter. The server unpads from the frame flag alone, so the two ends do not have
// to agree on these values.
func (c *client) framerOptions() protocol.FramerOptions {
	return protocol.FramerOptions{
		MaxPayload: protocol.DefaultMaxPayload,
		PadTo:      c.cfg.Obfuscation.PadTo,
		Jitter:     time.Duration(c.cfg.Obfuscation.JitterMillis) * time.Millisecond,
	}
}

// heartbeatLoop sends heartbeats until done is closed. It only writes, so it does
// not race with the session's reader.
func (c *client) heartbeatLoop(done <-chan struct{}, framer *protocol.Framer, seconds int) {
	if seconds <= 0 {
		seconds = 30
	}
	ticker := time.NewTicker(time.Duration(seconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := framer.WriteFrame(&protocol.Message{Type: protocol.TypeHeartbeat}); err != nil {
				return
			}
		}
	}
}

// serveStream opens a second connection to the server for one visiting connection
// and forwards it to the local service.
func (c *client) serveStream(session string, request protocol.DataRequest) {
	dialTimeout := time.Duration(c.cfg.Client.DialTimeoutSecs) * time.Second

	proxy, err := c.findProxy(request.Proxy)
	if err != nil {
		c.logger.Printf("cannot serve stream for %q: %v", request.Proxy, err)
		return
	}

	conn, err := net.DialTimeout("tcp", c.cfg.Client.ServerAddr, dialTimeout)
	if err != nil {
		c.logger.Printf("stream for %q: cannot reach the server: %v", request.Proxy, err)
		return
	}

	framer := protocol.NewFramerWithOptions(conn, c.cipher, c.framerOptions())
	fail := func(reason string) {
		c.logger.Printf("stream for %q: %s", request.Proxy, reason)
		_ = conn.Close()
	}

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := framer.WriteJSON(protocol.TypeDataOpen, protocol.DataOpen{
		Session:  session,
		Proxy:    request.Proxy,
		StreamID: request.StreamID,
	}); err != nil {
		fail(fmt.Sprintf("cannot send data-open: %v", err))
		return
	}

	var ack protocol.DataOpenAck
	if err := framer.ReadJSON(protocol.TypeDataOpenAck, &ack); err != nil {
		fail(fmt.Sprintf("cannot read data-open ack: %v", err))
		return
	}
	if !ack.OK {
		fail(fmt.Sprintf("server refused the stream: %s", ack.Error))
		return
	}
	_ = conn.SetDeadline(time.Time{})

	local, err := net.DialTimeout("tcp", proxy.LocalAddr(), dialTimeout)
	if err != nil {
		c.logger.Printf("stream for %q: cannot reach the local service %s: %v", request.Proxy, proxy.LocalAddr(), err)
		_ = conn.Close()
		return
	}

	serverSide := &cryptoStreamConn{Stream: crypto.NewStream(conn, c.cipher), conn: conn}
	idle := time.Duration(c.cfg.Client.IdleTimeoutSecs) * time.Second
	toServer, fromServer := flynet.Pipe(local, serverSide, idle)
	c.logger.Printf("stream for %q finished (sent %d bytes to the server, received %d)",
		request.Proxy, toServer, fromServer)
}

func (c *client) findProxy(name string) (config.ProxyConfig, error) {
	for _, proxy := range c.cfg.Proxies {
		if proxy.Name == name {
			return proxy, nil
		}
	}
	return config.ProxyConfig{}, errors.New("not in this client's configuration")
}

// cryptoStreamConn presents a crypto.Stream as a net.Conn for the pipe helper.
type cryptoStreamConn struct {
	*crypto.Stream
	conn net.Conn
}

func (c *cryptoStreamConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *cryptoStreamConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *cryptoStreamConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *cryptoStreamConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *cryptoStreamConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
