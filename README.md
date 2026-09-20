# AetherTunnel

**一个能把内网服务发布到公网的 TCP 隧道工具 · A small TCP tunnel that publishes a service behind NAT.**
服务端 + 客户端 + 内置 Web 面板，纯 Go，无 CGO，六个平台开箱可用。
Server, client and a built-in web panel. Pure Go, no CGO, cross-compiled for six platforms.

[![CI](https://github.com/000214075/aethertunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/000214075/aethertunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/tag/000214075/aethertunnel?label=release)](https://github.com/000214075/aethertunnel/releases)
[![License](https://img.shields.io/github/license/000214075/aethertunnel?label=license)](LICENSE)

---

## 中文说明

### 这是什么

AetherTunnel 让一台没有公网 IP 的机器（家里的 NAS、公司的开发机、树莓派）把本地端口
发布到一台有公网 IP 的服务器上。访问者连服务器的端口，流量通过隧道回到你的本地服务。

```
访问者 ──► 服务器公网端口 ──► [隧道] ──► 客户端 ──► 本地服务 127.0.0.1:22
```

真正的转发、加密、面板数据统计都在这一版里**可用**；这一点请对照下面「这一版有什么 / 没有什么」，
它是本版本最重要的部分。

### 30 秒上手

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

打开 `http://服务器IP:7500/` 就是面板，显示在线客户端、已注册的隧道、实时流量。

### 这一版有什么

| 能力 | 状态 | 说明 |
|---|---|---|
| TCP 隧道转发 | ✅ 可用 | 访问者 → 服务器公网端口 → 客户端 → 本地服务，支持半关闭，不截断响应 |
| 控制连接与会话 | ✅ 可用 | 认证、心跳、断线自动重连（指数退避 + 抖动）、连接数上限 |
| 隧道注册与端口发布 | ✅ 可用 | 客户端启动时注册；服务器真正监听 `remote_port` 并转发 |
| 可选的数据包加密 | ✅ 可用 | XChaCha20-Poly1305 或 AES-256-GCM，默认**关闭**；控制消息与隧道字节都加密 |
| 密钥派生 | ✅ 可用 | 口令经 HKDF-SHA256 派生 32 字节密钥，任意长度口令都可用；口令留空则用 auth_token |
| Web 面板 | ✅ 可用 | 单页、自带资源（编译进二进制，不依赖工作目录）、中英双语、手机可用 |
| 面板 API | ✅ 可用 | `/api/health` `/api/status` `/api/clients` `/api/proxies` `/api/config`，可设 Bearer token |
| 多平台 | ✅ 可用 | linux/darwin/windows × amd64/arm64，`scripts/build-release.*` 一键出 12 个产物 + SHA256 |
| 配置校验 | ✅ 可用 | 未知配置项会**报出来**而不是静默忽略；`--check` 只校验不启动 |
| 访问控制 | ✅ 可用 | `allow_cidrs` / `deny_cidrs` 在握手前执行；无法解析的来源在存在规则时按拒绝处理 |
| 连接限流 | ✅ 可用 | 按来源地址的令牌桶，在握手前执行；空闲桶会被回收 |
| 审计日志 | ✅ 可用 | JSON Lines，记录接入/拒绝、认证失败、上下线、代理注册与拒绝；按大小轮转 |
| Prometheus 指标 | ✅ 可用 | `GET /metrics`（文本格式 0.0.4），含连接、认证失败、拒绝、流、双向字节与按隧道的序列 |
| 健康探针 | ✅ 可用 | `GET /healthz` 恒 200；`GET /readyz` 在监听器未就绪或正在关闭时返回 503 |
| 帧长度填充与抖动 | ✅ 可用 | `[obfuscation] pad_to` / `jitter_millis`，在加密之后补齐，隐藏负载的精确长度 |
| 单元 + 端到端测试 | ✅ 可用 | 含"访问者→隧道→本地服务"的真实回环测试，明文与加密两种模式 |

### 这一版没有什么

v3.0.0 及更早版本的 README 列出了 20 项功能。对全部 Go 源码检索下列关键词，
出现次数均为 **0**：`webrtc`、`dht`、`blockchain`、`kyber`、`dilithium`、`zk-snark`、
`quic`、`mptcp`、`tun`、`tap`。相关描述已从文档中删除。当前**没有实现**的：

- ❌ WebRTC / P2P 直连、去中心化 DHT、区块链与代币激励、零知识证明、抗量子加密（Kyber/Dilithium）
- ❌ UDP / HTTP / HTTPS / STCP / XTCP / SUDP 等代理类型（只实现了 TCP；配成别的类型会被**明确拒绝**，不会静默失效）
- ❌ TUN/TAP 虚拟网卡与 VPN 数据面、多路径传输、移动端 App
- ❌ 负载均衡（`load_balance` 设为非默认值时会打印"尚未实现"警告）、Kubernetes 部署清单
- ❌ TLS 传输层：`enable_tls` 属于未知键，会被报告而不是被忽略；负载加密由 `[encryption]` 提供

`[vpn]` 配置段会被解析并忽略，启动时打印警告。

### 加密怎么开

两端必须配置一致，否则连接会在握手阶段报明确的 `encryption mismatch`：

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # 或 "aes-256-gcm"
passphrase = ""                     # 留空 = 用 auth_token 派生
salt = "aethertunnel"               # 两端必须相同
```

说明：密钥不是 auth_token 本身，而是 `HKDF-SHA256(passphrase, salt)` 派生的 32 字节；
每个数据包（控制帧）与每条隧道记录都有独立随机 nonce，篡改会被 AEAD 拒绝并断开连接。
留空口令时"能认证的人就能解密"这一点依然成立，想彻底分离就把 `passphrase` 单独设成另一个密钥。

### 面板

- 资源用 `go:embed` 打进二进制，**不需要**在二进制旁边放 `web/` 目录（上一版是相对路径读取，发布的压缩包里根本没有这些文件）。
- 默认只监听 `127.0.0.1`。要让外部访问，请设置 `[dashboard].token`，之后 `/api/*` 需要
  `Authorization: Bearer <token>`；`/api/health` 始终公开且不含敏感信息。
- 面板轮询 `/api/status`、`/api/clients`、`/api/proxies`，**屏幕上每个数字都来自服务器实时状态**；
  没有客户端时显示空状态，不会造假数据。上一版的面板一次网络请求都没有，数字全是 `Math.random()`。

### 构建与测试

```bash
make build          # 本机两个二进制 → bin/
make test           # 单元 + 端到端测试（隧道测试会绑定回环端口）
make vet
make cross          # 12 个产物 + dist/SHA256SUMS
make check          # 校验示例配置
```

Windows 无 make 时：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1 -Version v3.1.0
```

### 从旧版本升级

旧版命令行参数与配置键有一部分已经不同，请读 [`docs/MIGRATION.md`](docs/MIGRATION.md)。
一句话版本：`auth_token`、`bind_port`、`[[proxies]]` 的写法保持不变，其余请按新示例重写。

### 安全模型

- 认证是**共享密钥**（`auth_token`），不是证书；token 用常数时间比较，失败时**不会**把 token 写进日志。
- 没有 TLS 传输层加密：要么只在可信网络里跑，要么打开 `[encryption]`（它保护的是负载，不含协议外观）。
- 面板 API 只有 Bearer token 一种保护，没有登录会话、没有多用户、没有审计日志。
- 隧道只转发 TCP；没有连接级别的访问控制（谁能连到服务器的 `remote_port` 谁就能用这条隧道）。

详见 [`docs/SECURITY.md`](docs/SECURITY.md)。

---

## English

### What it is

AetherTunnel publishes a TCP service that sits behind NAT onto a machine with a public
address. A visitor connects to the server's public port; the bytes travel back through
the tunnel to the service running on your own machine.

```
visitor ──► server public port ──► [tunnel] ──► client ──► local service 127.0.0.1:22
```

The forwarding, the optional encryption and the dashboard's numbers are all real in this
release. What is *not* real is just as important — see "What this release does not do".

### Quick start

```bash
go build -o aethertunnel-server .          # server
go build -o aethertunnel-client ./client   # client
```

Server (`server.toml`) — bind address, port, a shared `auth_token`, and the dashboard:

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "replace-with-at-least-16-random-characters"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"   # set a token before exposing it
port = 7500
```

Client (`client.toml`) — where the server is and which local ports to publish:

```toml
[client]
server_addr = "your-server:7001"
auth_token = "same value as the server"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 6022        # opened on the server
```

```bash
./aethertunnel-server --config server.toml
./aethertunnel-client --config client.toml
ssh -p 6022 user@your-server
```

The dashboard is at `http://your-server:7500/` and shows live clients, tunnels and traffic.

### What this release does

| Capability | State | Notes |
|---|---|---|
| TCP tunnelling | ✅ works | visitor → public port → client → local service; half-close is preserved so responses are not truncated |
| Control session | ✅ works | auth, heartbeat, exponential-backoff reconnect with jitter, connection limit |
| Tunnel registration | ✅ works | the server really binds `remote_port`; registration failures are reported to the client |
| Optional packet encryption | ✅ works | XChaCha20-Poly1305 or AES-256-GCM, **off** by default, covers control frames and tunnelled bytes |
| Key derivation | ✅ works | HKDF-SHA256 over the passphrase (any length); empty passphrase falls back to `auth_token` |
| Web dashboard | ✅ works | one self-contained page, embedded in the binary, English + 简体中文, usable on a phone |
| Dashboard API | ✅ works | `/api/health`, `/api/status`, `/api/clients`, `/api/proxies`, `/api/config`, optional Bearer token |
| Platforms | ✅ works | linux/darwin/windows × amd64/arm64; `scripts/build-release.*` produces 12 binaries + SHA256 |
| Config validation | ✅ works | unknown keys are **reported**, not ignored; `--check` validates without starting |
| Tests | ✅ works | unit tests plus a real end-to-end tunnel test, in both cleartext and encrypted modes |

### What this release does not do

Earlier releases advertised twenty "disruptive" features. **None of them existed in the
code** — a search of every Go file for `webrtc`, `dht`, `blockchain`, `kyber`, `dilithium`,
`zk-snark`, `quic`, `mptcp` and `tun` returns **zero hits**. Those claims have been removed,
because a manual that describes software you cannot run is worse than no manual. Not
implemented today:

- ❌ WebRTC / P2P, DHT, blockchain or bandwidth market, zero-knowledge proofs, post-quantum crypto
- ❌ UDP, HTTP, HTTPS, STCP, XTCP, SUDP proxy types — only TCP exists, and asking for another type is refused with an explicit error rather than silently ignored
- ❌ TUN/TAP or any VPN data path, multipath, traffic obfuscation, AI routing, mobile apps
- ❌ Load balancing, Prometheus metrics, Kubernetes manifests

The `[obfuscation]` and `[vpn]` config sections still parse but are ignored, and startup says so.

### Encryption

Both ends must agree, or the handshake fails with a clear `encryption mismatch`:

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # or "aes-256-gcm"
passphrase = ""                     # empty = derive from auth_token
salt = "aethertunnel"               # must match on both ends
```

The key on the wire is `HKDF-SHA256(passphrase, salt)`, not the token itself; every
control frame and every stream record carries a fresh random nonce, and a tampered record
is rejected by the AEAD and drops the connection.

### Dashboard

Assets are embedded with `go:embed`, so a released binary serves its own UI with nothing
next to it. It listens on loopback by default; set `[dashboard].token` to expose it, after
which `/api/*` needs `Authorization: Bearer <token>` (`/api/health` stays public). Every
number on screen comes from live server state — empty lists say they are empty instead of
showing invented rows.

### Build and test

```bash
make build     # both binaries into bin/
make test      # unit + end-to-end tests
make vet
make cross     # 12 artifacts + dist/SHA256SUMS
make check     # validate the example configs
```

### Upgrading from an older release

Some flags and config keys changed; see [`docs/MIGRATION.md`](docs/MIGRATION.md). In short:
`auth_token`, `bind_port` and the `[[proxies]]` blocks keep their meaning, everything else
should be rewritten from the new examples.

### Security model, stated plainly

- Authentication is a **shared secret** (`auth_token`), not a certificate. The comparison is
  constant-time and a failed attempt never writes the offered token to the log.
- There is no transport-level TLS. Either keep it on a trusted network or enable
  `[encryption]` — which protects the payload, not the protocol's appearance.
- The dashboard has a single Bearer token: no user accounts, no sessions, no audit log.
- Only TCP is tunnelled, and there is no per-connection access control: whoever can reach a
  `remote_port` can use that tunnel.

See [`docs/SECURITY.md`](docs/SECURITY.md) for the full list, including what an attacker on
the path can still learn.

### License

MIT — see [LICENSE](LICENSE).
