package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is the process-wide counter set exported in the Prometheus text
// exposition format at GET /metrics.
//
// The implementation uses atomics only: scraping must not take a lock that the
// data path holds, and the counters have to stay meaningful while a scrape is in
// flight.
type Metrics struct {
	startedAt time.Time

	controlAccepted  atomic.Int64
	controlRejected  atomic.Int64
	authFailures     atomic.Int64
	dataConnections  atomic.Int64
	aclDenied        atomic.Int64
	rateLimited      atomic.Int64
	streamsActive    atomic.Int64
	streamsTotal     atomic.Int64
	bytesFromClients atomic.Int64
	bytesToClients   atomic.Int64
	udpDatagrams     atomic.Int64
	udpSessions      atomic.Int64
	httpRequests     atomic.Int64
	p2pPunches       atomic.Int64
	p2pDirect        atomic.Int64
	p2pRelayed       atomic.Int64

	mu        sync.RWMutex
	perTunnel map[string]*tunnelMetrics
}

type tunnelMetrics struct {
	streamsActive    atomic.Int64
	streamsTotal     atomic.Int64
	bytesFromClients atomic.Int64
	bytesToClients   atomic.Int64
	httpRequests     atomic.Int64
}

func newMetrics() *Metrics {
	return &Metrics{
		startedAt: time.Now(),
		perTunnel: make(map[string]*tunnelMetrics),
	}
}

func (m *Metrics) tunnel(name string) *tunnelMetrics {
	m.mu.RLock()
	entry, ok := m.perTunnel[name]
	m.mu.RUnlock()
	if ok {
		return entry
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.perTunnel[name]; ok {
		return entry
	}
	entry = &tunnelMetrics{}
	m.perTunnel[name] = entry
	return entry
}

// recordStream adds one finished stream to the global and per-tunnel counters.
func (m *Metrics) recordStream(tunnel string, toClient, fromClient int64) {
	m.streamsTotal.Add(1)
	m.bytesToClients.Add(toClient)
	m.bytesFromClients.Add(fromClient)

	entry := m.tunnel(tunnel)
	entry.streamsTotal.Add(1)
	entry.bytesToClients.Add(toClient)
	entry.bytesFromClients.Add(fromClient)
}

// streamOpened and streamClosed track concurrency, globally and per tunnel.
func (m *Metrics) streamOpened(tunnel string) {
	m.streamsActive.Add(1)
	m.tunnel(tunnel).streamsActive.Add(1)
}

func (m *Metrics) streamClosed(tunnel string) {
	m.streamsActive.Add(-1)
	m.tunnel(tunnel).streamsActive.Add(-1)
}

// Render produces the Prometheus text exposition format (version 0.0.4).
func (m *Metrics) Render() string {
	var b strings.Builder

	writeHelp := func(name, help, kind string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	counter := func(name, help string, value int64) {
		writeHelp(name, help, "counter")
		fmt.Fprintf(&b, "%s %d\n", name, value)
	}
	gauge := func(name, help string, value int64) {
		writeHelp(name, help, "gauge")
		fmt.Fprintf(&b, "%s %d\n", name, value)
	}

	gauge("aethertunnel_uptime_seconds", "Seconds since the process started.", int64(time.Since(m.startedAt).Seconds()))
	counter("aethertunnel_control_connections_total", "Control connections accepted.", m.controlAccepted.Load())
	counter("aethertunnel_control_rejected_total", "Control connections refused (capacity, ACL or rate limit).", m.controlRejected.Load())
	counter("aethertunnel_auth_failures_total", "Authentication attempts with an invalid token.", m.authFailures.Load())
	counter("aethertunnel_connections_denied_by_acl_total", "Connections refused by allow/deny lists.", m.aclDenied.Load())
	counter("aethertunnel_connections_rate_limited_total", "Connections refused by the per-IP rate limit.", m.rateLimited.Load())
	counter("aethertunnel_data_connections_total", "Data connections opened by clients.", m.dataConnections.Load())
	gauge("aethertunnel_streams_active", "Tunnelled streams currently open.", m.streamsActive.Load())
	counter("aethertunnel_streams_total", "Tunnelled streams completed.", m.streamsTotal.Load())
	counter("aethertunnel_bytes_from_clients_total", "Bytes received from clients.", m.bytesFromClients.Load())
	counter("aethertunnel_bytes_to_clients_total", "Bytes sent to clients.", m.bytesToClients.Load())
	counter("aethertunnel_udp_datagrams_total", "UDP datagrams relayed.", m.udpDatagrams.Load())
	gauge("aethertunnel_udp_sessions_active", "UDP visitor sessions currently tracked.", m.udpSessions.Load())
	counter("aethertunnel_http_requests_total", "Requests served by the shared virtual-host listener.", m.httpRequests.Load())
	counter("aethertunnel_p2p_punches_total", "Hole punching attempts started for xtcp proxies.", m.p2pPunches.Load())
	counter("aethertunnel_p2p_direct_total", "Hole punching attempts that produced a direct path.", m.p2pDirect.Load())
	counter("aethertunnel_p2p_relayed_total", "Hole punching attempts that fell back to the relayed path.", m.p2pRelayed.Load())

	names := make([]string, 0, len(m.perTunnel))
	m.mu.RLock()
	for name := range m.perTunnel {
		names = append(names, name)
	}
	m.mu.RUnlock()
	sort.Strings(names)

	if len(names) > 0 {
		writeHelp("aethertunnel_tunnel_streams_active", "Open streams per tunnel.", "gauge")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_streams_active{tunnel=%q} %d\n", name, m.perTunnel[name].streamsActive.Load())
		}
		writeHelp("aethertunnel_tunnel_streams_total", "Completed streams per tunnel.", "counter")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_streams_total{tunnel=%q} %d\n", name, m.perTunnel[name].streamsTotal.Load())
		}
		writeHelp("aethertunnel_tunnel_bytes_total", "Bytes relayed per tunnel and direction.", "counter")
		for _, name := range names {
			entry := m.perTunnel[name]
			fmt.Fprintf(&b, "aethertunnel_tunnel_bytes_total{tunnel=%q,direction=\"from_client\"} %d\n", name, entry.bytesFromClients.Load())
			fmt.Fprintf(&b, "aethertunnel_tunnel_bytes_total{tunnel=%q,direction=\"to_client\"} %d\n", name, entry.bytesToClients.Load())
		}
		writeHelp("aethertunnel_tunnel_http_requests_total", "HTTP requests served per tunnel.", "counter")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_http_requests_total{tunnel=%q} %d\n", name, m.perTunnel[name].httpRequests.Load())
		}
	}

	return b.String()
}
