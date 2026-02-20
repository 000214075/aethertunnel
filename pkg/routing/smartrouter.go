package routing

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// RouteStrategy 路由策略
type RouteStrategy string

const (
	StrategyLatency    RouteStrategy = "latency"    // 延迟优先
	StrategyBandwidth  RouteStrategy = "bandwidth"  // 带宽优先
	StrategyReliability RouteStrategy = "reliability" // 可靠性优先
	StrategyCost       RouteStrategy = "cost"       // 成本优先
	StrategyRoundRobin RouteStrategy = "roundrobin" // 轮询
	StrategyLeastConn  RouteStrategy = "leastconn"  // 最少连接
)

// RouteTarget 路由目标
type RouteTarget struct {
	ID            string        `json:"id"`
	Address       string        `json:"address"`
	Port          int           `json:"port"`
	Weight        int           `json:"weight"`
	Active        bool          `json:"active"`
	HealthScore   float64       `json:"health_score"`
	Latency       time.Duration `json:"latency"`
	Bandwidth     int64         `json:"bandwidth"`
	PacketLoss    float64       `json:"packet_loss"`
	ActiveConn    int           `json:"active_conn"`
	LastError     string        `json:"last_error"`
	LastErrorTime time.Time     `json:"last_error_time"`
	HealthChecks  int           `json:"health_checks"`
	Failures      int           `json:"failures"`
}

// RouteRule 路由规则
type RouteRule struct {
	Name        string                 `json:"name"`
	Strategy    RouteStrategy          `json:"strategy"`
	Targets     []*RouteTarget         `json:"targets"`
	Conditions  map[string]interface{} `json:"conditions"`
	Priority    int                    `json:"priority"`
	Enabled     bool                   `json:"enabled"`
}

// RouteDecision 路由决策
type RouteDecision struct {
	TargetID  string        `json:"target_id"`
	Address   string        `json:"address"`
	Port      int           `json:"port"`
	Score     float64       `json:"score"`
	Strategy  RouteStrategy `json:"strategy"`
	Timestamp time.Time     `json:"timestamp"`
}

// HealthChecker 健康检查器
type HealthChecker struct {
	checkInterval time.Duration
	timeout       time.Duration
	healthyThreshold int
	unhealthyThreshold int
}

// SmartRouter 智能路由器
type SmartRouter struct {
	ctx          context.Context
	cancel       context.CancelFunc
	rules        map[string]*RouteRule
	activeRule   string
	decisions    map[string]*RouteDecision
	metrics      *RouterMetrics
	checker      *HealthChecker
	mu           sync.RWMutex
	updateChan   chan *RouteDecision
}

// RouterMetrics 路由指标
type RouterMetrics struct {
	TotalDecisions    int64                         `json:"total_decisions"`
	ByTarget         map[string]int64               `json:"by_target"`
	ByStrategy       map[string]int64               `json:"by_strategy"`
	Switches         int64                         `json:"switches"`
	Failovers        int64                         `json:"failovers"`
	AvgDecisionTime  time.Duration                 `json:"avg_decision_time"`
	AvgLatency       map[string]time.Duration       `json:"avg_latency"`
	TargetHealth     map[string]*TargetHealthMetrics `json:"target_health"`
}

