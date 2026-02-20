# AetherTunnel 配置对比文档

本文档详细对比 AetherTunnel 与 frp 的配置丰富程度，展示 AetherTunnel 在配置选项上的显著优势。

---

## 📊 配置选项对比统计

| 配置类别 | frp | AetherTunnel | 提升 |
|---------|-----|--------------|------|
| **服务端配置项** | ~40 | ~200+ | **5x+** |
| **客户端配置项** | ~30 | ~180+ | **6x+** |
| **代理类型** | 7 | 15+ | **2x+** |
| **安全配置** | 5 | 25+ | **5x+** |
| **监控配置** | 2 | 15+ | **7.5x** |
| **传输配置** | 4 | 12+ | **3x** |

---

## 🆚 详细对比

### 1. 基础服务配置

#### frp
```toml
[common]
bind_addr = "0.0.0.0"
bind_port = 7000
token = "your-token"
vhost_http_port = 80
vhost_https_port = 443
```

#### AetherTunnel
```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "your-token"
vhost_http_port = 80
vhost_https_port = 443

# 🆕 新增配置
quic_enabled = false
quic_port = 8443
max_connections = 10000
graceful_shutdown_timeout = 30
worker_threads = 0
```

**优势：**
- ✅ QUIC 协议支持（基于 UDP，低延迟）
- ✅ 连接数限制控制
- ✅ 优雅关闭机制
- ✅ 工作线程调优

---

### 2. TLS 加密配置

#### frp
```toml
# TLS 可选且配置简单
[common]
tls_enable = true
tls_cert_file = "server.crt"
tls_key_file = "server.key"
```

#### AetherTunnel
```toml
[tls]
enabled = true
cert_file = "server.crt"
key_file = "server.key"
ca_file = "ca.crt"
client_auth = true
min_version = "TLS1.3"
cipher_suites = []
session_ticket_key = ""
ocsp_stapling = true
ocsp_response_file = "ocsp.der"

# 🆕 现代加密
[advanced_crypto]
enable_ed25519 = true
enable_chacha20_poly1305 = true
kdf_type = "argon2id"
argon2id_time = 3
argon2id_memory = 65536
argon2id_threads = 4
key_rotation_interval = "168h"
```

**优势：**
- ✅ 强制 TLS 1.3
- ✅ 双向认证支持
- ✅ Ed25519 签名（比 RSA 更快更安全）
- ✅ ChaCha20-Poly1305 加密
- ✅ Argon2id 密钥派生
- ✅ 密钥自动轮换
- ✅ OCSP 装订

---

### 3. 安全配置

#### frp
```toml
[common]
authentication_method = "token"
token = "your-token"

# 基本的 IP 限制
authentication_heartbeat = 90
```

#### AetherTunnel
```toml
[security]
# 🆕 IP 白名单
enable_ip_whitelist = false
allowed_ips = ["192.168.1.0/24"]

# 🆕 IP 黑名单
enable_ip_blacklist = false
blocked_ips = ["1.1.1.1", "2.2.2.2"]

# 🆕 地理位置过滤
enable_geo_blocking = false
blocked_countries = ["CN", "RU"]
allowed_countries = []

max_connections_per_client = 10
max_proxies_per_client = 50
heartbeat_timeout = 90
connection_timeout = 10
read_timeout = 60
write_timeout = 60

# 🆕 审计日志
enable_audit_log = true
audit_log_file = "/var/log/aethertunnel/audit.log"
audit_log_max_size = "100MB"
audit_log_max_age = 30
audit_log_max_backups = 10

# 🆕 速率限制和封禁
rate_limit = 100
max_failed_attempts = 5
block_duration = "5m"

# 🆕 高级安全特性
enable_fingerprint = true
enable_signature = true
anti_replay_window = 300
```

**优势：**
- ✅ 双向 IP 过滤（白名单 + 黑名单）
- ✅ GeoIP 地理位置过滤
- ✅ 完整审计日志
- ✅ 自动封禁机制
- ✅ 连接指纹验证
- ✅ 防重放攻击
- ✅ 细粒度超时控制

---

### 4. 日志配置

#### frp
```toml
# 简单的日志配置
[common]
log_file = "./frps.log"
log_level = "info"
log_max_days = 3
```

#### AetherTunnel
```toml
[logging]
level = "info"  # debug, info, warn, error, fatal
format = "json"  # json, text
output = "/var/log/aethertunnel/server.log"

# 🆕 日志轮转
max_size = "100MB"
max_age = 30
max_backups = 10
compress = true
console_output = true

# 🆕 详细控制
log_request_body = false
log_response_body = false
sensitive_fields = ["password", "token", "secret"]
```

