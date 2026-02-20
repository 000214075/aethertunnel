package visualization

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Metrics 实时指标
type Metrics struct {
	Connections      ConnectionMetrics      `json:"connections"`
	Traffic          TrafficMetrics        `json:"traffic"`
	Performance      PerformanceMetrics    `json:"performance"`
	Security         SecurityMetrics       `json:"security"`
	Network          NetworkMetrics        `json:"network"`
	LastUpdated      time.Time             `json:"last_updated"`
}

// ConnectionMetrics 连接指标
type ConnectionMetrics struct {
	Total       int            `json:"total"`
	Active      int            `json:"active"`
	ByProtocol  map[string]int `json:"by_protocol"`
	ByClient    map[string]int `json:"by_client"`
	ByProxy     map[string]int `json:"by_proxy"`
	Peak        int            `json:"peak"`
	PeakTime    time.Time      `json:"peak_time"`
}

// TrafficMetrics 流量指标
type TrafficMetrics struct {
	BytesIn      int64         `json:"bytes_in"`
	BytesOut     int64         `json:"bytes_out"`
	PacketsIn    int64         `json:"packets_in"`
	PacketsOut   int64         `json:"packets_out"`
	BytesPerSec  float64       `json:"bytes_per_sec"`
	PacketsPerSec float64      `json:"packets_per_sec"`
	ByProtocol   map[string]int64 `json:"by_protocol"`
	ByProxy      map[string]int64 `json:"by_proxy"`
	TopClients   []ClientStats `json:"top_clients"`
}

// PerformanceMetrics 性能指标
type PerformanceMetrics struct {
	LatencyAvg     time.Duration `json:"latency_avg"`
	LatencyP95     time.Duration `json:"latency_p95"`
	LatencyP99     time.Duration `json:"latency_p99"`
	JitterAvg      time.Duration `json:"jitter_avg"`
	PacketLoss     float64       `json:"packet_loss"`
	Throughput     float64       `json:"throughput"`
	ErrorRate      float64       `json:"error_rate"`
	ResponseTime   time.Duration `json:"response_time"`
}

// SecurityMetrics 安全指标
type SecurityMetrics struct {
	AuthAttempts   int               `json:"auth_attempts"`
	AuthFailures   int               `json:"auth_failures"`
	AuthSuccess    int               `json:"auth_success"`
	BlockedIPs     int               `json:"blocked_ips"`
	SuspiciousActs int               `json:"suspicious_acts"`
	ByEventType   map[string]int    `json:"by_event_type"`
	ByIP          map[string]int    `json:"by_ip"`
	RecentEvents  []SecurityEvent   `json:"recent_events"`
}

// NetworkMetrics 网络指标
type NetworkMetrics struct {
	ProtocolQuality map[string]QualityMetrics `json:"protocol_quality"`
	CurrentProtocol string                   `json:"current_protocol"`
	Switches        int                       `json:"switches"`
	LastError       string                    `json:"last_error"`
}

// QualityMetrics 质量指标
type QualityMetrics struct {
	Latency    time.Duration `json:"latency"`
	Bandwidth  int64         `json:"bandwidth"`
	PacketLoss float64       `json:"packet_loss"`
	Jitter     time.Duration `json:"jitter"`
	Score      float64       `json:"score"`
}

// ClientStats 客户端统计
type ClientStats struct {
	ClientID    string  `json:"client_id"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
	Connections int     `json:"connections"`
	LastActive  time.Time `json:"last_active"`
}

// SecurityEvent 安全事件
type SecurityEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	SourceIP  string    `json:"source_ip"`
	Details   map[string]interface{} `json:"details"`
}

// TrafficSnapshot 流量快照
type TrafficSnapshot struct {
	Timestamp time.Time `json:"timestamp"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	PacketsIn int64     `json:"packets_in"`
	PacketsOut int64    `json:"packets_out"`
}

// MetricsCollector 指标收集器
type MetricsCollector struct {
	mu                sync.RWMutex
	metrics           Metrics
	history           []TrafficSnapshot
	maxHistory        int
	eventBuffer       []SecurityEvent
	maxEventBuffer    int
	snapshotInterval  time.Duration
	snapshotTicker    *time.Ticker
	updateCallbacks  []func(Metrics)
}

