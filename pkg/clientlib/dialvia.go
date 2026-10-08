package clientlib

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// dialVia connects to target through the intermediary a dial_via URL names: a
// SOCKS5 or HTTP CONNECT proxy sitting between this client and the AetherTunnel
// server. Operators reach for it when the direct route is blocked or
// metered but an egress proxy is available; the tunnel's own encryption and
// authentication ride on top of whatever the intermediary carries, so the
// proxy sees opaque TLS-shaped traffic and learns nothing about the tunnels.
func dialVia(ctx context.Context, via string, dialer *net.Dialer, target string) (net.Conn, error) {
	u, err := url.Parse(via)
	if err != nil {
		return nil, fmt.Errorf("dial_via: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		return dialViaSOCKS5(ctx, u, dialer, target)
	case "http", "https":
		return dialViaConnect(ctx, u, dialer, target)
	default:
		return nil, fmt.Errorf("dial_via: scheme %q is not supported (use socks5, socks5h, http or https)", u.Scheme)
	}
}

// dialViaSOCKS5 walks the SOCKS5 handshake the x/net library implements, with
// the dialer's timeout bounding the connect to the proxy itself.
func dialViaSOCKS5(ctx context.Context, u *url.URL, dialer *net.Dialer, target string) (net.Conn, error) {
	var auth *proxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "1080"
	}
	d, err := proxy.SOCKS5("tcp", net.JoinHostPort(host, port), auth, forwardDialer{dialer})
	if err != nil {
		return nil, fmt.Errorf("dial_via: %w", err)
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		conn, err := cd.DialContext(ctx, "tcp", target)
		return conn, wrapDialVia(err)
	}
	conn, err := d.Dial("tcp", target)
	return conn, wrapDialVia(err)
}

// dialViaConnect tunnels through an HTTP proxy with the CONNECT method, the
// same request a browser sends when it opens an HTTPS site through a proxy.
// The https scheme wraps the CONNECT conversation itself in TLS.
func dialViaConnect(ctx context.Context, u *url.URL, dialer *net.Dialer, target string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("dial_via: reach the proxy: %w", err)
	}
	if u.Scheme == "https" {
		tlsConfig := &tls.Config{}
		if host, _, splitErr := net.SplitHostPort(u.Host); splitErr == nil {
			tlsConfig.ServerName = host
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("dial_via: TLS to the proxy: %w", err)
		}
		conn = tlsConn
	}

	// The dial and the TLS handshake are bounded by the context, but the CONNECT
	// exchange is not: request.Write and http.ReadResponse take no context, so a
	// proxy that accepted the connection and then said nothing blocked dialServer
	// forever — and the reconnect loop installs its reader and stop machinery only
	// after dialServer returns, so nothing could break the wait. The exchange
	// shares the deadline the caller set for the whole dial, and the connection is
	// handed over without one.
	deadline, bounded := ctx.Deadline()
	if !bounded && dialer.Timeout > 0 {
		deadline, bounded = time.Now().Add(dialer.Timeout), true
	}
	if bounded {
		_ = conn.SetDeadline(deadline)
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: target},
		Host:   target,
		Header: make(http.Header),
	}
	if u.User != nil {
		password, _ := u.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		request.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	request = request.WithContext(ctx)
	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dial_via: send CONNECT: %w", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dial_via: read the CONNECT answer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("dial_via: the proxy answered %s", response.Status)
	}
	if buffered := reader.Buffered(); buffered > 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("dial_via: the proxy sent %d unexpected byte(s) with its CONNECT answer", buffered)
	}
	if bounded {
		_ = conn.SetDeadline(time.Time{})
	}
	return conn, nil
}

// forwardDialer adapts *net.Dialer to the interface the SOCKS5 helper wants.
type forwardDialer struct{ d *net.Dialer }

func (f forwardDialer) Dial(network, addr string) (net.Conn, error) {
	return f.d.Dial(network, addr)
}

func (f forwardDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f.d.DialContext(ctx, network, addr)
}

func wrapDialVia(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("dial_via: %w", err)
}
