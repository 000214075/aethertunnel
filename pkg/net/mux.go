package net

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// MuxConnection 封装多路复用连接
type MuxConnection struct {
	session *yamux.Session
}

// NewMuxServer 创建服务端多路复用器
func NewMuxServer(conn net.Conn, config *yamux.Config) (*MuxConnection, error) {
	if config == nil {
		config = yamux.DefaultConfig()
		config.KeepAliveInterval = 30 * time.Second
		config.ConnectionWriteTimeout = 10 * time.Second
		config.MaxStreamWindowSize = 6 * 1024 * 1024
	}

	session, err := yamux.Server(conn, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create mux server: %w", err)
	}

	return &MuxConnection{session: session}, nil
}

// NewMuxClient 创建客户端多路复用器
func NewMuxClient(conn net.Conn, config *yamux.Config) (*MuxConnection, error) {
	if config == nil {
		config = yamux.DefaultConfig()
		config.KeepAliveInterval = 30 * time.Second
		config.ConnectionWriteTimeout = 10 * time.Second
		config.MaxStreamWindowSize = 6 * 1024 * 1024
	}

	session, err := yamux.Client(conn, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create mux client: %w", err)
	}

	return &MuxConnection{session: session}, nil
}

// Accept 接受新的流
func (m *MuxConnection) Accept() (net.Conn, error) {
	stream, err := m.session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return &MuxStream{Stream: stream}, nil
}

// Open 打开新的流
func (m *MuxConnection) Open() (net.Conn, error) {
	stream, err := m.session.OpenStream()
	if err != nil {
		return nil, err
	}
	return &MuxStream{Stream: stream}, nil
}

// Close 关闭多路复用连接
func (m *MuxConnection) Close() error {
	return m.session.Close()
}

// IsClosed 检查是否已关闭
func (m *MuxConnection) IsClosed() bool {
	return m.session.IsClosed()
}

// NumStreams 获取当前流数量
func (m *MuxConnection) NumStreams() int {
	return m.session.NumStreams()
}

// MuxStream 封装 yamux 流
type MuxStream struct {
	*yamux.Stream
}

// ReadData 读取数据（封装以提供更好的错误处理）
func (s *MuxStream) ReadData(p []byte) (int, error) {
	n, err := s.Stream.Read(p)
	if err != nil {
		return n, err
	}
	return n, nil
}

// WriteData 写入数据
func (s *MuxStream) WriteData(p []byte) (int, error) {
	n, err := s.Stream.Write(p)
	if err != nil {
		return n, err
	}
	return n, nil
}

// ContextConnection 带上下文的连接
type ContextConnection struct {
	conn   net.Conn
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

// NewContextConnection 创建带上下文的连接
func NewContextConnection(ctx context.Context, conn net.Conn) *ContextConnection {
	return &ContextConnection{
		conn: conn,
		ctx:  ctx,
	}
}

// Read 实现io.Reader
func (c *ContextConnection) Read(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	done := make(chan struct{})
	go func() {
		n, err = c.conn.Read(p)
		close(done)
	}()

	select {
	case <-done:
		if err != nil {
			c.closed = true
		}
		return
	case <-c.ctx.Done():
		c.conn.SetReadDeadline(time.Now())
		return 0, c.ctx.Err()
	}
}

// Write 实现io.Writer
func (c *ContextConnection) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	done := make(chan struct{})
	go func() {
		n, err = c.conn.Write(p)
		close(done)
	}()

	select {
	case <-done:
		if err != nil {
			c.closed = true
		}
		return
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}

// Close 关闭连接
func (c *ContextConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	return c.conn.Close()
}

// LocalAddr 返回本地地址
func (c *ContextConnection) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr 返回远程地址
func (c *ContextConnection) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline 设置读写截止时间
func (c *ContextConnection) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.SetDeadline(t)
}

// SetReadDeadline 设置读截止时间
func (c *ContextConnection) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline 设置写截止时间
func (c *ContextConnection) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.SetWriteDeadline(t)
}

// CreateTLSConfig 创建 TLS 配置
func CreateTLSConfig(certFile, keyFile, caFile string, clientAuth, isClient bool) (*tls.Config, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if isClient {
		// 客户端配置
		config.InsecureSkipVerify = false
		if certFile != "" && keyFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load client certificate: %w", err)
			}
			config.Certificates = []tls.Certificate{cert}
		}
	} else {
		// 服务端配置
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load server certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{cert}

		if clientAuth && caFile != "" {
			caCert, err := tls.LoadX509KeyPair(caFile, caFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load CA certificate: %w", err)
			}

			config.ClientCAs = cert.Leaf
			config.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}

	return config, nil
}

// DialTimeout 带超时的拨号
func DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

// DialContext 带上下文的拨号
func DialContext(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, "tcp", addr)
}

// JoinConnections 连接两个连接并进行双向数据转发
func JoinConnections(conn1, conn2 net.Conn) error {
	type result struct {
		n   int64
		err error
	}

	ch := make(chan result)

	// conn1 -> conn2
	go func() {
		n, err := io.Copy(conn2, conn1)
		ch <- result{n, err}
	}()

	// conn2 -> conn1
	go func() {
		n, err := io.Copy(conn1, conn2)
		ch <- result{n, err}
	}()

	// 等待任意一个方向完成
	r := <-ch
	conn1.Close()
	conn2.Close()

	return r.err
}

// CopyWithLimit 带速率限制的复制
func CopyWithLimit(dst io.Writer, src io.Reader, limit int64) (written int64, err error) {
	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			if limit > 0 && (written+int64(nr)) > limit {
				nr = int(limit - written)
			}

			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}

			if ew != nil {
				err = ew
				break
			}

			if nr != nw {
				err = io.ErrShortWrite
				break
			}

			if limit > 0 && written >= limit {
				break
			}
		}

		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}

		if limit > 0 && written >= limit {
			break
		}
	}

	return written, err
}
