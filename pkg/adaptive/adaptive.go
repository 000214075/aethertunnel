package adaptive

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// ProtocolType 协议类型
type ProtocolType string

const (
	ProtocolTCP  ProtocolType = "tcp"
	ProtocolUDP  ProtocolType = "udp"
	ProtocolQUIC ProtocolType = "quic"
	ProtocolWS   ProtocolType = "websocket"
	ProtocolKCP  ProtocolType = "kcp"
)

// NetworkQuality 网络质量指标
type NetworkQuality struct {
	Latency       time.Duration // 延迟
	PacketLoss    float64       // 丢包率 (0-1)
	Bandwidth     int64         // 带宽 (bps)
	Jitter        time.Duration // 抖动
	RTT           time.Duration // 往返时间
	Connection    int           // 连接数
	LastUpdated   time.Time     // 最后更新时间
}

// ProtocolScore 协议评分
type ProtocolScore struct {
	Protocol  ProtocolType
	Score     float64 // 0-100
	Quality   NetworkQuality
	Timestamp time.Time
}

// AdaptiveProtocolManager 自适应协议管理器
type AdaptiveProtocolManager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	monitor    *NetworkMonitor
	scorer     *ProtocolScorer
	strategy   *AdaptiveStrategy
	current    ProtocolType
	protocols  map[ProtocolType]bool
	mu         sync.RWMutex
	updateChan chan ProtocolType
}

// NetworkMonitor 网络监控器
type NetworkMonitor struct {
	measurer  *NetworkMeasurer
	quality   map[ProtocolType]*NetworkQuality
	mu        sync.RWMutex
	interval  time.Duration
}

// NetworkMeasurer 网络测量器
type NetworkMeasurer struct {
	addrs     []string
	timeout   time.Duration
}

// ProtocolScorer 协议评分器
type ProtocolScorer struct {
	weights map[string]float64
}

// AdaptiveStrategy 自适应策略
type AdaptiveStrategy struct {
	mode          StrategyMode
	threshold     float64
	switchCooldown time.Duration
	lastSwitch    time.Time
}

// StrategyMode 策略模式
type StrategyMode string

const (
	ModeLatencyPriority  StrategyMode = "latency"  // 延迟优先
	ModeBandwidthPriority StrategyMode = "bandwidth" // 带宽优先
	ModeReliability     StrategyMode = "reliability" // 可靠性优先
	ModeBalanced        StrategyMode = "balanced" // 平衡模式
	ModeGame            StrategyMode = "game" // 游戏模式
)

// NewAdaptiveProtocolManager 创建自适应协议管理器
func NewAdaptiveProtocolManager(ctx context.Context) *AdaptiveProtocolManager {
	childCtx, cancel := context.WithCancel(ctx)

	return &AdaptiveProtocolManager{
		ctx:        childCtx,
		cancel:     cancel,
		monitor:    NewNetworkMonitor(),
		scorer:     NewProtocolScorer(),
		strategy:   NewAdaptiveStrategy(ModeBalanced),
		current:    ProtocolTCP,
		protocols:  make(map[ProtocolType]bool),
		updateChan: make(chan ProtocolType, 10),
	}
}

// SetSupportedProtocols 设置支持的协议
func (m *AdaptiveProtocolManager) SetSupportedProtocols(protocols []ProtocolType) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.protocols = make(map[ProtocolType]bool)
	for _, proto := range protocols {
		m.protocols[proto] = true
	}
}

// SetStrategy 设置策略
func (m *AdaptiveProtocolManager) SetStrategy(mode StrategyMode, threshold float64) {
	m.strategy = NewAdaptiveStrategy(mode)
	m.strategy.threshold = threshold
}

// Start 启动自适应协议管理
func (m *AdaptiveProtocolManager) Start() {
	// 启动网络监控
	go m.monitor.Run(m.ctx)

	// 启动自适应切换
	go m.adaptLoop()
}

// Stop 停止
func (m *AdaptiveProtocolManager) Stop() {
	m.cancel()
}