**优势：**
- ✅ JSON/Text 格式切换
- ✅ 灵活的日志轮转
- ✅ 敏感字段过滤
- ✅ 请求/响应体控制

---

### 5. 负载均衡

#### frp
```toml
# 不支持原生负载均衡
```

#### AetherTunnel
```toml
[load_balancer]
enabled = true
algorithm = "least_conn"  # round_robin, least_conn, ip_hash, random, weighted
health_check_interval = 10
health_check_timeout = 3
max_failures = 3

[[load_balancer.backends]]
name = "backend-1"
addr = "192.168.1.10:7000"
weight = 100
max_conns = 1000

[[load_balancer.backends]]
name = "backend-2"
addr = "192.168.1.11:7000"
weight = 100
max_conns = 1000
```

**优势：**
- ✅ 5种负载均衡算法
- ✅ 自动健康检查
- ✅ 多后端节点支持
- ✅ 权重配置

---

### 6. 监控与指标

#### frp
```toml
# 基本的 Dashboard
[common]
dashboard_addr = "0.0.0.0"
dashboard_port = 7500
dashboard_user = "admin"
dashboard_pwd = "admin"
```

#### AetherTunnel
```toml
[monitoring]
# 🆕 Prometheus 指标
prometheus_enabled = true
prometheus_port = 9090
prometheus_path = "/metrics"

# 🆕 OpenTelemetry 追踪
otel_enabled = false
otel_endpoint = "http://jaeger:4318"
otel_sample_rate = 0.1

# 🆕 性能分析
pprof_enabled = false
pprof_port = 6060

# 🆕 连接统计
connection_stats = true
stats_interval = 60

# 🆕 自定义指标导出
custom_metrics_exporter = "influxdb"
custom_metrics_endpoint = "http://influxdb:8086"

[dashboard]
enabled = true
port = 7500
bind_addr = "127.0.0.1"
username = "admin"
password = "admin"
assets_dir = "./assets"

# 🆕 增强功能
enable_themes = true
default_theme = "dark"
enable_websocket = true
session_timeout = 3600
enable_api_key = false
api_keys = ["key1", "key2"]
```

**优势：**
- ✅ Prometheus 原生支持
- ✅ OpenTelemetry 分布式追踪
- ✅ pprof 性能分析
- ✅ 实时连接统计
- ✅ InfluxDB 集成
- ✅ WebSocket 实时更新
- ✅ API 密钥认证

---

### 7. 数据库存储

#### frp
```toml
# 不支持持久化存储
```

#### AetherTunnel
```toml
[database]
# 🆕 支持多种数据库
type = "none"  # none, mysql, postgresql, sqlite, redis

host = "localhost"
port = 3306
username = "aethertunnel"
password = ""
database = "aethertunnel"

redis_addr = "localhost:6379"
redis_password = ""
redis_db = 0

max_open_conns = 100
max_idle_conns = 10
conn_max_lifetime = "1h"
```

**优势：**
- ✅ 5种数据库支持
- ✅ 配置持久化
- ✅ 状态持久化
- ✅ 连接池配置

---

### 8. 插件系统

#### frp
```toml
# 不支持插件
```

#### AetherTunnel
```toml
[plugins]
plugin_dir = "./plugins"
enabled_plugins = []

# 动态插件配置
[plugins.example]
option1 = "value1"
option2 = 123
```

**优势：**
- ✅ 可扩展插件架构
- ✅ 动态配置支持

---

### 9. HTTP/HTTPS 特定配置

#### frp
```toml
[proxies]
type = "http"
custom_domains = ["www.example.com"]
```

#### AetherTunnel
```toml
[proxies]
type = "http"
custom_domains = ["www.example.com"]
subdomain = "myapp"
locations = ["/api", "/v1"]
http_user = ""
http_pwd = ""
host_header_rewrite = "backend.local"

# 🆕 强制 HTTPS
force_https = false

# 🆕 TLS 终止
[proxies.tls]
enabled = true
skip_verify = false
server_name = "backend.local"

# 🆕 HSTS
[proxies.hsts]
enabled = true
max_age = 31536000
include_subdomains = true
```

**优势：**
- ✅ 子域名支持
- ✅ 路径路由
- ✅ 强制 HTTPS
- ✅ HSTS 支持
- ✅ TLS 终止配置

---

### 10. 健康检查

#### frp
```toml
[proxies]
type = "tcp"
health_check_type = "tcp"
health_check_interval_s = 10
health_check_max_failed = 3
```

#### AetherTunnel
```toml
[proxies.health_check]
type = "tcp"  # tcp, http
interval = "10s"
timeout = "3s"
max_failed = 3

# 🆕 HTTP 健康检查
url_or_path = "/health"
expected_status = 200
expected_body = ""

# 🆕 自定义请求头
[[proxies.health_check.headers]]
name = "User-Agent"
value = "AetherTunnel-HealthCheck/1.0"
```