// NewMetricsCollector 创建指标收集器
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		metrics: Metrics{
			Connections: ConnectionMetrics{
				ByProtocol: make(map[string]int),
				ByClient:   make(map[string]int),
				ByProxy:    make(map[string]int),
			},
			Traffic: TrafficMetrics{
				ByProtocol: make(map[string]int64),
				ByProxy:    make(map[string]int64),
				TopClients: make([]ClientStats, 0),
			},
			Network: NetworkMetrics{
				ProtocolQuality: make(map[string]QualityMetrics),
			},
		},
		history:          make([]TrafficSnapshot, 0, 100),
		maxHistory:       100,
		eventBuffer:      make([]SecurityEvent, 0, 50),
		maxEventBuffer:   50,
		snapshotInterval: 5 * time.Second,
	}
}

// Start 启动收集器
func (c *MetricsCollector) Start() {
	c.snapshotTicker = time.NewTicker(c.snapshotInterval)
	go c.snapshotLoop()
}

// Stop 停止收集器
func (c *MetricsCollector) Stop() {
	if c.snapshotTicker != nil {
		c.snapshotTicker.Stop()
	}
}

// snapshotLoop 快照循环
func (c *MetricsCollector) snapshotLoop() {
	for range c.snapshotTicker.C {
		c.takeSnapshot()
		c.updateMetrics()
		c.notifyCallbacks()
	}
}

// takeSnapshot 获取快照
func (c *MetricsCollector) takeSnapshot() {
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot := TrafficSnapshot{
		Timestamp:  time.Now(),
		BytesIn:    c.metrics.Traffic.BytesIn,
		BytesOut:   c.metrics.Traffic.BytesOut,
		PacketsIn:   c.metrics.Traffic.PacketsIn,
		PacketsOut:  c.metrics.Traffic.PacketsOut,
	}

	c.history = append(c.history, snapshot)

	// 限制历史长度
	if len(c.history) > c.maxHistory {
		c.history = c.history[len(c.history)-c.maxHistory:]
	}
}

// updateMetrics 更新指标
func (c *MetricsCollector) updateMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.LastUpdated = time.Now()

	// 计算流量速率
	if len(c.history) >= 2 {
		last := c.history[len(c.history)-1]
		prev := c.history[len(c.history)-2]

		duration := last.Timestamp.Sub(prev.Timestamp).Seconds()
		if duration > 0 {
			c.metrics.Traffic.BytesPerSec = float64(last.BytesIn-prev.BytesIn) / duration
			c.metrics.Traffic.PacketsPerSec = float64(last.PacketsIn-prev.PacketsIn) / duration
		}
	}

	// 更新峰值连接数
	if c.metrics.Connections.Active > c.metrics.Connections.Peak {
		c.metrics.Connections.Peak = c.metrics.Connections.Active
		c.metrics.Connections.PeakTime = time.Now()
	}
}

// notifyCallbacks 通知回调
func (c *MetricsCollector) notifyCallbacks() {
	c.mu.RLock()
	metrics := c.metrics
	c.mu.RUnlock()

	for _, callback := range c.updateCallbacks {
		go callback(metrics)
	}
}

// RecordConnection 记录连接
func (c *MetricsCollector) RecordConnection(clientID, protocol, proxyName string, active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.Connections.Total++
	if active {
		c.metrics.Connections.Active++
		c.metrics.Connections.ByProtocol[protocol]++
		c.metrics.Connections.ByClient[clientID]++
		c.metrics.Connections.ByProxy[proxyName]++
	} else {
		c.metrics.Connections.Active--
	}
}

// RecordTraffic 记录流量
func (c *MetricsCollector) RecordTraffic(bytesIn, bytesOut, packetsIn, packetsOut int64, protocol, proxyName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.Traffic.BytesIn += bytesIn
	c.metrics.Traffic.BytesOut += bytesOut
	c.metrics.Traffic.PacketsIn += packetsIn
	c.metrics.Traffic.PacketsOut += packetsOut

	c.metrics.Traffic.ByProtocol[protocol] += bytesIn + bytesOut
	c.metrics.Traffic.ByProxy[proxyName] += bytesIn + bytesOut
}

