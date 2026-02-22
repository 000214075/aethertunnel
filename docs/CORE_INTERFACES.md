# AetherTunnel 核心模块接口规范

## 📋 文档说明

**版本**: v1.0.2
**更新日期**: 2026-02-22
**目标**: 定义AetherTunnel核心模块的统一接口规范

---

## 🎯 设计原则

### 1. 接口隔离原则（ISP）

每个接口只定义一个职责，避免臃肿的接口。

### 2. 依赖倒置原则（DIP）

高层模块不应依赖低层模块，都应依赖抽象。

### 3. 里氏替换原则（LSP）

子类可以完全替换父类，而不会影响程序正确性。

### 4. 开闭原则（OCP）

对扩展开放，对修改关闭。

### 5. 单一职责原则（SRP）

每个类只有一个改变的理由。

---

## 📐 接口层次结构

```
┌─────────────────────────────────────────────────────────────┐
│                   Core Infrastructure                        │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │  Component   │  │  Plugin      │  │  Middleware  │      │
│  │  Interface   │  │  Interface   │  │  Interface   │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
├─────────────────────────────────────────────────────────────┤
│                      Application Layer                       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │   Proxy      │  │   Dashboard  │  │   CLI        │      │
│  │   Interface  │  │   Interface  │  │   Interface  │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
├─────────────────────────────────────────────────────────────┤
│                     Innovation Layer                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │   Traffic    │  │   Adaptive   │  │   Smart      │      │
│  │   Obfuscator │  │   Protocol   │  │   Router     │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
├─────────────────────────────────────────────────────────────┤
│                      Security Layer                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │   PQC        │  │   mTLS       │  │  Zero-Knowledge│    │
│  │   Encryption │  │   Auth       │  │  Proof       │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
├─────────────────────────────────────────────────────────────┤
│                    Transport Layer                           │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │   QUIC       │  │   MPTCP      │  │  WebSocket   │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
└─────────────────────────────────────────────────────────────┘
```

---

## 🏗️ 1. Component 接口（统一组件接口）

### 1.1 接口定义

```go
// pkg/core/component.go

package core

import (
    "context"
    "io"
    "time"
)

// Component 定义所有核心组件的统一接口
type Component interface {
    // Lifecycle 生命周期管理
    Init(ctx context.Context) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    // Health 健康检查
    HealthCheck(ctx context.Context) (HealthStatus, error)

    // Metadata 元数据
    Name() string
    Version() string
    Description() string

    // Metrics 指标
    Metrics() Metrics
}

// HealthStatus 表示组件健康状态
type HealthStatus struct {
    Status      string    `json:"status"`      // healthy, degraded, unhealthy
    Timestamp   time.Time `json:"timestamp"`
    Latency     time.Duration `json:"latency,omitempty"`
    Message     string    `json:"message,omitempty"`
    Details     map[string]interface{} `json:"details,omitempty"`
}

// Metrics 组件指标
type Metrics struct {
    StartTime   time.Time   `json:"start_time"`
    Uptime      time.Duration `json:"uptime"`
    Connections int64       `json:"connections"`
    BytesIn     int64       `json:"bytes_in"`
    BytesOut    int64       `json:"bytes_out"`
    Errors      int64       `json:"errors"`
    Warnings    int64       `json:"warnings"`
}
```

### 1.2 使用示例

```go
// 实现Component接口
type MyComponent struct {
    name    string
    version string
    metrics Metrics
    started bool
}

func (c *MyComponent) Init(ctx context.Context) error {
    // 初始化逻辑
    return nil
}

func (c *MyComponent) Start(ctx context.Context) error {
    if c.started {
        return nil
    }

    // 启动逻辑
    c.started = true
    return nil
}

func (c *MyComponent) Stop(ctx context.Context) error {
    if !c.started {
        return nil
    }

    // 停止逻辑
    c.started = false
    return nil
}

func (c *MyComponent) HealthCheck(ctx context.Context) (HealthStatus, error) {
    return HealthStatus{
        Status:    "healthy",
        Timestamp: time.Now(),
    }, nil
}

func (c *MyComponent) Name() string {
    return c.name
}

func (c *MyComponent) Version() string {
    return c.version
}

func (c *MyComponent) Description() string {
    return "My custom component"
}

func (c *MyComponent) Metrics() Metrics {
    return c.metrics
}
```

