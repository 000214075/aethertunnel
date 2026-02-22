package vpn

import (
	"sync"
	"sync/atomic"
	"time"
)

// VPNStats tracks VPN statistics
type VPNStats struct {
	// Tunnel statistics
	totalTunnels      uint64
	activeTunnels     uint64
	tunnelConnections uint64
	tunnelFailures    uint64

	// Client statistics
	totalClients         uint64
	activeClients        uint64
	maxClients           uint64
	clientConnections    uint64
	clientDisconnections uint64

	// Traffic statistics
	bytesReceived   uint64
	bytesSent       uint64
	packetsReceived uint64
	packetsSent     uint64
	droppedPackets  uint64

	// Performance statistics
	avgLatency          time.Duration
	maxLatency          time.Duration
	latencyMeasurements int64
	packetLoss          float64

	// Error statistics
	encryptionErrors  uint64
	decryptionErrors  uint64
	obfuscationErrors uint64
	handshakeErrors   uint64
	routingErrors     uint64

	// Rate statistics
	connectionsPerSecond float64
	bytesPerSecond       float64
	uptime               time.Duration
	lastReset            time.Time

	mu sync.RWMutex
}

// NewVPNStats creates a new VPN stats tracker
func NewVPNStats() *VPNStats {
	return &VPNStats{
		lastReset: time.Now(),
	}
}

// Reset resets all statistics
func (s *VPNStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.StoreUint64(&s.totalTunnels, 0)
	atomic.StoreUint64(&s.activeTunnels, 0)
	atomic.StoreUint64(&s.tunnelConnections, 0)
	atomic.StoreUint64(&s.tunnelFailures, 0)

	atomic.StoreUint64(&s.totalClients, 0)
	atomic.StoreUint64(&s.activeClients, 0)
	atomic.StoreUint64(&s.maxClients, 0)
	atomic.StoreUint64(&s.clientConnections, 0)
	atomic.StoreUint64(&s.clientDisconnections, 0)

	atomic.StoreUint64(&s.bytesReceived, 0)
	atomic.StoreUint64(&s.bytesSent, 0)
	atomic.StoreUint64(&s.packetsReceived, 0)
	atomic.StoreUint64(&s.packetsSent, 0)
	atomic.StoreUint64(&s.droppedPackets, 0)

	atomic.StoreUint64(&s.encryptionErrors, 0)
	atomic.StoreUint64(&s.decryptionErrors, 0)
	atomic.StoreUint64(&s.obfuscationErrors, 0)
	atomic.StoreUint64(&s.handshakeErrors, 0)
	atomic.StoreUint64(&s.routingErrors, 0)

	atomic.StoreInt64(&s.latencyMeasurements, 0)

	s.connectionsPerSecond = 0
	s.bytesPerSecond = 0
	s.uptime = 0
	s.lastReset = time.Now()
}

// IncrementActiveConnections increments active connections counter
func (s *VPNStats) IncrementActiveConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.activeClients, 1)
	atomic.AddUint64(&s.clientConnections, 1)
	atomic.AddUint64(&s.totalClients, 1)

	// Update max clients
	if s.activeClients > s.maxClients {
		atomic.StoreUint64(&s.maxClients, s.activeClients)
	}

	// Update connection rate
	s.updateConnectionRate()
}

// DecrementActiveConnections decrements active connections counter
func (s *VPNStats) DecrementActiveConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.activeClients, ^uint64(0))
	atomic.AddUint64(&s.clientDisconnections, 1)
}

// IncrementActiveTunnels increments active tunnels counter
func (s *VPNStats) IncrementActiveTunnels() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.activeTunnels, 1)
	atomic.AddUint64(&s.tunnelConnections, 1)
	atomic.AddUint64(&s.totalTunnels, 1)
}

// DecrementActiveTunnels decrements active tunnels counter
func (s *VPNStats) DecrementActiveTunnels() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.activeTunnels, ^uint64(0))
}

// AddBytesReceived adds bytes to received counter
func (s *VPNStats) AddBytesReceived(bytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.bytesReceived, bytes)
	atomic.AddUint64(&s.packetsReceived, 1)

	s.updateBytesPerSecond()
}

// AddBytesSent adds bytes to sent counter
func (s *VPNStats) AddBytesSent(bytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.bytesSent, bytes)
	atomic.AddUint64(&s.packetsSent, 1)
}

// IncrementDroppedPackets increments dropped packets counter
func (s *VPNStats) IncrementDroppedPackets() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.droppedPackets, 1)
}

// IncrementEncryptionErrors increments encryption errors counter
func (s *VPNStats) IncrementEncryptionErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.encryptionErrors, 1)
}

// IncrementDecryptionErrors increments decryption errors counter
func (s *VPNStats) IncrementDecryptionErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.decryptionErrors, 1)
}

// IncrementObfuscationErrors increments obfuscation errors counter
func (s *VPNStats) IncrementObfuscationErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.obfuscationErrors, 1)
}

// IncrementHandshakeErrors increments handshake errors counter
func (s *VPNStats) IncrementHandshakeErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.handshakeErrors, 1)
}

// IncrementRoutingErrors increments routing errors counter
func (s *VPNStats) IncrementRoutingErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddUint64(&s.routingErrors, 1)
}