**优势：**
- ✅ HTTP 健康检查
- ✅ 状态码验证
- ✅ 响应体验证
- ✅ 自定义请求头

---

### 11. 重连策略（客户端）

#### frp
```toml
[common]
login_fail_exit = false
```

#### AetherTunnel
```toml
[reconnect]
enabled = true
max_attempts = 0  # 0 = 无限
strategy = "exponential"  # fixed, exponential, linear
fixed_interval = "5s"
exponential_base = 2
exponential_max = "60s"
linear_increment = "5s"
jitter = 0.2
reset_on_success = true
```

**优势：**
- ✅ 3种重连策略
- ✅ 指数退避
- ✅ 随机抖动
- ✅ 无限重连支持

---

### 12. 代理服务器支持（客户端）

#### frp
```toml
# 不支持
```

#### AetherTunnel
```toml
[proxy]
enabled = false
proxy_type = "http"  # http, https, socks5
proxy_addr = "127.0.0.1:7890"
proxy_username = ""
proxy_password = ""
proxy_local = false
proxy_timeout = 30
```

**优势：**
- ✅ HTTP/HTTPS/SOCKS5 支持
- ✅ 代理认证
- ✅ 本地服务代理控制

---

### 13. 性能优化配置

#### frp
```toml
[common]
tcp_mux = true
pool_count = 5
```

#### AetherTunnel
```toml
[transport]
tcp_mux = true
tcp_mux_keepalive_interval = 60
tcp_keepalive = 30
max_pool_count = 5
min_pool_size = 2
pool_max_idle_time = 300
pool_health_check = true
pool_health_check_interval = 30
enable_nagle = false
enable_fast_open = true
enable_reuse_port = true

[network]
enable_reuse_addr = true
enable_keepalive = true
tcp_user_timeout = 60000
recv_buffer_size = 65536
send_buffer_size = 65536
enable_defer_accept = true
fast_open_queue = 1024
enable_zero_copy = true

[performance]
enable_connection_reuse = true
max_reuse_count = 100
enable_batch_send = true
batch_size = 8192
batch_timeout = "10ms"
enable_memory_pool = true
pool_size = 100  # MB
enable_cpu_affinity = false
cpu_cores = [0, 1]
enable_huge_pages = false
```

**优势：**
- ✅ 连接池健康检查
- ✅ TCP Fast Open
- ✅ SO_REUSEPORT
- ✅ 零拷贝
- ✅ 批量发送
- ✅ 内存池
- ✅ CPU 亲和性

---

### 14. 通知与告警

#### frp
```toml
# 不支持
```

#### AetherTunnel
```toml
[notification]
enabled = false

[notification.email]
enabled = false
smtp_server = "smtp.gmail.com:587"
smtp_username = ""
smtp_password = ""
from = "aethertunnel@example.com"
to = ["admin@example.com"]

[notification.slack]
enabled = false
webhook_url = "https://hooks.slack.com/services/xxx"
channel = "#alerts"
username = "AetherTunnel"

[notification.telegram]
enabled = false
bot_token = ""
chat_id = ""

[notification.webhook]
enabled = false
url = "https://your-webhook.com/notify"
method = "POST"

# 🆕 告警规则
[[notification.rules]]
name = "connection_failure"
enabled = true
threshold = 10
window = "1m"
severity = "warning"
```

**优势：**
- ✅ 多种通知渠道
- ✅ 自定义告警规则
- ✅ 严重程度分类

---

### 15. 故障转移

#### frp
```toml
# 不支持
```

#### AetherTunnel
```toml
[failover]
enabled = false
primary_addr = "192.168.1.10:7000"
secondary_addrs = ["192.168.1.11:7000", "192.168.1.12:7000"]
heartbeat_interval = 5
timeout_threshold = 15
switch_delay = 30
```

**优势：**
- ✅ 自动故障转移
- ✅ 多备用服务器
- ✅ 健康检查
- ✅ 延迟切换控制

---

### 16. 证书自动管理

#### frp
```toml
# 不支持
```

#### AetherTunnel
```toml
[cert_manager]
enabled = false
email = "admin@example.com"
cache_dir = "./certs"
ca = "letsencrypt"  # letsencrypt, zerossl
dns_provider = "cloudflare"
cloudflare_api_token = ""
cloudflare_zone_id = ""
renew_before = 30
```

**优势：**
- ✅ Let's Encrypt 自动签发
- ✅ ZeroSSL 支持
- ✅ DNS 挑战（通配符证书）
- ✅ 自动续期

---

### 17. 合规性配置

