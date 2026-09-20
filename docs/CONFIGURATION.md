# 配置参考 · Configuration reference

**English summary.** Only the keys listed here are read by v3.1.0. Any other key is
reported at startup (and makes `--check` fail if you pass `RejectUnknownKeys`), which is
deliberate: earlier releases shipped examples full of keys that were silently ignored, so
people believed features were on when they were not. Every key below is exercised by a test
or by the example files that CI validates.

用 `--check` 校验而不启动：

```bash
aethertunnel-server --config server.toml --check
aethertunnel-client --config client.toml --check
```

## `[server]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `bind_addr` | string | 必填 | 监听地址。`0.0.0.0` 表示所有网卡 |
| `bind_port` | int | 必填 | 监听端口（1–65535）。控制连接与数据连接共用 |
| `auth_token` | string | 必填 | 与客户端共享的密钥。少于 16 位或形如占位符会**警告** |
| `max_connections` | int | 512 | 同时在线客户端上限，超出的会被明确拒绝 |
| `handshake_timeout_seconds` | int | 10 | 连接建立后必须在此时限内发出第一帧 |
| `read_timeout_seconds` | int | 120 | 隧道流的空闲上限，同时也是控制连接的空闲判据 |
| `heartbeat_seconds` | int | 30 | 期望客户端的心跳间隔；连续 3 次未收到即断开 |
| `dial_timeout_seconds` | int | 10 | 访问者到来后，等待客户端回拨数据连接的时限 |
| `graceful_shutdown_seconds` | int | 5 | 收到信号后用于收尾的时间 |

## `[client]`（客户端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `server_addr` | string | 必填 | `host:port`，必须是带端口的形式（缺端口会报错） |
| `auth_token` | string | 必填 | 与服务端一致 |
| `reconnect_seconds` | int | 3 | 重连退避的起始值 |
| `max_reconnect_seconds` | int | 60 | 退避上限；退避带 ±20% 抖动 |
| `heartbeat_seconds` | int | 30 | 心跳间隔 |
| `dial_timeout_seconds` | int | 10 | 连接服务端、等待 `DataOpenAck`、连接本地服务的超时 |
| `idle_timeout_seconds` | int | 300 | 单条隧道流的空闲上限 |

## `[[proxies]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 隧道名，同一客户端内不可重复 |
| `type` | string | `tcp` | **只支持 `tcp`**；其他值会被拒绝并提示 |
| `local_ip` | string | `127.0.0.1` | 本地服务地址 |
| `local_port` | int | 必填 | 本地服务端口（1–65535） |
| `remote_port` | int | 0 | 服务器上对外开放的端口；0 表示只注册不开端口 |

## `[dashboard]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否启动面板 |
| `bind_addr` | string | `127.0.0.1` | 面板监听地址。非回环地址且未设 `token` 时会**警告** |
| `port` | int | 7500 | 面板端口 |
| `token` | string | 空 | 设置后 `/api/*` 需要 `Authorization: Bearer <token>`；`/api/health` 始终公开 |

## `[encryption]`（两端，必须一致）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后控制帧与隧道记录都会被 AEAD 保护 |
| `algorithm` | string | `xchacha20-poly1305` | 也可用 `aes-256-gcm`；其他值会被拒绝 |
| `passphrase` | string | 空 | 密钥材料。留空则用本端的 `auth_token` |
| `salt` | string | `aethertunnel` | HKDF 的 salt，两端必须相同，否则解密失败 |

密钥 = `HKDF-SHA256(passphrase, salt, info="aethertunnel/v3/aead")` 取 32 字节。
两端配置不一致时，握手阶段会返回 `encryption mismatch`，而不是让每条消息都失败。

## 已知但不生效的段（会打印警告）

| 段 | 行为 |
|---|---|
| `[obfuscation]` | 解析但忽略。数据包混淆未实现，旧的 `[obfuscation]` 配置不会带来任何保护 |
| `[vpn]` | 解析但忽略。没有 TUN/TAP 与 VPN 数据面 |

`[server].enable_tls`、`cert_file`、`key_file` 属于**未知键**：会被列在启动警告里，
`--check` 也会提示。本项目没有 TLS 传输层加密，请用 `[encryption]` 或把隧道限制在可信网络。

## 完整示例

见仓库根目录的 [`server.toml.example`](../server.toml.example) 与
[`client.toml.example`](../client.toml.example)——CI 会用 `--check` 校验它们，
所以它们始终是可用的。
