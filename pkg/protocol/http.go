package protocol

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPConn represents an HTTP connection
type HTTPConn struct {
	mu       sync.RWMutex
	conn     net.Conn
	addr     net.Addr
	running  bool
	dataChan chan []byte
	errChan  chan error
	method   string
	path     string
	headers  http.Header
	body     []byte
	useTLS   bool
	compress bool
}

// HTTPServer represents an HTTP server
type HTTPServer struct {
	mu         sync.RWMutex
	listener   net.Listener
	running    bool
	config     *HTTPConfig
	handler    func(*HTTPConn)
	conns      map[*HTTPConn]struct{}
	httpServer *http.Server
}

// HTTPConfig represents HTTP configuration
type HTTPConfig struct {
	ReadTimeout       time.Duration `toml:"read_timeout"`
	WriteTimeout      time.Duration `toml:"write_timeout"`
	IdleTimeout       time.Duration `toml:"idle_timeout"`
	MaxHeaderBytes    int           `toml:"max_header_bytes"`
	EnableCompression bool          `toml:"enable_compression"`
	EnableTLS         bool          `toml:"enable_tls"`
	EnableCORS        bool          `toml:"enable_cors"`
	EnableChunked     bool          `toml:"enable_chunked"`
	MaxBodySize       int64         `toml:"max_body_size"`
	EnableKeepalive   bool          `toml:"enable_keepalive"`
	EnablePipelining  bool          `toml:"enable_pipelining"`
	EnableHTTP2       bool          `toml:"enable_http2"`
	EnableWebSocket   bool          `toml:"enable_websocket"`
	AllowedMethods    []string      `toml:"allowed_methods"`
	AllowedOrigins    []string      `toml:"allowed_origins"`
	EnableAuth        bool          `toml:"enable_auth"`
	AuthType          string        `toml:"auth_type"`
	EnableRateLimit   bool          `toml:"enable_rate_limit"`
	RateLimitRequests int           `toml:"rate_limit_requests"`
	RateLimitWindow   time.Duration `toml:"rate_limit_window"`
}

// DefaultHTTPConfig returns default HTTP configuration
func DefaultHTTPConfig() *HTTPConfig {
	return &HTTPConfig{
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
		EnableCompression: true,
		EnableTLS:         false,
		EnableCORS:        true,
		EnableChunked:     true,
		MaxBodySize:       10 * 1024 * 1024, // 10MB
		EnableKeepalive:   true,
		EnablePipelining:  true,
		EnableHTTP2:       false,
		EnableWebSocket:   true,
		AllowedMethods:    []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "HEAD", "CONNECT"},
		AllowedOrigins:    []string{"*"},
		EnableAuth:        false,
		AuthType:          "Bearer",
		EnableRateLimit:   false,
		RateLimitRequests: 1000,
		RateLimitWindow:   time.Minute,
	}
}

// NewHTTPServer creates a new HTTP server
func NewHTTPServer(config *HTTPConfig, handler func(*HTTPConn)) *HTTPServer {
	if config == nil {
		config = DefaultHTTPConfig()
	}

	// Create custom HTTP handler
	httpHandler := &httpHandler{
		config:  config,
		handler: handler,
	}

	httpServer := &http.Server{
		Handler:        httpHandler,
		ReadTimeout:    config.ReadTimeout,
		WriteTimeout:   config.WriteTimeout,
		IdleTimeout:    config.IdleTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}

	return &HTTPServer{
		config:     config,
		handler:    handler,
		conns:      make(map[*HTTPConn]struct{}),
		httpServer: httpServer,
	}
}

// httpHandler implements http.Handler interface
type httpHandler struct {
	config  *HTTPConfig
	handler func(*HTTPConn)
}