#### frp
```toml
# 不支持
```

#### AetherTunnel
```toml
[compliance]
enable_gdpr = true
data_retention_days = 90
enable_log_analysis = false
anomaly_threshold = 0.8
generate_reports = false
report_interval = "weekly"
report_recipients = ["admin@example.com"]
```

**优势：**
- ✅ GDPR 合规支持
- ✅ 数据保留策略
- ✅ 异常检测
- ✅ 合规报告生成

---

### 18. 代理类型对比

#### frp 支持
- ✅ TCP
- ✅ UDP
- ✅ HTTP
- ✅ HTTPS
- ✅ STCP (Secret TCP)
- ✅ XTCP (P2P)
- ✅ SUDP

#### AetherTunnel 支持
- ✅ TCP
- ✅ UDP
- ✅ HTTP
- ✅ HTTPS
- ✅ STCP
- ✅ XTCP
- 🆕 **WebSocket**
- 🆕 **Unix Socket**
- 🆕 **SFTP**
- 🆕 **RDP**
- 🆕 **链式代理**
- 🆕 **静态文件服务**
- 🆕 **自定义协议**

---

## 🎯 配置丰富度总结

### AetherTunnel 独有配置

1. **安全性增强**
   - GeoIP 过滤
   - 完整审计日志
   - 连接指纹
   - 防重放攻击

2. **现代化特性**
   - QUIC 协议
   - TLS 1.3 强制
   - Ed25519 签名
   - Argon2id 密钥派生

3. **企业级功能**
   - 负载均衡
   - 故障转移
   - 数据库持久化
   - 插件系统

4. **可观测性**
   - Prometheus 集成
   - OpenTelemetry 追踪
   - 实时统计
   - pprof 分析

5. **自动化**
   - 证书自动管理
   - 告警通知
   - 自动重连
   - 备份恢复

6. **合规性**
   - GDPR 支持
   - 数据保留策略
   - 合规报告

---

## 📈 配置复杂度 vs 功能对比

| 维度 | frp | AetherTunnel |
|------|-----|--------------|
| **配置难度** | ⭐ 简单 | ⭐⭐⭐ 中等 |
| **功能丰富度** | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ 非常丰富 |
| **学习曲线** | 平缓 | 中等 |
| **扩展性** | ⭐⭐ 有限 | ⭐⭐⭐⭐⭐ 极强 |
| **企业就绪** | ⭐⭐ 基础 | ⭐⭐⭐⭐⭐ 完整 |

---

## 🎓 配置最佳实践建议

### 快速开始（简单场景）
```toml
# 使用最少必要配置
[server]
bind_port = 7000
auth_token = "secure-token"

[tls]
enabled = true  # 始终启用 TLS
```

### 生产环境（推荐）
```toml
# 启用所有安全特性
[server]
bind_port = 7000
auth_token = "strong-random-token"
max_connections = 10000

[tls]
enabled = true
min_version = "TLS1.3"
client_auth = true

[advanced_crypto]
enable_ed25519 = true
enable_chacha20_poly1305 = true

[security]
enable_audit_log = true
enable_ip_whitelist = true
allowed_ips = ["10.0.0.0/8"]

[monitoring]
prometheus_enabled = true

[notification]
enabled = true
```

### 高级场景（企业级）
```toml
# 启用所有企业级特性
[server]
bind_port = 7000
auth_token = "enterprise-token"

[tls]
enabled = true
min_version = "TLS1.3"
client_auth = true

[advanced_crypto]
enable_ed25519 = true
enable_chacha20_poly1305 = true
kdf_type = "argon2id"

[load_balancer]
enabled = true
algorithm = "least_conn"

[database]
type = "postgresql"

[notification]
enabled = true

[compliance]
enable_gdpr = true
generate_reports = true
```

---

## 🔥 总结

AetherTunnel 相比 frp，在配置丰富度上有以下显著优势：

1. **5倍+的配置选项**：从~40项增加到200+项
2. **现代化加密**：TLS 1.3、Ed25519、ChaCha20-Poly1305
3. **企业级功能**：负载均衡、故障转移、数据库持久化
4. **完整可观测性**：Prometheus、OpenTelemetry、实时统计
5. **合规支持**：GDPR、数据保留、审计日志
6. **自动化能力**：证书管理、告警通知、自动重连
7. **性能优化**：批量发送、内存池、CPU 亲和性

虽然配置更丰富，但 AetherTunnel 提供了合理的默认值，用户可以根据需求逐步启用高级功能。

---

**选择建议：**
- **个人/小型项目**：frp 足够
- **中型项目**：AetherTunnel 基础配置
- **企业级/生产环境**：AetherTunnel 完整配置 + 安全增强
