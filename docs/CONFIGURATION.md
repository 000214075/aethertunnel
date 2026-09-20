# 配置参考 · Configuration reference

**English summary.** Every key this version reads is listed below. Any other key is reported
at startup, and `--check` fails when `RejectUnknownKeys` is set. Three credentials may also
come from the environment and take precedence over the file: `AETHERTUNNEL_AUTH_TOKEN`,
`AETHERTUNNEL_DASHBOARD_TOKEN`, `AETHERTUNNEL_ENCRYPTION_PASSPHRASE`. The example files in the
repository root are validated by CI.

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
| `allow_cidrs` | []string | 空 | CIDR 白名单。非空时只有匹配的来源可以连接 |
| `deny_cidrs` | []string | 空 | CIDR 黑名单，优先级高于白名单 |
| `rate_limit_per_second` | float | 0 | 按来源地址的连接速率（每秒），0 表示关闭 |
| `rate_limit_burst` | int | 20 | 令牌桶容量 |
| `http_port` | int | 0 | `http` 代理的共享监听端口，0 表示不启用 |
| `https_port` | int | 0 | `https` 代理的共享监听端口；非 0 时下面两项必须同时设置 |
| `https_cert_file` | string | 空 | 共享 HTTPS 监听的证书 |
| `https_key_file` | string | 空 | 共享 HTTPS 监听的私钥 |
| `subdomain_host` | string | 空 | 设置后，没有显式 `domains` 的 `http` 代理可用 `<代理名>.<该值>` 访问 |
| `p2p_port` | int | 0 | `xtcp` 打洞的 UDP 会合端口，0 表示不支持打洞（xtcp 走中继） |
| `load_balance` | string | `round-robin` | 代理池策略：`round-robin` `random` `latency` `failover` `adaptive`。只对声明了 `group` 的代理有影响 |

## `[client]`（客户端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `server_addr` | string | 条件必填 | `host:port`。为空时必须设置 `[dht] discover`，否则报错 |
| `auth_token` | string | 必填 | 与服务端一致 |
| `reconnect_seconds` | int | 3 | 重连退避的起始值 |
| `max_reconnect_seconds` | int | 60 | 退避上限；退避带 ±20% 抖动 |
| `heartbeat_seconds` | int | 30 | 心跳间隔 |
| `dial_timeout_seconds` | int | 10 | 连接服务端、等待 `DataOpenAck`、连接本地服务的超时 |
| `idle_timeout_seconds` | int | 300 | 单条隧道流的空闲上限 |

## `[[proxies]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 代理名，同一客户端内不可重复；同一 `group` 内多个客户端可以同名 |
| `type` | string | `tcp` | `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` |
| `local_ip` | string | `127.0.0.1` | 本地服务地址 |
| `local_port` | int | 必填 | 本地服务端口（1–65535） |
| `remote_port` | int | 0 | 服务器上对外开放的端口。`tcp`/`udp` 用它；`http`/`https` 与私有类型必须为 0 |
| `domains` | []string | 空 | `http`/`https` 必填。精确域名、`*.通配` 或配合 `subdomain_host` 的后缀 |
| `secret_key` | string | 空 | 私有类型必填，也是访客侧的凭据 |
| `auth_method` | string | `secret` | `secret` 直接比对；`nizk` 用 Schnorr 证明，secret 不出现在线上 |
| `group` | string | 空 | 填入同一名字的多个客户端组成代理池 |
| `multipath` | int | 0 | 数据报代理最多使用的数据连接数（0–8）。字节流代理上会给出警告 |

## `[[visitors]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 本机监听器的名字 |
| `type` | string | `stcp` | `stcp` `sudp` `xtcp` |
| `server_name` | string | 必填 | 服务端上对应的私有代理名 |
| `secret_key` | string | 必填 | 必须与代理侧一致 |
| `auth_method` | string | `secret` | `secret` 或 `nizk` |
| `bind_addr` | string | `127.0.0.1` | 本机监听地址 |
| `bind_port` | int | 必填 | 本机监听端口（1–65535） |

## `[dashboard]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否启动面板 |
| `bind_addr` | string | `127.0.0.1` | 面板监听地址。非回环地址且未设 `token` 时会**警告** |
| `port` | int | 7500 | 面板端口 |
| `token` | string | 空 | 设置后 `/api/*` 需要 `Authorization: Bearer <token>`；`/api/health`、`/healthz`、`/readyz` 始终公开 |

