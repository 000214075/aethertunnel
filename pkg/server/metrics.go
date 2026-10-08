package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Metrics is the process-wide counter set exported in the Prometheus text
// exposition format at GET /metrics.
//
// The implementation uses atomics only: scraping must not take a lock that the
// data path holds, and the counters have to stay meaningful while a scrape is in
// flight.
type Metrics struct {
	startedAt time.Time

	controlAccepted   atomic.Int64
	controlRejected   atomic.Int64
	authFailures      atomic.Int64
	dataConnections   atomic.Int64
	dataUnmatched     atomic.Int64
	aclDenied         atomic.Int64
	rateLimited       atomic.Int64
	bans              atomic.Int64
	banRefused        atomic.Int64
	visitorDenied     atomic.Int64
	unusableFrames    atomic.Int64
	handshakeFailures atomic.Int64
	dashboardRefused  atomic.Int64
	socksMalformed    atomic.Int64
	socksRequests     atomic.Int64
	socksUDPAssoc     atomic.Int64
	socksUDPDatagrams atomic.Int64
	drainRefused      atomic.Int64
	streamsActive     atomic.Int64
	streamsTotal      atomic.Int64
	bytesFromClients  atomic.Int64
	bytesToClients    atomic.Int64
	udpDatagrams      atomic.Int64
	udpSessions       atomic.Int64
	udpOversize       atomic.Int64
	udpSessionLimit   atomic.Int64
	httpRequests      atomic.Int64
	poolParked        atomic.Int64
	poolUsed          atomic.Int64
	poolMissed        atomic.Int64
	poolRefused       atomic.Int64
	p2pPunches        atomic.Int64
	p2pDirect         atomic.Int64
	p2pRelayed        atomic.Int64

	// audit is the log whose health is reported; nil when the server was built
	// without one, in which case the series are omitted rather than reported as
	// zero, which would claim an audit log exists and is healthy.
	audit *Auditor

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

// withAudit attaches the audit log whose health the exposition reports.
func (m *Metrics) withAudit(auditor *Auditor) *Metrics {
	m.audit = auditor
	return m
}

// maxTunnelSeries bounds how many per-tunnel series are kept. A client may
// register a fresh proxy name as often as it likes, and every name becomes a
// permanent family in the exposition, so without a bound one client could grow
// this map — and every scrape — for the life of the process.
const maxTunnelSeries = 4096

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
	if len(m.perTunnel) >= maxTunnelSeries {
		m.pruneLocked()
	}
	entry = &tunnelMetrics{}
	if len(m.perTunnel) >= maxTunnelSeries {
		// Pruning frees nothing when every series has a stream open, so the cap
		// is enforced here: the counters of a name that does not fit are kept on
		// an entry nobody stores, rather than growing the scrape without bound.
		return entry
	}
	m.perTunnel[name] = entry
	return entry
}

// pruneLocked drops the series of tunnels that are not carrying anything right
// now. It runs only at the cap, so an ordinary deployment never loses a series
// it was using.
func (m *Metrics) pruneLocked() {
	for name, entry := range m.perTunnel {
		if entry.streamsActive.Load() == 0 {
			delete(m.perTunnel, name)
		}
	}
}

// forgetTunnel drops the series of a proxy that has been unpublished, so a name
// that no longer exists does not keep a family in every scrape.
func (m *Metrics) forgetTunnel(name string) {
	m.mu.Lock()
	delete(m.perTunnel, name)
	m.mu.Unlock()
}

// lookupTunnel returns the per-tunnel counters when the series exists. It never
// creates one: a proxy that has been unpublished has no series, and a stream that
// was already in flight when it went away must not put it back in every scrape for
// the life of the process.
func (m *Metrics) lookupTunnel(name string) (*tunnelMetrics, bool) {
	m.mu.RLock()
	entry, ok := m.perTunnel[name]
	m.mu.RUnlock()
	return entry, ok
}

