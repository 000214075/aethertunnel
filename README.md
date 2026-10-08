# AetherTunnel

**中文** · [English](README.en.md)

[![Release](https://img.shields.io/github/v/tag/000214075/aethertunnel?label=release)](https://github.com/000214075/aethertunnel/releases)
[![License](https://img.shields.io/github/license/000214075/aethertunnel?label=license)](LICENSE)

AetherTunnel 是一个专注于内网穿透的反向代理应用，支持 TCP、UDP、HTTP、HTTPS、SOCKS5 等
九种代理类型，且支持 P2P 通信。可以把没有公网 IP 的机器（家里的 NAS、公司的开发机、树莓派）
上的服务，通过一台有公网 IP 的服务器安全、便捷地发布到公网；也可以把客户端作为库嵌进你
自己的程序。

```
访问者 ──► 服务器公网端口 ──► [隧道] ──► 客户端 ──► 本地服务 127.0.0.1:22
```

**从 frp 过来？** 配置词汇有意相似（`[[proxies]]`、访客、`remote_port`、`stcp`/`xtcp`、
按代理限速），对照表、差异与「有意不抄」的地方见 [`docs/VS-FRP.md`](docs/VS-FRP.md)；
frp 的键名（`bindPort`、`localIP`、`customDomains`、`[common]`）粘过来时，加载器会逐项
写出本项目该写什么。

## 为什么使用 AetherTunnel ？

- **九种代理类型**：`tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5` `tcpmux`——
  从整机端口转发、按域名分流的 web 服务，到只给拿到口令者的私有隧道与正向代理。
- **代理池与负载均衡**：同名代理可由多个客户端组成池，`round-robin` `random` `latency`
  `failover` `adaptive` 五种策略，外加按每条流应答速度在线学习的 `bandit`；成员进出不断流。
- **P2P 打洞**：`xtcp` 打洞成功后访客与客户端直连，流量不经过服务器中转；打不通自动回退中继。
- **安全**：可选端到端加密（XChaCha20-Poly1305 / AES-256-GCM）与后量子混合协商
  （X25519 + ML-KEM-768）；TLS 传输、Ed25519 主机身份、来源白/黑名单、令牌桶限流、
  认证失败自动封禁、JSONL 审计；`stcp`/`xtcp` 的访客可选用 zk-SNARK 证明登录，全程不发送密钥。
- **去中心化目录**：内置 Kademlia DHT，只凭公钥按名字解析服务，通告带签名、有 TTL、可撤回。
- **可离线核验的带宽账本**：Ed25519 签名 + 哈希链的用量记录，**只凭公钥即可离线核验**，
  支持导出到第 n 条为止的前缀交给审计方。
- **三层隧道**：Linux（TUN）/ Windows（Wintun）/ macOS（utun）打开真实设备，Android 由 App 的
  VpnService 提供接口；没有设备可用时明确报错退出，不会静默降级。
- **原生客户端插件**：静态文件发布、HTTP 反向代理、TLS 终结与协议转换（`https2http`、
  `tls2raw` 等）、正向代理；不接本地服务也能当一个独立小工具用。
- **服务端与客户端都带 Web 面板**：中英双语、手机可用；Prometheus 指标、`/healthz` 与
  `/readyz` 探针、优雅关闭；配置校验会把未知配置项**报出来**，不静默忽略。
- **三种形态，一份实现**：命令行程序、可嵌入库 `pkg/clientlib`、Android App（gomobile 绑定 +
  最小 App）；`Dockerfile` 与 `deploy/kubernetes/` 清单开箱可用。

每一项能力都有仓库里能自己跑的检查；细节在各目录的 README 与 [`docs/`](docs/README.md) 里。

## 30 秒上手

```bash
# 1. 下载对应平台的二进制（Release 页面），或在源码目录自己构建
go build -o aethertunnel-server .          # 服务端
go build -o aethertunnel-client ./client   # 客户端
```

服务端 `server.toml`：

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "把这里换成一串至少 16 位的随机字符"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"   # 想对外暴露就必须设置 token
port = 7500
```

客户端 `client.toml`：

```toml
[client]
server_addr = "你的服务器IP:7001"
auth_token = "与服务端完全一致"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 6022        # 服务器上对外开放的端口
```

```bash
./aethertunnel-server --config server.toml      # 服务端
./aethertunnel-client --config client.toml      # 客户端（放在内网机器上）
ssh -p 6022 user@你的服务器IP                    # 从任何地方访问
```

在服务器本机打开 `http://127.0.0.1:7500/` 就是面板，显示在线客户端、已注册的隧道、实时流量；想让别的机器访问，把 `dashboard.bind_addr` 改成 `0.0.0.0` 并设置 `token`。

## 常用用法

### 把内网 web 服务发布到域名

`http`/`https` 代理走服务端的共享监听，按请求的 Host 选隧道——服务端不必为每个服务开端口。

```toml
# 服务端
[server]
bind_port = 7001
http_port = 7080                       # http/https 代理的共享监听端口
subdomain_host = "tunnel.example.com"  # 客户端只声明标签时的域名后缀
```

```toml
# 客户端
[[proxies]]
name = "web"
type = "http"
local_port = 8000
domains = ["app.example.com"]   # 也可以只写 subdomain = "app"，由服务端拼成 app.tunnel.example.com
```

### 给手机一个 socks5 出口

让手机（或任何只会说 SOCKS5 的程序）从客户端所在的网络出网：

```toml
[[proxies]]
name = "phone-exit"
type = "socks5"
remote_port = 6100
allow_targets = ["192.168.1.0/24"]   # 必填：允许拨号的地址范围，范围外一律拒绝
```

手机上把 SOCKS5 代理指向 `服务器IP:6100` 即可。

### 私有隧道：只给拿到口令的人

```toml
# 发布方：这条隧道不占任何公网端口
[[proxies]]
name = "db"
type = "stcp"
local_port = 5432
secret_key = "只有访问方知道的一串随机字符"
```

```toml
# 访问方：在本地开一个端口，转发进这条私有隧道
[[visitors]]
name = "db-visitor"
type = "stcp"
server_name = "db"
bind_addr = "127.0.0.1"
bind_port = 15432
secret_key = "与发布方完全一致"
```

```bash
psql -h 127.0.0.1 -p 15432            # 口令不对就拿不到任何数据
```

### 把客户端嵌进你自己的程序

```go
cfg, err := config.LoadClient("client.toml")
if err != nil {
    return err
}
// logger 是标准库的 *log.Logger；Run 一直运行到 ctx 被取消
return clientlib.Run(ctx, cfg, logger)
```

## 仓库结构

每个目录有自己的 README，介绍词写的是它实际做的事。

| 目录 | 内容 |
| --- | --- |
| [`client/`](client/) | `aethertunnel-client` 命令入口（旗标、配置、信号） |
| [`pkg/`](pkg/) | 全部实现包：clientlib、server、vpn、obfs、snarkauth、webrtcvisitor……每个子包各有 README |
| [`mobile/`](mobile/) | gomobile 绑定与 Android 最小 App（含 VpnService 壳） |
| [`web/`](web/) | 内嵌面板 |
| [`deploy/`](deploy/) | Kubernetes 部署清单 |
| [`scripts/`](scripts/) | 构建、功能套件与各平台实测脚本 |
| [`tools/`](tools/) | SNARK 电路与密钥生成 |
| [`docs/`](docs/) | 架构、配置参考、平台记录、安全模型、设计边界 |

## 设计边界

区块链/代币、以及移动端的商店应用与设备级 VPN 两项
是**有意的设计取舍**，不是待办事项；每一项的考虑、以及同样目的下本程序可用的做法，
见 [`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md)。（客户端实现是可嵌入的库，仓库里还带一个能构建出 debug APK 的最小 Android App。）

## 加密怎么开

两端必须配置一致，否则连接会在握手阶段报明确的 `encryption mismatch`：

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # 或 "aes-256-gcm"
passphrase = ""                     # 留空 = 用 auth_token 派生
salt = "aethertunnel"               # 两端必须相同
post_quantum = true                 # 再用 X25519 + ML-KEM-768 协商每条连接的密钥
```

密钥不是 auth_token 本身，而是 `HKDF-SHA256(passphrase, salt)` 派生的 32 字节；
每个数据包（控制帧）与每条隧道记录都有独立随机 nonce，篡改会被 AEAD 拒绝并断开连接。
开启 `post_quantum` 后，会话密钥由 X25519 与 ML-KEM-768 两个共享秘密共同派生，
每条数据连接再用 `HKDF(session_key, stream_id)` 单独派生一把密钥。

## 面板

- 资源用 `go:embed` 打进二进制，**不需要**在二进制旁边放 `web/` 目录。
- 默认只监听 `127.0.0.1`。要让外部访问，请设置 `[dashboard].token`，之后 `/api/*` 需要
  `Authorization: Bearer <token>`；`/api/health`、`/healthz`、`/readyz` 始终公开且不含敏感信息。
- 面板轮询 `/api/status`、`/api/clients`、`/api/proxies`，**屏幕上每个数字都来自服务器实时状态**；
  没有客户端时显示空状态。
- `/api/ledger` 返回公钥、链头、最近的账本条目与按客户端汇总的用量；条目本身已签名，
  拿到响应的人可以独立校验。面板的「账本」页渲染的就是这份响应（`?limit=` 限制条目数）。
- `/api/dht` 返回 DHT 节点标识、绑定地址、已知节点数、对外通告的主机名、当前已通告的代理名，
  以及 `signing_key`（通告用的 Ed25519 公钥；未签名部署为空串）。面板的「目录」页渲染的就是
  这份响应。

## 运维

```bash
# 校验配置
./aethertunnel-server --config server.toml --check

# 按名字查一条代理记录（用服务端配置即可，查询节点自己绑临时端口）
./aethertunnel-server --config server.toml --dht-lookup ssh

# 读出通告签名公钥，填进客户端的 dht.trusted_keys
./aethertunnel-server --config server.toml --dht-key

# 从客户端角色解析同一个名字
./aethertunnel-client --config client.toml --discover ssh

# 读出本客户端的身份公钥，填进服务端的 identity.allowed_keys
./aethertunnel-client --config client.toml --identity

# 通过一个 socks5 出口访问内网地址
curl --socks5-hostname 服务器IP:6100 http://10.0.0.5:8080/

# 离线核对带宽账本，只需要公钥
./aethertunnel-server --verify-ledger ledger.jsonl --ledger-key <64 位十六进制公钥>

# 导出到第 41 条为止的账本前缀：给审计方这一段用量，不必交出整条链
./aethertunnel-server --ledger-proof ledger.jsonl --proof-index 41 > proof.jsonl
```

封禁与按代理 ACL 都不需要额外命令，但它们留下的痕迹可以这样看（`/metrics` 挂在面板端口上，需要 `[metrics] enabled = true`，`server.toml.example` 里已开启）：

```bash
curl -s http://127.0.0.1:7500/metrics | grep -E 'sources_banned|banned_connections_refused|visitors_denied_by_proxy|socks5_requests'
grep -E 'source_banned|ban_refused|proxy_visitor_denied' aethertunnel-audit.jsonl
```

每条能力都有仓库里可直接复跑的检查（功能套件、真实 tun 设备上的三层隧道、真实 k3s 上的
部署清单、真实浏览器里的面板）；命令见下面「构建与测试」，各脚本的作用见
[`scripts/README.md`](scripts/README.md)。

想手工做一遍优雅关闭：起一台服务器、保持一条流，然后 Linux 与 macOS 上 `kill -TERM <PID>`
（Windows 用不带 `/F` 的 `taskkill /PID <PID>`；`Stop-Process` 是硬杀，会跳过整个排空过程），
日志里应出现 `shutting down:` 与一行 `graceful shutdown: ...`。

## 构建与测试

```bash
make build          # 本机两个二进制 → bin/
make test           # 单元 + 端到端测试
make test-race      # 同上，带竞态检测（需要 CGO 与 C 编译器；没有编译器时 scripts/race-toolchain.sh 会解出一个，不需要 root）
make lint           # gofmt 未格式化的文件 + go vet
make vet
make cross          # 12 个产物 + dist/SHA256SUMS
make check          # 校验示例配置（未知键直接失败）
```

跑检查（Linux 与 arm64 只绑回环端口、不需要 root）：

```bash
scripts/functional-linux.sh bin/aethertunnel-server bin/aethertunnel-client
# 关键行：ALL LINUX FUNCTIONAL CHECKS PASSED
```

三层隧道与 Kubernetes 部署各有一个实测脚本，需要 root 与额外组件；缺任何一样会打印缺什么
并以 0 退出，不会把环境问题误报成失败（脚本说明见 [`scripts/README.md`](scripts/README.md)）：

```bash
sudo bash scripts/vpn-linux-test.sh bin/aethertunnel-server bin/aethertunnel-client
# 关键行：ALL LAYER-3 CHECKS PASSED
sudo bash scripts/kubernetes-linux.sh bin/aethertunnel-server bin/aethertunnel-client
# 关键行：ALL KUBERNETES CHECKS PASSED
```

没有 arm64 机器时，`scripts/emulate-linux-arm64.sh <qemu-aarch64> [sysroot]` 交叉编译出 arm64
产物、用 qemu 跑整套单测，再把包装脚本交给 `scripts/functional-linux.sh` 跑整套功能检查
（静态产物不需要 sysroot）。

Windows 无 make 时：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1 -Version v1.0.0
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1
```

## 安全模型

- 认证可以是共享密钥（`auth_token`）或 Ed25519 身份，两者都不是证书；token 用常数时间比较，
  失败时**不会**把 token 写进日志。
- `[encryption]` 保护负载，`[transport] enable_tls` 提供传输层加密与服务器证书认证，
  `[obfuscation] disguise` 只改变外观、不提供任何机密性。
- 面板 API 只有 Bearer token 一种保护，没有登录会话、没有多用户。
- 私有隧道（stcp/sudp/xtcp）的 secret_key 用 `auth_method = "nizk"` 时不会出现在线上，
  但代理的元数据（名字、类型、地址）会出现在 DHT 里，除非把 `[dht]` 关掉；
  启用 `[dht] signing_key_file` 后这些记录带签名，读取端可以据此拒绝改写的记录。
- 自动封禁按**来源地址**记账：部署在负载均衡、反向代理或监控系统后面时，那些地址必须写进
  `ban_ignore_cidrs`，否则它们会与攻击者共享同一个来源地址并被一起拒绝。

详见 [`docs/SECURITY.md`](docs/SECURITY.md)。

---

## 参与贡献

- **报告问题**：bug 与功能建议各有一个表单（见 [`.github/ISSUE_TEMPLATE/`](.github/ISSUE_TEMPLATE/)）；
  **安全问题不要开公开 issue**，按 [`docs/SECURITY.md`](docs/SECURITY.md) 私下报告。
- **改代码**：[`CONTRIBUTING.md`](CONTRIBUTING.md) 写明本地开发、检查清单与「一个 PR 只做一件事」；
  行为准则见 [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md)。
- **每个能力都有可执行的检查**：新能力随 PR 带上测试与功能套件的一节，是本仓库的门槛，也是
  能力清单里每一条都可信的原因。

## License

MIT — see [LICENSE](LICENSE).
