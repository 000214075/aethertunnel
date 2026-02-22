# AetherTunnel 技术栈清单

## 📋 目录

1. [核心语言](#核心语言)
2. [依赖库](#依赖库)
3. [加密库](#加密库)
4. [网络库](#网络库)
5. [协议库](#协议库)
6. [配置库](#配置库)
7. [工具库](#工具库)
8. [开发工具](#开发工具)
9. [部署工具](#部署工具)
10. [测试工具](#测试工具)

---

## 核心语言

### Go 语言

- **版本**: 1.22.2
- **用途**: 核心开发语言
- **特性**:
  - 高性能并发
  - 静态类型
  - 跨平台编译
  - 强大的标准库

**版本选择理由**:
- Go 1.22.2 提供了最新的语言特性
- 良好的向后兼容性
- 优秀的性能优化
- 强大的标准库支持

---

## 依赖库

### 核心依赖

#### 1. BurntSushi/toml

```go
github.com/BurntSushi/toml v1.3.2
```

**用途**: TOML 配置文件解析

**功能**:
- 解析和生成 TOML 配置
- 类型安全的配置访问
- 验证配置完整性

**使用场景**:
- 服务端配置文件
- 客户端配置文件
- 环境变量配置

#### 2. Gorilla/websocket

```go
github.com/gorilla/websocket v1.5.3
```

**用途**: WebSocket 协议实现

**功能**:
- WebSocket 服务器和客户端
- 子协议支持
- 扩展帧处理
- 兼容性良好

**使用场景**:
- 实时通信
- Web Dashboard
- 客户端-服务端通信

#### 3. Libp2p/go-sctp

```go
github.com/libp2p/go-sctp v0.0.0-00010101000000-000000000000
```

**用途**: SCTP 协议实现

**功能**:
- SCTP 流控制传输协议
- 多宿主支持
- 健壮性保障

**使用场景**:
- 多路径传输
- 高可靠性通信
- SCTP over TCP

**注意**: 使用本地 fake 实现 `./sctp-fake/sctp.go`

#### 4. Golang.org/x/crypto

```go
golang.org/x/crypto v0.17.0
```

**用途**: 加密算法库

**子模块**:
- **chacha20poly1305**: ChaCha20-Poly1305 AEAD 加密
- **ed25519**: Ed25519 签名算法
- **hkdf**: HKDF 密钥派生

**功能**:
- ChaCha20-Poly1305 AEAD
- Ed25519 签名和验证
- HKDF 密钥派生
- PBKDF2 密码派生

**使用场景**:
- 数据加密
- 签名验证
- 密钥派生

---

## 加密库

### 1. TLS 加密

**使用**: `crypto/tls` (Go 标准库)

**版本要求**:
- TLS 1.3 (推荐)
- TLS 1.2 (兼容)

**密码套件**:
```
TLS_AES_256_GCM_SHA384
TLS_CHACHA20_POLY1305_SHA256
TLS_AES_128_GCM_SHA256
```

**特性**:
- 完美前向保密 (PFS)
- 1-RTT 握手
- 0-RTT 数据传输 (可选)

### 2. AEAD 加密

**使用**: `golang.org/x/crypto/chacha20poly1305`

**算法**: ChaCha20-Poly1305

**参数**:
- 密钥长度: 32 字节
- Nonce 长度: 12 字节
- 标签长度: 16 字节

**优势**:
- 比 AES-GCM 性能更好 (ARM 架构)
- 抗侧信道攻击
- 低内存占用

### 3. 签名算法

**使用**: `crypto/ed25519` (Go 标准库)

**算法**: Ed25519

**参数**:
- 密钥长度: 32 字节 (公钥)
- 密钥长度: 64 字节 (私钥)
- 签名长度: 64 字节

**优势**:
- 高性能
- 安全性高
- 简单易用

### 4. 密钥派生

**使用**: `golang.org/x/crypto/hkdf`

**算法**: HKDF (HMAC-based Key Derivation Function)

**参数**:
- 哈希函数: SHA-256
- 密钥长度: 32 字节
- Salt 长度: 16 字节

**使用场景**:
- 主密钥派生
- 会话密钥派生
- 密钥轮换

---

## 网络库

### 1. 多路复用

**使用**: `hashicorp/yamux` (外部依赖，需要添加)

**功能**:
- 多路复用单个 TCP 连接
- 流控制
- 错误恢复

**协议**:
- 基于 Go 的 `net.Conn` 接口
- Go 协议实现

**优势**:
- 连接复用
- 减少握手开销
- 自动重连

### 2. 网络工具

**使用**: Go 标准库 `net` 包

**功能**:
- TCP 连接
- UDP 通信
- IP 地址处理
- NAT 穿透

**子包**:
- `net/http`: HTTP 客户端/服务器
- `net/netip`: IP 地址操作
- `net/dial`: 连接拨号

### 3. QUIC 支持

**使用**: `google/go-quic` (外部依赖，需要添加)

**功能**:
- QUIC 协议实现
- UDP 传输
- TLS 1.3 集成

**版本**: quic-go v0.35.0

**优势**:
- 低延迟
- 连接迁移
- 流复用

---

## 协议库

### 1. WebSocket

**使用**: `github.com/gorilla/websocket v1.5.3`

**功能**:
- WebSocket 服务器
- WebSocket 客户端
- 子协议支持
- 扩展帧

**版本**: v1.5.3

**特性**:
- 兼容 RFC 6455
- 自动心跳
- 消息队列

### 2. SCTP

**使用**: `github.com/libp2p/go-sctp` (本地 fake 实现)

**功能**:
- SCTP 流控制传输协议
- 多宿主支持
- 健壮性保障

**使用场景**:
- 多路径传输
- 高可靠性通信

### 3. HTTP/2

**使用**: Go 标准库 `net/http` (HTTP/2 服务器)

**功能**:
- HTTP/2 服务器
- HTTP/2 客户端
- 流复用
- 头部压缩

**版本**: Go 1.22.2 内置支持

---

## 配置库

### 1. TOML 解析

**使用**: `github.com/BurntSushi/toml v1.3.2`

**功能**:
- 解析 TOML 配置文件
- 类型安全的访问
- 验证和默认值

**使用场景**:
- 服务端配置
- 客户端配置
- 环境变量配置

### 2. 环境变量

**使用**: Go 标准库 `os` 和 `os/env`

**功能**:
- 环境变量读取
- 环境变量设置
- 配置覆盖

---

## 工具库

### 1. 日志库

**使用**: Go 标准库 `log` 包

**功能**:
- 标准日志输出
- 日志格式化
- 日志轮转

**扩展**: 可集成 `lumberjack` 实现日志轮转

### 2. 时间处理

**使用**: Go 标准库 `time` 包

**功能**:
- 时间戳处理
- 定时器
- 时区转换

**使用场景**:
- 心跳机制
- 超时控制
- 时间戳验证

### 3. 错误处理

**使用**: Go 标准库 `errors` 包

**功能**:
- 错误包装
- 错误检查
- 错误链

**扩展**: 可集成 `pkg/errors` 实现错误上下文

---

## 开发工具

### 1. Go Modules

**版本**: v2

**功能**:
- 依赖管理
- 版本控制
- 模块代理

**配置**: `go.mod` 和 `go.sum`

### 2. 编译工具

**工具**: `go build`, `go run`

**功能**:
- 代码编译
- 代码执行
- 依赖下载

### 3. 测试工具

**工具**: `go test`, `go test -race`, `go test -cover`

**功能**:
- 单元测试
- 竞态检测
- 代码覆盖率

---

## 部署工具

### 1. 编译脚本

**文件**: `scripts/build.sh`

**功能**:
- 跨平台编译
- 多架构支持
- 二进制打包

**支持平台**:
- Linux (x86_64, ARM64, ARM v7, MIPS64, MIPS64LE)
- macOS (x86_64, ARM64)
- Windows (x86_64, ARM64)
- FreeBSD (x86_64)

### 2. Docker

**文件**: `Dockerfile.build`

**功能**:
- 容器化构建
- 环境隔离
- 一键部署

**使用**:
```bash
docker build -f Dockerfile.build -t aethertunnel .
```

### 3. Makefile

**文件**: `Makefile`

**功能**:
- 构建命令封装
- 代码格式化
- 代码检查

**常用命令**:
```makefile
make build       # 构建项目
make test        # 运行测试
make clean       # 清理构建
make fmt         # 格式化代码
make lint        # 代码检查
```

---

## 测试工具

### 1. 单元测试

**框架**: Go 标准库 `testing`

**功能**:
- 单元测试
- 表驱动测试
- 测试覆盖率

**示例**:
```go
func TestProxy(t *testing.T) {
    // 测试代码
}
```

### 2. 竞态检测

**工具**: `go test -race`

**功能**:
- 竞态条件检测
- 并发安全检查

### 3. 代码覆盖率

**工具**: `go test -cover`

**功能**:
- 代码覆盖率统计
- 覆盖率报告

**示例**:
```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### 4. 静态分析

**工具**: `golangci-lint`

**功能**:
- 代码质量检查
- 潜在问题检测
- 代码规范检查

**配置**: `.golangci.yml`

### 5. 安全扫描

**工具**: `gosec`

**功能**:
- 安全漏洞扫描
- 代码安全检查

**使用**:
```bash
gosec ./...
```

---

## 性能监控

### 1. Prometheus (计划中)

**功能**:
- 指标收集
- 指标导出
- 监控面板

**集成**: `github.com/prometheus/client_golang`

### 2. Grafana (计划中)

**功能**:
- 可视化仪表板
- 实时监控
- 报警通知

---

## 可观测性

### 1. 日志

**格式**: 结构化日志 (JSON)

**级别**:
- DEBUG
- INFO
- WARN
- ERROR

**输出**:
- 标准输出
- 文件
- 远程日志服务

### 2. 追踪 (计划中)

**框架**: Jaeger (计划中)

**功能**:
- 分布式追踪
- 性能分析
- 问题定位

### 3. 健康检查

**接口**: `/health` (计划中)

**功能**:
- 服务健康状态
- 资源使用情况
- 故障自愈

---

## 依赖管理

### 当前依赖清单

```go
module github.com/aethertunnel/aethertunnel

go 1.22.2

require (
    github.com/BurntSushi/toml v1.3.2
    github.com/gorilla/websocket v1.5.3
    github.com/libp2p/go-sctp v0.0.0-00010101000000-000000000000
    golang.org/x/crypto v0.17.0
)

require (
    golang.org/x/sys v0.15.0 // indirect
)

replace github.com/libp2p/go-sctp => ./sctp-fake
```

### 依赖版本策略

- **固定版本**: 生产环境使用固定版本
- **语义化版本**: 使用语义化版本号
- **Go Modules**: 使用 Go Modules 管理依赖
- **安全更新**: 定期检查安全更新

---

## 技术选型理由

### 1. 为什么选择 Go？

- **高性能**: 编译型语言，性能接近 C
- **并发支持**: 原生 goroutine，高并发处理
- **跨平台**: 一次编译，到处运行
- **简单易用**: 语法简洁，学习曲线平缓
- **强大标准库**: 丰富的标准库，减少依赖
- **静态类型**: 编译时检查，减少运行时错误

### 2. 为什么选择 ChaCha20-Poly1305？

- **性能**: ARM 架构上比 AES-GCM 更快
- **安全性**: 抗侧信道攻击
- **兼容性**: 支持所有现代 CPU
- **标准**: NIST 推荐的 AEAD 算法

### 3. 为什么选择 Ed25519？

- **性能**: 比 ECDSA 更快
- **安全性**: 抗量子攻击
- **简单**: 密钥短，签名短
- **标准**: 广泛使用

### 4. 为什么选择 TLS 1.3？

- **性能**: 1-RTT 握手
- **安全**: 最新的加密套件
- **兼容**: 广泛支持
- **PFS**: 完美前向保密

---

## 未来技术栈

### 计划中的依赖

#### 1. QUIC 支持

```go
github.com/google/go-quic v0.35.0
```

**用途**: QUIC 协议实现

#### 2. 多路复用

```go
github.com/hashicorp/yamux v2.0.0
```

**用途**: yamux 多路复用协议

#### 3. 日志轮转

```go
gopkg.in/natefinch/lumberjack.v2 v2.2.1
```

**用途**: 日志文件轮转

#### 4. 指标导出

```go
github.com/prometheus/client_golang v1.17.0
```

**用途**: Prometheus 指标导出

#### 5. 分布式追踪

```go
github.com/opentracing/opentracing-go v1.2.0
```

**用途**: 分布式追踪

#### 6. 负载均衡

```go
github.com/hashicorp/consul/api v1.19.0
```

**用途**: 服务发现和负载均衡

#### 7. 配置中心

```go
github.com/spf13/viper v1.18.0
```

**用途**: 配置管理

#### 8. 仪表板

```go
github.com/prometheus/client_golang/prometheus/promhttp v0.42.0
```

**用途**: Prometheus HTTP 服务器

---

## 安全技术栈

### 加密算法

- **传输加密**: TLS 1.3
- **数据加密**: ChaCha20-Poly1305
- **签名**: Ed25519
- **密钥派生**: HKDF

### 认证机制

- **双向认证**: mTLS
- **Token 认证**: JWT (计划中)
- **签名验证**: Ed25519

### 访问控制

- **IP 白名单**: iptables 集成
- **连接限制**: 速率限制
- **自动封禁**: IP 封禁机制

---

## 总结

AetherTunnel 采用了成熟、稳定、高性能的技术栈：

### 核心技术
- Go 1.22.2: 高性能、并发、跨平台
- TLS 1.3: 现代加密
- ChaCha20-Poly1305: 高性能 AEAD
- Ed25519: 高性能签名

### 协议支持
- WebSocket: 实时通信
- SCTP: 高可靠性传输
- HTTP/2: 高效 HTTP

### 工具链
- Go Modules: 依赖管理
- 跨平台编译: 多平台支持
- 测试工具: 质量保障

### 未来规划
- QUIC: 低延迟传输
- Prometheus: 监控指标
- Jaeger: 分布式追踪

---

**技术栈版本**: v1.0.2
**最后更新**: 2026-02-23
**维护者**: AetherTunnel Team