// RecordPerformance 记录性能
func (c *MetricsCollector) RecordPerformance(latency, jitter time.Duration, error bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 简化的性能更新
	c.metrics.Performance.LatencyAvg = latency
	c.metrics.Performance.JitterAvg = jitter

	if error {
		c.metrics.Performance.ErrorRate += 0.01
	}
}

// RecordSecurityEvent 记录安全事件
func (c *MetricsCollector) RecordSecurityEvent(event SecurityEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	event.Timestamp = time.Now()

	c.metrics.Security.AuthAttempts++
	if event.Type == "auth_failure" {
		c.metrics.Security.AuthFailures++
	} else if event.Type == "auth_success" {
		c.metrics.Security.AuthSuccess++
	}

	if event.Severity == "high" || event.Severity == "critical" {
		c.metrics.Security.SuspiciousActs++
	}

	c.metrics.Security.ByEventType[event.Type]++
	if event.SourceIP != "" {
		c.metrics.Security.ByIP[event.SourceIP]++
	}

	c.eventBuffer = append(c.eventBuffer, event)
	c.metrics.Security.RecentEvents = append([]SecurityEvent{event}, c.metrics.Security.RecentEvents...)

	// 限制事件缓冲区
	if len(c.eventBuffer) > c.maxEventBuffer {
		c.eventBuffer = c.eventBuffer[1:]
	}
	if len(c.metrics.Security.RecentEvents) > 10 {
		c.metrics.Security.RecentEvents = c.metrics.Security.RecentEvents[:10]
	}
}

// RecordNetworkQuality 记录网络质量
func (c *MetricsCollector) RecordNetworkQuality(protocol string, quality QualityMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.Network.ProtocolQuality[protocol] = quality
}

// RecordProtocolSwitch 记录协议切换
func (c *MetricsCollector) RecordProtocolSwitch(newProtocol string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.Network.CurrentProtocol = newProtocol
	c.metrics.Network.Switches++
}

// GetMetrics 获取指标
func (c *MetricsCollector) GetMetrics() Metrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.metrics
}

// GetHistory 获取历史
func (c *MetricsCollector) GetHistory() []TrafficSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	history := make([]TrafficSnapshot, len(c.history))
	copy(history, c.history)
	return history
}

// RegisterUpdateCallback 注册更新回调
func (c *MetricsCollector) RegisterUpdateCallback(callback func(Metrics)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updateCallbacks = append(c.updateCallbacks, callback)
}

// VisualizationServer 可视化服务器
type VisualizationServer struct {
	collector *MetricsCollector
	server    *http.Server
	addr      string
	username  string
	password  string
}

// NewVisualizationServer 创建可视化服务器
func NewVisualizationServer(collector *MetricsCollector, addr, username, password string) *VisualizationServer {
	return &VisualizationServer{
		collector: collector,
		addr:      addr,
		username:  username,
		password:  password,
	}
}

// Start 启动服务器
func (s *VisualizationServer) Start() error {
	mux := http.NewServeMux()

	// API 路由
	mux.HandleFunc("/api/metrics", s.metricsHandler)
	mux.HandleFunc("/api/metrics/history", s.historyHandler)
	mux.HandleFunc("/api/events", s.eventsHandler)

	// 静态文件（简化）
	mux.HandleFunc("/", s.indexHandler)

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: s.authMiddleware(mux),
	}

	fmt.Printf("[Visualization] Starting server on %s\n", s.addr)
	return s.server.ListenAndServe()
}

// Stop 停止服务器
func (s *VisualizationServer) Stop() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// authMiddleware 认证中间件
func (s *VisualizationServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 简化的认证检查
		// 实际实现应该使用更安全的方式
		username, password, ok := r.BasicAuth()
		if !ok || username != s.username || password != s.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="AetherTunnel"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Unauthorized"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// metricsHandler 指标处理器
func (s *VisualizationServer) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	metrics := s.collector.GetMetrics()
	json.NewEncoder(w).Encode(metrics)
}

// historyHandler 历史处理器
func (s *VisualizationServer) historyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	history := s.collector.GetHistory()
	json.NewEncoder(w).Encode(history)
}