---

## 🔌 2. Plugin 接口（插件系统）

### 2.1 接口定义

```go
// pkg/plugin/plugin.go

package plugin

import (
    "context"
    "io"
)

// Plugin 定义插件接口
type Plugin interface {
    // Lifecycle 生命周期管理
    Init(ctx context.Context, config map[string]interface{}) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    // Metadata 元数据
    Name() string
    Version() string
    Description() string

    // Hook points 钩子点
    OnConnect(conn io.ReadWriteCloser) error
    OnDisconnect(conn io.ReadWriteCloser) error
    OnMessage(msg []byte) ([]byte, error)
    OnError(err error) error

    // Config 配置
    ConfigSchema() map[string]interface{}
}

// PluginMetadata 插件元数据
type PluginMetadata struct {
    Name        string `json:"name"`
    Version     string `json:"version"`
    Description string `json:"description"`
    Author      string `json:"author"`
    License     string `json:"license"`
    Dependencies []string `json:"dependencies"`
}
```

### 2.2 使用示例

```go
// 实现Plugin接口
type MyPlugin struct {
    config map[string]interface{}
    running bool
}

func (p *MyPlugin) Init(ctx context.Context, config map[string]interface{}) error {
    p.config = config
    // 初始化插件
    return nil
}

func (p *MyPlugin) Start(ctx context.Context) error {
    if p.running {
        return nil
    }

    // 启动插件
    p.running = true
    return nil
}

func (p *MyPlugin) Stop(ctx context.Context) error {
    if !p.running {
        return nil
    }

    // 停止插件
    p.running = false
    return nil
}

func (p *MyPlugin) Name() string {
    return "my-plugin"
}

func (p *MyPlugin) Version() string {
    return "1.0.0"
}

func (p *MyPlugin) Description() string {
    return "My custom plugin"
}

func (p *MyPlugin) OnConnect(conn io.ReadWriteCloser) error {
    // 处理连接
    return nil
}

func (p *MyPlugin) OnDisconnect(conn io.ReadWriteCloser) error {
    // 处理断开
    return nil
}

func (p *MyPlugin) OnMessage(msg []byte) ([]byte, error) {
    // 处理消息
    return msg, nil
}

func (p *MyPlugin) OnError(err error) error {
    // 处理错误
    return nil
}

func (p *MyPlugin) ConfigSchema() map[string]interface{} {
    return map[string]interface{}{
        "enabled": map[string]interface{}{
            "type":    "bool",
            "default": true,
            "required": true,
        },
        "option1": map[string]interface{}{
            "type":    "string",
            "default": "value",
        },
    }
}
```

---

## 🔄 3. Middleware 接口（中间件系统）

### 3.1 接口定义

```go
// pkg/middleware/middleware.go

package middleware

import (
    "context"
)

// Middleware 定义中间件接口
type Middleware interface {
    // Name 返回中间件名称
    Name() string

    // Apply 应用中间件
    Apply(ctx context.Context, next HandlerFunc) HandlerFunc

    // Config 返回配置
    Config() map[string]interface{}
}

// HandlerFunc 定义处理函数类型
type HandlerFunc func(ctx context.Context, req interface{}) (interface{}, error)

// ChainMiddleware 链式中间件
func ChainMiddleware(ctx context.Context, mw []Middleware, handler HandlerFunc) HandlerFunc {
    for i := len(mw) - 1; i >= 0; i-- {
        handler = mw[i].Apply(ctx, handler)
    }
    return handler
}
```

### 3.2 使用示例