// recordStream adds one finished stream to the global and per-tunnel counters.
func (m *Metrics) recordStream(tunnel string, toClient, fromClient int64) {
	m.streamsTotal.Add(1)
	m.bytesToClients.Add(toClient)
	m.bytesFromClients.Add(fromClient)

	if entry, ok := m.lookupTunnel(tunnel); ok {
		entry.streamsTotal.Add(1)
		entry.bytesToClients.Add(toClient)
		entry.bytesFromClients.Add(fromClient)
	}
}

// recordDatagramSession counts the bytes a relayed datagram session carried. It is
// separate from recordStream because a datagram session is not a stream, but the byte
// counters have to include it: a udp or sudp proxy moves real traffic, and a
// "bytes from clients" total that ignores it under-reports the data path.
func (m *Metrics) recordDatagramSession(tunnel string, toClient, fromClient int64) {
	m.bytesToClients.Add(toClient)
	m.bytesFromClients.Add(fromClient)

	if entry, ok := m.lookupTunnel(tunnel); ok {
		entry.bytesToClients.Add(toClient)
		entry.bytesFromClients.Add(fromClient)
	}
}

// streamOpened and streamClosed track concurrency, globally and per tunnel.
//
// streamOpened returns the series the stream is counted against and streamClosed
// takes that same entry back. Pairing on the entry rather than on the name is
// what keeps the gauge right across a withdraw: the close of a stream that was
// open when the name was withdrawn, and that ends after the name is registered
// again, would otherwise be subtracted from the new generation's entry, which
// never counted it, and the gauge would report -1 for the rest of the process.
//
// It creates the series when it is missing, which is how a proxy whose series was
// pruned at the cap gets it back. openStreamWith refuses a member that is already
// closed before it counts, which narrows the window in which a withdrawn name gets
// its series back from the whole dial to the few instructions between that check
// and this call — narrowed, not closed: a member removed inside that window still
// recreates the series for the one stream it was asked for.
func (m *Metrics) streamOpened(tunnel string) *tunnelMetrics {
	m.streamsActive.Add(1)
	entry := m.tunnel(tunnel)
	entry.streamsActive.Add(1)
	return entry
}

// streamClosed books the end of a stream against the entry streamOpened handed
// out for it.
func (m *Metrics) streamClosed(entry *tunnelMetrics) {
	m.streamsActive.Add(-1)
	if entry != nil {
		entry.streamsActive.Add(-1)
	}
}

