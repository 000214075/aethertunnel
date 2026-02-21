package vpn

// PerformanceOptimizerInterface defines the interface for performance optimization
type PerformanceOptimizerInterface interface {
	Enable()
	Disable() bool
	IsEnabled() bool
	AllowConnection() bool
	AllowRequest() bool
	RecordSuccess()
	RecordFailure()
	UpdateStats(bytes int, latency time.Duration)
	GetStats() *PerformanceStats
	GetConnection(addr string, timeout time.Duration) (net.Conn, error)
	PutConnection(conn net.Conn)
}

// PerformanceStats defines the interface for performance statistics
type PerformanceStatsInterface interface {
	UpdateStats(bytes int, latency time.Duration)
	ReleaseConnection()
	GetStats() map[string]interface{}
}

// ConnectionPoolInterface defines the interface for connection pooling
type ConnectionPoolInterface interface {
	Get(addr string, timeout time.Duration) (net.Conn, error)
	Put(conn net.Conn)
}

// RateLimiterInterface defines the interface for rate limiting
type RateLimiterInterface interface {
	Allow() bool
}

// CircuitBreakerInterface defines the interface for circuit breaking
type CircuitBreakerInterface interface {
	Allow() bool
	RecordSuccess()
	RecordFailure()
}