// adaptLoop 自适应循环
func (m *AdaptiveProtocolManager) adaptLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.evaluateAndSwitch()
		}
	}
}

// evaluateAndSwitch 评估并切换协议
func (m *AdaptiveProtocolManager) evaluateAndSwitch() {
	// 检查冷却时间
	if time.Since(m.strategy.lastSwitch) < m.strategy.switchCooldown {
		return
	}

	// 获取所有协议的评分
	scores := m.scorer.ScoreAll(m.monitor.GetQualities())

	// 找出最佳协议
	bestProto, bestScore := m.getBestProtocol(scores)

	// 检查是否需要切换
	currentScore := scores[m.current]
	if bestProto != m.current && bestScore.Score > currentScore.Score+10 {
		m.switchProtocol(bestProto)
	}
}

// getBestProtocol 获取最佳协议
func (m *AdaptiveProtocolManager) getBestProtocol(scores map[ProtocolType]*ProtocolScore) (ProtocolType, *ProtocolScore) {
	var bestProto ProtocolType
	var bestScore *ProtocolScore

	for proto, score := range scores {
		// 检查协议是否支持
		m.mu.RLock()
		supported := m.protocols[proto]
		m.mu.RUnlock()

		if !supported {
			continue
		}

		if bestScore == nil || score.Score > bestScore.Score {
			bestProto = proto
			bestScore = score
		}
	}

	return bestProto, bestScore
}

// switchProtocol 切换协议
func (m *AdaptiveProtocolManager) switchProtocol(newProto ProtocolType) {
	m.mu.Lock()
	oldProto := m.current
	m.current = newProto
	m.mu.Unlock()

	m.strategy.lastSwitch = time.Now()

	fmt.Printf("[Adaptive] Switched protocol: %s -> %s\n", oldProto, newProto)

	// 通知监听者
	select {
	case m.updateChan <- newProto:
	default:
		// Channel full, skip
	}
}

// GetCurrentProtocol 获取当前协议
func (m *AdaptiveProtocolManager) GetCurrentProtocol() ProtocolType {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// GetProtocolUpdateChan 获取协议更新通道
func (m *AdaptiveProtocolManager) GetProtocolUpdateChan() <-chan ProtocolType {
	return m.updateChan
}

// NewNetworkMonitor 创建网络监控器
func NewNetworkMonitor() *NetworkMonitor {
	return &NetworkMonitor{
		measurer: &NetworkMeasurer{
			addrs:   []string{"8.8.8.8:53", "1.1.1.1:53"},
			timeout: 2 * time.Second,
		},
		quality:  make(map[ProtocolType]*NetworkQuality),
		interval: 2 * time.Second,
	}
}

// Run 运行网络监控
func (m *NetworkMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// 初始化所有协议的质量
	m.quality[ProtocolTCP] = &NetworkQuality{}
	m.quality[ProtocolUDP] = &NetworkQuality{}
	m.quality[ProtocolQUIC] = &NetworkQuality{}
	m.quality[ProtocolWS] = &NetworkQuality{}
	m.quality[ProtocolKCP] = &NetworkQuality{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.measureAll()
		}
	}
}

// measureAll 测量所有协议
func (m *NetworkMonitor) measureAll() {
	// 测量 TCP
	m.measureProtocol(ProtocolTCP)

	// 测量 UDP
	m.measureProtocol(ProtocolUDP)

	// 测量 QUIC（简化）
	m.measureProtocol(ProtocolQUIC)

	// 测量 WebSocket（简化）
	m.measureProtocol(ProtocolWS)
}

// measureProtocol 测量单个协议
func (m *NetworkMonitor) measureProtocol(proto ProtocolType) {
	m.mu.Lock()
	defer m.mu.Unlock()

	quality := m.quality[proto]

	// 测量延迟（模拟）
	latency := m.measureLatency(proto)

	// 模拟丢包率（实际应该测量）
	packetLoss := m.estimatePacketLoss(proto)

	// 模拟带宽（实际应该测量）
	bandwidth := m.estimateBandwidth(proto)

	// 计算抖动
	jitter := m.estimateJitter(proto)

	// 更新质量指标
	quality.Latency = latency
	quality.PacketLoss = packetLoss
	quality.Bandwidth = bandwidth
	quality.Jitter = jitter
	quality.RTT = latency * 2
	quality.LastUpdated = time.Now()
}

