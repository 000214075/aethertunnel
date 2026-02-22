# AetherTunnel 模块接口定义

## 📋 目录

1. [核心接口](#核心接口)
2. [协议层接口](#协议层接口)
3. [加密层接口](#加密层接口)
4. [网络层接口](#网络层接口)
5. [服务层接口](#服务层接口)
6. [业务层接口](#业务层接口)
7. [扩展层接口](#扩展层接口)

---

## 核心接口

### 1. Proxy 接口 - 代理接口

```go
package interfaces

// Proxy 定义了代理的基本行为
type Proxy interface {
    // Run 启动代理
    Run() error

    // HandleWorkConn 处理工作连接
    HandleWorkConn(conn net.Conn, msg *protocol.StartWorkConn) error

    // Close 关闭代理
    Close() error

    // Name 获取代理名称
    Name() string

    // Type 获取代理类型
    Type() string

    // Stats 获取代理统计信息
    Stats() *ProxyStats
}

// ProxyStats 代理统计信息
type ProxyStats struct {
    Name              string
    Type              string
    ActiveConnections int
    TotalConnections  uint64
    BytesSent         uint64
    BytesReceived     uint64
    LastSeen          time.Time
}
```

### 2. ConnectionManager 接口 - 连接管理器

```go
// ConnectionManager 管理所有连接
type ConnectionManager interface {
    // HandleConnection 处理新连接
    HandleConnection(conn net.Conn) error

    // GetConnection 获取连接
    GetConnection(id string) (net.Conn, bool)

    // RemoveConnection 移除连接
    RemoveConnection(id string) error

    // CloseAll 关闭所有连接
    CloseAll() error

    // Stats 获取连接统计
    Stats() *ConnectionStats
}

// ConnectionStats 连接统计
type ConnectionStats struct {
    TotalConnections int
    ActiveConnections int
    ClosedConnections uint64
    ErrorConnections uint64
}
```

---

## 协议层接口

### 3. MessageHandler 接口 - 消息处理器

```go
package protocol

// MessageHandler 处理协议消息
type MessageHandler interface {
    // HandleAuth 处理认证消息
    HandleAuth(conn net.Conn, payload []byte) error

    // HandleHeartbeat 处理心跳消息
    HandleHeartbeat(conn net.Conn) error

    // HandleProxyRequest 处理代理请求
    HandleProxyRequest(conn net.Conn, payload []byte) error

    // HandleData 处理数据消息
    HandleData(conn net.Conn, payload []byte) error

    // HandleError 处理错误消息
    HandleError(conn net.Conn, err error) error
}
```

### 4. ProtocolAdapter 接口 - 协议适配器

```go
// ProtocolAdapter 协议适配器接口
type ProtocolAdapter interface {
    // Connect 连接到目标
    Connect(target string) (net.Conn, error)

    // Listen 监听端口
    Listen(port int) (net.Listener, error)

    // Close 关闭适配器
    Close() error

    // Type 获取协议类型
    Type() string
}
```

### 5. ProtocolFactory 接口 - 协议工厂

```go
// ProtocolFactory 协议工厂接口
type ProtocolFactory interface {
    // Create 创建协议适配器
    Create(target string) (ProtocolAdapter, error)

    // SupportedTypes 支持的协议类型
    SupportedTypes() []string

    // DefaultType 默认协议类型
    DefaultType() string
}
```

---

## 加密层接口

### 6. Cipher 接口 - 加密器

```go
package crypto

// Cipher 加密接口
type Cipher interface {
    // Encrypt 加密数据
    Encrypt(plaintext []byte) ([]byte, error)

    // Decrypt 解密数据
    Decrypt(ciphertext []byte) ([]byte, error)

    // EncryptToString 加密并编码为字符串
    EncryptToString(plaintext string) (string, error)

    // DecryptFromString 解码并解密
    DecryptFromString(encrypted string) (string, error)

    // Key 返回密钥
    Key() []byte

    // Name 返回加密算法名称
    Name() string
}
```

### 7. Signer 接口 - 签名器

```go
// Signer 签名接口
type Signer interface {
    // Sign 签名数据
    Sign(data []byte) ([]byte, error)

    // Verify 验证签名
    Verify(data, signature []byte) bool

    // PublicKey 返回公钥
    PublicKey() []byte

    // PrivateKey 返回私钥
    PrivateKey() []byte
}
```

### 8. KeyManager 接口 - 密钥管理器

```go
// KeyManager 密钥管理器接口
type KeyManager interface {
    // GenerateKeyPair 生成密钥对
    GenerateKeyPair() (Signer, error)

    // LoadKeyPair 加载密钥对
    LoadKeyPair(privateKey, publicKey []byte) (Signer, error)

    // RotateKey 轮换密钥
    RotateKey() error

    // CurrentKey 当前密钥
    CurrentKey() Signer

    // ExportPublicKey 导出公钥
    ExportPublicKey() ([]byte, error)
}
```

---

## 网络层接口

### 9. Transport 接口 - 传输层

```go
package net

// Transport 传输层接口
type Transport interface {
    // Dial 建立连接
    Dial(network, address string) (net.Conn, error)

    // Listen 监听端口
    Listen(network, address string) (net.Listener, error)

    // Close 关闭传输层
    Close() error

    // Type 获取传输类型
    Type() string

    // Stats 获取统计信息
    Stats() *TransportStats
}

// TransportStats 传输统计
type TransportStats struct {
    TotalDials       uint64
    ActiveDials      int
    TotalListens     uint64
    ActiveListens    int
    Errors           uint64
}
```

### 10. Multiplexer 接口 - 多路复用器

```go
// Multiplexer 多路复用器接口
type Multiplexer interface {
    // OpenChannel 打开通道
    OpenChannel(id string) (Channel, error)

    // CloseChannel 关闭通道
    CloseChannel(id string) error

    // GetChannel 获取通道
    GetChannel(id string) (Channel, bool)

    // CloseAll 关闭所有通道
    CloseAll() error

    // Stats 获取统计信息
    Stats() *MuxStats
}

// Channel 通道接口
type Channel interface {
    // Read 读取数据
    Read(p []byte) (n int, err error)

    // Write 写入数据
    Write(p []byte) (n int, err error)

    // Close 关闭通道
    Close() error

    // ID 获取通道ID
    ID() string

    // LocalAddr 本地地址
    LocalAddr() net.Addr

    // RemoteAddr 远程地址
    RemoteAddr() net.Addr
}
```

### 11. Obfuscator 接口 - 流量混淆器

```go
package obfuscation

// Obfuscator 流量混淆接口
type Obfuscator interface {
    // Obfuscate 混淆数据
    Obfuscate(data []byte) ([]byte, error)

    // Deobfuscate 解混淆
    Deobfuscate(data []byte) ([]byte, error)

    // Layer 返回混淆层类型
    Layer() string

    // Config 配置
    Config() *ObfuscationConfig
}

// ObfuscationConfig 混淆配置
type ObfuscationConfig struct {
    Type       string
    TargetHost string
    Key        []byte
    Padding    bool
}
```

---

## 服务层接口

### 12. ControlManager 接口 - 控制管理器

```go
package server

// ControlManager 控制管理器接口
type ControlManager interface {
    // HandleConnection 处理连接
    HandleConnection(conn net.Conn) error

    // Authenticate 认证
    Authenticate(conn net.Conn, token string) (bool, error)

    // Heartbeat 处理心跳
    Heartbeat(conn net.Conn) error

    // RegisterProxy 注册代理
    RegisterProxy(conn net.Conn, proxy *ProxyConfig) error

    // GetClient 获取客户端
    GetClient(id string) (*Client, bool)

    // RemoveClient 移除客户端
    RemoveClient(id string) error

    // Stats 获取统计信息
    Stats() *ControlStats
}

// Client 客户端信息
type Client struct {
    ID          string
    RemoteAddr  string
    AuthToken   string
    ConnectedAt time.Time
    LastSeen    time.Time
    Proxies     map[string]*ProxyConfig
}
```

### 13. ProxyManager 接口 - 代理管理器

```go
// ProxyManager 代理管理器接口
type ProxyManager interface {
    // CreateProxy 创建代理
    CreateProxy(proxy *ProxyConfig) (Proxy, error)

    // RemoveProxy 移除代理
    RemoveProxy(name string) error

    // GetProxy 获取代理
    GetProxy(name string) (Proxy, bool)

    // ListProxies 列出代理
    ListProxies() []*ProxyConfig

    // HandleConnection 处理连接
    HandleConnection(conn net.Conn) error

    // Stats 获取统计信息
    Stats() *ProxyManagerStats
}

// ProxyConfig 代理配置
type ProxyConfig struct {
    Name      string
    Type      string
    LocalIP   string
    LocalPort int
    RemotePort int
    UseTLS    bool
    UseEncryption bool
}
```

### 14. DashboardServer 接口 - 仪表板服务器

```go
// DashboardServer 仪表板服务器接口
type DashboardServer interface {
    // Start 启动服务器
    Start() error

    // Stop 停止服务器
    Stop() error

    // RegisterHandler 注册处理器
    RegisterHandler(pattern string, handler http.Handler)

    // Stats 获取统计信息
    Stats() *DashboardStats
}

// DashboardStats 仪表板统计
type DashboardStats struct {
    ActiveSessions int
    TotalRequests  uint64
    ActiveUsers    int
    Uptime         time.Duration
}
```

---

## 业务层接口

### 15. AuditLogger 接口 - 审计日志器

```go
package audit

// AuditLogger 审计日志接口
type AuditLogger interface {
    // Log 记录日志
    Log(event *AuditEvent) error

    // Query 查询日志
    Query(filter *AuditFilter) ([]*AuditEvent, error)

    // Export 导出日志
    Export(format string, filter *AuditFilter) ([]byte, error)

    // Close 关闭日志器
    Close() error
}

// AuditEvent 审计事件
type AuditEvent struct {
    Timestamp   time.Time
    EventType   string
    ClientID    string
    IP          string
    UserID      string
    Action      string
    Details     map[string]interface{}
    Success     bool
    ErrorMessage string
}

// AuditFilter 审计过滤条件
type AuditFilter struct {
    EventType   []string
    ClientID    []string
    TimeStart   time.Time
    TimeEnd     time.Time
    Page        int
    PageSize    int
}
```

### 16. HealthChecker 接口 - 健康检查器

```go
// HealthChecker 健康检查接口
type HealthChecker interface {
    // Check 检查健康状态
    Check(target string) (*HealthStatus, error)

    // Start 启动检查
    Start(interval time.Duration) error

    // Stop 停止检查
    Stop() error

    // Results 获取检查结果
    Results() map[string]*HealthStatus
}

// HealthStatus 健康状态
type HealthStatus struct {
    Target       string
    Healthy      bool
    ResponseTime int64
    LastChecked  time.Time
    Error        string
}
```

### 17. MetricsCollector 接口 - 指标收集器

```go
package metrics

// MetricsCollector 指标收集接口
type MetricsCollector interface {
    // Increment 增加计数
    Increment(name string, value int64) error

    // Gauge 设置仪表值
    Gauge(name string, value float64) error

    // Histogram 记录直方图
    Histogram(name string, value float64) error

    // Record 记录指标
    Record(name string, value interface{}) error

    // Export 导出指标
    Export(format string) ([]byte, error)

    // Reset 重置指标
    Reset() error
}
```

---

## 扩展层接口

### 18. Plugin 接口 - 插件接口

```go
package plugin

// Plugin 插件接口
type Plugin interface {
    // Name 插件名称
    Name() string

    // Version 插件版本
    Version() string

    // Init 初始化插件
    Init(config map[string]interface{}) error

    // Start 启动插件
    Start() error

    // Stop 停止插件
    Stop() error

    // Config 获取配置
    Config() map[string]interface{}
}

// PluginManager 插件管理器
type PluginManager interface {
    // Register 注册插件
    Register(plugin Plugin) error

    // Unregister 注销插件
    Unregister(name string) error

    // Get 获取插件
    Get(name string) (Plugin, bool)

    // StartAll 启动所有插件
    StartAll() error

    // StopAll 停止所有插件
    StopAll() error
}
```

### 19. Middleware 接口 - 中间件

```go
// Middleware 中间件接口
type Middleware interface {
    // Handle 处理请求
    Handle(next Handler) Handler

    // Name 获取名称
    Name() string
}

// Handler 处理器接口
type Handler interface {
    // ServeHTTP 处理HTTP请求
    ServeHTTP(w http.ResponseWriter, r *http.Request) error

    // Next 下一个处理器
    Next(w http.ResponseWriter, r *http.Request) error
}
```

---

## 📝 接口使用示例

### 代理接口实现示例

```go
package tcpproxy

import (
    "net"
    "github.com/aethertunnel/aethertunnel/pkg/interfaces"
)

type TCPProxy struct {
    name      string
    localAddr string
    remoteAddr string
    // ... 其他字段
}

func (p *TCPProxy) Run() error {
    // 实现代理运行逻辑
    listener, err := net.Listen("tcp", p.localAddr)
    if err != nil {
        return err
    }
    // ... 处理连接
    return nil
}

func (p *TCPProxy) HandleWorkConn(conn net.Conn, msg *protocol.StartWorkConn) error {
    // 实现工作连接处理
    return nil
}

func (p *TCPProxy) Close() error {
    // 实现关闭逻辑
    return nil
}

func (p *TCPProxy) Name() string {
    return p.name
}

func (p *TCPProxy) Type() string {
    return "tcp"
}

func (p *TCPProxy) Stats() *interfaces.ProxyStats {
    // 返回统计信息
    return &interfaces.ProxyStats{}
}
```

---

**接口版本**: v1.0.2
**最后更新**: 2026-02-23
**维护者**: AetherTunnel Team