// promLabel renders a label value the way the Prometheus text format accepts:
// backslash, double quote and newline are its only escapes, and a label value has
// to be valid UTF-8. Go's %q would emit \t and \xNN for other control bytes, which
// that parser rejects — one proxy name with such a byte would make the whole
// exposition unreadable and take every scrape with it. A name is not validated
// where it is registered (an SSH gateway exec command can carry any byte), so the
// same failure is closed from this side. The mapping has to be injective: two
// different names must render as two different label values, or one scrape
// carries two identical series lines and a compliant parser rejects the whole
// exposition — so a byte that does not decode as a rune is percent-encoded, and a
// literal '%' is escaped, so that neither can collide with another name's
// rendering.
func promLabel(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			// One invalid byte: an encoded U+FFFD decodes with size 3 and is a
			// legal label value, so only the size-1 case is replaced.
			b.WriteByte('%')
			b.WriteByte(hex[value[i]>>4])
			b.WriteByte(hex[value[i]&0xF])
			i++
			continue
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '%':
			// An unescaped '%' would let a legal name that spells an escape,
			// such as a%FFb, render as the encoding of a name with the raw
			// byte — two names, one label value.
			b.WriteString(`%%`)
		default:
			b.WriteString(value[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
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
	counter("aethertunnel_control_connections_total", "Control connections that completed the handshake.", m.controlAccepted.Load())
	counter("aethertunnel_control_rejected_total", "Control-port connections refused instead of accepted: capacity, ACL, rate limit, ban, a first frame that could not be read, an unusable first frame, or credentials that do not check out (refusals on the visitor entry point and on the SSH tunnel gateway entry are counted here too). Some of these are closed without an answer, so this is the series an alert on the port watches; the specific ones say why.", m.controlRejected.Load())
	counter("aethertunnel_auth_failures_total", "Authentication attempts that failed a credential check: a wrong token, a failed identity assertion, a failed key agreement, or a visitor's failed proof for a proxy's secret key.", m.authFailures.Load())
	counter("aethertunnel_connections_denied_by_acl_total", "Connections refused by allow/deny lists.", m.aclDenied.Load())
	counter("aethertunnel_connections_rate_limited_total", "Connections refused by the per-IP rate limit.", m.rateLimited.Load())
	counter("aethertunnel_sources_banned_total", "Sources banned after repeated authentication failures.", m.bans.Load())
	counter("aethertunnel_banned_connections_refused_total", "Connections refused because the source is banned.", m.banRefused.Load())
	counter("aethertunnel_visitors_denied_by_proxy_total", "Visitors refused by the server's policy for a name or by the proxy's own allow/deny lists.", m.visitorDenied.Load())
	counter("aethertunnel_unusable_request_frames_total", "Connections whose first frame was not a usable request: a payload that does not parse (an auth request, a visitor-connect, or a data-open), or a frame type that cannot start a connection.", m.unusableFrames.Load())
	counter("aethertunnel_handshake_failures_total", "Connections closed without an answer because no usable first frame could be read: a disguise, encryption or TLS mismatch that leaves the bytes undecodable, or a peer that closed or went quiet before sending one, which a TCP health check also does.", m.handshakeFailures.Load())
	counter("aethertunnel_dashboard_unauthorized_total", "Requests refused for a missing or wrong token on the dashboard listener or on a standalone metrics listener: every /api endpoint that needs one, and /metrics with a token the scraper did not present. Neither listener has a rate limit of its own, so this is the series that shows something is reaching for a surface it has no credential for.", m.dashboardRefused.Load())
	counter("aethertunnel_socks5_malformed_requests_total", "Connections on a socks5 endpoint that did not carry a usable SOCKS5 request.", m.socksMalformed.Load())
	counter("aethertunnel_streams_refused_while_draining_total", "Streams refused because the server was shutting down.", m.drainRefused.Load())
	counter("aethertunnel_socks5_requests_total", "SOCKS5 CONNECT requests served.", m.socksRequests.Load())
	counter("aethertunnel_socks5_udp_associations_total", "SOCKS5 UDP ASSOCIATE associations accepted.", m.socksUDPAssoc.Load())
	counter("aethertunnel_socks5_udp_datagrams_total", "SOCKS5 UDP datagrams relayed.", m.socksUDPDatagrams.Load())
	counter("aethertunnel_data_connections_total", "Data connections opened by clients.", m.dataConnections.Load())
	counter("aethertunnel_pool_parked_total", "Data connections parked by a client in advance, so a stream skips the dial (client.pool_count).", m.poolParked.Load())
	counter("aethertunnel_pool_used_total", "Streams served on a connection that was parked in advance.", m.poolUsed.Load())
	counter("aethertunnel_pool_missed_total", "Parked connections that were not usable when a stream needed one, so the stream was asked for over the control connection instead. A client that was restarted while its connections sat parked appears here.", m.poolMissed.Load())
	counter("aethertunnel_pool_refused_total", "Parked connections refused because the session already held the maximum.", m.poolRefused.Load())
	counter("aethertunnel_data_connections_unmatched_total", "Data connections whose data-open named a session or a stream the server was not waiting for: the session was unknown or had expired, or the visitor that asked for the stream had gone. A client that cannot reach its own local service is not one of these — it reports that itself, and the visitor waiting for the stream is told.", m.dataUnmatched.Load())
	gauge("aethertunnel_streams_active", "Tunnelled streams currently open.", m.streamsActive.Load())
	counter("aethertunnel_streams_total", "Tunnelled streams completed.", m.streamsTotal.Load())
	counter("aethertunnel_bytes_from_clients_total", "Bytes received from clients.", m.bytesFromClients.Load())
	counter("aethertunnel_bytes_to_clients_total", "Bytes sent to clients.", m.bytesToClients.Load())
	counter("aethertunnel_udp_datagrams_total", "UDP datagrams relayed by a udp or sudp proxy.", m.udpDatagrams.Load())
	gauge("aethertunnel_udp_sessions_active", "UDP visitor sessions currently tracked, which is what a udp proxy keeps per source address.", m.udpSessions.Load())
	counter("aethertunnel_udp_oversize_dropped_total", "UDP datagrams dropped for exceeding server.udp_packet_size.", m.udpOversize.Load())
	counter("aethertunnel_udp_sessions_limit_dropped_total",
		"UDP datagrams dropped because the proxy already tracks its maximum number of visitor source addresses.",
		m.udpSessionLimit.Load())
	counter("aethertunnel_http_requests_total", "Requests served by the shared virtual-host listener.", m.httpRequests.Load())
	counter("aethertunnel_p2p_punches_total", "Hole punching attempts started for xtcp proxies.", m.p2pPunches.Load())
	counter("aethertunnel_p2p_direct_total", "Hole punching attempts that produced a direct path.", m.p2pDirect.Load())
	counter("aethertunnel_p2p_relayed_total", "Hole punching attempts that fell back to the relayed path.", m.p2pRelayed.Load())
	// A server with no audit log publishes none of these: a permanent zero would
	// say the log is healthy when there is no log at all.
	if m.audit.Configured() {
		counter("aethertunnel_audit_write_failures_total", "Audit records whose first write attempt failed.", m.audit.Failures())
		counter("aethertunnel_audit_records_lost_total", "Audit records that could not be written at all.", m.audit.Lost())
		counter("aethertunnel_audit_records_recovered_total", "Audit records that landed after the file was reopened.", m.audit.Recovered())
	}

	// Snapshot the map under the read lock: tunnel() inserts concurrently,
	// and a bare lookup below would be a map read racing that write, which
	// is a process-fatal crash. The pointers the snapshot holds stay valid
	// even when forgetTunnel or pruneLocked drops the entry from the map,
	// so rendering through them needs no further locking.
	m.mu.RLock()
	names := make([]string, 0, len(m.perTunnel))
	snapshot := make(map[string]*tunnelMetrics, len(m.perTunnel))
	for name := range m.perTunnel {
		names = append(names, name)
		snapshot[name] = m.perTunnel[name]
	}
	m.mu.RUnlock()
	sort.Strings(names)

	if len(names) > 0 {
		writeHelp("aethertunnel_tunnel_streams_active", "Open streams per tunnel.", "gauge")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_streams_active{tunnel=%s} %d\n", promLabel(name), snapshot[name].streamsActive.Load())
		}
		writeHelp("aethertunnel_tunnel_streams_total", "Completed streams per tunnel.", "counter")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_streams_total{tunnel=%s} %d\n", promLabel(name), snapshot[name].streamsTotal.Load())
		}
		writeHelp("aethertunnel_tunnel_bytes_total", "Bytes relayed per tunnel and direction.", "counter")
		for _, name := range names {
			entry := snapshot[name]
			fmt.Fprintf(&b, "aethertunnel_tunnel_bytes_total{tunnel=%s,direction=\"from_client\"} %d\n", promLabel(name), entry.bytesFromClients.Load())
			fmt.Fprintf(&b, "aethertunnel_tunnel_bytes_total{tunnel=%s,direction=\"to_client\"} %d\n", promLabel(name), entry.bytesToClients.Load())
		}
		writeHelp("aethertunnel_tunnel_http_requests_total", "HTTP requests served per tunnel.", "counter")
		for _, name := range names {
			fmt.Fprintf(&b, "aethertunnel_tunnel_http_requests_total{tunnel=%s} %d\n", promLabel(name), snapshot[name].httpRequests.Load())
		}
	}

	return b.String()
}
