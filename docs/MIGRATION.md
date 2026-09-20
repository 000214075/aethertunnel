# 从旧版本迁移 · Migrating from an older release

**English summary.** The wire protocol and several configuration keys changed in v3.1.0, and
both ends must be upgraded together. The table below says what to change. The old
documentation set was removed because it described features that never existed; its removal
is recorded here so nothing disappears silently.

---

## 1. 必须先知道的两件事

1. **协议不兼容**：v3.1.0 使用新的帧格式与消息编号（`ProtocolVersion = 3`）。
   客户端与服务端必须**同时升级**，混用会在握手阶段报清楚的原因。
2. **旧文档已删除**：旧版本仓库里有 27 个描述"20 项颠覆性功能"的文档
   （`PROJECT_SUMMARY.md`、`QUALITY_REPORT.md`、`DELIVERY_CHECKLIST.md`、三个安全审计
   报告、`docs/` 下 19 个文件等）。那些功能在代码里不存在，删除它们是为了让说明书与
   代码一致。被删文件的清单见 [CHANGELOG.md](CHANGELOG.md) 的 v3.1.0 条目。

## 2. 命令行的变化

| 旧写法 | 新写法 | 说明 |
|---|---|---|
| `aethertunnel-server server.toml` | 仍然可用 | 位置参数仍被当作配置文件路径 |
| — | `--config server.toml` | 推荐写法 |
| — | `--check` | 只校验配置，不启动 |
| `aethertunnel-server --version` | 可用，且现在**真的**打印版本 | 旧版会把 `--version` 当成文件名 |

## 3. 配置的变化

### 保留原样

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

### 新增

```toml
[server]
max_connections = 512
handshake_timeout_seconds = 10
read_timeout_seconds = 120
heartbeat_seconds = 30
dial_timeout_seconds = 10

[client]
reconnect_seconds = 3
max_reconnect_seconds = 60
idle_timeout_seconds = 300

[dashboard]
bind_addr = "127.0.0.1"   # 新增
token = ""                 # 新增：对外暴露时必须设置

[encryption]               # 全新段，默认关闭
enabled = false
algorithm = "xchacha20-poly1305"
passphrase = ""
salt = "aethertunnel"
```

### 不再生效（会被报告或警告）

| 旧键/段 | 现在的行为 |
|---|---|
| `server.enable_tls`、`cert_file`、`key_file` | **未知键**：启动时列出、`--check` 提示。本项目没有 TLS 传输层加密 |
| `[obfuscation]` | 解析但忽略，启动时警告（数据包混淆未实现） |
| `[vpn]` | 解析但忽略，启动时警告（没有 VPN 数据面） |
| `[webrtc]`、`[dht]`、`[pqc]`、`[gaming_mode]`、`[load_balancer]`、`[monitoring]`、`[failover]` 等 | **未知键**：启动时列出。这些功能从未实现 |
| 代理 `type` 为 `udp`/`http`/`https`/`stcp`/`xtcp`/`sudp` | 注册时被**明确拒绝**并回传错误，不会被静默忽略 |
| `client.custom_domains`、`proxies.sk` 等 | 未知键，会被列出 |

如果你的旧配置里有大量此类键，最快的做法是从新的
[`server.toml.example`](../server.toml.example) 与
[`client.toml.example`](../client.toml.example) 重新开始写，只把需要的值搬过去。

## 4. 面板地址的变化

旧文档写的是 `/dashboard/index.html`、`/dashboard/server.html`、`/dashboard/client.html`。
现在只有一个页面，路径是 `/`（或 `/index.html`）。`/dashboard/...` 不再存在，
`server.html` 与 `client.html` 已删除。

## 5. 构建的变化

| 旧写法 | 新写法 |
|---|---|
| `go build -o aethertunnel-server ./server` | `go build -o aethertunnel-server .` |
| `scripts/build.sh`（14 平台，实际全部失败） | `scripts/build-release.sh` / `scripts/build-release.ps1`（6 平台 × 2 二进制 + SHA256） |
| `Dockerfile.build`（`./server` + Go 1.21 与要求 1.22.2 冲突） | 已删除；用 `make cross` 或上面的脚本 |
| `make build`（`-X main.Version` 大小写错误，版本号打不进去） | `make build` 现在能正确注入 `version`/`buildTime`/`gitCommit` |

`--version` 应输出类似：

```
aethertunnel-server v3.1.0 (protocol 3, built 2026-09-20T03:25:31Z, commit 765d24f)
```

若仍显示 `dev` 或旧版本号，说明你是用不带 `-ldflags` 的 `go build` 构建的。

## 6. 升级检查清单

1. 两端一起换成 v3.1.0 的二进制。
2. 用 `--check` 校验新配置，把报出的未知键逐个处理掉。
3. 决定是否开启 `[encryption]`；开启时两端必须填写**完全相同**的 `algorithm`、`salt`
   与 `passphrase`。
4. 若面板需要对外访问，设置 `[dashboard].token`。
5. 启动后确认：客户端日志出现 `connected ... as session <id>` 与
   `server confirms N tunnel(s)`；面板 `/api/status` 的连接数与隧道数符合预期。
6. 用真实客户端做一次访问（例如 `ssh -p <remote_port> ...`），确认数据真的通。