// measureLatency 测量延迟
func (m *NetworkMonitor) measureLatency(proto ProtocolType) time.Duration {
	start := time.Now()

	// 根据协议类型模拟不同的延迟
	var baseLatency time.Duration
	switch proto {
	case ProtocolTCP:
		baseLatency = 30 * time.Millisecond
	case ProtocolUDP:
		baseLatency = 20 * time.Millisecond
	case ProtocolQUIC:
		baseLatency = 15 * time.Millisecond
	case ProtocolWS:
		baseLatency = 50 * time.Millisecond
	case ProtocolKCP:
		baseLatency = 25 * time.Millisecond
	default:
		baseLatency = 30 * time.Millisecond
	}

	// 添加随机抖动
	jitter := time.Duration(time.Now().UnixNano()%10) * time.Millisecond
	return baseLatency + jitter
}

// estimatePacketLoss 估算丢包率
func (m *NetworkMonitor) estimatePacketLoss(proto ProtocolType) float64 {
	// 根据协议类型模拟不同的丢包率
	switch proto {
	case ProtocolTCP:
		return 0.001 // 0.1%
	case ProtocolUDP:
		return 0.02 // 2%
	case ProtocolQUIC:
		return 0.005 // 0.5%
	case ProtocolWS:
		return 0.001 // 0.1%
	case ProtocolKCP:
		return 0.01 // 1%
	default:
		return 0.01
	}
}

// estimateBandwidth 估算带宽
func (m *NetworkMonitor) estimateBandwidth(proto ProtocolType) int64 {
	// 根据协议类型模拟不同的带宽（bps）
	switch proto {
	case ProtocolTCP:
		return 100 * 1024 * 1024 // 100 Mbps
	case ProtocolUDP:
		return 80 * 1024 * 1024 // 80 Mbps
	case ProtocolQUIC:
		return 120 * 1024 * 1024 // 120 Mbps
	case ProtocolWS:
		return 50 * 1024 * 1024 // 50 Mbps
	case ProtocolKCP:
		return 90 * 1024 * 1024 // 90 Mbps
	default:
		return 100 * 1024 * 1024
	}
}

// estimateJitter 估算抖动
func (m *NetworkMonitor) estimateJitter(proto ProtocolType) time.Duration {
	// 根据协议类型模拟不同的抖动
	switch proto {
	case ProtocolTCP:
		return 5 * time.Millisecond
	case ProtocolUDP:
		return 15 * time.Millisecond
	case ProtocolQUIC:
		return 8 * time.Millisecond
	case ProtocolWS:
		return 10 * time.Millisecond
	case ProtocolKCP:
		return 12 * time.Millisecond
	default:
		return 10 * time.Millisecond
	}
}

// GetQualities 获取所有协议的质量
func (m *NetworkMonitor) GetQualities() map[ProtocolType]*NetworkQuality {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[ProtocolType]*NetworkQuality)
	for k, v := range m.quality {
		// 复制
		quality := *v
		result[k] = &quality
	}
	return result
}

// NewProtocolScorer 创建协议评分器
func NewProtocolScorer() *ProtocolScorer {
	// 默认权重配置
	weights := map[string]float64{
		"latency":    0.3,  // 延迟权重
		"bandwidth":  0.25, // 带宽权重
		"reliability": 0.25, // 可靠性权重
		"jitter":     0.2,  // 抖动权重
	}

	return &ProtocolScorer{
		weights: weights,
	}
}

// SetWeights 设置权重
func (s *ProtocolScorer) SetWeights(weights map[string]float64) {
	s.weights = weights
}