// eventsHandler 事件处理器
func (s *VisualizationServer) eventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	metrics := s.collector.GetMetrics()
	json.NewEncoder(w).Encode(metrics.Security.RecentEvents)
}

// indexHandler 首页处理器
func (s *VisualizationServer) indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	html := `<!DOCTYPE html>
<html>
<head>
    <title>AetherTunnel Dashboard</title>
    <meta charset="utf-8">
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; }
        .container { max-width: 1200px; margin: 0 auto; }
        .card { background: #f5f5f5; padding: 20px; margin: 10px 0; border-radius: 5px; }
        .metric { display: inline-block; margin: 10px; padding: 15px; background: #fff; border-radius: 5px; min-width: 150px; }
        .metric-label { color: #666; font-size: 12px; }
        .metric-value { font-size: 24px; font-weight: bold; color: #333; }
        h1 { color: #333; }
        h2 { color: #666; border-bottom: 2px solid #ddd; padding-bottom: 10px; }
        .btn { background: #007bff; color: white; padding: 10px 20px; border: none; border-radius: 5px; cursor: pointer; }
        .btn:hover { background: #0056b3; }
    </style>
</head>
<body>
    <div class="container">
        <h1>🚀 AetherTunnel Dashboard</h1>

        <div class="card">
            <h2>Real-time Metrics</h2>
            <div class="metric">
                <div class="metric-value" id="connections">0</div>
                <div class="metric-label">Active Connections</div>
            </div>
            <div class="metric">
                <div class="metric-value" id="bytes-in">0</div>
                <div class="metric-label">Bytes In (MB)</div>
            </div>
            <div class="metric">
                <div class="metric-value" id="bytes-out">0</div>
                <div class="metric-label">Bytes Out (MB)</div>
            </div>
            <div class="metric">
                <div class="metric-value" id="latency">0</div>
                <div class="metric-label">Avg Latency (ms)</div>
            </div>
            <div class="metric">
                <div class="metric-value" id="throughput">0</div>
                <div class="metric-label">Throughput (Mbps)</div>
            </div>
            <div class="metric">
                <div class="metric-value" id="protocol">TCP</div>
                <div class="metric-label">Protocol</div>
            </div>
        </div>

        <div class="card">
            <h2>Security Events</h2>
            <div id="events"></div>
        </div>

        <div class="card">
            <h2>Network Quality</h2>
            <div id="quality"></div>
        </div>

        <button class="btn" onclick="location.reload()">Refresh</button>
    </div>

    <script>
        async function updateMetrics() {
            try {
                const response = await fetch('/api/metrics');
                const data = await response.json();

                document.getElementById('connections').textContent = data.connections.active;
                document.getElementById('bytes-in').textContent = (data.traffic.bytes_in / 1024 / 1024).toFixed(2);
                document.getElementById('bytes-out').textContent = (data.traffic.bytes_out / 1024 / 1024).toFixed(2);
                document.getElementById('latency').textContent = data.performance.latency_avg;
                document.getElementById('throughput').textContent = (data.performance.throughput).toFixed(2);
                document.getElementById('protocol').textContent = data.network.current_protocol.toUpperCase();

                // 更新安全事件
                const eventsDiv = document.getElementById('events');
                let eventsHtml = '<ul>';
                data.security.recent_events.forEach(event => {
                    eventsHtml += '<li>' + event.type + ' - ' + event.message + '</li>';
                });
                eventsHtml += '</ul>';
                eventsDiv.innerHTML = eventsHtml;

                // 更新网络质量
                const qualityDiv = document.getElementById('quality');
                let qualityHtml = '<ul>';
                for (const [protocol, quality] of Object.entries(data.network.protocol_quality)) {
                    qualityHtml += '<li>' + protocol.toUpperCase() + ': ' + quality.score.toFixed(1) + '</li>';
                }
                qualityHtml += '</ul>';
                qualityDiv.innerHTML = qualityHtml;

            } catch (error) {
                console.error('Error updating metrics:', error);
            }
        }

        updateMetrics();
        setInterval(updateMetrics, 5000);
    </script>
</body>
</html>`

	w.Write([]byte(html))
}