```go
// 指标中间件
type MetricsMiddleware struct {
    name    string
    metrics *Metrics
}

func NewMetricsMiddleware(metrics *Metrics) *MetricsMiddleware {
    return &MetricsMiddleware{
        name:    "metrics",
        metrics: metrics,
    }
}

func (m *MetricsMiddleware) Name() string {
    return m.name
}

func (m *MetricsMiddleware) Apply(ctx context.Context, next HandlerFunc) HandlerFunc {
    return func(ctx context.Context, req interface{}) (interface{}, error) {
        start := time.Now()

        resp, err := next(ctx, req)

        duration := time.Since(start)

        m.metrics.BytesOut++
        m.metrics.Uptime = time.Since(m.metrics.StartTime)

        return resp, err
    }
}

// 日志中间件
type LoggingMiddleware struct {
    name string
}

func NewLoggingMiddleware() *LoggingMiddleware {
    return &LoggingMiddleware{name: "logging"}
}

func (m *LoggingMiddleware) Name() string {
    return m.name
}

func (m *LoggingMiddleware) Apply(ctx context.Context, next HandlerFunc) HandlerFunc {
    return func(ctx context.Context, req interface{}) (interface{}, error) {
        log.Printf("[%s] Request: %v", m.name, req)

        resp, err := next(ctx, req)

        log.Printf("[%s] Response: %v, Error: %v", m.name, resp, err)

        return resp, err
    }
}

// 限流中间件
type RateLimitMiddleware struct {
    limiter *rate.Limiter
    name    string
}

func NewRateLimitMiddleware(rps int) *RateLimitMiddleware {
    return &RateLimitMiddleware{
        limiter: rate.NewLimiter(rate.Limit(rps), rps),
        name:    "rate_limit",
    }
}

func (m *RateLimitMiddleware) Name() string {
    return m.name
}

func (m *RateLimitMiddleware) Apply(ctx context.Context, next HandlerFunc) HandlerFunc {
    return func(ctx context.Context, req interface{}) (interface{}, error) {
        if !m.limiter.Allow() {
            return nil, errors.New("rate limit exceeded")
        }

        return next(ctx, req)
    }
}

// 使用中间件
func main() {
    metrics := &Metrics{StartTime: time.Now()}
    mw := []Middleware{
        NewMetricsMiddleware(metrics),
        NewLoggingMiddleware(),
        NewRateLimitMiddleware(100),
    }

    handler := ChainMiddleware(context.Background(), mw, myHandler)

    resp, err := handler(context.Background(), req)
    // ...
}
```

---

## 🚀 4. Proxy 接口（代理接口）

### 4.1 接口定义

```go
// pkg/proxy/proxy.go

package proxy

import (
    "context"
    "io"
)

// Proxy 定义代理接口
type Proxy interface {
    // Lifecycle 生命周期管理
    Init(ctx context.Context, config map[string]interface{}) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    // Connection handling 连接处理
    HandleConnection(ctx context.Context, conn io.ReadWriteCloser) error

    // Metadata 元数据
    Name() string
    Type() string
    Description() string

    // Config 配置
    Config() map[string]interface{}
    ValidateConfig(config map[string]interface{}) error
}

// ProxyConfig 代理配置
type ProxyConfig struct {
    Name        string                 `json:"name"`
    Type        string                 `json:"type"`
    LocalIP     string                 `json:"local_ip"`
    LocalPort   int                    `json:"local_port"`
    RemotePort  int                    `json:"remote_port"`
    CustomConfig map[string]interface{} `json:"custom_config"`
}
```

### 4.2 使用示例

```go
// TCP代理实现
type TCPProxy struct {
    config      *ProxyConfig
    running     bool
    listener    net.Listener
}

func NewTCPProxy(cfg *ProxyConfig) *TCPProxy {
    return &TCPProxy{
        config: cfg,
    }
}

func (p *TCPProxy) Init(ctx context.Context, config map[string]interface{}) error {
    // 初始化代理
    return nil
}

func (p *TCPProxy) Start(ctx context.Context) error {
    if p.running {
        return nil
    }

    // 启动监听
    listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", p.config.LocalIP, p.config.LocalPort))
    if err != nil {
        return err
    }

    p.listener = listener
    p.running = true

    // 启动处理循环
    go p.handleConnections()

    return nil
}

func (p *TCPProxy) Stop(ctx context.Context) error {
    if !p.running {
        return nil
    }

    // 停止监听
    if p.listener != nil {
        p.listener.Close()
    }

    p.running = false
    return nil
}

func (p *TCPProxy) HandleConnection(ctx context.Context, conn io.ReadWriteCloser) error {
    // 处理连接
    return nil
}

func (p *TCPProxy) Name() string {
    return p.config.Name
}

func (p *TCPProxy) Type() string {
    return p.config.Type
}

func (p *TCPProxy) Description() string {
    return fmt.Sprintf("TCP proxy for %s:%d", p.config.LocalIP, p.config.LocalPort)
}

func (p *TCPProxy) Config() map[string]interface{} {
    return map[string]interface{}{
        "name":        p.config.Name,
        "type":        p.config.Type,
        "local_ip":    p.config.LocalIP,
        "local_port":  p.config.LocalPort,
        "remote_port": p.config.RemotePort,
    }
}

func (p *TCPProxy) ValidateConfig(config map[string]interface{}) error {
    // 验证配置
    return nil
}

func (p *TCPProxy) handleConnections() {
    for {
        conn, err := p.listener.Accept()
        if err != nil {
            if !p.running {
                break
            }
            continue
        }

        go p.handleConnection(conn)
    }
}

func (p *TCPProxy) handleConnection(conn net.Conn) {
    defer conn.Close()

    // 处理连接逻辑
    // ...
}
```

