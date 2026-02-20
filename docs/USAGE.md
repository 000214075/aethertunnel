# AetherTunnel 使用文档

## 目录

1. [快速开始](#快速开始)
2. [安装](#安装)
3. [配置](#配置)
4. [使用示例](#使用示例)
5. [高级功能](#高级功能)
6. [故障排查](#故障排查)
7. [常见问题](#常见问题)

## 快速开始

### 30 秒快速体验

#### 1. 下载程序

从 [Releases](https://github.com/aethertunnel/aethertunnel/releases) 下载适合你平台的二进制文件。

#### 2. 编写配置文件

服务端 `server.toml`：

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "my-secret-token"

[security]
heartbeat_timeout = 90
```

客户端 `client.toml`：

```toml
[client]
server_addr = "your-server-ip"
server_port = 7000
auth_token = "my-secret-token"

[[proxies]]
name = "web"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 80
remote_port = 8080
```

#### 3. 运行

```bash
# 服务端
./aethertunnel-server ../server.toml

# 客户端
./aethertunnel-client ../client.toml
```

#### 4. 访问

现在可以通过 `http://your-server-ip:8080` 访问本地 80 端口的服务。

## 安装

### 方式 1：二进制安装（推荐）

从 GitHub Releases 下载适合的版本：

```bash
# Linux x86_64
wget https://github.com/aethertunnel/aethertunnel/releases/download/v1.0.0/aethertunnel-1.0.0-linux-amd64.tar.gz
tar xzf aethertunnel-1.0.0-linux-amd64.tar.gz
cd linux_amd64
chmod +x aethertunnel-server aethertunnel-client
```

### 方式 2：从源码编译

```bash
# 克隆仓库
git clone https://github.com/aethertunnel/aethertunnel.git
cd aethertunnel

# 编译
go build -o aethertunnel-server ./server
go build -o aethertunnel-client ./client
```

### 方式 3：使用 Go install

```bash
go install github.com/aethertunnel/aethertunnel/server@latest
go install github.com/aethertunnel/aethertunnel/client@latest
```

## 配置

### 服务端配置

#### 基础配置

```toml
[server]
bind_addr = "0.0.0.0"        # 监听地址
bind_port = 7000             # 控制端口
auth_token = "your-token"    # 认证令牌

[dashboard]
enabled = true               # 启用仪表板
port = 7500                  # 仪表板端口
username = "admin"           # 用户名
password = "admin"           # 密码
```

#### TLS 配置（推荐生产环境）

```toml
[tls]
enabled = true
cert_file = "/path/to/server.crt"
key_file = "/path/to/server.key"
ca_file = "/path/to/ca.crt"          # 用于验证客户端
client_auth = true                    # 要求客户端证书
min_version = "TLS1.2"
```

#### 安全配置

```toml
[security]
enable_ip_whitelist = true
allowed_ips = [
    "192.168.1.0/24",
    "10.0.0.0/8"
]
max_connections_per_client = 10
heartbeat_timeout = 90
connection_timeout = 10
enable_audit_log = true
audit_log_file = "/var/log/aethertunnel/audit.log"
rate_limit = 100
block_duration = "5m"
```

#### 传输配置

```toml
[transport]
tcp_mux = true                         # 启用 TCP 多路复用
tcp_mux_keepalive_interval = 60
tcp_keepalive = 30
max_pool_count = 5                     # 工作连接池大小
```

#### 代理配置

```toml
[proxy]
bind_addr = "0.0.0.0"
allow_ports = "8000-9000"              # 允许的端口范围
```

### 客户端配置

#### 基础配置

```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "your-token"
user = "client1"                      # 用户标识
client_id = ""                         # 空则自动生成
pool_count = 1                         # 工作连接池大小
```

#### TLS 配置

```toml
[tls]
enabled = true
cert_file = "/path/to/client.crt"
key_file = "/path/to/client.key"
ca_file = "/path/to/ca.crt"           # 用于验证服务端
tls_server_name = "your-server.com"    # SNI
```

#### 代理配置

##### TCP 代理

```toml
[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
use_encryption = false
use_compression = false
```

##### HTTP 代理

```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]
http_user = ""
http_pwd = ""
host_header_rewrite = "backend.local"
```

##### HTTPS 代理

```toml
[[proxies]]
name = "web-secure"
type = "https"
local_ip = "127.0.0.1"
local_port = 443
custom_domains = ["secure.example.com"]
```

##### STCP 代理（安全 TCP）

```toml
[[proxies]]
name = "secret-service"
type = "stcp"
local_ip = "127.0.0.1"
local_port = 3306
sk = "my-secret-key"                   # 共享密钥
allow_users = ["user1", "user2"]
```

##### XTCP 代理（P2P）

```toml
[[proxies]]
name = "p2p-service"
type = "xtcp"
local_ip = "127.0.0.1"
local_port = 22
sk = "my-secret-key"
```

##### 带健康检查的代理

```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]

[proxies.health_check]
type = "http"                          # tcp 或 http
interval = "10s"                       # 检查间隔
timeout = "3s"                         # 超时时间
max_failed = 3                         # 最大失败次数
url_or_path = "/health"                # HTTP 健康检查路径
```

## 使用示例

### 示例 1：穿透 SSH

场景：远程访问内网 Linux 服务器的 SSH 服务。

**服务端配置：**

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "secure-token"

[security]
heartbeat_timeout = 90
```

**客户端配置：**

```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "secure-token"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222
```

**使用：**

```bash
ssh -p 2222 your-user@your-server.com
```

### 示例 2：穿透 Web 服务

场景：将内网的 Web 服务暴露到公网。

**服务端配置：**

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
vhost_http_port = 80
vhost_https_port = 443
auth_token = "web-token"
```

**客户端配置：**

```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "web-token"

[[proxies]]
name = "blog"
type = "http"
local_ip = "127.0.0.1"
local_port = 8080
custom_domains = ["blog.example.com"]
```

**DNS 配置：**

```
blog.example.com A your-server-ip
```

**访问：**

```
http://blog.example.com
```

### 示例 3：穿透多个服务

场景：同时穿透 Web、SSH 和数据库。

**客户端配置：**

```toml
[client]
server_addr = "your-server.com"
server_port = 7000
auth_token = "multi-token"

[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["web.example.com"]

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222

[[proxies]]
name = "mysql"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 3306
remote_port = 3306
use_encryption = true
```

### 示例 4：使用 TLS 安全连接

场景：生产环境，要求所有连接都加密。

**生成证书：**

```bash
# 生成 CA
openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 \
  -keyout ca.key -out ca.crt -subj "/CN=AetherTunnel CA"

# 生成服务端证书
openssl req -newkey rsa:4096 -sha256 -days 3650 \
  -keyout server.key -out server.csr \
  -subj "/CN=server.example.com"
openssl x509 -req -sha256 -days 3650 \
  -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt

# 生成客户端证书
openssl req -newkey rsa:4096 -sha256 -days 3650 \
  -keyout client.key -out client.csr -subj "/CN=client"
openssl x509 -req -sha256 -days 3650 \
  -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt
```

**服务端配置：**

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "secure-token"

[tls]
enabled = true
cert_file = "server.crt"
key_file = "server.key"
ca_file = "ca.crt"
client_auth = true
```

**客户端配置：**

```toml
[client]
server_addr = "server.example.com"
server_port = 7000
auth_token = "secure-token"

[tls]
enabled = true
cert_file = "client.crt"
key_file = "client.key"
ca_file = "ca.crt"
tls_server_name = "server.example.com"
```

## 高级功能

### 1. 负载均衡

多个客户端共享同一个代理：

```toml
[[proxies]]
name = "web"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 80
remote_port = 8080
group = "web-group"
group_key = "web-group-key"
```

### 2. 带宽限制

限制代理带宽：

```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]
bandwidth_limit = "1MB"    # 限制为 1MB/s
bandwidth_limit_mode = "client"  # client 或 server
```

### 3. 自定义元数据

```toml
[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
custom_domains = ["www.example.com"]

[proxies.metas]
env = "production"
owner = "team-a"
version = "1.0.0"
```

### 4. 仪表板

访问仪表板查看状态：

```
http://your-server:7500
```

功能：
- 查看在线客户端
- 查看代理状态
- 查看连接统计
- 查看实时日志

### 5. 热重载（计划中）

发送信号重载配置：

```bash
kill -HUP $(pidof aethertunnel-server)
kill -HUP $(pidof aethertunnel-client)
```

## 故障排查

### 1. 连接失败

**症状**：客户端无法连接到服务器

**排查步骤**：

1. 检查网络连通性
```bash
ping your-server.com
telnet your-server.com 7000
```

2. 检查服务端日志
```bash
tail -f /var/log/aethertunnel/server.log
```

3. 检查防火墙
```bash
sudo iptables -L -n | grep 7000
```

4. 检查配置文件中的 Token 是否一致

### 2. 认证失败

**症状**：登录被拒绝

**排查步骤**：

1. 检查 Token 配置
2. 检查 TLS 证书（如果启用）
3. 检查 IP 白名单
4. 查看审计日志

### 3. 代理无法访问

**症状**：代理已注册但无法访问

**排查步骤**：

1. 检查本地服务是否运行
```bash
curl http://127.0.0.1:local_port
```

2. 检查端口是否被占用
```bash
netstat -tuln | grep remote_port
```

3. 检查服务器防火墙
```bash
sudo iptables -L -n | grep remote_port
```

### 4. 心跳超时

**症状**：客户端被断开

**排查步骤**：

1. 检查网络稳定性
2. 增加心跳超时时间
```toml
[security]
heartbeat_timeout = 180
```

3. 检查服务器负载

### 5. 性能问题

**症状**：连接延迟高、吞吐量低

**排查步骤**：

1. 启用 TCP 多路复用
```toml
[transport]
tcp_mux = true
```

2. 增加连接池大小
```toml
[client]
pool_count = 5
```

3. 检查网络带宽

## 常见问题

### Q1: 如何修改端口？

**A:** 修改配置文件中的 `bind_port`（服务端）或 `server_port`（客户端）。

### Q2: 如何同时穿透多个服务？

**A:** 在客户端配置中添加多个 `[[proxies]]` 块。

### Q3: 如何限制只有特定 IP 能访问？

**A:** 配置 IP 白名单：
```toml
[security]
enable_ip_whitelist = true
allowed_ips = ["1.2.3.4", "192.168.1.0/24"]
```

### Q4: 如何查看连接统计？

**A:** 访问仪表板或查看日志。

### Q5: 如何实现高可用？

**A:** 部署多个服务器，客户端配置多个服务器地址（需要修改代码支持）。

### Q6: 支持 UDP 吗？

**A:** 支持，在代理配置中设置 `type = "udp"`。

### Q7: 如何升级？

**A:** 停止服务，替换二进制文件，重新启动。

### Q8: 数据会经过服务器吗？

**A:** 默认是会经过的。如果使用 XTCP 类型，会尝试 P2P 直连。

### Q9: 安全吗？

**A:** AetherTunnel 采用了多层安全机制，比 frp 更安全。详见 [安全文档](SECURITY.md)。

### Q10: 可以商用吗？

**A:** 可以，项目采用 MIT 许可证。

## 获取帮助

- 文档：查看项目根目录下的 `docs/` 文件夹
- Issues：[GitHub Issues](https://github.com/aethertunnel/aethertunnel/issues)
- 讨论区：[GitHub Discussions](https://github.com/aethertunnel/aethertunnel/discussions)

## 贡献

欢迎贡献代码、报告 Bug、提出建议！

如需了解更多贡献信息，请查看：
- [GitHub Issues](https://github.com/aethertunnel/aethertunnel/issues) - 报告 Bug
- [GitHub Discussions](https://github.com/aethertunnel/aethertunnel/discussions) - 讨论交流
- [Pull Requests](https://github.com/aethertunnel/aethertunnel/pulls) - 贡献代码
