package server

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/obfs"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// teeConn records everything written through it and passes it on, so a test can
// look at the bytes a wrapper puts on the wire.
type teeConn struct {
	net.Conn

	mu      sync.Mutex
	written bytes.Buffer
}

func (c *teeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.written.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *teeConn) onWire() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}

// disguisedConfig is a server configuration that wraps every connection.
func disguisedConfig(t *testing.T, disguise string) *config.Config {
	t.Helper()
	cfg := testConfig(t, false)
	cfg.Obfuscation.Enabled = true
	cfg.Obfuscation.Disguise = disguise
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

func TestDisguiseWrapsTheWireAndBothEndsAgree(t *testing.T) {
	cfg := disguisedConfig(t, obfs.DisguiseTLSRecord)
	rs := startServer(t, cfg)

	raw, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer raw.Close()

	tee := &teeConn{Conn: raw}
	wrapped, err := obfs.Wrap(tee, obfs.DisguiseTLSRecord)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	framer := protocol.NewFramer(wrapped, nil, 0)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "disguise-test", Protocol: protocol.ProtocolVersion,
	}); err != nil {
		t.Fatalf("send the auth request: %v", err)
	}

	var response protocol.AuthResponse
	if err := framer.ReadJSON(protocol.TypeAuthResponse, &response); err != nil {
		t.Fatalf("read the auth response: %v", err)
	}
	if !response.OK {
		t.Fatalf("the session was refused: %s", response.Error)
	}

	// What the client put on the wire has to be a TLS record, not this program's
	// frame header: type 0x17, version 0303, then a two-byte length.
	onWire := tee.onWire()
	if len(onWire) < 5 {
		t.Fatalf("only %d bytes were written", len(onWire))
	}
	if onWire[0] != 0x17 || onWire[1] != 0x03 || onWire[2] != 0x03 {
		t.Fatalf("the first bytes on the wire are %#x, want a 170303 record header", onWire[:3])
	}
	length := int(onWire[3])<<8 | int(onWire[4])
	if length != len(onWire)-5 {
		t.Errorf("the record announces %d bytes and carries %d", length, len(onWire)-5)
	}
}

func TestDisguisedServerRefusesAPeerThatDoesNotWrap(t *testing.T) {
	cfg := disguisedConfig(t, obfs.DisguiseTLSRecord)
	rs := startServer(t, cfg)

	conn, err := net.DialTimeout("tcp", rs.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// An unwrapped frame: the server reads it as a record header and rejects it, so
	// nothing comes back.
	framer := protocol.NewFramer(conn, nil, 0)
	if err := framer.WriteJSON(protocol.TypeAuthRequest, protocol.AuthRequest{
		Token: testToken, ClientVersion: "plain", Protocol: protocol.ProtocolVersion,
	}); err != nil {
		t.Fatalf("send the auth request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 16)
	if n, err := conn.Read(buffer); err == nil {
		t.Fatalf("the server answered an unwrapped client with %#x", buffer[:n])
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("the server neither answered nor closed the connection")
	}
}

func TestDisguiseIsOptionalAndConfigurable(t *testing.T) {
	// A server without a disguise still works with a plain client.
	cfg := testConfig(t, false)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	rs := startServer(t, cfg)

	client, err := newTestClient(t, rs.addr, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.close()
	if _, err := client.authenticate("plain-test", testToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if cfg.ObfuscationDisguise() != obfs.DisguiseNone {
		t.Errorf("a configuration without a disguise reports %q", cfg.ObfuscationDisguise())
	}
}

func TestDisguiseValidation(t *testing.T) {
	cfg := testConfig(t, false)
	cfg.Obfuscation.Enabled = true
	cfg.Obfuscation.Disguise = "look-like-quic"
	err := cfg.Validate(config.RoleServer)
	if err == nil {
		t.Fatal("an unknown disguise was accepted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("obfuscation.disguise")) {
		t.Errorf("the error is %v, which does not name the key", err)
	}

	// A disguise without the section enabled is reported rather than applied.
	warnCfg := testConfig(t, false)
	warnCfg.Obfuscation.Disguise = obfs.DisguiseTLSRecord
	if err := warnCfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	found := false
	for _, warning := range warnCfg.Warnings {
		if bytes.Contains([]byte(warning), []byte("obfuscation.enabled is false")) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning that the section is disabled, got %v", warnCfg.Warnings)
	}
}