---

## 🔐 5. Encryption 接口（加密接口）

### 5.1 接口定义

```go
// pkg/crypto/encryption.go

package crypto

import (
    "context"
    "io"
)

// Encryption 定义加密接口
type Encryption interface {
    // Encryption 加密解密
    Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
    Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)

    // Stream 流式加密解密
    NewEncryptor(ctx context.Context) (io.WriteCloser, error)
    NewDecryptor(ctx context.Context) (io.ReadCloser, error)

    // Key management 密钥管理
    GenerateKey() ([]byte, error)
    ExportKey() ([]byte, error)
    ImportKey(key []byte) error

    // Metadata 元数据
    Name() string
    Version() string
    Algorithm() string
}

// KeyType 密钥类型
type KeyType string

const (
    KeyTypeSymmetric KeyType = "symmetric"
    KeyTypeAsymmetric KeyType = "asymmetric"
    KeyTypePQC       KeyType = "pqc"
)

// KeyInfo 密钥信息
type KeyInfo struct {
    Type     KeyType `json:"type"`
    Length   int     `json:"length"`
    Version  string  `json:"version"`
    Metadata string  `json:"metadata"`
}
```

### 5.2 使用示例

```go
// AES加密实现
type AESEncryption struct {
    key []byte
    name string
    version string
}

func NewAESEncryption(key []byte) *AESEncryption {
    return &AESEncryption{
        key: key,
        name: "AES",
        version: "1.0.0",
    }
}

func (e *AESEncryption) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
    // AES加密实现
    return plaintext, nil
}

func (e *AESEncryption) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
    // AES解密实现
    return ciphertext, nil
}

func (e *AESEncryption) NewEncryptor(ctx context.Context) (io.WriteCloser, error) {
    return nil, errors.New("not implemented")
}

func (e *AESEncryption) NewDecryptor(ctx context.Context) (io.ReadCloser, error) {
    return nil, errors.New("not implemented")
}

func (e *AESEncryption) GenerateKey() ([]byte, error) {
    key := make([]byte, 32) // 256-bit
    _, err := rand.Read(key)
    return key, err
}

func (e *AESEncryption) ExportKey() ([]byte, error) {
    return e.key, nil
}

func (e *AESEncryption) ImportKey(key []byte) error {
    e.key = key
    return nil
}

func (e *AESEncryption) Name() string {
    return e.name
}

func (e *AESEncryption) Version() string {
    return e.version
}

func (e *AESEncryption) Algorithm() string {
    return "AES-256-GCM"
}
```

---

## 🌐 6. Protocol 接口（协议接口）

### 6.1 接口定义

```go
// pkg/protocol/protocol.go

package protocol

import (
    "context"
    "io"
)

// Protocol 定义协议接口
type Protocol interface {
    // Message handling 消息处理
    ParseMessage(ctx context.Context, data []byte) (Message, error)
    SerializeMessage(ctx context.Context, msg Message) ([]byte, error)

    // Stream handling 流处理
    NewEncoder(ctx context.Context, w io.Writer) (io.WriteCloser, error)
    NewDecoder(ctx context.Context, r io.Reader) (io.ReadCloser, error)

    // Metadata 元数据
    Name() string
    Version() string
    Type() string
}

// Message 消息定义
type Message struct {
    Type    string  `json:"type"`
    ID      string  `json:"id"`
    Payload []byte  `json:"payload"`
    Timestamp time.Time `json:"timestamp"`
    Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// MessageHandler 消息处理函数
type MessageHandler func(ctx context.Context, msg Message) (Message, error)
```

