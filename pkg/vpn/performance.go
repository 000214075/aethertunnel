package vpn

import (
	"sync"
	"sync/atomic"
	"time"
)

// PerformanceOptimizer provides performance optimization for VPN
type PerformanceOptimizer struct {
	mu               sync.RWMutex
	enabled          bool
	stats            *PerformanceStats
	pool             *ConnectionPool
	limiter          *RateLimiter
	breaker          *CircuitBreaker
}

// PerformanceStats tracks performance metrics
type PerformanceStats struct {
	TotalConnections    uint64        `json:"total_connections"`
	ActiveConnections   uint64        `json:"active_connections"`
	MaxConnections      uint64        `json:"max_connections"`
	ConnectionRate      float64       `json:"connection_rate"`
	BytesPerSecond      float64       `json:"bytes_per_second"`
	Latency             time.Duration `json:"latency"`
	PacketLoss          float64       `json:"packet_loss"`
	mu                  sync.RWMutex
}

// ConnectionPool manages connection pooling
type ConnectionPool struct {
	pool             chan net.Conn
	maxSize          int
	currentSize      int32
	idleTimeout      time.Duration
	mu               sync.RWMutex
}

// RateLimiter implements rate limiting
type RateLimiter struct {
	limit            int64
	burst            int64
	tokens           int64
	lastUpdate       int64
	mu               sync.Mutex
}

// CircuitBreaker implements circuit breaker pattern
type CircuitBreaker struct {
	failureThreshold int
	successThreshold int
	failures         int
	successes        int
	state            int32 // 0: closed, 1: open, 2: half-open
	timeout          time.Duration
	mu               sync.RWMutex
}

// NewPerformanceOptimizer creates a new performance optimizer
func NewPerformanceOptimizer() *PerformanceOptimizer {
	return &PerformanceOptimizer{
		enabled: true,
		stats:   &PerformanceStats{},
		pool:    NewConnectionPool(1000, 30*time.Minute),
		limiter: NewRateLimiter(1000, 100),
		breaker: NewCircuitBreaker(5, 2, 30*time.Second),
	}
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(maxSize int, idleTimeout time.Duration) *ConnectionPool {
	return &ConnectionPool{
		pool:        make(chan net.Conn, maxSize),
		maxSize:     maxSize,
		idleTimeout: idleTimeout,
	}
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit, burst int64) *RateLimiter {
	return &RateLimiter{
		limit:  limit,
		burst:  burst,
		tokens: burst,
	}
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		timeout:          timeout,
	}
}

// Enable enables performance optimization
func (p *PerformanceOptimizer) Enable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = true
}

// Disable disables performance optimization
func (p *PerformanceOptimizer) Disable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = false
}

// IsEnabled returns whether performance optimization is enabled
func (p *PerformanceOptimizer) IsEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

// GetStats returns performance statistics
func (p *PerformanceOptimizer) GetStats() *PerformanceStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stats
}

// AllowConnection checks if connection is allowed by rate limiter
func (p *PerformanceOptimizer) AllowConnection() bool {
	if !p.enabled {
		return true
	}
	return p.limiter.Allow()
}

// AllowRequest checks if request is allowed by circuit breaker
func (p *PerformanceOptimizer) AllowRequest() bool {
	if !p.enabled {
		return true
	}
	return p.breaker.Allow()
}

// RecordSuccess records a successful request
func (p *PerformanceOptimizer) RecordSuccess() {
	if !p.enabled {
		return
	}
	p.breaker.RecordSuccess()
}

// RecordFailure records a failed request
func (p *PerformanceOptimizer) RecordFailure() {
	if !p.enabled {
		return
	}
	p.breaker.RecordFailure()
}

// UpdateStats updates performance statistics
func (p *PerformanceOptimizer) UpdateStats(bytes int, latency time.Duration) {
	if !p.enabled {
		return
	}

	p.stats.mu.Lock()
	defer p.stats.mu.Unlock()

	atomic.AddUint64(&p.stats.TotalConnections, 1)
	atomic.AddUint64(&p.stats.ActiveConnections, 1)

	// Update connection rate (connections per second)
	now := time.Now()
	if p.stats.lastUpdate.IsZero() {
		p.stats.lastUpdate = now
	} else {
		elapsed := now.Sub(p.stats.lastUpdate).Seconds()
		if elapsed > 0 {
			p.stats.ConnectionRate = float64(p.stats.TotalConnections) / elapsed
		}
	}

	// Update bytes per second
	p.stats.BytesPerSecond = float64(bytes) / latency.Seconds()

	// Update latency
	p.stats.Latency = latency

	// Update max connections
	if p.stats.ActiveConnections > p.stats.MaxConnections {
		p.stats.MaxConnections = p.stats.ActiveConnections
	}
}

// ReleaseConnection releases a connection
func (p *PerformanceOptimizer) ReleaseConnection() {
	if !p.enabled {
		return
	}

	p.stats.mu.Lock()
	defer p.stats.mu.Unlock()
	atomic.AddUint64(&p.stats.ActiveConnections, ^uint64(0))
}