// Score 评分单个协议
func (s *ProtocolScorer) Score(proto ProtocolType, quality *NetworkQuality) float64 {
	// 延迟评分（越低越好）
	latencyScore := s.scoreLatency(quality.Latency)

	// 带宽评分（越高越好）
	bandwidthScore := s.scoreBandwidth(quality.Bandwidth)

	// 可靠性评分（1 - 丢包率）
	reliabilityScore := (1 - quality.PacketLoss) * 100

	// 抖动评分（越低越好）
	jitterScore := s.scoreJitter(quality.Jitter)

	// 综合评分
	totalScore := latencyScore*s.weights["latency"] +
		bandwidthScore*s.weights["bandwidth"] +
		reliabilityScore*s.weights["reliability"] +
		jitterScore*s.weights["jitter"]

	return totalScore
}

// ScoreAll 评分所有协议
func (s *ProtocolScorer) ScoreAll(qualities map[ProtocolType]*NetworkQuality) map[ProtocolType]*ProtocolScore {
	scores := make(map[ProtocolType]*ProtocolScore)

	for proto, quality := range qualities {
		score := s.Score(proto, quality)
		scores[proto] = &ProtocolScore{
			Protocol:  proto,
			Score:     score,
			Quality:   *quality,
			Timestamp: time.Now(),
		}
	}

	return scores
}

// scoreLatency 延迟评分
func (s *ProtocolScorer) scoreLatency(latency time.Duration) float64 {
	// 延迟评分：
	// < 10ms: 100
	// < 50ms: 80
	// < 100ms: 60
	// < 200ms: 40
	// >= 200ms: 20

	ms := latency.Milliseconds()
	if ms < 10 {
		return 100
	} else if ms < 50 {
		return 80 + (50-ms)*0.5
	} else if ms < 100 {
		return 60 + (100-ms)*0.4
	} else if ms < 200 {
		return 40 + (200-ms)*0.2
	} else {
		return 20
	}
}

// scoreBandwidth 带宽评分
func (s *ProtocolScorer) scoreBandwidth(bandwidth int64) float64 {
	// 带宽评分：
	// > 100 Mbps: 100
	// > 50 Mbps: 80
	// > 20 Mbps: 60
	// > 10 Mbps: 40
	// <= 10 Mbps: 20

	mbps := bandwidth / (1024 * 1024)
	if mbps > 100 {
		return 100
	} else if mbps > 50 {
		return 80 + float64(mbps-50)*0.4
	} else if mbps > 20 {
		return 60 + float64(mbps-20)*0.666
	} else if mbps > 10 {
		return 40 + float64(mbps-10)*2
	} else {
		return 20
	}
}

// scoreJitter 抖动评分
func (s *ProtocolScorer) scoreJitter(jitter time.Duration) float64 {
	// 抖动评分：
	// < 5ms: 100
	// < 10ms: 80
	// < 20ms: 60
	// < 50ms: 40
	// >= 50ms: 20

	ms := jitter.Milliseconds()
	if ms < 5 {
		return 100
	} else if ms < 10 {
		return 80 + (10-ms)*4
	} else if ms < 20 {
		return 60 + (20-ms)*2
	} else if ms < 50 {
		return 40 + (50-ms)*0.666
	} else {
		return 20
	}
}

// NewAdaptiveStrategy 创建自适应策略
func NewAdaptiveStrategy(mode StrategyMode) *AdaptiveStrategy {
	return &AdaptiveStrategy{
		mode:          mode,
		threshold:     10.0,
		switchCooldown: 30 * time.Second,
		lastSwitch:    time.Time(),
	}
}

// GetMode 获取策略模式
func (s *AdaptiveStrategy) GetMode() StrategyMode {
	return s.mode
}

// GetRecommendedProtocol 获取推荐协议
func (s *AdaptiveStrategy) GetRecommendedProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	switch s.mode {
	case ModeLatencyPriority:
		return s.getLowestLatencyProtocol(scores)
	case ModeBandwidthPriority:
		return s.getHighestBandwidthProtocol(scores)
	case ModeReliability:
		return s.getMostReliableProtocol(scores)
	case ModeGame:
		return s.getGameOptimizedProtocol(scores)
	case ModeBalanced:
		return s.getBalancedProtocol(scores)
	default:
		return s.getBalancedProtocol(scores)
	}
}

