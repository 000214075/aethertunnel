package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// WebSocket 消息类型
	WebSocketTextMessage   = websocket.TextMessage
	WebSocketBinaryMessage = websocket.BinaryMessage
	WebSocketCloseMessage  = websocket.CloseMessage
	WebSocketPingMessage   = websocket.PingMessage
	WebSocketPongMessage   = websocket.PongMessage

	// WebSocket 配置
	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 4096
	defaultWriteTimeout    = 10 * time.Second
	defaultReadTimeout     = 60 * time.Second
	defaultPingPeriod      = 30 * time.Second
)

// WebSocketConn represents a WebSocket connection
type WebSocketConn struct {
	mu          sync.RWMutex
	conn        *websocket.Conn
	addr        net.Addr
	running     bool
	dataChan    chan []byte
	errChan     chan error
	messageType int
	config      *WebSocketConfig
}

// WebSocketConfig represents WebSocket configuration
type WebSocketConfig struct {
	ReadBufferSize     int           `toml:"read_buffer_size"`
	WriteBufferSize    int           `toml:"write_buffer_size"`
	EnableCompression  bool          `toml:"enable_compression"`
	CompressionLevel   int           `toml:"compression_level"`
	MaxMessageSize     int64         `toml:"max_message_size"`
	ReadTimeout        time.Duration `toml:"read_timeout"`
	WriteTimeout       time.Duration `toml:"write_timeout"`
	PingPeriod         time.Duration `toml:"ping_period"`
	PongTimeout        time.Duration `toml:"pong_timeout"`
	HandshakeTimeout   time.Duration `toml:"handshake_timeout"`
	EnableTLS          bool          `toml:"enable_tls"`
	AllowedOrigins     []string      `toml:"allowed_origins"`
	EnableCORS         bool          `toml:"enable_cors"`
	EnableUTF8         bool          `toml:"enable_utf8"`
	EnablePongHandler  bool          `toml:"enable_pong_handler"`
	MaxConnections     int           `toml:"max_connections"`
	EnableBinary       bool          `toml:"enable_binary"`
	EnableText         bool          `toml:"enable_text"`
}

// WebSocketServer represents a WebSocket server
type WebSocketServer struct {
	mu          sync.RWMutex
	listener    net.Listener
	running     bool
	connChan    chan *WebSocketConn
	config      *WebSocketConfig
	upgrader    *websocket.Upgrader
	handler     func(*WebSocketConn)
	connections map[*WebSocketConn]struct{}
}

// DefaultWebSocketConfig returns default WebSocket configuration
func DefaultWebSocketConfig() *WebSocketConfig {
	return &WebSocketConfig{
		ReadBufferSize:    defaultReadBufferSize,
		WriteBufferSize:   defaultWriteBufferSize,
		EnableCompression: true,
		CompressionLevel:  6,
		MaxMessageSize:    10 * 1024 * 1024, // 10MB
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		PingPeriod:        defaultPingPeriod,
		PongTimeout:       60 * time.Second,
		HandshakeTimeout:  10 * time.Second,
		EnableTLS:         false,
		AllowedOrigins:    []string{"*"},
		EnableCORS:        true,
		EnableUTF8:        true,
		EnablePongHandler: true,
		MaxConnections:    1000,
		EnableBinary:      true,
		EnableText:        true,
	}
}

// NewWebSocketServer creates a new WebSocket server
func NewWebSocketServer(config *WebSocketConfig, handler func(*WebSocketConn)) *WebSocketServer {
	if config == nil {
		config = DefaultWebSocketConfig()
	}

	upgrader := &websocket.Upgrader{
		ReadBufferSize:  config.ReadBufferSize,
		WriteBufferSize: config.WriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			return config.EnableCORS
		},
		EnableCompression: config.EnableCompression,
		CompressionLevel:  config.CompressionLevel,
		Subprotocols:      []string{"aethertunnel"},
		ReadTimeout:       config.ReadTimeout,
		WriteTimeout:      config.WriteTimeout,
		HandshakeTimeout:  config.HandshakeTimeout,
		AllowOldCipherSuites: false,
		EnableUTF8:          config.EnableUTF8,
	}

	return &WebSocketServer{
		config:      config,
		upgrader:    upgrader,
		handler:     handler,
		connections: make(map[*WebSocketConn]struct{}),
	}
}

// Start starts the WebSocket server
func (s *WebSocketServer) Start(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("WebSocket server is already running")
	}

	// Create HTTP server
	httpServer := &http.Server{
		Addr:         addr,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
		IdleTimeout:  s.config.PingPeriod * 2,
	}

	// Handle WebSocket upgrade
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.NotFound(w, r)
			return
		}

		s.handleWebSocketUpgrade(w, r)
	})

	// Start listening
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", addr, err)
	}

	s.listener = listener
	s.running = true
	s.connChan = make(chan *WebSocketConn, s.config.MaxConnections)

	// Start accepting connections
	go s.acceptConnections(httpServer)

	log.Printf("WebSocket server started on %s", addr)
	return nil
}

