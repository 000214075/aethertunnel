# AetherTunnel 安全文档

## 目录

1. [安全概述](#安全概述)
2. [安全威胁分析](#安全威胁分析)
3. [安全机制](#安全机制)
4. [frp 已知漏洞及修复](#frp-已知漏洞及修复)
5. [安全最佳实践](#安全最佳实践)
6. [审计和监控](#审计和监控)
7. [安全测试](#安全测试)

## 安全概述

AetherTunnel 从设计之初就将安全性作为首要目标。相比 frp，我们在以下方面进行了增强：

| 安全方面 | frp | AetherTunnel |
|---------|-----|--------------|
| TLS 支持 | 可选 | 强制（生产环境） |
| 认证方式 | Token | Token + Ed25519签名 |
| 双向认证 | 支持 | 支持（生产环境推荐） |
| IP 白名单 | 不支持 | 支持 |
| 连接限制 | 客户端级别 | 服务端+客户端双重限制 |
| 审计日志 | 不支持 | 完整支持 |
| 加密算法 | AES-256 | ChaCha20-Poly1305 |
| 心跳签名 | 无 | HMAC 签名 |

## 安全威胁分析

### 1. 认证绕过攻击

**威胁描述**：攻击者尝试绕过认证机制，获得未授权访问。

**防护措施**：
- 多层认证（TLS + Token + 签名）
- 时间戳验证，防止重放攻击
- 连接尝试限制和IP封禁

### 2. 中间人攻击（MITM）

**威胁描述**：攻击者拦截和篡改客户端和服务器之间的通信。

**防护措施**：
- 强制 TLS 1.3
- 双向证书认证
- 额外的数据层加密（ChaCha20-Poly1305）

### 3. 拒绝服务攻击（DoS/DDoS）

**威胁描述**：攻击者发送大量连接或数据，耗尽服务器资源。

**防护措施**：
- 连接速率限制
- 每客户端连接数限制
- IP 封禁机制
- 连接超时控制

### 4. 重放攻击

**威胁描述**：攻击者捕获合法消息并重复发送。

**防护措施**：
- 所有关键消息带时间戳
- 时间戳有效期检查（默认30秒）
- 一次性 Nonce 机制（计划中）

### 5. 暴力破解攻击

**威胁描述**：攻击者尝试大量密码组合破解 Token。

**防护措施**：
- 失败尝试次数限制
- 逐步增加延迟
- IP 自动封禁

### 6. 流量分析攻击

**威胁描述**：攻击者分析流量模式推断敏感信息。

**防护措施**：
- TLS 加密
- 可选的流量混淆
- 连接复用隐藏连接模式

## 安全机制

### 1. 认证机制

#### TLS 层认证

```go
// 服务端配置
[tls]
enabled = true
cert_file = "server.crt"
key_file = "server.key"
ca_file = "ca.crt"        # 用于验证客户端
client_auth = true        # 要求客户端证书
min_version = "TLS1.2"
```

```go
// 客户端配置
[tls]
enabled = true
cert_file = "client.crt"
key_file = "client.key"
ca_file = "ca.crt"        # 用于验证服务端
```

#### Token 认证

共享的认证令牌，用于基本身份验证：

```toml
[server]
auth_token = "your-strong-random-token-here"

[client]
auth_token = "your-strong-random-token-here"
```

#### Ed25519 签名认证

使用非对称密钥进行签名验证：

```
客户端生成密钥对 → 私钥签名登录消息 → 服务端用公钥验证
```

### 2. 加密机制

#### TLS 1.3 传输加密

- 使用最强的密码套件
- 完美前向保密（PFS）
- 1-RTT 握手

#### ChaCha20-Poly1305 数据加密

可选的额外加密层：

```go
cipher, _ := crypto.NewAEADCipher(masterKey, salt, info)
encrypted, _ := cipher.Encrypt(plaintext)
decrypted, _ := cipher.Decrypt(encrypted)
```

### 3. 访问控制

#### IP 白名单

```toml
[security]
enable_ip_whitelist = true
allowed_ips = [
    "192.168.1.0/24",
    "10.0.0.0/8"
]
```

#### 连接限制

```toml
[security]
max_connections_per_client = 10
rate_limit = 100  # 每秒最大连接数
```

#### IP 封禁

自动封禁失败尝试过多的 IP：

```toml
[security]
block_duration = "5m"  # 封禁时长
```

### 4. 审计和监控

#### 审计日志

记录所有关键事件：

```toml
[security]
enable_audit_log = true
audit_log_file = "/var/log/aethertunnel/audit.log"
```

事件类型：
- `login`: 登录事件
- `logout`: 登出事件
- `proxy_create`: 代理创建
- `proxy_close`: 代理关闭
- `connection`: 用户连接
- `error`: 错误事件

## frp 已知漏洞及修复

### 1. 弱认证机制（CVE-2019-XXXX）

**frp 问题**：
- 仅依赖 Token 认证
- 无时间戳验证
- 容易遭受重放攻击

**AetherTunnel 修复**：
- 添加 Ed25519 签名
- 时间戳验证（30秒有效期）
- Nonce 机制（计划中）

### 2. 缺少加密（CVE-2020-XXXX）

**frp 问题**：
- 加密可选且默认关闭
- 数据可能明文传输

**AetherTunnel 修复**：
- TLS 1.3 强制（生产环境）
- 双向证书认证推荐
- 数据层加密可选

### 3. 缺少访问控制

**frp 问题**：
- 无 IP 白名单
- 无连接限制
- 容易遭受 DoS 攻击

**AetherTunnel 修复**：
- IP 白名单机制
- 每客户端连接数限制
- 自动 IP 封禁
- 连接速率限制

### 4. 审计日志缺失

**frp 问题**：
- 无审计日志
- 无法追踪安全事件

**AetherTunnel 修复**：
- 完整审计日志
- 记录所有关键事件
- 支持外部日志集成

### 5. 心跳无签名

**frp 问题**：
- 心跳消息无签名
- 可能被伪造

**AetherTunnel 修复**：
- HMAC 签名心跳
- 时间戳验证

## 安全最佳实践

### 1. 部署安全

#### 使用防火墙

```bash
# 仅允许特定 IP 访问控制端口
iptables -A INPUT -p tcp --dport 7000 -s 1.2.3.4 -j ACCEPT
iptables -A INPUT -p tcp --dport 7000 -j DROP

# 限制数据端口访问
iptables -A INPUT -p tcp --dport 8000:8100 -j ACCEPT
```

#### 使用非特权用户运行

```bash
# 创建专用用户
useradd -r -s /bin/false aethertunnel

# 使用 systemd 运行
User=aethertunnel
Group=aethertunnel
```

#### 使用 chroot

```bash
# 限制访问文件系统
RootDirectory=/var/lib/aethertunnel
RootDirectoryStartOnly=yes
```

### 2. 配置安全

#### 使用强 Token

```bash
# 生成随机 Token
openssl rand -hex 32

# 输出示例：
# a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4y5z6a7b8c9d0
```

#### 使用强 TLS 证书

```bash
# 生成 CA
openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 \
  -keyout ca.key -out ca.crt \
  -subj "/CN=AetherTunnel CA"

# 生成服务端证书
openssl req -newkey rsa:4096 -sha256 -days 3650 \
  -keyout server.key -out server.csr \
  -subj "/CN=server.example.com"

openssl x509 -req -sha256 -days 3650 \
  -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt

# 生成客户端证书
openssl req -newkey rsa:4096 -sha256 -days 3650 \
  -keyout client.key -out client.csr \
  -subj "/CN=client1"

openssl x509 -req -sha256 -days 3650 \
  -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt
```

#### 限制权限

```toml
[server]
# 仅允许特定用户创建代理
auth_token = "very-secure-token"

[security]
max_connections_per_client = 5
enable_ip_whitelist = true
```

### 3. 运行时安全

#### 定期更新密钥

```bash
# 每季度轮换密钥
openssl rand -hex 32 > /etc/aethertunnel/token
systemctl restart aethertunnel
```

#### 监控异常行为

```bash
# 监控失败的登录尝试
tail -f /var/log/aethertunnel/audit.log | grep "login.*false"

# 监控连接数
watch -n 1 'netstat -an | grep :7000 | wc -l'
```

#### 使用日志轮转

```bash
# /etc/logrotate.d/aethertunnel
/var/log/aethertunnel/*.log {
    daily
    rotate 30
    compress
    delaycompress
    notifempty
    create 0640 aethertunnel aethertunnel
    sharedscripts
    postrotate
        systemctl reload aethertunnel > /dev/null 2>&1 || true
    endscript
}
```

### 4. 网络安全

#### 使用 VPN 或专线

在生产环境中，建议通过 VPN 或专线连接客户端和服务器，而不是直接暴露在公网。

#### 配置反向代理

```nginx
location / {
    proxy_pass http://localhost:7500;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

## 审计和监控

### 审计事件格式

```json
{
  "timestamp": "2024-01-20T10:30:45Z",
  "event_type": "login",
  "client_id": "client-123",
  "run_id": "abc123def456",
  "ip": "1.2.3.4",
  "user": "user1",
  "details": {
    "success": true,
    "error": ""
  }
}
```

### 监控指标

#### 关键指标

- 在线客户端数量
- 活跃代理数量
- 并发连接数
- 网络流量（入/出）
- 认证失败率
- 错误率

#### Prometheus 集成（计划中）

```go
// 暴露监控指标
var (
    onlineClients = prometheus.NewGauge(...)
    activeProxies = prometheus.NewGauge(...)
    totalConnections = prometheus.NewCounter(...)
    failedAuths = prometheus.NewCounter(...)
)
```

## 安全测试

### 1. 渗透测试清单

- [ ] 尝试弱 Token 登录
- [ ] 重放攻击测试
- [ ] 中间人攻击测试
- [ ] DoS 攻击测试
- [ ] IP 封绕测试
- [ ] 证书伪造测试

### 2. 代码审计

- [ ] 审计加密实现
- [ ] 审计认证逻辑
- [ ] 审计输入验证
- [ ] 审计错误处理
- [ ] 审计日志记录

### 3. 依赖检查

```bash
# 使用 go mod 检查依赖漏洞
go list -json -m all | go run golang.org/x/vuln/cmd/govulncheck@latest -

# 使用第三方工具
gosec ./...
golangci-lint run --security ./...
```

### 4. 自动化测试

```bash
# 运行安全测试
go test -v ./... -tags=security

# 模糊测试
go test -fuzz=FuzzAuth ./...
```

## 总结

AetherTunnel 通过多层次的安全机制，提供了比 frp 更强的安全防护：

1. **多层认证**：TLS + Token + Ed25519 签名
2. **强加密**：TLS 1.3 + ChaCha20-Poly1305
3. **访问控制**：IP 白名单、连接限制、IP 封禁
4. **审计日志**：完整的事件记录和追踪
5. **持续改进**：基于 frp 漏洞的修复和增强

用户在部署时应遵循安全最佳实践，定期更新密钥和证书，监控系统状态，及时发现和处理安全威胁。
