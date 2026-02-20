# 🚀 AetherTunnel 快速开始指南

本指南将帮助你在 5 分钟内快速上手 AetherTunnel。

---

## 📋 前提条件

- **操作系统**：Windows/Linux/macOS/ARM 等
- **Go 版本**：1.21+（如果从源码编译）
- **内存**：至少 512MB RAM
- **网络**：能够访问公网或局域网

---

## 📦 快速安装

### 方式 1：下载预编译二进制（推荐）

```bash
# 下载最新版本
wget https://github.com/aethertunnel/aethertunnel/releases/latest/download/aethertunnel-linux-amd64.tar.gz

# 解压
tar -xzf aethertunnel-linux-amd64.tar.gz

# 安装
sudo cp aethertunnel-*/aethertunnel-server /usr/local/bin/
sudo cp aethertunnel-*/aethertunnel-client /usr/local/bin/

# 验证安装
aethertunnel-server --version
aethertunnel-client --version
```

### 方式 2：从源码编译

```bash
# 克隆仓库
git clone https://github.com/aethertunnel/aethertunnel.git
cd aethertunnel

# 编译服务端
go build -o aethertunnel-server ./server

# 编译客户端
go build -o aethertunnel-client ./client

# 安装
sudo cp aethertunnel-* /usr/local/bin/
```

### 方式 3：使用 Docker（推荐生产环境）

```bash
# 拉取镜像
docker pull aethertunnel/server:latest
docker pull aethertunnel/client:latest
```

---

## 🎯 5 分钟快速体验

### 第 1 步：配置服务端（1 分钟）

创建 `server.toml`：

```toml
# 基础配置
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "your-secure-token-here"

# 启用 TLS（推荐）
[tls]
enabled = true
cert_file = "/path/to/server.crt"
key_file = "/path/to/server.key"

# 基础安全
[security]
max_connections_per_client = 10
enable_audit_log = true

# Web 管理面板
[dashboard]
enabled = true
port = 7500
username = "admin"
password = "change-me"
```

**启动服务端**：

```bash
# 使用配置文件启动
aethertunnel-server server.toml

# 或者使用 Docker
docker run -d --name aether-server \
  -p 7000:7000 \
  -p 7500:7500 \
  -v $(pwd)/server.toml:/etc/aethertunnel/server.toml \
  aethertunnel/server:latest
```

### 第 2 步：配置客户端（1 分钟）

创建 `client.toml`：

```toml
# 基础配置
[client]
server_addr = "your-server-ip"
server_port = 7000
auth_token = "your-secure-token-here"

# TLS 配置（与服务端一致）
[tls]
enabled = true
skip_verify = false

# 添加代理（SSH）
[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222

# 添加代理（Web 服务）
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["your-domain.com"]
```

**启动客户端**：

```bash
# 使用配置文件启动
aethertunnel-client client.toml

# 或者使用 Docker
docker run -d --name aether-client \
  -v $(pwd)/client.toml:/etc/aethertunnel/client.toml \
  --network host \
  aethertunnel/client:latest
```

### 第 3 步：测试连接（30 秒）

```bash
# 测试 SSH 代理
ssh -p 2222 your-user@your-server-ip

# 测试 HTTP 代理
curl -H "Host: your-domain.com" http://your-server-ip

# 访问 Web 管理面板
# 浏览器打开 http://your-server-ip:7500
# 用户名：admin，密码：change-me
```

### 第 4 步：查看状态（30 秒）

```bash
# 查看客户端日志
aethertunnel-client client.toml --log-level debug

# 查看服务端统计
curl http://your-server-ip:7500/api/stats

# 或在 Web 管理面板中查看
```

### 第 5 步：完成！🎉

恭喜！你已经成功运行 AetherTunnel。

---

## 🌟 启用颠覆性功能

### WebRTC P2P 直连

服务端配置：
```toml
[webrtc]
enabled = true
signaling_server = "wss://signaling.example.com"

[webrtc.data_channel]
enabled = true
```

客户端配置：
```toml
[webrtc]
enabled = true
```

### 去中心化 DHT 网络

服务端配置：
```toml
[dht]
enabled = true
network_type = "kademlia"
node_id = "your-node-id"

[dht.routing_table]
refresh_interval = "10m"
```

客户端配置：
```toml
[dht]
enabled = true
node_id = "your-node-id"
listen_port = 6881
```

### 流量伪装

服务端配置：
```toml
[traffic_obfuscation]
enabled = true
obfuscation_type = "https"

[traffic_obfuscation.https]
sni = "www.youtube.com"
ja3_fingerprint = "chrome"
```