### 6.2 使用示例

```go
// JSON协议实现
type JSONProtocol struct {
    name string
    version string
}

func NewJSONProtocol() *JSONProtocol {
    return &JSONProtocol{
        name: "JSON",
        version: "1.0.0",
    }
}

func (p *JSONProtocol) ParseMessage(ctx context.Context, data []byte) (Message, error) {
    var msg Message
    err := json.Unmarshal(data, &msg)
    if err != nil {
        return Message{}, err
    }
    return msg, nil
}

func (p *JSONProtocol) SerializeMessage(ctx context.Context, msg Message) ([]byte, error) {
    return json.Marshal(msg)
}

func (p *JSONProtocol) NewEncoder(ctx context.Context, w io.Writer) (io.WriteCloser, error) {
    return json.NewEncoder(w), nil
}

func (p *JSONProtocol) NewDecoder(ctx context.Context, r io.Reader) (io.ReadCloser, error) {
    return json.NewDecoder(r), nil
}

func (p *JSONProtocol) Name() string {
    return p.name
}

func (p *JSONProtocol) Version() string {
    return p.version
}

func (p *JSONProtocol) Type() string {
    return "json"
}
```

---

## 📊 7. Metrics 接口（指标接口）

### 7.1 接口定义

```go
// pkg/metrics/metrics.go

package metrics

import (
    "context"
    "time"
)

// Metrics 指标接口
type Metrics interface {
    // Record 记录指标
    Record(ctx context.Context, name string, value float64, labels map[string]string) error

    // Counter 计数器
    Counter(name string, labels map[string]string) Counter

    // Gauge 仪表
    Gauge(name string, labels map[string]string) Gauge

    // Histogram 直方图
    Histogram(name string, buckets []float64, labels map[string]string) Histogram

    // Timer 计时器
    Timer(name string, labels map[string]string) Timer

    // Export 导出指标
    Export(ctx context.Context) ([]byte, error)

    // Reset 重置指标
    Reset(ctx context.Context) error
}

// Counter 计数器接口
type Counter interface {
    Inc(ctx context.Context, value float64, labels map[string]string) error
    Get(ctx context.Context, labels map[string]string) (float64, error)
}

// Gauge 仪表接口
type Gauge interface {
    Set(ctx context.Context, value float64, labels map[string]string) error
    Inc(ctx context.Context, value float64, labels map[string]string) error
    Dec(ctx context.Context, value float64, labels map[string]string) error
    Get(ctx context.Context, labels map[string]string) (float64, error)
}

// Histogram 直方图接口
type Histogram interface {
    Observe(ctx context.Context, value float64, labels map[string]string) error
    Get(ctx context.Context, labels map[string]string) (*HistogramData, error)
}

// HistogramData 直方图数据
type HistogramData struct {
    Count    int64   `json:"count"`
    Sum      float64 `json:"sum"`
    Mean     float64 `json:"mean"`
    StdDev   float64 `json:"stddev"`
    Min      float64 `json:"min"`
    Max      float64 `json:"max"`
    Buckets  []Bucket `json:"buckets"`
}

// Bucket 分桶数据
type Bucket struct {
    LowerBound float64 `json:"lower_bound"`
    UpperBound float64 `json:"upper_bound"`
    Count      int64   `json:"count"`
}

// Timer 计时器接口
type Timer interface {
    Start(ctx context.Context, labels map[string]string) (TimerContext, error)
    Get(ctx context.Context, labels map[string]string) (*TimerData, error)
}

// TimerContext 计时器上下文
type TimerContext interface {
    End(ctx context.Context) error
    Record(ctx context.Context, value float64, labels map[string]string) error
}

// TimerData 计时器数据
type TimerData struct {
    Count    int64   `json:"count"`
    Sum      float64 `json:"sum"`
    Mean     float64 `json:"mean"`
    StdDev   float64 `json:"stddev"`
    Min      float64 `json:"min"`
    Max      float64 `json:"max"`
}
```

### 7.2 使用示例

