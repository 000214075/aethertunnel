# 🌐 AetherTunnel Web 管理面板配置指南

本指南详细介绍 AetherTunnel Web 管理面板的所有配置选项。

---

## 📋 目录

1. [快速开始](#快速开始)
2. [基础配置](#基础配置)
3. [认证配置](#认证配置)
4. [安全配置](#安全配置)
5. [界面配置](#界面配置)
6. [API 配置](#api-配置)
7. [集成配置](#集成配置)
8. [最佳实践](#最佳实践)
9. [常见问题](#常见问题)

---

## 🚀 快速开始

### 最小化配置

```toml
[dashboard]
enabled = true
port = 7500
bind_addr = "127.0.0.1"

[dashboard.auth]
enabled = true
username = "admin"
password = "admin123"
```

**访问方式**：http://localhost:7500

### 推荐配置（生产环境）

```toml
[dashboard]
enabled = true
port = 7500
bind_addr = "127.0.0.1"

[dashboard.auth]
enabled = true
username = "admin"
password = "Str0ngP@ssw0rd123!"
session_timeout = 3600

[dashboard.auth.rate_limit]
enabled = true
max_attempts_per_minute = 5

[dashboard.tls]
enabled = true
cert_file = "/etc/ssl/certs/dashboard.crt"
key_file = "/etc/ssl/private/dashboard.key"

[dashboard.logging.audit]
enabled = true
audit_log_file = "/var/log/aethertunnel/audit.log"
```

---

## 📐 基础配置

### 启用/禁用面板

```toml
[dashboard]
# 是否启用 Web 管理面板
enabled = true
```

### 端口配置

```toml
[dashboard]
# 监听端口（1-65535）
port = 7500
```

**注意事项**：
- 端口不能与代理端口冲突
- 小于 1024 的端口需要 root 权限
- 常用端口：7500（默认）、8080、8443

### 绑定地址

```toml
[dashboard]
# 绑定地址
bind_addr = "127.0.0.1"
```

**选项说明**：
- `127.0.0.1` - 仅本地访问（推荐，更安全）
- `0.0.0.0` - 所有接口（可以从外网访问）
- `192.168.1.100` - 指定 IP 地址

**安全建议**：
- ✅ 生产环境使用 `127.0.0.1`
- ✅ 通过反向代理（Nginx、Caddy）访问
- ❌ 不要直接使用 `0.0.0.0`（除非有额外安全措施）

### 访问路径

```toml
[dashboard]
# 访问路径（使用反向代理时设置）
base_path = ""
```

**示例**：
- `""` - 根路径（http://localhost:7500）
- `"/aethertunnel"` - 子路径（http://localhost:7500/aethertunnel）
- `"/admin"` - 管理路径（http://localhost:7500/admin）

---

## 🔐 认证配置

### 基础认证

```toml
[dashboard.auth]
# 是否启用认证（强烈建议！）
enabled = true

# 认证方式
mode = "basic"  # basic, jwt, ldap, oauth2, saml

# Session 配置
[dashboard.auth.session]
timeout = 3600  # 1小时
max_concurrent_sessions = 5
idle_timeout = 1800  # 30分钟
```

### 用户名密码

```toml
[dashboard]
username = "admin"
password = "admin123"
```

**密码选项**：
- 明文密码（开发环境）
- bcrypt 哈希（生产环境推荐）

**生成 bcrypt 哈希**：
```bash
# 使用 Go
go run -mod=mod ./scripts/hash-password.go "your-password"

# 使用 Python
python -c "import bcrypt; print(bcrypt.hashpw(b'your-password', bcrypt.gensalt()).decode())"

# 使用在线工具
https://bcrypt-generator.com/
```

### JWT 认证

```toml
[dashboard.auth]
mode = "jwt"

[dashboard.auth.session]
[dashboard.auth.session.jwt]
secret = "your-secret-key-here"  # 必须保密！
expiration = 3600  # 1小时
algorithm = "HS256"
```

### LDAP 认证

```toml
[dashboard.auth]
mode = "ldap"

[dashboard.auth.ldap]
enabled = true
server_url = "ldap://ldap.example.com:389"
base_dn = "dc=example,dc=com"
user_dn_template = "uid=%s,ou=users,dc=example,dc=com"
bind_dn = "cn=admin,dc=example,dc=com"
bind_password = ""
```

### OAuth2 认证

```toml
[dashboard.auth]
mode = "oauth2"

[dashboard.auth.oauth2]
provider = "github"  # github, google, gitlab, azuread

[dashboard.auth.oauth2.github]
client_id = "your-client-id"
client_secret = "your-client-secret"
callback_url = "http://localhost:7500/oauth2/callback"
scopes = ["user:email"]
```

### SAML 认证

```toml
[dashboard.auth]
mode = "saml"

[dashboard.auth.saml]
enabled = true
idp_metadata_url = "https://idp.example.com/saml/metadata"
callback_url = "http://localhost:7500/saml/callback"
entity_id = "https://aethertunnel.example.com"
certificate_file = "/path/to/cert.pem"
key_file = "/path/to/key.pem"
```

---

## 🔒 安全配置

### 速率限制

```toml
[dashboard.auth.rate_limit]
enabled = true
max_attempts_per_minute = 5
max_attempts_per_hour = 20

[dashboard.auth.rate_limit.ip]
max_attempts_per_minute = 10
block_duration = 300  # 5分钟

[dashboard.auth.rate_limit.account]
max_failed_attempts = 5
lockout_duration = 900  # 15分钟
```

### IP 白名单

```toml
[dashboard.auth.ip_whitelist]
enabled = true
allowed_ips = [
    "127.0.0.1",
    "192.168.1.0/24",  # CIDR 格式
    "10.0.0.0/8"
]
```

### IP 黑名单

```toml
[dashboard.auth.ip_blacklist]
enabled = true
blocked_ips = [
    "1.1.1.1",
    "2.2.2.2"
]
```

### 地理位置限制

```toml
[dashboard.auth.geo_restrictions]
enabled = true
allowed_countries = ["CN", "US", "JP"]
blocked_countries = ["RU"]
```

**国家代码**：ISO 3166-1 alpha-2（CN, US, JP, RU 等）

---

## 🎨 界面配置

### 主题配置

```toml
[dashboard.ui.theme]
default_theme = "dark"  # light, dark, auto
allow_theme_switch = true
available_themes = ["light", "dark", "midnight", "ocean", "forest"]
```

### 品牌配置

```toml
[dashboard.ui.branding]
app_name = "AetherTunnel"
logo_url = "/static/logo.png"
favicon_url = "/static/favicon.ico"
page_title = "AetherTunnel 管理面板"
footer_text = "Powered by AetherTunnel"
```

### 布局配置

```toml
[dashboard.ui.layout]
sidebar_position = "left"  # left, right, top, bottom
sidebar_width = 250
sidebar_collapsed = false
show_breadcrumbs = true
default_page = "overview"
```

### 语言配置

```toml
[dashboard.ui.i18n]
default_language = "zh-CN"
available_languages = [
    "zh-CN",  # 简体中文
    "zh-TW",  # 繁体中文
    "en-US",  # 英语
    "ja-JP",  # 日语
    "ko-KR"   # 韩语
]
auto_detect = true
```

---

## 🔌 API 配置

### 启用 API

```toml
[dashboard.api]
enabled = true
base_path = "/api/v1"
```

### API 认证

```toml
[dashboard.api.auth]
mode = "jwt"  # jwt, api_key, session

# JWT 配置
[dashboard.api.auth.jwt]
secret = "your-jwt-secret-key-here"
expiration = 3600
algorithm = "HS256"

# API 密钥配置
[dashboard.api.auth.api_key]
enabled = true
[[dashboard.api.auth.api_key.keys]]
name = "Production Key"
key = "sk_prod_xxxxx"
expires_at = ""
permissions = ["read", "write"]
```

### CORS 配置

```toml
[dashboard.api.cors]
enabled = true
allowed_origins = ["*"]
allowed_methods = ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
allowed_headers = ["Content-Type", "Authorization"]
max_age = 86400
```

### API 速率限制

```toml
[dashboard.api.rate_limit]
enabled = true
requests_per_minute = 60
requests_per_hour = 1000
```

---

## 🔗 集成配置

### Prometheus 集成

```toml
[dashboard.integrations.prometheus]
enabled = true
pushgateway_url = "http://pushgateway:9091"
```

### Grafana 集成

```toml
[dashboard.integrations.grafana]
enabled = true
dashboard_url = "http://grafana:3000"
api_key = ""
```

### Slack 集成

```toml
[dashboard.integrations.slack]
enabled = true
webhook_url = "https://hooks.slack.com/services/xxx"
channel = "#aethertunnel-alerts"
username = "AetherTunnel Bot"
```

### Telegram 集成

```toml
[dashboard.integrations.telegram]
enabled = true
bot_token = "your-bot-token"
chat_id = "your-chat-id"
```

### 邮件集成

```toml
[dashboard.integrations.email]
enabled = true
smtp_server = "smtp.gmail.com:587"
smtp_username = "your-email@gmail.com"
smtp_password = "your-password"
from_address = "aethertunnel@example.com"
to_addresses = ["admin@example.com"]
```

---

## 💡 最佳实践

### 安全建议

1. ✅ **始终启用认证**
   ```toml
   [dashboard.auth]
   enabled = true
   ```

2. ✅ **使用强密码**
   - 至少 12 位
   - 包含大小写字母、数字、特殊字符
   - 定期更换

3. ✅ **启用速率限制**
   ```toml
   [dashboard.auth.rate_limit]
   enabled = true
   max_attempts_per_minute = 5
   ```

4. ✅ **使用 HTTPS**
   ```toml
   [dashboard.tls]
   enabled = true
   cert_file = "/path/to/cert.pem"
   key_file = "/path/to/key.pem"
   ```

5. ✅ **限制访问**
   ```toml
   [dashboard]
   bind_addr = "127.0.0.1"  # 仅本地访问
   ```

6. ✅ **启用审计日志**
   ```toml
   [dashboard.logging.audit]
   enabled = true
   ```

### 性能优化

1. ✅ **合理设置 Session 超时**
   ```toml
   [dashboard.auth.session]
   timeout = 3600  # 1小时
   ```

2. ✅ **启用压缩**
   ```toml
   [dashboard.performance]
   compress_enabled = true
   ```

3. ✅ **配置连接池**
   ```toml
   [dashboard.performance.connection_pool]
   max_idle_connections = 100
   max_open_connections = 1000
   ```

### 生产环境配置示例

```toml
[dashboard]
enabled = true
port = 7500
bind_addr = "127.0.0.1"

[dashboard.auth]
enabled = true
mode = "basic"
username = "admin"
password = "$2a$10$..."  # bcrypt 哈希

[dashboard.auth.session]
timeout = 3600
max_concurrent_sessions = 5
idle_timeout = 1800

[dashboard.auth.rate_limit]
enabled = true
max_attempts_per_minute = 5
max_attempts_per_hour = 20

[dashboard.auth.ip_whitelist]
enabled = false  # 如果使用 VPN 或内网

[dashboard.tls]
enabled = true
cert_file = "/etc/ssl/certs/dashboard.crt"
key_file = "/etc/ssl/private/dashboard.key"

[dashboard.api]
enabled = true

[dashboard.api.auth.api_key]
enabled = true
api_keys = ["sk_prod_xxxxx"]

[dashboard.logging.audit]
enabled = true
audit_log_file = "/var/log/aethertunnel/audit.log"

[dashboard.integrations.slack]
enabled = true
webhook_url = "https://hooks.slack.com/services/xxx"
```

---

## ❓ 常见问题

### Q1: 如何修改端口？

**A**：修改 `port` 配置
```toml
[dashboard]
port = 8080  # 改为 8080
```

### Q2: 如何禁用密码认证？

**A**：禁用 `auth.enabled`（不推荐）
```toml
[dashboard.auth]
enabled = false
```

### Q3: 如何配置多个管理员？

**A**：使用用户管理配置
```toml
[[dashboard.users.admins]]
username = "admin1"
password = "$2a$10$..."
email = "admin1@example.com"

[[dashboard.users.admins]]
username = "admin2"
password = "$2a$10$..."
email = "admin2@example.com"
```

### Q4: 如何启用 HTTPS？

**A**：配置 TLS 证书
```toml
[dashboard.tls]
enabled = true
cert_file = "/path/to/cert.pem"
key_file = "/path/to/key.pem"
```

### Q5: 如何限制访问 IP？

**A**：配置 IP 白名单
```toml
[dashboard.auth.ip_whitelist]
enabled = true
allowed_ips = ["192.168.1.0/24"]
```

### Q6: 如何启用 API？

**A**：配置 API
```toml
[dashboard.api]
enabled = true
```

### Q7: 如何配置 LDAP 认证？

**A**：参考 LDAP 配置
```toml
[dashboard.auth]
mode = "ldap"

[dashboard.auth.ldap]
enabled = true
server_url = "ldap://ldap.example.com:389"
```

### Q8: 如何启用 OAuth2？

**A**：配置 OAuth2
```toml
[dashboard.auth]
mode = "oauth2"

[dashboard.auth.oauth2]
provider = "github"
```

### Q9: 如何配置 Slack 通知？

**A**：配置 Slack 集成
```toml
[dashboard.integrations.slack]
enabled = true
webhook_url = "https://hooks.slack.com/services/xxx"
```

### Q10: 如何启用审计日志？

**A**：配置审计日志
```toml
[dashboard.logging.audit]
enabled = true
audit_log_file = "/var/log/aethertunnel/audit.log"
```

---

## 📚 相关文档

- [完整配置示例](dashboard-full-config.example)
- [快速配置示例](dashboard-quick-config.example)
- [安全最佳实践](SECURITY.md)
- [API 文档](API.md)

---

<div align="center">

**🎉 祝你配置顺利！**

如有问题，请参考相关文档或联系支持团队。

Made with ❤️ by AetherTunnel Team

</div>