// TargetHealthMetrics 目标健康指标
type TargetHealthMetrics struct {
	SuccessRate  float64       `json:"success_rate"`
	AvgLatency   time.Duration `json:"avg_latency"`
	Uptime       float64       `json:"uptime"`
	LastSuccess  time.Time     `json:"last_success"`
	LastFailure  time.Time     `json:"last_failure"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
}

// NewSmartRouter 创建智能路由器
func NewSmartRouter(ctx context.Context) *SmartRouter {
	childCtx, cancel := context.WithCancel(ctx)

	return &SmartRouter{
		ctx:        childCtx,
		cancel:     cancel,
		rules:      make(map[string]*RouteRule),
		decisions:  make(map[string]*RouteDecision),
		metrics: &RouterMetrics{
			ByTarget:      make(map[string]int64),
			ByStrategy:    make(map[string]int64),
			AvgLatency:    make(map[string]time.Duration),
			TargetHealth:  make(map[string]*TargetHealthMetrics),
		},
		checker: &HealthChecker{
			checkInterval:     10 * time.Second,
			timeout:           5 * time.Second,
			healthyThreshold:   3,
			unhealthyThreshold: 3,
		},
		updateChan: make(chan *RouteDecision, 100),
	}
}

// AddRule 添加路由规则
func (r *SmartRouter) AddRule(rule *RouteRule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.rules[rule.Name]; ok {
		return fmt.Errorf("rule %s already exists", rule.Name)
	}

	// 初始化目标健康指标
	for _, target := range rule.Targets {
		r.metrics.TargetHealth[target.ID] = &TargetHealthMetrics{
			Uptime: 100,
		}
	}

	r.rules[rule.Name] = rule

	// 如果是第一个规则，设为活动规则
	if r.activeRule == "" {
		r.activeRule = rule.Name
	}

	return nil
}

// RemoveRule 移除路由规则
func (r *SmartRouter) RemoveRule(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.rules[name]; !ok {
		return fmt.Errorf("rule %s not found", name)
	}

	delete(r.rules, name)

	// 如果删除的是活动规则，切换到其他规则
	if r.activeRule == name {
		for ruleName := range r.rules {
			r.activeRule = ruleName
			break
		}
	}

	return nil
}

// SetActiveRule 设置活动规则
func (r *SmartRouter) SetActiveRule(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.rules[name]; !ok {
		return fmt.Errorf("rule %s not found", name)
	}

	r.activeRule = name
	return nil
}

// Route 路由决策
func (r *SmartRouter) Route() (*RouteDecision, error) {
	startTime := time.Now()

	r.mu.RLock()
	rule := r.rules[r.activeRule]
	r.mu.RUnlock()

	if rule == nil {
		return nil, fmt.Errorf("no active rule")
	}

	// 过滤可用目标
	availableTargets := r.filterAvailableTargets(rule.Targets)
	if len(availableTargets) == 0 {
		return nil, fmt.Errorf("no available targets")
	}

	// 根据策略选择目标
	target := r.selectTarget(rule.Strategy, availableTargets)

	decision := &RouteDecision{
		TargetID:  target.ID,
		Address:   target.Address,
		Port:      target.Port,
		Score:     target.HealthScore,
		Strategy:  rule.Strategy,
		Timestamp: time.Now(),
	}

	// 记录决策
	r.recordDecision(decision, time.Since(startTime))

	// 通知更新
	select {
	case r.updateChan <- decision:
	default:
	}

	return decision, nil
}

// filterAvailableTargets 过滤可用目标
func (r *SmartRouter) filterAvailableTargets(targets []*RouteTarget) []*RouteTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	available := make([]*RouteTarget, 0, len(targets))

	for _, target := range targets {
		if target.Active && r.isTargetHealthy(target.ID) {
			available = append(available, target)
		}
	}

	return available
}

// isTargetHealthy 检查目标是否健康
func (r *SmartRouter) isTargetHealthy(targetID string) bool {
	health, ok := r.metrics.TargetHealth[targetID]
	if !ok {
		return true
	}

	// 检查连续失败次数
	if health.ConsecutiveFailures >= r.checker.unhealthyThreshold {
		return false
	}

	// 检查成功率
	if health.SuccessRate < 0.5 {
		return false
	}

	return true
}

// selectTarget 根据策略选择目标
func (r *SmartRouter) selectTarget(strategy RouteStrategy, targets []*RouteTarget) *RouteTarget {
	switch strategy {
	case StrategyLatency:
		return r.selectByLatency(targets)
	case StrategyBandwidth:
		return r.selectByBandwidth(targets)
	case StrategyReliability:
		return r.selectByReliability(targets)
	case StrategyLeastConn:
		return r.selectByLeastConn(targets)
	case StrategyRoundRobin:
		return r.selectRoundRobin(targets)
	case StrategyCost:
		return r.selectByCost(targets)
	default:
		return r.selectByReliability(targets)
	}
}

// selectByLatency 按延迟选择
func (r *SmartRouter) selectByLatency(targets []*RouteTarget) *RouteTarget {
	var best *RouteTarget
	minLatency := time.Duration(1<<63 - 1)

	for _, target := range targets {
		if target.Latency < minLatency {
			best = target
			minLatency = target.Latency
		}
	}

	return best
}

// selectByBandwidth 按带宽选择
func (r *SmartRouter) selectByBandwidth(targets []*RouteTarget) *RouteTarget {
	var best *RouteTarget
	maxBandwidth := int64(0)

	for _, target := range targets {
		if target.Bandwidth > maxBandwidth {
			best = target
			maxBandwidth = target.Bandwidth
		}
	}

	return best
}

// selectByReliability 按可靠性选择
func (r *SmartRouter) selectByReliability(targets []*RouteTarget) *RouteTarget {
	var best *RouteTarget
	maxScore := float64(0)

	for _, target := range targets {
		if target.HealthScore > maxScore {
			best = target
			maxScore = target.HealthScore
		}
	}

	return best
}

// selectByLeastConn 按最少连接选择
func (r *SmartRouter) selectByLeastConn(targets []*RouteTarget) *RouteTarget {
	var best *RouteTarget
	minConn := int(1<<31 - 1)

	for _, target := range targets {
		if target.ActiveConn < minConn {
			best = target
			minConn = target.ActiveConn
		}
	}

	return best
}

// selectRoundRobin 轮询选择
func (r *SmartRouter) selectRoundRobin(targets []*RouteTarget) *RouteTarget {
	// 简化的轮询实现
	// 实际应该维护一个索引
	return r.selectByLeastConn(targets)
}

// selectByCost 按成本选择（简化为轮询）
func (r *SmartRouter) selectByCost(targets []*RouteTarget) *RouteTarget {
	return r.selectRoundRobin(targets)
}

// recordDecision 记录路由决策
func (r *SmartRouter) recordDecision(decision *RouteDecision, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.metrics.TotalDecisions++
	r.metrics.ByTarget[decision.TargetID]++
	r.metrics.ByStrategy[string(decision.Strategy)]++

	// 更新平均决策时间
	if r.metrics.TotalDecisions == 1 {
		r.metrics.AvgDecisionTime = duration
	} else {
		r.metrics.AvgDecisionTime = (r.metrics.AvgDecisionTime*time.Duration(r.metrics.TotalDecisions-1) + duration) / time.Duration(r.metrics.TotalDecisions)
	}
}

// UpdateTargetHealth 更新目标健康状态
func (r *SmartRouter) UpdateTargetHealth(targetID string, success bool, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	health, ok := r.metrics.TargetHealth[targetID]
	if !ok {
		health = &TargetHealthMetrics{
			Uptime: 100,
		}
		r.metrics.TargetHealth[targetID] = health
	}

	health.HealthChecks++

	if success {
		health.LastSuccess = time.Now()
		health.ConsecutiveFailures = 0

		// 更新平均延迟
		if health.AvgLatency == 0 {
			health.AvgLatency = latency
		} else {
			health.AvgLatency = (health.AvgLatency + latency) / 2
		}
	} else {
		health.LastFailure = time.Now()
		health.ConsecutiveFailures++
	}

	// 计算成功率
	successCount := health.HealthChecks - health.ConsecutiveFailures
	health.SuccessRate = float64(successCount) / float64(health.HealthChecks)

	// 更新路由规则中的目标延迟
	for _, rule := range r.rules {
		for _, target := range rule.Targets {
			if target.ID == targetID {
				target.Latency = latency
				target.HealthScore = r.calculateHealthScore(target)
			}
		}
	}
}

// calculateHealthScore 计算健康分数
func (r *SmartRouter) calculateHealthScore(target *RouteTarget) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	health := r.metrics.TargetHealth[target.ID]
	if health == nil {
		return 100
	}

	// 综合分数 = 成功率 * 40 + 延迟分数 * 30 + 带宽分数 * 30
	successScore := health.SuccessRate * 40

	// 延迟分数（越低越好）
	var latencyScore float64
	if target.Latency < 50*time.Millisecond {
		latencyScore = 30
	} else if target.Latency < 100*time.Millisecond {
		latencyScore = 20
	} else if target.Latency < 200*time.Millisecond {
		latencyScore = 10
	} else {
		latencyScore = 0
	}

	// 带宽分数（越高越好）
	var bandwidthScore float64
	if target.Bandwidth > 100*1024*1024 {
		bandwidthScore = 30
	} else if target.Bandwidth > 50*1024*1024 {
		bandwidthScore = 20
	} else if target.Bandwidth > 10*1024*1024 {
		bandwidthScore = 10
	} else {
		bandwidthScore = 0
	}

	return successScore + latencyScore + bandwidthScore
}

// GetMetrics 获取路由指标
func (r *SmartRouter) GetMetrics() *RouterMetrics {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 深拷贝指标
	metrics := &RouterMetrics{
		TotalDecisions:   r.metrics.TotalDecisions,
		ByTarget:        make(map[string]int64),
		ByStrategy:      make(map[string]int64),
		Switches:        r.metrics.Switches,
		Failovers:       r.metrics.Failovers,
		AvgDecisionTime: r.metrics.AvgDecisionTime,
		AvgLatency:      make(map[string]time.Duration),
		TargetHealth:    make(map[string]*TargetHealthMetrics),
	}

	for k, v := range r.metrics.ByTarget {
		metrics.ByTarget[k] = v
	}
	for k, v := range r.metrics.ByStrategy {
		metrics.ByStrategy[k] = v
	}
	for k, v := range r.metrics.AvgLatency {
		metrics.AvgLatency[k] = v
	}
	for k, v := range r.metrics.TargetHealth {
		metrics.TargetHealth[k] = &TargetHealthMetrics{
			SuccessRate:  v.SuccessRate,
			AvgLatency:   v.AvgLatency,
			Uptime:       v.Uptime,
			LastSuccess:  v.LastSuccess,
			LastFailure:  v.LastFailure,
			ConsecutiveFailures: v.ConsecutiveFailures,
		}
	}

	return metrics
}

// StartHealthChecks 启动健康检查
func (r *SmartRouter) StartHealthChecks() {
	go r.healthCheckLoop()
}

// healthCheckLoop 健康检查循环
func (r *SmartRouter) healthCheckLoop() {
	ticker := time.NewTicker(r.checker.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.performHealthChecks()
		}
	}
}

// performHealthChecks 执行健康检查
func (r *SmartRouter) performHealthChecks() {
	r.mu.RLock()
	targets := make([]*RouteTarget, 0)
	for _, rule := range r.rules {
		targets = append(targets, rule.Targets...)
	}
	r.mu.RUnlock()

	for _, target := range targets {
		go r.checkTarget(target)
	}
}

// checkTarget 检查单个目标
func (r *SmartRouter) checkTarget(target *RouteTarget) {
	startTime := time.Now()

	// 尝试连接
	address := fmt.Sprintf("%s:%d", target.Address, target.Port)
	conn, err := net.DialTimeout("tcp", address, r.checker.timeout)
	latency := time.Since(startTime)

	if err != nil {
		r.UpdateTargetHealth(target.ID, false, latency)
		return
	}

	defer conn.Close()

	// 计算带宽（简化）
	bandwidth := r.measureBandwidth(conn, target)

	// 更新指标
	r.mu.Lock()
	target.Bandwidth = bandwidth
	target.Latency = latency
	target.HealthScore = r.calculateHealthScore(target)
	r.mu.Unlock()

	r.UpdateTargetHealth(target.ID, true, latency)
}

// measureBandwidth 测量带宽（简化）
func (r *SmartRouter) measureBandwidth(conn net.Conn, target *RouteTarget) int64 {
	// 简化的带宽测量
	// 实际实现应该发送测试数据
	return 100 * 1024 * 1024 // 100 Mbps
}

// GetUpdateChan 获取更新通道
func (r *SmartRouter) GetUpdateChan() <-chan *RouteDecision {
	return r.updateChan
}