```go
// Prometheus实现
type PrometheusMetrics struct {
    registry *prometheus.Registry
    counters map[string]*prometheus.CounterVec
    gauges   map[string]*prometheus.GaugeVec
    histograms map[string]*prometheus.HistogramVec
}

func NewPrometheusMetrics() *PrometheusMetrics {
    return &PrometheusMetrics{
        registry: prometheus.NewRegistry(),
        counters: make(map[string]*prometheus.CounterVec),
        gauges:   make(map[string]*prometheus.GaugeVec),
        histograms: make(map[string]*prometheus.HistogramVec),
    }
}

func (m *PrometheusMetrics) Counter(name string, labels map[string]string) Counter {
    // 创建计数器
    return nil
}

func (m *PrometheusMetrics) Gauge(name string, labels map[string]string) Gauge {
    // 创建仪表
    return nil
}

func (m *PrometheusMetrics) Histogram(name string, buckets []float64, labels map[string]string) Histogram {
    // 创建直方图
    return nil
}

func (m *PrometheusMetrics) Timer(name string, labels map[string]string) Timer {
    // 创建计时器
    return nil
}

func (m *PrometheusMetrics) Record(ctx context.Context, name string, value float64, labels map[string]string) error {
    // 记录指标
    return nil
}

func (m *PrometheusMetrics) Export(ctx context.Context) ([]byte, error) {
    // 导出指标
    return nil, nil
}

func (m *PrometheusMetrics) Reset(ctx context.Context) error {
    // 重置指标
    return nil
}
```

---

## 📝 接口使用规范

### 1. 初始化顺序

```go
// 1. 初始化组件
comp := NewMyComponent()
err := comp.Init(ctx)
if err != nil {
    return err
}

// 2. 启动组件
err = comp.Start(ctx)
if err != nil {
    return err
}

// 3. 使用组件
// ...

// 4. 停止组件
err = comp.Stop(ctx)
if err != nil {
    return err
}
```

### 2. 错误处理

```go
func (p *MyProxy) Start(ctx context.Context) error {
    // 检查是否已启动
    if p.running {
        return errors.New("proxy already running")
    }

    // 尝试启动
    err := p.doStart(ctx)
    if err != nil {
        // 记录错误
        log.Printf("Failed to start proxy: %v", err)
        return fmt.Errorf("failed to start proxy: %w", err)
    }

    p.running = true
    return nil
}
```

### 3. 上下文传递

```go
func (p *MyProxy) HandleConnection(ctx context.Context, conn io.ReadWriteCloser) error {
    // 从上下文获取trace ID
    traceID := tracing.GetTraceID(ctx)

    // 使用trace ID记录日志
    log.Printf("[%s] Connection from %s", traceID, conn.RemoteAddr())

    // 处理连接
    // ...

    return nil
}
```

### 4. 指标记录

```go
func (p *MyProxy) HandleConnection(ctx context.Context, conn io.ReadWriteCloser) error {
    timer := metrics.Timer("proxy.connection_duration", nil).Start(ctx, nil)
    defer timer.End(ctx)

    // 记录连接数
    metrics.Counter("proxy.connections_total", nil).Inc(ctx, 1, nil)

    // 处理连接
    // ...

    return nil
}
```

---

## 🎯 最佳实践

### 1. 接口设计原则

- ✅ 接口要小而精，职责单一
- ✅ 接口方法要有明确的语义
- ✅ 接口要有合理的默认实现
- ✅ 接口要有完善的文档

### 2. 实现原则

- ✅ 所有方法都要处理错误
- ✅ 使用context.Context传递超时和取消信号
- ✅ 使用defer释放资源
- ✅ 添加适当的日志和指标

### 3. 测试原则

- ✅ 为每个接口编写测试
- ✅ 测试正常流程
- ✅ 测试异常流程
- ✅ 测试边界条件

---

## 📚 参考文档

- [Clean Architecture](https://blog.cleancoder.com/uncle-bob/2012/08/13/the-clean-architecture.html)
- [Go Interfaces](https://go.dev/tour/methods/14)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/SOLID)
- [Dependency Inversion Principle](https://en.wikipedia.org/wiki/SOLID)

---

**接口规范文档完成！**

**下一步**: 按照接口规范实现各个模块