// GetConnection gets a connection from pool
func (p *PerformanceOptimizer) GetConnection(addr string, timeout time.Duration) (net.Conn, error) {
	if !p.enabled {
		return net.DialTimeout("tcp", addr, timeout)
	}

	return p.pool.Get(addr, timeout)
}

// PutConnection puts a connection back to pool
func (p *PerformanceOptimizer) PutConnection(conn net.Conn) {
	if !p.enabled {
		conn.Close()
		return
	}

	p.pool.Put(conn)
}

// GetConnection gets a connection from pool or creates a new one
func (p *ConnectionPool) Get(addr string, timeout time.Duration) (net.Conn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	select {
	case conn := <-p.pool:
		// Check if connection is still valid
		if conn != nil && p.isValidConnection(conn) {
			return conn, nil
		}
	default:
	}

	// Create new connection
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}

	atomic.AddInt32(&p.currentSize, 1)
	return conn, nil
}

// Put puts a connection back to pool
func (p *ConnectionPool) Put(conn net.Conn) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if conn == nil {
		atomic.AddInt32(&p.currentSize, ^int32(0))
		return
	}

	select {
	case p.pool <- conn:
		// Connection added to pool
	default:
		// Pool is full, close connection
		conn.Close()
		atomic.AddInt32(&p.currentSize, ^int32(0))
	}
}

// isValidConnection checks if connection is still valid
func (p *ConnectionPool) isValidConnection(conn net.Conn) bool {
	// Simple check: try to read 0 bytes
	var b [1]byte
	conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	n, err := conn.Read(b[:])
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout is expected, connection is still valid
			conn.SetReadDeadline(time.Time{})
			return true
		}
		// Other error, connection is invalid
		return false
	}
	// Got data, connection is still valid
	if n == 0 {
		return true
	}
	return true
}

// Allow checks if request is allowed
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UnixNano()
	elapsed := float64(now-atomic.LoadInt64(&r.lastUpdate)) / 1e9

	// Add tokens based on elapsed time
	if elapsed > 0 {
		added := int64(elapsed * float64(r.limit))
		if added > 0 {
			atomic.StoreInt64(&r.lastUpdate, now)
			atomic.AddInt64(&r.tokens, added)
			if atomic.LoadInt64(&r.tokens) > r.burst {
				atomic.StoreInt64(&r.tokens, r.burst)
			}
		}
	}

	tokens := atomic.LoadInt64(&r.tokens)
	if tokens <= 0 {
		return false
	}

	atomic.AddInt64(&r.tokens, -1)
	return true
}

// Allow checks if request is allowed by circuit breaker
func (c *CircuitBreaker) Allow() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if atomic.LoadInt32(&c.state) == 1 { // Open state
		// Check if timeout has passed
		if time.Since(time.Unix(0, atomic.LoadInt64(&c.lastFailureTime))) > c.timeout {
			atomic.StoreInt32(&c.state, 2) // Half-open state
			return true
		}
		return false
	}

	return true
}

// RecordSuccess records a successful request
func (c *CircuitBreaker) RecordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if atomic.LoadInt32(&c.state) == 2 { // Half-open state
		c.successes++
		if c.successes >= c.successThreshold {
			c.reset()
		}
	} else {
		c.reset()
	}
}

// RecordFailure records a failed request
func (c *CircuitBreaker) RecordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if atomic.LoadInt32(&c.state) == 2 { // Half-open state
		c.failures++
		if c.failures >= c.failureThreshold {
			c.trip()
		}
	} else {
		c.failures++
		if c.failures >= c.failureThreshold {
			c.trip()
		}
	}
}

// trip trips the circuit breaker to open state
func (c *CircuitBreaker) trip() {
	atomic.StoreInt32(&c.state, 1)
	atomic.StoreInt64(&c.lastFailureTime, time.Now().UnixNano())
}

// reset resets the circuit breaker to closed state
func (c *CircuitBreaker) reset() {
	atomic.StoreInt32(&c.state, 0)
	c.failures = 0
	c.successes = 0
}

// UpdateStats updates performance statistics
func (s *PerformanceStats) UpdateStats(bytes int, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.TotalConnections, 1)
	atomic.AddUint64(&s.ActiveConnections, 1)

	// Update bytes per second
	s.BytesPerSecond = float64(bytes) / latency.Seconds()

	// Update latency
	s.Latency = latency
}

// ReleaseConnection releases a connection
func (s *PerformanceStats) ReleaseConnection() {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddUint64(&s.ActiveConnections, ^uint64(0))
}

// GetStats returns current statistics
func (s *PerformanceStats) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]interface{}{
		"total_connections":    atomic.LoadUint64(&s.TotalConnections),
		"active_connections":   atomic.LoadUint64(&s.ActiveConnections),
		"max_connections":      atomic.LoadUint64(&s.MaxConnections),
		"connection_rate":      s.ConnectionRate,
		"bytes_per_second":     s.BytesPerSecond,
		"latency_ms":           s.Latency.Milliseconds(),
		"packet_loss":          s.PacketLoss,
	}
}