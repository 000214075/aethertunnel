# 从旧版本迁移 · Migrating from an older release

**English summary.** v3.2.0 speaks protocol 4. A v3.1.0 peer still completes the handshake but
logs `protocol mismatch`, and the features added here need both ends upgraded: the padding
flag and message types 16–18 are unknown to a version 3 peer. A v3.1.0 configuration file
keeps working as it is — every section added in this version defaults to off. The sections
below list what changed, what is accepted now that used to be rejected, and the upgrade
checklist.

---

## 1. 升级前必须知道的三件事

1. **两端一起升级**：本版本是 protocol 4，v3.1.0 是 protocol 3。旧对端不认识帧填充标志位与
   16–18 号消息类型（访客挑战、访客证明、VPN 包），会把填充帧里的长度前缀当成数据而拒绝
   该帧。版本不一致不会让握手失败，但两端日志都会写出 `protocol mismatch`。
2. **配置文件向后兼容**：v3.1.0 的配置可以原样使用。本版本新增的段（`[transport]`、
   `[identity]`、`[metrics]`、`[audit]`、`[ledger]`、`[dht]`、`[[visitors]]`）都是可选的，
   并且默认关闭。
3. **曾经"解析但忽略"的段现在生效了**：`[obfuscation]` 与 `[vpn]` 在 v3.1.0 里只会产生一条
   警告；现在它们真的会改变行为，键名也换了（见第 2.3 节）。

## 2. 从 v3.1.0 升到 v3.2.0

### 2.1 不用改的部分

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "..."