// getLowestLatencyProtocol 获取最低延迟协议
func (s *AdaptiveStrategy) getLowestLatencyProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	var bestProto ProtocolType
	var lowestLatency time.Duration

	for proto, score := range scores {
		if lowestLatency == 0 || score.Quality.Latency < lowestLatency {
			bestProto = proto
			lowestLatency = score.Quality.Latency
		}
	}

	return bestProto
}

// getHighestBandwidthProtocol 获取最高带宽协议
func (s *AdaptiveStrategy) getHighestBandwidthProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	var bestProto ProtocolType
	var highestBandwidth int64

	for proto, score := range scores {
		if highestBandwidth == 0 || score.Quality.Bandwidth > highestBandwidth {
			bestProto = proto
			highestBandwidth = score.Quality.Bandwidth
		}
	}

	return bestProto
}

// getMostReliableProtocol 获取最可靠协议
func (s *AdaptiveStrategy) getMostReliableProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	var bestProto ProtocolType
	var lowestPacketLoss float64

	for proto, score := range scores {
		if lowestPacketLoss == 0 || score.Quality.PacketLoss < lowestPacketLoss {
			bestProto = proto
			lowestPacketLoss = score.Quality.PacketLoss
		}
	}

	return bestProto
}

// getGameOptimizedProtocol 获取游戏优化协议
func (s *AdaptiveStrategy) getGameOptimizedProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	// 游戏模式：优先 UDP，然后是 QUIC
	if score, ok := scores[ProtocolUDP]; ok {
		return score.Protocol
	}
	if score, ok := scores[ProtocolQUIC]; ok {
		return score.Protocol
	}

	// 否则选择低延迟协议
	return s.getLowestLatencyProtocol(scores)
}

// getBalancedProtocol 获取平衡协议
func (s *AdaptiveStrategy) getBalancedProtocol(scores map[ProtocolType]*ProtocolScore) ProtocolType {
	var bestProto ProtocolType
	var highestScore float64

	for proto, score := range scores {
		if highestScore == 0 || score.Score > highestScore {
			bestProto = proto
			highestScore = score.Score
		}
	}

	return bestProto
}

// AdaptiveConnection 自适应连接包装器
type AdaptiveConnection struct {
	protocols map[ProtocolType]net.Conn
	current   ProtocolType
	manager   *AdaptiveProtocolManager
	mu        sync.RWMutex
}

// NewAdaptiveConnection 创建自适应连接
func NewAdaptiveConnection(manager *AdaptiveProtocolManager) *AdaptiveConnection {
	return &AdaptiveConnection{
		protocols: make(map[ProtocolType]net.Conn),
		current:   manager.GetCurrentProtocol(),
		manager:   manager,
	}
}

// AddProtocol 添加协议连接
func (c *AdaptiveConnection) AddProtocol(proto ProtocolType, conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.protocols[proto] = conn
}

// GetCurrent 获取当前连接
func (c *AdaptiveConnection) GetCurrent() net.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.protocols[c.current]
}

// Switch 切换协议
func (c *AdaptiveConnection) Switch(newProto ProtocolType) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.protocols[newProto]; !ok {
		return fmt.Errorf("protocol %s not available", newProto)
	}

	oldProto := c.current
	c.current = newProto

	fmt.Printf("[AdaptiveConnection] Switched: %s -> %s\n", oldProto, newProto)
	return nil
}

// Write 写入数据
func (c *AdaptiveConnection) Write(p []byte) (int, error) {
	conn := c.GetCurrent()
	if conn == nil {
		return 0, fmt.Errorf("no active connection")
	}
	return conn.Write(p)
}

// Read 读取数据
func (c *AdaptiveConnection) Read(p []byte) (int, error) {
	conn := c.GetCurrent()
	if conn == nil {
		return 0, fmt.Errorf("no active connection")
	}
	return conn.Read(p)
}

// Close 关闭所有连接
func (c *AdaptiveConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	for _, conn := range c.protocols {
		if conn != nil {
			if err := conn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