// handleWebSocketUpgrade handles WebSocket upgrade request
func (s *WebSocketServer) handleWebSocketUpgrade(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.running {
		http.Error(w, "WebSocket server is not running", http.StatusServiceUnavailable)
		return
	}

	// Check connection limit
	if len(s.connections) >= s.config.MaxConnections {
		http.Error(w, "Connection limit reached", http.StatusTooManyRequests)
		return
	}

	// Upgrade connection
	conn, err := s.upgrader.Upgrade(w, r, w.Header())
	if err != nil {
		log.Printf("Failed to upgrade WebSocket connection: %v", err)
		return
	}

	// Set max message size
	conn.SetReadLimit(s.config.MaxMessageSize)

	// Create WebSocket connection wrapper
	wsConn := &WebSocketConn{
		conn:        conn,
		addr:        r.RemoteAddr,
		running:     true,
		dataChan:    make(chan []byte, 1024),
		errChan:     make(chan error, 10),
		messageType: s.getBestMessageType(),
		config:      s.config,
	}

	// Set message handlers
	conn.SetPongHandler(func(appData string) error {
		return wsConn.handlePong(appData)
	})

	conn.SetPingHandler(func(appData string) error {
		return wsConn.handlePing(appData)
	})

	// Start handling connection
	s.connections[wsConn] = struct{}{}
	go wsConn.handleIncomingMessages()
	go wsConn.handlePings()

	s.handler(wsConn)
}

// getBestMessageType determines the best message type to use
func (s *WebSocketServer) getBestMessageType() int {
	if s.config.EnableBinary {
		return WebSocketBinaryMessage
	} else if s.config.EnableText {
		return WebSocketTextMessage
	}
	return WebSocketBinaryMessage
}

// acceptConnections accepts incoming HTTP connections
func (s *WebSocketServer) acceptConnections(httpServer *http.Server) {
	for s.running {
		conn, err := s.listener.Accept()
		if err != nil {
			if !s.running {
				return
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go httpServer.ServeConn(conn)
	}
}

// Stop stops the WebSocket server
func (s *WebSocketServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	s.running = false
	close(s.connChan)

	// Close all connections
	for conn := range s.connections {
		conn.Close()
	}
	s.connections = make(map[*WebSocketConn]struct{})

	// Close listener
	if s.listener != nil {
		s.listener.Close()
		s.listener = nil
	}

	log.Println("WebSocket server stopped")
	return nil
}

// handleIncomingMessages handles incoming WebSocket messages
func (c *WebSocketConn) handleIncomingMessages() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for c.running {
		c.conn.SetReadDeadline(time.Now().Add(c.config.ReadTimeout))
		messageType, reader, err := c.conn.NextReader()
		if err != nil {
			if !c.running {
				return
			}
			c.errChan <- fmt.Errorf("failed to read WebSocket message: %v", err)
			return
		}

		// Read message
		data, err := io.ReadAll(reader)
		if err != nil {
			c.errChan <- fmt.Errorf("failed to read WebSocket data: %v", err)
			continue
		}

		// Validate message type
		if (messageType == WebSocketTextMessage && !c.config.EnableText) ||
			(messageType == WebSocketBinaryMessage && !c.config.EnableBinary) {
			continue
		}

		// Send data to channel
		select {
		case c.dataChan <- data:
		case <-time.After(c.config.WriteTimeout):
			log.Printf("WebSocket data channel full, dropping message")
		}
	}
}

// handlePings handles WebSocket ping messages
func (c *WebSocketConn) handlePings() {
	ticker := time.NewTicker(c.config.PingPeriod)
	defer ticker.Stop()

	for c.running {
		select {
		case <-ticker.C:
			c.mu.RLock()
			if c.running {
				c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
				if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					c.mu.RUnlock()
					log.Printf("Failed to send WebSocket ping: %v", err)
					return
				}
			}
			c.mu.RUnlock()
		}
	}
}

// handlePong handles WebSocket pong messages
func (c *WebSocketConn) handlePong(appData string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.config.EnablePongHandler {
		c.conn.SetReadDeadline(time.Now().Add(c.config.PongTimeout))
	}
	return nil
}

// handlePing handles WebSocket ping messages
func (c *WebSocketConn) handlePing(appData string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.running {
		c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
		return c.conn.WriteMessage(websocket.PongMessage, []byte(appData))
	}
	return nil
}

// Read reads data from WebSocket connection
func (c *WebSocketConn) Read() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return nil, fmt.Errorf("connection is not running")
	}

	select {
	case data := <-c.dataChan:
		return data, nil
	case err := <-c.errChan:
		return nil, err
	case <-time.After(c.config.ReadTimeout):
		return nil, fmt.Errorf("read timeout")
	}
}

// Write writes data to WebSocket connection
func (c *WebSocketConn) Write(data []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return fmt.Errorf("connection is not running")
	}

	c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
	return c.conn.WriteMessage(c.messageType, data)
}

// Close closes the WebSocket connection
func (c *WebSocketConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	c.running = false
	close(c.dataChan)
	close(c.errChan)

	// Send close message
	c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteTimeout))
	c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

	return c.conn.Close()
}

// Addr returns the local address
func (c *WebSocketConn) Addr() net.Addr {
	return c.addr
}

// RemoteAddr returns the remote address
func (c *WebSocketConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// IsRunning checks if the connection is running
func (c *WebSocketConn) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// SetMessageType sets the message type
func (c *WebSocketConn) SetMessageType(messageType int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messageType = messageType
}

// GetConfig returns the WebSocket configuration
func (c *WebSocketConn) GetConfig() *WebSocketConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config
}