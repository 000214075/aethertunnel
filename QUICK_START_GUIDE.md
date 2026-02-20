# 🚀 AetherTunnel 小白快速开始指南

**5 分钟内让你的内网服务可以被外网访问！**

---

## 📋 准备工作

### 你需要什么

1. ✅ **一台服务器**（VPS 或云服务器）
   - 阿里云、腾讯云、华为云、AWS 等都可以
   - 操作系统：Linux（推荐）、Windows、macOS
   - 需要：1 核心 CPU、512MB 内存即可

2. ✅ **一个域名**（可选，但推荐）
   - 如果你想用域名访问服务，需要一个
   - 例如：`myserver.example.com`
   - 如果没有，可以直接用 IP 访问

3. ✅ **AetherTunnel 程序**
   - 从 Releases 下载对应平台的二进制文件
   - 或自行编译

### 你想穿透什么服务

常见的服务类型：
- 🔹 SSH 远程连接（端口 22）
- 🔹 Web 网站（端口 80/443）
- 🔹 MySQL 数据库（端口 3306）
- 🔹 Redis 数据库（端口 6379）
- 🔹 远程桌面（端口 3389）
- 🔹 游戏服务器（端口如 25565）

---

## 🎯 5 分钟快速上手

### 第 1 步：准备服务器（1 分钟）

1. **购买服务器**
   - 选择云服务商
   - 购买最便宜的配置即可
   - 获取服务器 IP 地址（如：123.45.67.89）

2. **开放防火墙端口**
   - 在云服务商控制台
   - 开放端口：**7000**（AetherTunnel 通信端口）
   - 如果要 SSH，再开放：**2222**（穿透后的 SSH 端口）
   - 协议：TCP

### 第 2 步：配置服务端（1 分钟）

1. **下载配置文件**
   ```bash
   # 下载简化版服务端配置
   wget https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/server-simple.toml
   ```

2. **修改配置文件**
   ```bash
   # 编辑配置文件
   vi server-simple.toml
   ```

3. **只修改这两项！**
   ```toml
   [server]
   # 改成一个非默认端口，比如 7001
   bind_port = 7001

   # 改成一个强随机字符串（至少 16 位）
   # 生成随机 token 的方法：
   # Linux: openssl rand -hex 16
   # Windows: 在线搜索"random string generator"
   auth_token = "a3f9e2c1b8d4e6f5a7"  # ⚠️ 记住这个！
   ```

4. **保存并退出**

### 第 3 步：启动服务端（1 分钟）

```bash
# 赋予执行权限
chmod +x aethertunnel-server

# 启动服务端
./aethertunnel-server server-simple.toml

# 如果成功，你会看到：
# 📦 AetherTunnel Server started on :7001
# ✅ Authentication token: a3f9e2c1b8d4e6f5a7
```

### 第 4 步：配置客户端（1 分钟）

1. **下载配置文件**
   ```bash
   # 下载简化版客户端配置
   wget https://github.com/aethertunnel/aethertunnel/releases/download/v0.1.0/client-simple.toml
   ```

2. **修改配置文件**
   ```bash
   # 编辑配置文件
   vi client-simple.toml
   ```

3. **修改这三个必填项！**
   ```toml
   [client]
   # 改成你的服务器 IP 地址
   server_addr = "123.45.67.89"  # ⚠️ 改成你的服务器 IP！

   # 改成你服务端设置的端口
   server_port = 7001  # ⚠️ 和服务端一致！

   # 改成和服务端一样的 token
   auth_token = "a3f9e2c1b8d4e6f5a7"  # ⚠️ 和服务端一致！
   ```

4. **添加你想穿透的服务（最常用的：SSH）**
   ```toml
   # 远程访问家里的电脑 SSH

   [[proxies]]
   name = "home-ssh"  # 给这个代理起个名字
   type = "tcp"        # 类型：tcp = 最常用
   local_ip = "127.0.0.1"  # 本地地址（不要改）
   local_port = 22     # SSH 默认端口（不要改）
   remote_port = 2222  # 外部访问的端口（不要改）
   ```

5. **保存并退出**

### 第 5 步：启动客户端（1 分钟）

```bash
# 赋予执行权限
chmod +x aethertunnel-client

# 启动客户端
./aethertunnel-client client-simple.toml

# 如果成功，你会看到：
# 📦 AetherTunnel Client connected to 123.45.67.89:7001
# ✅ Proxy 'home-ssh' started
```

### 第 6 步：测试连接（30 秒）

现在，你可以在**外面的电脑**上连接你家里的电脑了！

```bash
# SSH 连接命令
ssh -p 2222 root@123.45.67.89

# 如果配置了用户名
ssh -p 2222 your-username@123.45.67.89
```

**成功了！** 🎉 你现在可以从外网访问你家里的电脑了！

---

## 🌟 更多场景示例

### 场景 2：远程访问家里的网站

```toml
[[proxies]]
name = "my-website"
type = "http"
local_ip = "127.0.0.1"
local_port = 80
remote_port = 8080

# 如果你有域名，可以配置
custom_domains = ["www.example.com"]
```