## `[encryption]`（两端，必须一致）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后控制帧与隧道记录都会被 AEAD 保护 |
| `algorithm` | string | `xchacha20-poly1305` | 也可用 `aes-256-gcm`；其他值会被拒绝 |
| `passphrase` | string | 空 | 密钥材料。留空则用本端的 `auth_token` |
| `salt` | string | `aethertunnel` | HKDF 的 salt，两端必须相同，否则解密失败 |
| `post_quantum` | bool | false | 用 X25519 + ML-KEM-768 协商会话密钥，并为每条数据连接派生独立密钥。需要 `enabled = true` |

密钥 = `HKDF-SHA256(passphrase, salt, info="aethertunnel/v3/aead")` 取 32 字节。
`post_quantum` 打开时，会话密钥由两个共享秘密共同派生，每条数据连接再派生
`HKDF(session_key, stream_id)`。两端配置不一致时，握手阶段会返回 `encryption mismatch`。

## `[transport]`（TLS 传输层）

| 键 | 类型 | 默认 | 端 | 说明 |
|---|---|---|---|---|
| `enable_tls` | bool | false | 两端 | 控制端口与所有数据连接使用 TLS |
| `cert_file` | string | 空 | 服务端 | 证书链（`enable_tls` 时必须） |
| `key_file` | string | 空 | 服务端 | 私钥（`enable_tls` 时必须） |
| `ca_file` | string | 空 | 客户端 | 信任锚；留空使用系统根证书 |
| `server_name` | string | 空 | 客户端 | 覆盖证书校验用的名字；留空取 `client.server_addr` 的主机部分 |
| `insecure_skip_verify` | bool | false | 客户端 | 接受任意证书。连接仍然加密，但服务器未被认证，会**警告** |

## `[identity]`（Ed25519 身份，两端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后客户端在握手时附带签名断言 |
| `key_file` | string | `aethertunnel-identity.key` | 客户端私钥，首次使用时生成 |
| `allowed_keys` | []string | 空 | 服务端接受的白名单公钥（十六进制）。留空表示接受任何能正确签名的公钥 |
| `require_identity` | bool | false | 服务端拒绝没有有效身份断言的客户端 |

## `[metrics]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否提供 `GET /metrics`（Prometheus 文本格式 0.0.4），由面板监听器提供 |
| `token` | string | 空 | 该 token 与面板 token 任一可用；两者都未设置时 `/metrics` 不需要鉴权 |

指标序列：控制连接、被拒绝的控制连接、认证失败、ACL 拒绝、限流拒绝、数据连接、
活动与累计流数、双向字节、UDP 数据报与活动会话，以及按 `tunnel` 标签的活动流、
累计流与双向字节。

## `[audit]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否写审计日志 |
| `path` | string | `aethertunnel-audit.jsonl` | 日志路径 |
| `max_bytes` | int | 33554432 | 超过该大小后轮转，旧文件保留为 `<path>.1` |

每行一个 JSON 对象，字段为 `time`、`event`、`client_id`、`remote`、`proxy`、`detail`、`outcome`；
`event` 取值为 `control_accepted`、`control_rejected`、`auth_failed`、`client_disconnected`、
`proxy_registered`、`proxy_rejected`、`proxy_removed`、`acl_denied`、`rate_limited`、
`dashboard_action`、`visitor_accepted`、`visitor_rejected`、`p2p_direct`、`p2p_relayed`、
`vpn_address_assigned`、`vpn_address_rejected`。

## `[ledger]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否记录带宽账本 |
| `path` | string | `aethertunnel-ledger.jsonl` | 账本文件，每行一条 JSON 记录 |
| `signing_key_file` | string | `aethertunnel-ledger.key` | Ed25519 私钥种子（32 字节十六进制），首次使用时生成，权限 0600 |

每条记录包含序号、时间、客户端、代理、双向字节、上一条的哈希、本条哈希与签名。
`GET /api/ledger` 发布公钥、链头、最近条目与按客户端汇总。
`aethertunnel-server --verify-ledger <文件> --ledger-key <公钥或私钥文件>` 可以离线校验，
校验只需要公钥。

