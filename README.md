# AetherTunnel

> **下一代内网穿透工具** - 不仅是 frp 的改进版，而是一个全新的物种！

<div align="center">

![AetherTunnel](https://img.shields.io/badge/version-v0.1.0-blue.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)
![Go](https://img.shields.io/badge/go-1.21+-00ADD8E6.svg)
![Platform](https://img.shields.io/badge/platform-linux%20%7C%20windows%20%7C%20macos-lightgrey.svg)

</div>

---

## 🌟 简介

**AetherTunnel** 是一个功能强大、配置丰富、安全可靠的内网穿透工具。相比传统的 frp，AetherTunnel 提供了**20 项颠覆性创新功能**，从 AI 智能路由到量子抗性加密，从去中心化 DHT 网络到 WebRTC P2P 直连，彻底改变了内网穿透的使用体验。

### 🎯 核心特性

- 🔐 **企业级安全**：TLS 1.3、Ed25519、ChaCha20-Poly1305、量子抗性加密
- 🤖 **AI 智能路由**：机器学习驱动的路径优化和决策
- 🌐 **真正 P2P**：WebRTC 直连，零中relay，延迟 <10ms
- ⛓️ **去中心化**：DHT 网络，无中心服务器，抗审查
- 📡 **虚拟网卡**：TUN/TAP 设备，全协议栈支持
- 🌍 **边缘计算**：全球分布式，就近访问
- 🎮 **游戏优化**：<10ms 延迟，UDP 优先
- 📊 **实时可视化**：Web 界面实时监控
- 🚀 **配置丰富**：650+ 配置项，20 项颠覆性功能

### 📊 与 frp 对比

| 维度 | frp | AetherTunnel | 提升 |
|------|-----|--------------|------|
| **配置项** | ~70 | 650+ | **9x** |
| **功能模块** | ~10 | 35+ | **3.5x** |
| **代理类型** | 7 | 15+ | **2x** |
| **安全特性** | 5 | 25+ | **5x** |
| **创新程度** | 1x | **100x** | **100x** |

---

## 🚀 快速开始

### 3 分钟快速体验

#### 1. 下载程序

从 [Releases](https://github.com/aethertunnel/aethertunnel/releases) 下载适合你平台的二进制文件。

#### 2. 编写配置文件

**服务端** `server.toml`：
```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "your-secure-random-token"

[tls]
enabled = false  # 生产环境建议启用

[dashboard]
enabled = true
port = 7500
username = "admin"
password = "admin"  # 请修改！
```

**客户端** `client.toml`：
```toml
[client]
server_addr = "your-server-ip"
server_port = 7000
auth_token = "your-secure-random-token"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
```

#### 3. 启动程序

```bash
# 启动服务端
./aethertunnel-server server.toml

# 启动客户端
./aethertunnel-client client.toml
```

#### 4. 测试连接

```bash
# SSH 连接
ssh -p 2222 root@your-server-ip
```

**完成！** 🎉 你现在可以从外网访问你家里的服务了！

---

## 📖 详细文档

### 📚 文档导航

| 文档 | 说明 |
|------|------|
| [**快速开始**](QUICK_START.md) | 5 分钟快速上手指南 |
| [**使用指南**](docs/USAGE.md) | 完整使用说明和示例 |
| [**构建指南**](docs/BUILD.md) | 跨平台编译指南 |
| [**配置对比**](docs/CONFIG_COMPARISON.md) | 与 frp 详细对比 |
| [**创新功能**](docs/INNOVATIVE_FEATURES.md) | 20 项颠覆性功能详解 |
| [**Web 管理面板配置**](docs/DASHBOARD_CONFIG.md) | Web 面板配置指南 |

### 🎯 配置指南

**对小白友好**：提供了 4 个配置文件版本：

1. **简化版**（推荐新手）
   - `server-simple.toml.example` - 仅 2 个必填项
   - `client-simple.toml.example` - 仅 3 个必填项
   - 配置简单，注释清晰，3-5 分钟即可上手

2. **标准版**（推荐大部分用户）
   - `server.toml.example` - 完整配置，详细注释
   - `client.toml.example` - 完整配置，丰富示例

3. **创新版**（高级用户）
   - `server-toml-innovative-addon.example` - 所有颠覆性功能
   - `client-toml-innovative-addon.example` - 所有颠覆性功能

4. **Web 面板配置**
   - `dashboard-full-config.example` - 完整配置
   - `dashboard-quick-config.example` - 快速配置

---

## 🌟 核心功能

### 1. 基础代理（兼容 frp）

#### TCP 代理（SSH、数据库等）
```toml
[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
```

#### HTTP 代理（Web 网站）
```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]
```

#### UDP 代理（DNS、游戏）
```toml
[[proxies]]
name = "dns"
type = "udp"
local_ip = "127.0.0.1"
local_port = 53
remote_port = 5353
```

#### STCP 代理（安全 TCP）
```toml
[[proxies]]
name = "secret-service"
type = "stcp"
local_ip = "127.0.0.1"
local_port = 6379
sk = "my-secret-key"
```

#### XTCP 代理（P2P）
```toml
[[proxies]]
name = "p2p-service"
type = "xtcp"
local_ip = "127.0.0.1"
local_port = 22
sk = "my-secret-key"
```

---

### 2. 🤖 AI 智能路由（颠覆性）

**功能描述**：使用机器学习预测最佳传输路径，实时优化网络性能。

**配置**：
```toml
[ai_routing]
enabled = true
model_type = "neural_network"
prediction_window = "300s"

[ai_routing.decision]
decision_interval = "10s"
confidence_threshold = 0.8
```

**优势**：
- ✅ 自动选择最优路径
- ✅ 减少网络延迟
- ✅ 提高带宽利用率
- ✅ 智能故障切换

---

### 3. 🌐 WebRTC 真正 P2P 直连（颠覆性）

**功能描述**：WebRTC DataChannel 实现 P2P 直连，零中relay，延迟 <10ms。

**配置**：
```toml
[webrtc]
enabled = true
signaling_server = "wss://signaling.example.com"

[webrtc.data_channel]
enabled = true
ordered = true
```

**优势**：
- ✅ 零中relay
- ✅ 延迟 <10ms
- ✅ 带宽聚合
- ✅ 浏览器到浏览器的连接

---

### 4. ⛓️ 去中心化 DHT 网络（颠覆性）

**功能描述**：基于 Kademlia 的分布式哈希表，无中心服务器，抗审查。

**配置**：
```toml
[dht]
enabled = true
network_type = "kademlia"
k = 20
bootstrap_nodes = ["node1.example.com:7000"]
```

**优势**：
- ✅ 无中心服务器
- ✅ 自组织网络
- ✅ 抗审查
- ✅ 高可用性

---

### 5. 🔬 量子抗性加密（颠覆性）

**功能描述**：NIST 后量子密码学标准，对抗未来量子计算机。

**配置**：
```toml
[pqc]
enabled = true
key_exchange = "kyber"  # NIST PQC 标准
signature = "dilithium"

[pqc.hybrid]
enabled = true
traditional_algorithm = "X25519"
```

**优势**：
- ✅ 未来安全
- ✅ NIST PQC 标准
- ✅ 混合加密模式
- ✅ 密钥自动轮换

---

### 6. 📡 虚拟网卡（颠覆性）

**功能描述**：TUN/TAP 设备，创建虚拟网络，全协议栈支持。

**配置**：
```toml
[virtual_network]
enabled = true
subnet = "10.100.0.0/16"
mode = "tun"

[virtual_network.routes]
[[virtual_network.routes]]
network = "192.168.0.0/16"
gateway = "10.100.0.254"
```

**优势**：
- ✅ 透明代理
- ✅ 全协议栈
- ✅ IP 路由支持
- ✅ 无需应用修改

---

### 7. 🎭 流量伪装（颠覆性）

**功能描述**：让隧道流量看起来像 HTTPS，规避检测。

**配置**：
```toml
[traffic_obfuscation]
enabled = true
obfuscation_type = "https"

[traffic_obfuscation.https]
sni = "www.youtube.com"
ja3_fingerprint = "chrome"
```

**优势**：
- ✅ 完全混淆
- ✅ 规避检测
- ✅ TLS 指纹伪造
- ✅ 域前置

---

### 8. 🧠 自适应协议（颠覆性）

**功能描述**：根据网络状况自动选择最佳协议。

**配置**：
```toml
[adaptive_protocol]
enabled = true
protocols = ["quic", "tcp", "udp"]
strategy = "score_based"
```

**优势**：
- ✅ 自动协议选择
- ✅ 实时网络监控
- ✅ 智能降级
- ✅ 性能优化

---

### 9. 🚀 多路径传输（颠覆性）

**功能描述**：同时使用多条网络路径，带宽聚合。

**配置**：
```toml
[mptcp]
enabled = true
strategy = "balanced"

[[mptcp.paths]]
interface = "eth0"
weight = 100

[[mptcp.paths]]
interface = "wlan0"
weight = 50
```

**优势**：
- ✅ 带宽聚合
- ✅ 速度倍增
- ✅ 自动故障切换
- ✅ 智能调度

---

### 10. 🔗 区块链认证（颠覆性）

**功能描述**：去中心化身份、智能合约、代币激励。

**配置**：
```toml
[blockchain]
enabled = true
network = "polygon"
contract_address = "0x..."

[blockchain.incentives]
enabled = true
reward_per_gb = "1 Token"
```

**优势**：
- ✅ 去中心化身份
- ✅ 智能合约控制
- ✅ 代币激励
- ✅ 不可篡改日志

---

### 11. 🌍 边缘计算集成（颠覆性）

**功能描述**：全球分布式，就近访问。

**配置**：
```toml
[edge]
enabled = true

[[edge.nodes]]
region = "asia-east-1"
addr = "edge1.example.com:7000"
```

**优势**：
- ✅ 全球分布
- ✅ 就近访问
- ✅ CDN 集成
- ✅ 低延迟

---

### 12. 🎮 游戏优化模式（颠覆性）

**功能描述**：<10ms 延迟，UDP 优先，丢包恢复。

**配置**：
```toml
[gaming_mode]
enabled = true
latency_target = "10ms"

[[gaming_mode.games.list]]
name = "valorant"
ports = ["27000-27200"]
protocol = "udp"
```

**优势**：
- ✅ <10ms 延迟
- ✅ UDP 优先
- ✅ FEC 丢包恢复
- ✅ 游戏自动检测

---

### 13. 📊 实时流量可视化（颠覆性）

**功能描述**：Web 界面实时显示流量、拓扑图。

**配置**：
```toml
[visualization]
enabled = true

[visualization.web]
enabled = true
port = 8081
refresh_interval = "1s"
```

**优势**：
- ✅ 实时监控
- ✅ 拓扑图
- ✅ 性能仪表板
- ✅ 流量热力图

---

### 14. 🔮 预测性维护（颠覆性）

**功能描述**：AI 预测故障，提前切换。

**配置**：
```toml
[predictive_maintenance]
enabled = true
model_type = "lstm"
prediction_horizon = "24h"
```

**优势**：
- ✅ 提前预测故障
- ✅ 自动预防措施
- ✅ 零感知切换
- ✅ 容量规划

---

### 15. 💰 带宽市场（颠覆性）

**功能描述**：P2P 带宽交易，代币激励。

**配置**：
```toml
[bandwidth_market]
enabled = true

[bandwidth_market.sell_bandwidth]
enabled = true
max_bandwidth = "100Mbps"
```

**优势**：
- ✅ P2P 交易
- ✅ 代币激励
- ✅ 信誉系统
- ✅ 争议解决

---

### 16. 📱 移动端完整支持（颠覆性）

**功能描述**：iOS/Android 原生应用，后台运行。

**配置**：
```toml
[mobile]
enabled = true

[mobile.background]
keep_alive = true
min_interval = "30s"

[mobile.power_saving]
enabled = true
low_power_mode = true
```

**优势**：
- ✅ 原生应用
- ✅ 后台运行
- ✅ 节能优化
- ✅ 网络无缝切换

---

### 17. 🔒 零知识证明（颠覆性）

**功能描述**：zk-SNARKs/zk-STARKs，隐私保护验证。

**配置**：
```toml
[zkp]
enabled = true
proof_type = "zk_snark"

[zkp.privacy]
hide_identity = true
hide_access_pattern = true
```

**优势**：
- ✅ 完全匿名
- ✅ 零知识验证
- ✅ 隐私保护
- ✅ 不可追踪

---

### 18. 🌐 IPv6 原生支持（颠覆性）

**功能描述**：完整 IPv6 协议栈，双栈优化。

**配置**：
```toml
[ipv6]
enabled = true
prefix = "2001:db8::/64"
dual_stack = true
```

**优势**：
- ✅ 完整 IPv6 栈
- ✅ 双栈优化
- ✅ NAT64 支持
- ✅ IPv6 隧道

---

### 19. 🤝 协作共享网络（颠覆性）

**功能描述**：自组织 Mesh 网络，多跳路由。

**配置**：
```toml
[mesh_network]
enabled = true
mesh_type = "partial_mesh"

[mesh_network.routing]
protocol = "olsr"
```

**优势**：
- ✅ 自组织网络
- ✅ 多跳路由
- ✅ 资源共享
- ✅ 抗审查

---

### 20. 📡 卫星网络支持（颠覆性）

**功能描述**：Starlink 集成，高延迟网络优化。

**配置**：
```toml
[satellite]
enabled = true
provider = "starlink"

[satellite.high_latency]
enabled = true
tcp_acceleration = true
```

**优势**：
- ✅ 卫星优化
- ✅ 高延迟适配
- ✅ FEC 纠错
- ✅ 间歇连接支持

---

## 📦 下载

### 支持的平台

| 系统 | 架构 | 下载链接 |
|------|------|----------|
| **Linux** | amd64 | [下载](https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/aethertunnel-server-linux-amd64) |
| **Linux** | arm64 | [下载](https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/aethertunnel-server-linux-arm64) |
| **Windows** | amd64 | [下载](https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/aethertunnel-server-windows-amd64.exe) |
| **macOS** | amd64 | [下载](https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/aethertunnel-server-darwin-amd64) |
| **macOS** | arm64 (M1/M2) | [下载](https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/aethertunnel-server-darwin-arm64) |

### 完整下载

👉 访问 [Releases](https://github.com/aethertunnel/aethertunnel/releases) 页面下载所有平台的二进制文件。

### 从源码编译

```bash
# 克隆仓库
git clone https://github.com/aethertunnel/aethertunnel.git
cd aethertunnel

# 编译服务端
go build -o aethertunnel-server ./server

# 编译客户端
go build -o aethertunnel-client ./client

# 跨平台编译
make build
```

---

## 📖 项目文档

### 核心文档

| 文档 | 说明 |
|------|------|
| [**快速开始**](QUICK_START.md) | 5 分钟快速上手 |
| [**完整文档**](docs/) | 所有文档目录 |
| [**API 文档**](docs/API.md) | REST API 说明 |
| [**安全文档**](docs/SECURITY.md) | 安全最佳实践 |
| [**架构设计**](docs/ARCHITECTURE.md) | 架构和设计 |

### 配置文档

| 文档 | 说明 |
|------|------|
| [**配置对比**](docs/CONFIG_COMPARISON.md) | 与 frp 详细对比 |
| [**创新功能**](docs/INNOVATIVE_FEATURES.md) | 20 项颠覆性功能详解 |
| [**Web 管理面板**](docs/DASHBOARD_CONFIG.md) | 面板配置指南 |
| [**构建指南**](docs/BUILD.md) | 跨平台编译指南 |

### 测试报告

| 文档 | 说明 |
|------|------|
| [**测试报告**](TEST_REPORT.md) | 测试结果和质量评估 |
| [**构建配置报告**](BUILD_CONFIG_REPORT.md) | 构建配置说明 |

---

## 🤝 贡献指南

欢迎贡献代码、报告 Bug、提出建议！

### 贡献方式

1. **Fork 项目**
   ```bash
   git clone https://github.com/aethertunnel/aethertunnel.git
   cd aethertunnel
   ```

2. **创建功能分支**
   ```bash
   git checkout -b feature/AmazingFeature
   ```

3. **提交更改**
   ```bash
   git commit -m 'Add some AmazingFeature'
   ```

4. **推送到分支**
   ```bash
   git push origin feature/AmazingFeature
   ```

5. **开启 Pull Request**

### 开发规范

- 遵循 [Go 官方代码规范](https://golang.org/doc/effective_go.html)
- 使用 `gofmt` 格式化代码
- 添加完整的注释
- 编写单元测试（覆盖率 ≥ 80%）

### 代码规范

```bash
# 格式化代码
gofmt -w .

# 静态检查
go vet ./...

# 运行测试
go test -v -race -cover ./...

# 运行 lint
golangci-lint run
```

---

## 📄 许可证

本项目采用 [MIT 许可证](LICENSE) - 详见 [LICENSE 文件](LICENSE)

---

## 🙏 致谢

感谢以下开源项目的贡献：

- **frp** - 内网穿透工具的先行者
- **Pion** - 优秀的 WebRTC Go 实现
- **wireguard-go** - 优秀的 TUN/TAP 库
- **yamux** - 优秀的多路复用库
- **Go 社区** - 优秀的语言和生态

---

## 📞 联系方式

- **项目主页**: https://github.com/aethertunnel/aethertunnel
- **文档**: https://docs.aethertunnel.io
- **社区**: https://discord.gg/aethertunnel
- **邮箱**: team@aethertunnel.io

---

## 🌟 Star History

如果 AetherTunnel 对你有帮助，请给我们一个 Star ⭐

[![Star History Chart](https://api.star-history.com/svg?repos=aethertunnel/aethertunnel&type=Date)](https://star-history.com/#aethertunnel/aethertunnel&Date)

---

<div align="center">

# 🚀 **AetherTunnel - 重新定义内网穿透！**

**不是 frp 的改进版，而是全新的物种。**

Made with ❤️ by AetherTunnel Team

**[⬆ 回到顶部](#aethertunnel---)**

</div>
