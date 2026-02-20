# AetherTunnel VPN 功能使用指南

## 📋 **目录**

1. [简介](#简介)
2. [快速开始](#快速开始)
3. [VPN 配置文件详解](#vpn-配置文件详解)
4. [数据混淆配置](#数据混淆配置)
5. [Web 管理界面](#web-管理界面)
6. [高级配置](#高级配置)
7. [故障排除](#故障排除)
8. [最佳实践](#最佳实践)

---

## 简介

AetherTunnel 的 VPN 功能允许您在客户端和服务端之间建立安全的虚拟专用网络，实现：

- 🔐 **端到端加密通信**
- 🎭 **智能数据混淆**（防DPI检测）
- 🌐 **虚拟局域网**（支持多设备组网）
- 📊 **实时监控和管理**
- 🛡️ **量子抗性加密**

---

## 快速开始

### 1. 服务端配置

编辑 `server.toml`，启用 VPN 功能：

```toml
[vpn]
enabled = true
port = 7100
local_ip = "10.0.0.1"
remote_ip = "10.0.0.2"
netmask = "255.255.255.0"
protocol = "tcp"
obfuscation = true
vpn_auth_token = "your-vpn-auth-token-123"
```

### 2. 客户端配置

编辑 `client.toml`，配置 VPN 客户端：

```toml
[vpn]
enabled = true
vpn_server_addr = "your-server.com"
vpn_server_port = 7100
vpn_auth_token = "your-vpn-auth-token-123"
vpn_tunnel_type = "tcp"
vpn_obfuscation = true
```

### 3. 启动服务

```bash
# 启动服务端
./aethertunnel-server server.toml

# 启动客户端
./aethertunnel-client client.toml
```

---

## VPN 配置文件详解

### 服务端配置选项

| 选项 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | boolean | `false` | 是否启用 VPN 功能 |
| `bind_addr` | string | `"0.0.0.0"` | VPN 监听地址 |
| `port` | int | `7100` | VPN 监听端口 |
| `local_ip` | string | `"10.0.0.1"` | 服务端虚拟 IP |
| `remote_ip` | string | `"10.0.0.2"` | 客户端起始 IP |
| `netmask` | string | `"255.255.255.0"` | 子网掩码 |
| `protocol` | string | `"tcp"` | VPN 协议 (tcp/udp/webrtc) |
| `obfuscation` | boolean | `false` | 是否启用数据混淆 |
| `vpn_auth_token` | string | `""` | VPN 认证令牌 |
| `max_peers` | int | `254` | 最大客户端数量 |
| `mtu` | int | `1500` | VPN 接口 MTU |

### 客户端配置选项

| 选项 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | boolean | `false` | 是否启用 VPN 客户端 |
| `vpn_server_addr` | string | `""` | VPN 服务器地址 |
| `vpn_server_port` | int | `7100` | VPN 服务器端口 |
| `vpn_auth_token` | string | `""` | VPN 认证令牌 |
| `vpn_tunnel_name` | string | `"default"` | 隧道名称 |
| `vpn_tunnel_type` | string | `"tcp"` | 隧道类型 |
| `vpn_local_ip` | string | `""` | 本地 VPN IP |
| `vpn_obfuscation` | boolean | `false` | 是否启用混淆 |
| `vpn_obfuscation_type` | string | `"stego"` | 混淆类型 |

---

## 数据混淆配置

### 混淆类型说明

AetherTunnel 支持 6 种数据混淆方法：

1. **`none`** - 无混淆（明文传输）
2. **`xor`** - XOR加密（性能最优）
3. **`aes`** - AES加密（安全性最高）
4. **`chacha`** - ChaCha加密（移动设备最佳）
5. **`stego`** - 隐写术（隐藏在HTTP协议中）
6. **`morph`** - 流量整形（模仿其他协议）

### 配置混淆

服务端配置：

```toml
[obfuscation]
enabled = true
default_type = "stego"
adaptive_enabled = true
packet_padding = true
traffic_morphing = true
```

客户端配置：

```toml
[obfuscation]
enabled = true
default_type = "stego"
adaptive_enabled = true
```

### 自适应混淆

自适应混淆会根据网络环境自动选择最佳混淆方式：

- **HTTP 环境** → 使用 `stego`（隐写术）
- **HTTPS 环境** → 使用 `morph`（流量整形）
- **SSH 环境** → 使用 `xor`（简单加密）
- **VPN 环境** → 使用 `aes`（强加密）

---

## Web 管理界面

### 访问 Web 面板

默认情况下，Web 面板运行在 `http://localhost:7500`。

### VPN 管理功能

#### 1. 隧道管理

- **创建隧道**：点击"创建VPN隧道"按钮
- **启动/停止隧道**：点击对应按钮
- **编辑隧道**：修改配置后保存
- **删除隧道**：移除不需要的隧道

#### 2. 客户端管理

- **查看在线客户端**：显示所有连接的设备
- **断开客户端**：强制断开指定客户端
- **客户端统计**：查看流量和连接时间

#### 3. 混淆配置

- **启用/禁用混淆**：切换混淆状态
- **选择混淆类型**：从下拉菜单中选择
- **自适应混淆**：启用智能混淆选择
- **保存配置**：应用更改

#### 4. 实时监控

- **隧道数量**：当前活跃的隧道数
- **客户端数量**：已连接的客户端数
- **流量统计**：实时带宽使用情况
- **混淆状态**：显示混淆是否启用

---

## 高级配置

### 路由配置

客户端可以配置自定义路由规则：

```toml
[vpn.routes]
enabled = true

[[vpn.routes.items]]
network = "10.0.0.0/24"
via_vpn = true
description = "内部网络"

[[vpn.routes.items]]
network = "192.168.1.0/24"
via_vpn = false
description = "家庭网络"
```

### 安全配置

```toml
[vpn.security]
verify_cert = true
cert_file = "client.crt"
key_file = "client.key"
ca_file = "ca.crt"
```

### 性能调优

```toml
[vpn.advanced]
enable_compression = false
enable_qos = false
bandwidth_limit_up = "0"  # 0 = 无限制
bandwidth_limit_down = "0"  # 0 = 无限制
buffer_size = 64  # KB
```

### 多隧道配置

服务端可以配置多个隧道：

```toml
# 隧道1：默认隧道
[vpn.tunnel1]
name = "default"
local_ip = "10.0.0.1"
remote_ip = "10.0.0.2"
netmask = "255.255.255.0"

# 隧道2：高级隧道
[vpn.tunnel2]
name = "premium"
local_ip = "10.1.0.1"
remote_ip = "10.1.0.2"
netmask = "255.255.0.0"
protocol = "udp"
```

---

## 故障排除

### 常见问题

#### 1. 无法连接 VPN 服务器

**可能原因：**
- 服务器地址或端口错误
- 防火墙阻止连接
- 认证令牌不匹配
- VPN 功能未启用

**解决方案：**
```bash
# 检查服务器状态
telnet your-server.com 7100

# 检查防火墙
iptables -L -n | grep 7100

# 检查日志
tail -f /var/log/aethertunnel/server.log
```

#### 2. 混淆不生效

**可能原因：**
- 客户端和服务端混淆配置不一致
- 混淆类型不支持当前网络环境
- 密钥不匹配

**解决方案：**
- 确保客户端和服务端 `obfuscation.enabled` 都为 `true`
- 尝试不同的混淆类型
- 检查密钥和认证令牌

#### 3. 连接速度慢

**可能原因：**
- 网络延迟高
- 混淆算法开销大
- 带宽限制

**解决方案：**
- 使用 `xor` 混淆（性能最优）
- 禁用不必要的功能
- 增加带宽限制

#### 4. 客户端无法获取 IP

**可能原因：**
- IP 池耗尽
- 路由配置错误
- 权限问题

**解决方案：**
- 检查 `vpn.netmask` 配置
- 扩大 IP 地址范围
- 检查系统路由表

### 调试技巧

1. **启用详细日志：**
   ```toml
   [logging]
   level = "debug"
   verbose = true
   ```

2. **检查连接状态：**
   ```bash
   # 查看 VPN 连接
   ip addr show tun0

   # 查看路由表
   ip route list
   ```

3. **测试连通性：**
   ```bash
   # 从客户端 ping 服务端 VPN IP
   ping 10.0.0.1

   # 从服务端 ping 客户端 VPN IP
   ping 10.0.0.2
   ```

---

## 最佳实践

### 安全建议

1. **使用强认证令牌**
   - 至少 16 位随机字符串
   - 包含大小写字母和数字
   - 定期更换

2. **启用证书验证**
   - 使用 TLS 证书
   - 验证服务端身份
   - 定期更新证书

3. **最小权限原则**
   - 限制客户端 IP 范围
   - 禁用不必要的路由
   - 定期审计连接

### 性能优化

1. **选择合适的混淆类型**
   - 高性能需求：使用 `xor`
   - 高安全性需求：使用 `aes`
   - 移动网络：使用 `chacha`

2. **调整缓冲区大小**
   ```toml
   [vpn.advanced]
   buffer_size = 128  # 大文件传输时使用更大值
   ```

3. **启用多路径传输**
   ```toml
   [vpn.advanced]
   enable_multipath = true
   ```

### 网络架构

1. **星型拓扑**
   ```
   客户端1 → 服务端 ← 客户端2
   ```

2. **网状拓扑**
   ```
   客户端1 ↔ 服务端 ↔ 客户端2
             ↓
           客户端3
   ```

3. **混合拓扑**
   ```
   [办公室网络] → VPN 网关 → [互联网]
                         ↓
                    [远程员工]
   ```

### 监控和维护

1. **定期检查日志**
   - 检查错误和警告
   - 监控连接状态
   - 分析安全事件

2. **备份配置**
   - 定期备份配置文件
   - 测试恢复流程
   - 记录配置变更

3. **更新和升级**
   - 关注安全更新
   - 测试新版本
   - 回滚计划

---

## 示例配置

### 简单 VPN 配置

**服务端 (`server.toml`):**
```toml
[server]
bind_port = 7000
auth_token = "your-server-token"

[vpn]
enabled = true
port = 7100
local_ip = "10.0.0.1"
remote_ip = "10.0.0.2"
netmask = "255.255.255.0"
vpn_auth_token = "your-vpn-token"
```

**客户端 (`client.toml`):**
```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "your-server-token"

[vpn]
enabled = true
vpn_server_addr = "your-server.com"
vpn_server_port = 7100
vpn_auth_token = "your-vpn-token"
```

### 高级 VPN 配置

**服务端 (`server.toml`):**
```toml
[server]
bind_port = 7000
auth_token = "your-server-token"
max_connections = 1000

[vpn]
enabled = true
port = 7100
local_ip = "10.0.0.1"
remote_ip = "10.0.0.2"
netmask = "255.255.255.0"
protocol = "tcp"
obfuscation = true
vpn_auth_token = "your-vpn-token"
max_peers = 254
mtu = 1400

[obfuscation]
enabled = true
default_type = "stego"
adaptive_enabled = true
packet_padding = true
traffic_morphing = true
key_rotation = 60
```

**客户端 (`client.toml`):**
```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "your-server-token"

[vpn]
enabled = true
vpn_server_addr = "your-server.com"
vpn_server_port = 7100
vpn_auth_token = "your-vpn-token"
vpn_tunnel_type = "tcp"
vpn_obfuscation = true
vpn_obfuscation_type = "stego"
vpn_local_ip = "10.0.0.100"

[vpn.connection]
auto_reconnect = true
max_reconnect_attempts = 0
reconnect_interval = 5
reconnect_timeout = 30

[vpn.keepalive]
enabled = true
interval = 30
timeout = 10
max_failures = 3

[obfuscation]
enabled = true
default_type = "stego"
adaptive_enabled = true
packet_padding = true
traffic_morphing = true
strength = 7
```

---

## 结论

AetherTunnel VPN 功能为企业级安全通信提供了完整的解决方案。通过本指南，您应该能够：

✅ **快速部署 VPN 服务**
✅ **配置数据混淆保护**
✅ **管理 VPN 连接**
✅ **解决常见问题**
✅ **优化性能和安全性**

如需更多帮助，请参考：
- [官方文档](https://aethertunnel.github.io)
- [GitHub Issues](https://github.com/aethertunnel/aethertunnel/issues)
- [社区论坛](https://discuss.aethertunnel.io)

祝您使用愉快！🎉