// RecordLatency records latency measurement
func (s *VPNStats) RecordLatency(latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.AddInt64(&s.latencyMeasurements, 1)

	// Update average latency
	currentMeasurements := atomic.LoadInt64(&s.latencyMeasurements)
	if currentMeasurements == 1 {
		atomic.StoreInt64((*int64)(&s.avgLatency), int64(latency))
	} else {
		// Calculate new average: (old_avg * n + new) / (n + 1)
		oldAvg := s.avgLatency
		newAvg := (oldAvg.Nanoseconds()*int64(currentMeasurements-1) + latency.Nanoseconds()) / int64(currentMeasurements)
		atomic.StoreInt64((*int64)(&s.avgLatency), newAvg)
	}

	// Update max latency
	if latency > s.maxLatency {
		atomic.StoreInt64((*int64)(&s.maxLatency), int64(latency))
	}
}

// UpdatePacketLoss updates packet loss percentage
func (s *VPNStats) UpdatePacketLoss(sent, received uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sent > 0 {
		lost := sent - received
		s.packetLoss = float64(lost) / float64(sent) * 100
	}
}

// updateConnectionRate updates connection rate
func (s *VPNStats) updateConnectionRate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	elapsed := time.Since(s.lastReset).Seconds()
	if elapsed > 0 {
		s.connectionsPerSecond = float64(atomic.LoadUint64(&s.clientConnections)) / elapsed
	}
}

// updateBytesPerSecond updates bytes per second
func (s *VPNStats) updateBytesPerSecond() {
	s.mu.Lock()
	defer s.mu.Unlock()

	elapsed := time.Since(s.lastReset).Seconds()
	if elapsed > 0 {
		s.bytesPerSecond = float64(atomic.LoadUint64(&s.bytesReceived)) / elapsed
	}
}

// GetStats returns current statistics
func (s *VPNStats) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]interface{}{
		// Tunnel stats
		"total_tunnels":      atomic.LoadUint64(&s.totalTunnels),
		"active_tunnels":     atomic.LoadUint64(&s.activeTunnels),
		"tunnel_connections": atomic.LoadUint64(&s.tunnelConnections),
		"tunnel_failures":    atomic.LoadUint64(&s.tunnelFailures),

		// Client stats
		"total_clients":         atomic.LoadUint64(&s.totalClients),
		"active_clients":        atomic.LoadUint64(&s.activeClients),
		"max_clients":           atomic.LoadUint64(&s.maxClients),
		"client_connections":    atomic.LoadUint64(&s.clientConnections),
		"client_disconnections": atomic.LoadUint64(&s.clientDisconnections),

		// Traffic stats
		"bytes_received":   atomic.LoadUint64(&s.bytesReceived),
		"bytes_sent":       atomic.LoadUint64(&s.bytesSent),
		"packets_received": atomic.LoadUint64(&s.packetsReceived),
		"packets_sent":     atomic.LoadUint64(&s.packetsSent),
		"dropped_packets":  atomic.LoadUint64(&s.droppedPackets),

		// Performance stats
		"avg_latency_ms":         s.avgLatency.Milliseconds(),
		"max_latency_ms":         s.maxLatency.Milliseconds(),
		"packet_loss_percent":    s.packetLoss,
		"connections_per_second": s.connectionsPerSecond,
		"bytes_per_second":       s.bytesPerSecond,

		// Error stats
		"encryption_errors":  atomic.LoadUint64(&s.encryptionErrors),
		"decryption_errors":  atomic.LoadUint64(&s.decryptionErrors),
		"obfuscation_errors": atomic.LoadUint64(&s.obfuscationErrors),
		"handshake_errors":   atomic.LoadUint64(&s.handshakeErrors),
		"routing_errors":     atomic.LoadUint64(&s.routingErrors),

		// Uptime
		"uptime_seconds": time.Since(s.lastReset).Seconds(),
	}
}

// GetUptime returns service uptime
func (s *VPNStats) GetUptime() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastReset)
}

// GetConnectionStats returns connection statistics
func (s *VPNStats) GetConnectionStats() map[string]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]uint64{
		"active_clients":        atomic.LoadUint64(&s.activeClients),
		"max_clients":           atomic.LoadUint64(&s.maxClients),
		"client_connections":    atomic.LoadUint64(&s.clientConnections),
		"client_disconnections": atomic.LoadUint64(&s.clientDisconnections),
	}
}

// GetTrafficStats returns traffic statistics
func (s *VPNStats) GetTrafficStats() map[string]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]uint64{
		"bytes_received":   atomic.LoadUint64(&s.bytesReceived),
		"bytes_sent":       atomic.LoadUint64(&s.bytesSent),
		"packets_received": atomic.LoadUint64(&s.packetsReceived),
		"packets_sent":     atomic.LoadUint64(&s.packetsSent),
		"dropped_packets":  atomic.LoadUint64(&s.droppedPackets),
	}
}

// GetErrorStats returns error statistics
func (s *VPNStats) GetErrorStats() map[string]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]uint64{
		"encryption_errors":  atomic.LoadUint64(&s.encryptionErrors),
		"decryption_errors":  atomic.LoadUint64(&s.decryptionErrors),
		"obfuscation_errors": atomic.LoadUint64(&s.obfuscationErrors),
		"handshake_errors":   atomic.LoadUint64(&s.handshakeErrors),
		"routing_errors":     atomic.LoadUint64(&s.routingErrors),
	}
}