## `[dht]`（两端）

| 键 | 类型 | 默认 | 端 | 说明 |
|---|---|---|---|---|
| `enabled` | bool | false | 两端 | 是否启动 DHT 节点 |
| `listen_addr` | string | `0.0.0.0:7001` | 两端 | 节点绑定的 UDP 地址 |
| `bootstrap` | []string | 空 | 两端 | 启动时要联系的其它节点，`host:port`。留空表示自成一个单节点网络 |
| `node_id` | string | 空 | 两端 | 40 位十六进制标识；留空则随机生成，每次重启都会变 |
| `namespace` | string | `aethertunnel` | 两端 | 键前缀，两套部署可以共用一个 DHT |
| `ttl_seconds` | int | 3600 | 两端 | 记录在 DHT 中的存活时间，不能短于 `announce_ttl_seconds` |
| `announce_ttl_seconds` | int | 90 | 两端 | 一条通告对读取端有效的时长 |
| `republish_seconds` | int | `announce_ttl_seconds / 3` | 两端 | 重新发布已有通告的间隔，必须短于 `announce_ttl_seconds` |
| `lookup_timeout_seconds` | int | 5 | 两端 | 单次解析的时限 |
| `advertise_host` | string | 空 | 服务端 | 通告里写给客户端的主机名，**不含端口**（端口按代理类型取）。留空取 `server.bind_addr`，广播地址会**警告** |
| `discover` | string | 空 | 客户端 | `client.server_addr` 为空时要解析的代理名 |

## `[vpn]`（三层隧道）

| 键 | 类型 | 默认 | 端 | 说明 |
|---|---|---|---|---|
| `enabled` | bool | false | 两端 | 是否启用三层隧道 |
| `device` | string | 空 | 两端 | 网卡名。服务端是读写的网卡，客户端是要创建的网卡；留空由内核分配（仅 Linux） |
| `address` | string | 空 | 服务端 | 分配给客户端的 IPv4 子网（CIDR），服务端占用第一个可用地址。必填 |
| `mtu` | int | 0 | 两端 | 覆盖网卡 MTU（576–9000）；0 表示用网卡自身的值 |
| `require` | bool | false | 服务端 | 拒绝没有申请隧道地址的会话 |

地址分配与路由是两个独立动作：本程序只分配地址，**不会**改动主机路由表。
服务端需要自行加上指向该子网的路由，例如 `ip route add 10.7.0.0/24 dev tun0`。
非 Linux 平台打开设备会失败，服务端因此拒绝启动并说明原因。

## `[obfuscation]`（两端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后下列各项才会生效 |
| `pad_to` | int | 256 | 帧负载补齐到该值的整数倍；0 表示不补齐 |
| `jitter_millis` | int | 0 | 每次写入前插入 `[0, 该值)` 毫秒的随机延迟 |
| `disguise` | string | `none` | `none` 原样写出；`tls-record` 把每个写入包进 TLS 1.2 应用数据记录，两端必须一致 |
| `default_type` | string | 空 | 保留键，当前未被使用 |

补齐在加密之后进行，帧内保留真实长度前缀；接收端仅凭帧标志位去补齐，两端不需要配置一致。
伪装是最外层，不做握手：它能骗过只看首字节的识别器，骗不过会建模 TLS 会话的识别器。

## 环境变量

| 变量 | 覆盖 | 生效时机 |
|---|---|---|
| `AETHERTUNNEL_AUTH_TOKEN` | `[server] auth_token` 与 `[client] auth_token` | 校验之前，因此配置文件里可以不放密钥 |
| `AETHERTUNNEL_DASHBOARD_TOKEN` | `[dashboard] token` | 同上 |
| `AETHERTUNNEL_ENCRYPTION_PASSPHRASE` | `[encryption] passphrase` | 同上 |

覆盖发生时会在启动日志里列出被覆盖的变量名。

## 完整示例

见仓库根目录的 [`server.toml.example`](../server.toml.example) 与
[`client.toml.example`](../client.toml.example)——CI 会用 `--check` 校验它们，
所以它们始终是可用的。Kubernetes 下的用法见
[`deploy/kubernetes/README.md`](../deploy/kubernetes/README.md)。