**访问方式**：`http://123.45.67.89:8080` 或 `http://www.example.com`

### 场景 3：远程访问 MySQL 数据库

```toml
[[proxies]]
name = "mysql"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 3306
remote_port = 3306

# 建议开启加密
use_encryption = true
```

**访问方式**：MySQL 连接工具连接 `123.45.67.89:3306`

### 场景 4：远程访问游戏服务器

```toml
[[proxies]]
name = "minecraft"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 25565
remote_port = 25565
```

**访问方式**：游戏客户端连接 `123.45.67.89:25565`

### 场景 5：HTTPS 网站（需要证书）

```toml
[[proxies]]
name = "https-site"
type = "https"
local_ip = "127.0.0.1"
local_port = 443
custom_domains = ["secure.example.com"]

# TLS 配置（需要证书文件）
[tls]
enabled = true
cert_file = "/path/to/cert.pem"
key_file = "/path/to/key.pem"
```

---

## ❓ 常见问题

### Q1: 启动报错怎么办？

**问题**：
```
panic: bind: address already in use
```

**原因**：端口被占用

**解决**：
1. 检查端口是否被其他程序占用
```bash
# Linux
netstat -tuln | grep 7001

# Windows
netstat -ano | findstr :7001
```
2. 修改配置文件，换个端口

---

### Q2: 连接不上怎么办？

**检查清单**：
1. ✅ 服务器端是否正常运行？
   ```bash
   # 查看服务端日志
   ./aethertunnel-server --log-level debug
   ```

2. ✅ 防火墙端口是否开放？
   ```bash
   # 检查端口是否开放
   telnet 123.45.67.89 7001
   ```

3. ✅ 配置是否正确？
   - server_addr 是否正确？
   - server_port 是否正确？
   - auth_token 是否一致？

4. ✅ 服务器是否有资源限制？
   - 检查 CPU 和内存使用
   - 检查磁盘空间

---

### Q3: 忘记 auth_token 怎么办？

**方法 1：查看服务端日志**
```bash
./aethertunnel-server --log-level debug
# 日志会显示当前的 auth_token
```

**方法 2：重新生成**
1. 停止服务端
2. 修改配置文件，重新生成 auth_token
3. 重启服务端
4. 同步更新客户端的 auth_token

---

### Q4: 想要更多功能怎么办？

AetherTunnel 有很多高级功能，比如：
- 🔒 TLS 加密连接
- 📊 Web 管理界面
- 🛡️ 访问控制（IP 白名单）
- 📝 审计日志
- 🤖 AI 智能路由
- 🌐 WebRTC P2P 直连
- ⛓️ 去中心化 DHT 网络
- 🔬 量子抗性加密
- ... 等等 20+ 项颠覆性功能

**如何开启？**
- 查看 `server-toml-innovative-addon.example`
- 查看 `client-toml-innovative-addon.example`
- 参考文档：`docs/INNOVATIVE_FEATURES.md`

---

## 🔒 生产环境安全建议

### 小白（初次使用）

- ✅ 使用简化的配置文件
- ✅ 所有默认配置即可
- ✅ 不要对外暴露管理面板

### 进阶（生产环境）

- ✅ **使用强随机 token**（至少 32 位）
- ✅ **启用 TLS 加密**
- ✅ **启用防火墙规则**
- ✅ **修改默认端口**
- ✅ **启用审计日志**
- ✅ **配置 IP 白名单**
- ✅ **定期更新程序**

### 高级（企业级）

- ✅ **启用所有安全特性**
- ✅ **配置 failover**
- ✅ **启用监控和告警**
- ✅ **使用数据库持久化**
- ✅ **配置负载均衡**
- ✅ **开启颠覆性功能**

---

## 📖 更多学习资源

### 官方文档
- [完整配置指南](CONFIG_COMPARISON.md)
- [创新功能详解](INNOVATIVE_FEATURES.md)
- [Web 管理面板配置](DASHBOARD_CONFIG.md)
- [编译指南](docs/BUILD.md)

### 社区资源
- [GitHub Issues](https://github.com/aethertunnel/aethertunnel/issues) - 报告 Bug
- [GitHub Discussions](https://github.com/aethertunnel/aethertunnel/discussions) - 提问交流
- [GitHub Wiki](https://github.com/aethertunnel/aethertunnel/wiki) - 更多文档

### 视频教程（计划中）
- 基础入门
- 常见问题排查
- 高级功能讲解
- 最佳实践

---

## 🎉 恭喜！

你已经成功配置并运行了 AetherTunnel！

现在你可以：
- ✅ 从任何地方访问家里的电脑
- ✅ 访问家里的网站和服务
- ✅ 给朋友分享你的内网服务
- ✅ 玩转你自己的云服务器

---

<div align="center">

**🚀 享受 AetherTunnel 带来的便利！**

**遇到问题？** 查看文档或提交 Issue

Made with ❤️ by AetherTunnel Team

</div>