// ServeHTTP implements http.Handler.ServeHTTP
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check rate limit
	if h.config.EnableRateLimit {
		if !h.checkRateLimit(r) {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// Check allowed methods
	if !h.isMethodAllowed(r.Method) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check CORS
	if h.config.EnableCORS {
		h.handleCORS(w, r)
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Read request body
	body, err := h.readRequestBody(r)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Create HTTP connection wrapper
	httpConn := &HTTPConn{
		conn:     r.Body.(io.Closer).(net.Conn),                     // This is a simplification
		addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}, // Simplified
		running:  true,
		dataChan: make(chan []byte, 1024),
		errChan:  make(chan error, 10),
		method:   r.Method,
		path:     r.URL.Path,
		headers:  r.Header,
		body:     body,
		useTLS:   r.TLS != nil,
	}

	// Store original connection for later use
	h.handler(httpConn)

	// Wait for response from handler
	select {
	case data := <-httpConn.dataChan:
		h.writeResponse(w, data, httpConn)
	case err := <-httpConn.errChan:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case <-time.After(h.config.WriteTimeout):
		http.Error(w, "Handler timeout", http.StatusGatewayTimeout)
	}
}

// checkRateLimit checks rate limiting
func (h *httpHandler) checkRateLimit(r *http.Request) bool {
	// Simplified rate limiting - in production use a proper rate limiter
	return true
}

// isMethodAllowed checks if method is allowed
func (h *httpHandler) isMethodAllowed(method string) bool {
	for _, allowed := range h.config.AllowedMethods {
		if allowed == method {
			return true
		}
	}
	return false
}

// handleCORS handles CORS headers
func (h *httpHandler) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(h.config.AllowedMethods, ", "))
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

// readRequestBody reads request body
func (h *httpHandler) readRequestBody(r *http.Request) ([]byte, error) {
	if r.ContentLength > h.config.MaxBodySize {
		return nil, fmt.Errorf("request body too large")
	}

	var reader io.Reader = r.Body
	if h.config.EnableCompression {
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(r.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to create gzip reader: %v", err)
			}
			defer gzipReader.Close()
			reader = gzipReader
		}
	}

	return io.ReadAll(reader)
}

// writeResponse writes response
func (h *httpHandler) writeResponse(w http.ResponseWriter, data []byte, conn *HTTPConn) {
	// Set response headers
	w.Header().Set("Server", "AetherTunnel")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-XSS-Protection", "1; mode=block")

	// Handle compression
	if h.config.EnableCompression && conn.compress {
		w.Header().Set("Content-Encoding", "gzip")
		var buf bytes.Buffer
		gzipWriter := gzip.NewWriter(&buf)
		gzipWriter.Write(data)
		gzipWriter.Close()
		data = buf.Bytes()
	}

	// Write response
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// Start starts the HTTP server
func (s *HTTPServer) Start(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("HTTP server is already running")
	}

	// Create TLS config if enabled
	var listener net.Listener
	var err error

	if s.config.EnableTLS {
		listener, err = s.createTLSListener(addr)
		if err != nil {
			return fmt.Errorf("failed to create TLS listener: %v", err)
		}
	} else {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %v", addr, err)
		}
	}

	s.listener = listener
	s.running = true
	s.conns = make(map[*HTTPConn]struct{})

	// Start HTTP server
	go s.httpServer.Serve(listener)

	log.Printf("HTTP server started on %s", addr)
	return nil
}

// createTLSListener creates a TLS listener
func (s *HTTPServer) createTLSListener(addr string) (net.Listener, error) {
	// In production, use proper TLS configuration
	return net.Listen("tcp", addr)
}

// Stop stops the HTTP server
func (s *HTTPServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	s.running = false

	// Close all connections
	for conn := range s.conns {
		conn.Close()
	}
	s.conns = make(map[*HTTPConn]struct{})

	// Shutdown HTTP server
	if err := s.httpServer.Shutdown(nil); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Close listener
	if s.listener != nil {
		s.listener.Close()
		s.listener = nil
	}

	log.Println("HTTP server stopped")
	return nil
}

// Read reads data from HTTP connection
func (c *HTTPConn) Read() ([]byte, error) {
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
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("read timeout")
	}
}

// Write writes data to HTTP connection
func (c *HTTPConn) Write(data []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return fmt.Errorf("connection is not running")
	}

	select {
	case c.dataChan <- data:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("write timeout")
	}
}

// Close closes the HTTP connection
func (c *HTTPConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	c.running = false
	close(c.dataChan)
	close(c.errChan)

	return c.conn.Close()
}

// SetCompression enables or disables compression
func (c *HTTPConn) SetCompression(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compress = enabled
}

// IsRunning checks if the connection is running
func (c *HTTPConn) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// GetMethod returns the HTTP method
func (c *HTTPConn) GetMethod() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.method
}

// GetPath returns the HTTP path
func (c *HTTPConn) GetPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// GetHeaders returns the HTTP headers
func (c *HTTPConn) GetHeaders() http.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.headers
}

// GetBody returns the HTTP body
func (c *HTTPConn) GetBody() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.body
}

// IsTLS checks if the connection uses TLS
func (c *HTTPConn) IsTLS() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.useTLS
}