[client]
server_addr = "server:7001"
auth_token = "..."

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 6022
```

上表中的每个键在 v3.2.0 里含义不变。`[dashboard]`、`[encryption]` 也没有变化，
只是 `[encryption]` 多了 `post_quantum`。

### 2.2 现在可以按需打开的能力

| 段 / 键 | 作用 | 默认 |
|---|---|---|
| `[server] allow_cidrs` / `deny_cidrs` | 按 CIDR 的访问控制，握手前执行 | 空（不限制） |
| `[server] rate_limit_per_second` / `rate_limit_burst` | 按来源地址的令牌桶限流 | 0（关闭） |
| `[server] http_port` / `https_port` / `subdomain_host` | 共享的 HTTP/HTTPS 监听 | 0（关闭） |
| `[server] p2p_port` | `xtcp` 打洞的会合端口 | 0（xtcp 走中继） |
| `[server] load_balance` | 代理池策略：`round-robin` `random` `latency` `failover` `adaptive` | `round-robin` |
| `[transport]` | TLS 传输层（`enable_tls`、证书、`ca_file`、`server_name`） | 关闭 |
| `[identity]` | Ed25519 客户端身份、`allowed_keys`、`require_identity` | 关闭 |
| `[metrics]` | `GET /metrics`（Prometheus 文本格式） | 关闭 |
| `[audit]` | JSON Lines 审计日志与轮转 | 关闭 |
| `[ledger]` | 签名哈希链带宽账本与 `--verify-ledger` | 关闭 |
| `[dht]` | 代理名发布与解析（服务端 `advertise_host`，客户端 `discover`） | 关闭 |
| `[vpn]` | 三层隧道（仅 Linux 打开设备） | 关闭 |
| `[obfuscation] pad_to` / `jitter_millis` / `disguise` | 帧长度补齐、写入抖动、TLS 记录伪装 | 关闭 |
| `[[proxies]] group` / `multipath` | 代理池成员与数据报多路径 | 空 / 0 |
| `[[proxies]] auth_method` | `secret`（发送密钥）或 `nizk`（Schnorr 证明） | `secret` |
| `[[visitors]]` | 访问私有代理的本机监听器 | 无 |

代理类型 `udp` `http` `https` `stcp` `sudp` `xtcp` 在 v3.1.0 里会被**明确拒绝**，
现在都被接受；私有类型的详细键见 [docs/CONFIGURATION.md](CONFIGURATION.md)。

### 2.3 键名与段的变化

| 旧写法 | 现在的行为 |
|---|---|
| `[vpn] bind_addr` / `port` / `auth_token` / `protocol` | **未知键**：启动时列出、`--check` 失败。`[vpn]` 的键改为 `enabled`、`device`、`address`、`mtu`、`require`。三层隧道走的仍是已经认证过的控制连接，没有单独的监听端口与凭据 |
| `[vpn]` 段本身 | v3.1.0 会解析但忽略并警告；现在真的会打开 tun 设备，非 Linux 平台启动即报错 |
| `[obfuscation]` 段 | v3.1.0 会解析但忽略并警告；现在按 `pad_to`、`jitter_millis`、`disguise` 生效 |
| `[obfuscation] default_type` | 保留键，当前未被使用 |
| `[server] enable_tls` / `cert_file` / `key_file` | 仍是**未知键**。TLS 在 `[transport]` 段里配置：`enable_tls`、`cert_file`、`key_file` |
| `[dht]`、`[transport]`、`[identity]`、`[metrics]`、`[audit]`、`[ledger]`、`[[visitors]]` | v3.1.0 会把它们当未知键列出来；现在它们都是有效段 |
| `[webrtc]`、`[pqc]`、`[gaming_mode]`、`[load_balancer]`、`[monitoring]`、`[failover]` 等 | 仍然是**未知键**：启动时列出。这些功能从未实现 |

`[server] auth_token`、`[client] auth_token`、`[dashboard] token`、
`[encryption] passphrase` 四个凭据也可以由环境变量提供：
`AETHERTUNNEL_AUTH_TOKEN`、`AETHERTUNNEL_DASHBOARD_TOKEN`、
`AETHERTUNNEL_ENCRYPTION_PASSPHRASE`。环境变量在校验之前生效，因此配置文件里可以不放密钥，
覆盖时会打印被覆盖的变量名。Kubernetes 部署用这种做法，见
[`deploy/kubernetes/README.md`](../deploy/kubernetes/README.md)。

### 2.4 面板接口的变化

新增 `GET /api/ledger` 与 `GET /api/dht`（两者都需要面板 token），`GET /api/proxies` 增加了
代理池字段（`member_count`、`members`、`healthy`、`consecutive_failures`）。
`/healthz` 与 `/readyz` 始终公开，`/metrics` 在 `[metrics] enabled = true` 时由面板监听器提供。
面板页面本身与 v3.1.0 相同：一个页面，路径是 `/`。

## 3. 从 v3.0 及更早版本升级

### 3.1 旧文档已删除

v3.1.0 之前的仓库里有 27 个描述"20 项颠覆性功能"的文档（`PROJECT_SUMMARY.md`、
`QUALITY_REPORT.md`、`DELIVERY_CHECKLIST.md`、三个安全审计报告、`docs/` 下 19 个文件等）。
那些功能在代码里不存在，删除它们是为了让说明书与代码一致。被删文件的清单见
[CHANGELOG.md](CHANGELOG.md) 的 v3.1.0 条目。

### 3.2 命令行的变化

| 旧写法 | 新写法 | 说明 |
|---|---|---|
| `aethertunnel-server server.toml` | 仍然可用 | 位置参数仍被当作配置文件路径 |
| — | `--config server.toml` | 推荐写法 |
| — | `--check` | 只校验配置，不启动 |
| `aethertunnel-server --version` | 可用，且现在**真的**打印版本 | 旧版会把 `--version` 当成文件名 |

### 3.3 面板地址的变化

旧文档写的是 `/dashboard/index.html`、`/dashboard/server.html`、`/dashboard/client.html`。
现在只有一个页面，路径是 `/`（或 `/index.html`）。`/dashboard/...` 不再存在，
`server.html` 与 `client.html` 已删除。

### 3.4 构建的变化

| 旧写法 | 新写法 |
|---|---|
| `go build -o aethertunnel-server ./server` | `go build -o aethertunnel-server .` |
| `scripts/build.sh`（14 平台，实际全部失败） | `scripts/build-release.sh` / `scripts/build-release.ps1`（6 平台 × 2 二进制 + SHA256） |
| `Dockerfile.build`（`./server` + Go 1.21 与要求 1.22.2 冲突） | 已删除；用 `make cross` 或上面的脚本，容器镜像用根目录的 `Dockerfile` |
| `make build`（`-X main.Version` 大小写错误，版本号打不进去） | `make build` 现在能正确注入 `version`/`buildTime`/`gitCommit` |

### 3.5 工具链

本版本要求 **Go 1.24**（`crypto/mlkem` 是标准库的一部分，v3.1.0 的 go.mod 下限是 1.22.2）。
`make test` 的超时是 300 秒，`make test-race` 需要 cgo 且超时 900 秒。

`--version` 应输出类似：

```
aethertunnel-server v3.2.0 (protocol 4, built 2026-09-20T07:12:44Z, commit 1234567)
```

若仍显示 `dev` 或旧版本号，说明你是用不带 `-ldflags` 的 `go build` 构建的。

## 4. 升级检查清单

1. 两端一起换成 v3.2.0 的二进制。
2. 用 `--check` 校验新配置，把报出的未知键逐个处理掉；`[vpn]` 的旧键必须删掉。
3. 决定是否开启 `[encryption]` 与 `[transport]`；加密的两端必须填写**完全相同**的
   `algorithm`、`salt` 与 `passphrase`，TLS 的客户端必须能校验服务端证书。
4. 若面板需要对外访问，设置 `[dashboard].token`；对外暴露 `/metrics` 时设置 `[metrics].token`。
5. 启动后确认：客户端日志出现 `connected ... as session <id>` 与
   `server confirms N tunnel(s)`；面板 `/api/status` 的连接数与隧道数符合预期。
6. 用真实客户端做一次访问（例如 `ssh -p <remote_port> ...`），确认数据真的通。
7. 用到的新功能各自验证一次：`--dht-lookup`、`--verify-ledger`、`--discover`，
   或直接跑 `scripts/smoke-test.ps1`。