客户端配置：
```toml
[traffic_obfuscation]
enabled = true
obfuscation_type = "https"
```

### 量子抗性加密

服务端配置：
```toml
[pqc]
enabled = true
key_exchange = "kyber"
signature = "dilithium"

[pqc.hybrid]
enabled = true
traditional_algorithm = "X25519"
```

客户端配置：
```toml
[pqc]
enabled = true
key_exchange = "kyber"
```

### 游戏优化模式

客户端配置：
```toml
[gaming_mode]
enabled = true
latency_target = "10ms"

[[gaming_mode.games.list]]
name = "valorant"
ports = ["27000-27200"]
protocol = "udp"
```

---

## 📊 常用代理类型

### TCP 代理（SSH、数据库）
```toml
[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
```

### HTTP 代理（Web 服务）
```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]
```

### HTTPS 代理
```toml
[[proxies]]
name = "web-secure"
type = "https"
local_ip = "127.0.0.1"
local_port = 443
custom_domains = ["secure.example.com"]
```

### STCP 代理（安全 TCP）
```toml
[[proxies]]
name = "secret-service"
type = "stcp"
local_ip = "127.0.0.1"
local_port = 6379
sk = "my-secret-key"
```

### UDP 代理（DNS）
```toml
[[proxies]]
name = "dns"
type = "udp"
local_ip = "127.0.0.1"
local_port = 53
remote_port = 5353
```

---

## 🔧 高级配置

### 负载均衡
```toml
[load_balancer]
enabled = true
algorithm = "least_conn"

[[load_balancer.backends]]
name = "backend-1"
addr = "192.168.1.10:7000"
weight = 100
```

### 监控集成
```toml
[monitoring]
prometheus_enabled = true
prometheus_port = 9090

[monitoring.otel]
enabled = true
endpoint = "http://jaeger:4318"
```

### 故障转移
```toml
[failover]
enabled = false
primary_addr = "192.168.1.10:7000"
secondary_addrs = ["192.168.1.11:7000"]
```

---

## 🐛 故障排查

### 客户端无法连接
```bash
# 检查网络连通性
telnet your-server-ip 7000

# 检查防火墙
sudo iptables -L -n | grep 7000

# 查看服务端日志
aethertunnel-server server.toml --log-level debug
```

### TLS 错误
```bash
# 验证证书
openssl x509 -in server.crt -text -noout

# 检查证书过期
openssl x509 -in server.crt -noout -dates

# 重新生成证书
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes
```

### 性能问题
```bash
# 启用性能分析
[server]
pprof_enabled = true
pprof_port = 6060

# 查看 pprof
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

---

## 📚 下一步

1. **阅读文档**
   - [完整配置指南](docs/CONFIG_COMPARISON.md)
   - [创新功能详解](docs/INNOVATIVE_FEATURES.md)
   - [安全最佳实践](docs/SECURITY.md)

2. **探索功能**
   - [AI 智能路由](docs/INNOVATIVE_FEATURES.md#-ai-智能路由)
   - [WebRTC P2P](docs/INNOVATIVE_FEATURES.md#-webrtc-真正-p2p-直连)
   - [量子抗性加密](docs/INNOVATIVE_FEATURES.md#-量子抗性加密)

3. **部署到生产**
   - [Docker 部署指南](docs/DEPLOYMENT.md)
   - [Kubernetes 部署](docs/KUBERNETES.md)
   - [监控和告警](docs/MONITORING.md)

---

## 💡 最佳实践

### 安全建议
- ✅ 始终启用 TLS
- ✅ 使用强随机 token
- ✅ 定期轮换密钥
- ✅ 启用审计日志
- ✅ 使用 IP 白名单

### 性能优化
- ✅ 启用 TCP 多路复用
- ✅ 调整连接池大小
- ✅ 使用 QUIC 协议
- ✅ 启用压缩
- ✅ 使用边缘节点

### 生产部署
- ✅ 使用 Docker/Kubernetes
- ✅ 配置故障转移
- ✅ 启用监控
- ✅ 配置告警
- ✅ 定期备份

---

## 🤝 获取帮助

- **文档**：https://docs.aethertunnel.io
- **GitHub Issues**：https://github.com/aethertunnel/aethertunnel/issues
- **Discord 社区**：https://discord.gg/aethertunnel
- **邮件支持**：support@aethertunnel.io

---

<div align="center">

**🎉 祝你使用愉快！**

如有任何问题，请随时联系我们。

**[⬆ 回到首页](README.md)**

Made with ❤️ by AetherTunnel Team

</div>
