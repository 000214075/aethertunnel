# 安全说明 · Security

**English summary.** This version authenticates with a shared secret compared in constant
time, and adds three optional layers on top of it: Ed25519 client identities, a TLS
transport (`[transport]`), and an AEAD over frame payloads (`[encryption]`, off by default).
Private proxies are protected by a secret key or by a Schnorr proof of knowledge of it.
Layer-3 tunnel peers may only send packets whose source address is the address they were
assigned. Announcements published through `[dht]` are **not signed**: anyone who can write
to the same namespace can publish a record under any proxy name. This document lists what is
protected, what is not, and the limitations to assume when deploying it.

---

## 1. 认证

- 客户端用 `AuthRequest{token, client_version, protocol, encryption}` 打开会话；服务端用
  `crypto.EqualTokens`（`crypto/subtle.ConstantTimeCompare`）比较，**不会**因为提前返回
  而泄漏前缀。
- 认证失败只记录来源地址与客户端自报版本，**不记录**客户端提交的 token。
- 认证成功后服务端返回随机会话 ID（8 字节），数据连接必须携带它，且只能认领**尚未被
  认领**的流 ID。
- 共享密钥可以放在 `[server] auth_token` / `[client] auth_token`，也可以放在
  `AETHERTUNNEL_AUTH_TOKEN` 环境变量里；环境变量在校验之前生效，因此配置文件里可以不放
  密钥。两种来源取值相同时行为不变。

**限制**：这是共享密钥，不是双向证书。token 泄漏 = 任何人可以开隧道。
请用至少 16 位随机字符（`openssl rand -hex 32`），并把它当成密码管理。

## 2. 身份（Ed25519，可选）

`[identity] enabled = true` 后，客户端在握手时附带一个断言：对自己的 32 字节随机 nonce
与当前时间做 Ed25519 签名。签名消息带域分隔前缀，长度前缀编码，因此同一把钥匙签出的其它
结构不能被当作挑战重放。

服务端侧的三项检查：

| 检查 | 行为 |
|---|---|
| 公钥白名单 | `allowed_keys` 非空时只接受其中的公钥；留空表示接受任何签名正确的公钥 |
| 时间窗 | 时间戳与服务器时钟相差超过 `IdentityWindow`（120 秒）即拒绝 |
| nonce 去重 | 已经见过的 nonce 即拒绝，这是阻止重放握手的关键 |

身份断言是**附加**在共享密钥之上的：`[identity] enabled` 不会取代 `auth_token`，
`require_identity` 只要求客户端额外提供有效断言。

## 3. 私有代理的凭据

`stcp` `sudp` `xtcp` 用 `secret_key` 保护，两端的 `auth_method` 必须一致。

| `auth_method` | 线上内容 | 说明 |
|---|---|---|
| `secret` | 访客把 `secret_key` 原样放进请求 | `[encryption]` 关闭时它是明文；打开后被 AEAD 保护 |
| `nizk` | 访客只发 97 字节的 Schnorr 证明 | 服务端先发 `VisitorChallenge`（32 字节 nonce），访客用 nonce 与代理名组成上下文做证明 |

Schnorr 证明在 NIST P-256 上，用 Fiat-Shamir 去交互；上下文含服务端生成的 nonce 与代理名，
所以抓到的证明换一次握手或换一个代理都无法复用。

**限制**：证明式 `z*G = R + c*Y` 里 `Y` 是公开可算的，所以抓到一次证明的人可以算出
`Y = g^H(secret)`，再对弱口令做离线字典攻击。因此 `secret_key` 必须用高熵随机值
（`openssl rand -hex 32`），不要用词典里的词。另外 `secret` 方式在未开启加密时会明文发送
密钥；需要它不出现在线上时，用 `nizk`，并同时开启 `[encryption]` 或 `[transport]`。

## 4. 加密（AEAD）

| 项 | 值 |
|---|---|
| 算法 | `xchacha20-poly1305`（默认）或 `aes-256-gcm`；其他值在校验阶段被拒绝 |
| 密钥派生 | `HKDF-SHA256(passphrase, salt, info="aethertunnel/v3/aead")`，取 32 字节 |
| 口令 | `[encryption] passphrase`；留空则退回本端的 `auth_token`，也可由 `AETHERTUNNEL_ENCRYPTION_PASSPHRASE` 提供 |
| nonce | 每条记录独立随机，随密文一起传输 |
| 保护范围 | 控制帧负载 + 隧道流的每条记录 |
| 篡改 | AEAD 校验失败 → `ErrAuthFailed` → 断开该连接 |
| 默认 | **关闭** |

`[encryption] post_quantum = true` 时，会话密钥由 **X25519 与 ML-KEM-768 两个共享秘密**共同
派生，并且每条数据连接再派生 `HKDF(session_key, stream_id)`；它要求 `enabled = true`。
两端配置不一致时握手返回 `encryption mismatch`，不会静默降级。

**帧头仍是明文**：类型、标志与长度字段不加密，且长度是补齐后的长度。观察者仍能看出连接
方向、包大小分布、时序、连接持续时间与双方 IP。需要抗流量分析请叠加别的工具。

**密钥与凭据耦合**：`passphrase` 留空时，能通过认证的人就能解密流量。需要分离时把
`passphrase` 设成另一串随机值，只分发给需要解密的一端。

## 5. TLS 传输层（可选）

`[transport] enable_tls = true` 后，控制端口与所有数据连接都走 TLS，最低版本
`tls.VersionTLS12`。

| 键 | 端 | 作用 |
|---|---|---|
| `cert_file` / `key_file` | 服务端 | 证书链与私钥，`enable_tls` 时为必填 |
| `ca_file` | 客户端 | 信任锚；留空用系统根证书 |
| `server_name` | 客户端 | 覆盖证书校验用的名字；留空取 `client.server_addr` 的主机部分 |
| `insecure_skip_verify` | 客户端 | 接受任意证书：连接仍加密，但服务器**未被认证**，启动时警告 |

服务端不做客户端证书校验：客户端身份由 `auth_token` 与可选的 `[identity]` 提供。

## 6. 流量混淆（`[obfuscation]`）

| 项 | 作用 |
|---|---|
| `pad_to` | 把帧负载补齐到该值的整数倍；补齐在加密**之后**，帧内保留真实长度前缀 |
| `jitter_millis` | 每次写入前插入 `[0, 该值)` 毫秒的随机延迟 |
| `disguise = "tls-record"` | 把每个写入包进 TLS 1.2 应用数据记录（类型 `0x17`、版本 `0x0303`），没有握手 |

**它不是加密，也不参与认证**：不含密钥、不含握手、不改变负载内容。它能骗过只看首字节的
协议识别规则，骗不过会建模 TLS 会话的识别器——没有 ClientHello、没有证书、没有密钥交换。
接收端只凭帧标志位去补齐，两端不需要配置一致；`disguise` 必须两端一致。

## 7. 访问控制、限流、封禁与审计

| 能力 | 配置 | 行为 |
|---|---|---|
| 来源白名单 | `[server] allow_cidrs` | 非空时只有匹配的来源可以建立连接 |
| 来源黑名单 | `[server] deny_cidrs` | 优先级高于白名单；先匹配 deny，再匹配 allow |
| 连接限流 | `[server] rate_limit_per_second` / `rate_limit_burst` | 按来源地址的令牌桶，在握手前执行；空闲桶会被回收 |
| 自动封禁 | `[server] ban_after_failures` / `ban_seconds` / `ban_max_seconds` / `ban_ignore_cidrs` | 同一来源认证失败达到次数后，在握手前拒绝该来源；每次封禁时长翻倍，直到上限 |
| 按代理的访客 ACL | `[[proxies]] allow_cidrs` / `deny_cidrs` | 服务器整体接受之后、建立隧道之前，再按该代理自己的名单判断 |
| 审计日志 | `[audit]` | JSON Lines，超过 `max_bytes` 后按 `keep` 代轮转（`<path>.1` … `<path>.<keep>`，默认保留一代）；写不进去的记录会重开文件重试，仍然失败的计入 `aethertunnel_audit_records_lost_total` 并出现在 `GET /api/status` 的 `audit` 段与面板上 |

规则在握手**之前**执行，被拒绝的连接不会消耗会话槽位，也不会读取任何帧。白名单或黑名单
非空时，来源地址无法解析的连接按拒绝处理。

封禁只统计**认证失败**（令牌错误、身份断言无效、抗量子密钥协商失败），且按**来源地址**记账，
不区分是哪个客户端触发的：一个地址被封后，同一地址上任何凭据都会被拒。失败计数在 10 分钟
的窗口内累积，成功认证会清零；重复被封的地址时长按倍数增长（`ban_seconds`、2 倍、4 倍…），
到 `ban_max_seconds` 为止。因此**负载均衡、健康检查与监控地址必须写进 `ban_ignore_cidrs`**，
否则它们会和攻击者共用同一个来源地址。

按代理的 ACL 只匹配**来源地址**，不做目标地址或用户的判断。

审计 `event` 取值：`control_accepted`、`control_rejected`、`auth_failed`、
`client_disconnected`、`proxy_registered`、`proxy_rejected`、`proxy_removed`、`acl_denied`、
`rate_limited`、`source_banned`、`ban_refused`、`proxy_visitor_denied`、
`dashboard_action`、`visitor_accepted`、`visitor_rejected`、
`p2p_direct`、`p2p_relayed`、`p2p_abandoned`、
`vpn_address_assigned`、`vpn_address_rejected`。

`proxy_removed` 在代理成员被移除时记录，`dashboard_action` 记录从面板发起的操作；
`p2p_abandoned` 表示那次打洞的结果无法判定（访客既没有要求中继、也没有回报直连路径），
所以它不会被当成直连成功记进审计或指标。

审计日志本身的失败也必须看得见：一条记录写不进去时会重新打开 `path` 并重试一次，
所以外部的日志轮转或一次瞬时错误不会留下空洞；仍然写不下去的记录计入
`aethertunnel_audit_records_lost_total`，`GET /api/status` 的 `audit` 段报出
`writable`、`records_lost` 与 `last_error`，面板的"服务器状态"栏与横幅显示同一件事。
这是在抄日志和删日志之外，唯一能让人发现"审计已经停了"的地方——审计停下来本身不留痕迹。
写不进去**不会**让服务器停止服务，这是有意的：否则一个只读的日志目录就能让隧道下线。

公开的 `remote_port` 仍然可以被上面这些规则之外的任何人连接：按来源的名单挡的是"谁能连"，
挡不住"连上之后能做什么"。`socks5` 出口多一层 `allow_targets`，它限制的是客户端能拨到哪些
地址，是唯一按**目标**判断的名单。

## 8. 面板与指标

- 默认只监听 `127.0.0.1`。绑定非回环地址而未设置 `token` 时，启动会**警告**。
- 设置 `token` 后，除 `/api/health` 外的所有 `/api/*` 需要
  `Authorization: Bearer <token>`（常数时间比较）。`/healthz` 与 `/readyz` 始终公开，
  返回状态、版本与就绪情况。
- `/api/config` 返回**脱敏**后的设置：不含 `auth_token`、不含面板 token、不含加密口令。
- `/api/ledger` 返回账本公钥、链头与最近条目；`/api/dht` 返回 DHT 节点标识与它发布的代理。
  两者都要通过面板 token 鉴权。
- `/metrics`（`[metrics] enabled = true`）由面板监听器提供，接受 `[metrics] token` 或
  面板 token；两个 token 都为空时 `/metrics` 不需要鉴权。
- 面板只读 + 一个 `DELETE /api/clients/{id}`（断开客户端）。没有登录会话、没有多用户、
  没有 CSRF token —— 因此**不要把面板暴露到公网**，用 SSH 端口转发或 WireGuard 访问。

## 9. 带宽账本

`[ledger] enabled = true` 后，服务端为每个「客户端 × 代理」的用量追加一条记录：序号、时间、
双向字节、上一条的哈希、本条哈希、Ed25519 签名。签名密钥首次使用时生成到
`signing_key_file`（32 字节十六进制种子，权限 0600）。

- 持有公钥的第三方可以离线校验一段账本没有被改动：
  `aethertunnel-server --verify-ledger <文件> --ledger-key <公钥或私钥文件>`。
- 哈希链会把任何改动暴露成后续条目的哈希不匹配；删掉末尾若干条不会破坏链，因此**发现截断
  需要把链头哈希另外发布并在校验时比对**（`GET /api/ledger` 会给出当前链头）。
- 记账发生在客户端断开时，写入该会话在各代理上累计的字节数：`tcp` 与 `stcp` 按流、
  `udp` 与 `sudp` 按数据报会话、`xtcp` 按直连或中继的流、`http` 与 `https` 按每个完成的
  请求。这是**用量声明**，不做流量分析级别的核对。

## 10. DHT 发现

`[dht]` 是一个 Kademlia DHT 节点，服务端把「代理名 → 服务器地址与端口」发布到
`<namespace>/proxy/<name>`，客户端可以只凭代理名解析。记录自带过期时间，读取端把过期记录
当作不存在。

DHT 的值谁都能写：同一 `namespace` 下的任何节点都可以为任意代理名写一条指向别处的记录。
因此服务端可以给每条通告签名（`[dht] signing_key_file`，Ed25519，密钥首次使用时生成，
权限 0600），签名覆盖名字、类型、地址、域名、发布时间与失效时间以及签名公钥本身；
读取端有两种策略：

- `require_signed = true`：没有签名的记录直接拒绝；
- `trusted_keys = ["<公钥十六进制>"]`：只接受这些公钥签发的记录，同时也会拒绝无签名记录。

校验失败的错误分别是 `the announcement carries no signature`、
`the announcement's signature does not verify`（有人改写了值）与
`the announcement is signed by a key that is not trusted`。
签名**不隐藏**元数据：代理名、类型与地址仍然对 DHT 上的任何人可见，只保证它们确实来自
被信任的服务端。要连元数据也不暴露，就不要启用 `[dht]`。

仍然有效的缓解手段：

- 客户端显式配置 `client.server_addr`，不使用 `[dht] discover`；
- 只把 `bootstrap` 指向自己控制的节点；
- 用 `namespace` 把两套部署分开，并用非默认值避免与陌生部署同名共享；
- 在客户端设置 `trusted_keys`，这样即使键被投毒，指向别处的记录也会被拒绝。

即使发现被投毒，连接仍然要过 `auth_token`（以及可选的身份与 TLS 校验），攻击者拿到的是
让客户端连到自己服务器上的机会，不是解密既有流量的能力。

## 11. 三层隧道（`[vpn]`）

- 仅在 Linux 上打开设备；其它平台启动即失败并说明原因，不会静默降级。
- 服务端打开一块网卡，用 `address` 指定的子网分配地址（服务端占用第一个可用地址），并按
  目的地址把每个包交给持有该地址的会话。
- **来源地址被强制绑定**：一个会话发出的包，源地址必须等于分配给它的地址，否则丢弃并记日志
  —— 否则任何客户端都能伪造别人的地址并收到其流量。
- 每个包都先做完整性校验（版本、头长度、总长度与 MTU），不合法即丢弃。
- `[vpn] require = true` 时，服务端拒绝没有申请隧道地址的会话；地址在会话结束时回收。
- 隧道内没有 NAT、没有防火墙规则、没有路由表改动：**只分配地址**。需要访问子网以外的目标
  时由服务端自行配置转发与路由（例如 `ip route add 10.7.0.0/24 dev tun0`）。
- 三层数据走客户端的控制连接（`TypeVPNPacket` 帧），因此它继承该连接上配置的
  `[encryption]` 与 `[transport]`；这两项都关闭时，隧道内的 IP 包以明文形态出现在链路上。

## 12. 资源与稳健性

| 攻击 | 防护 |
|---|---|
| 用超大长度字段骗内存 | 帧长度先与上限（1 MiB）比较，超限直接拒绝，不分配 |
| 连接后不发数据 | `handshake_timeout_seconds` 内的读期限 |
| 心跳静默 | 连续 3 个心跳周期未收到心跳即断开 |
| 隧道流挂死 | `read_timeout_seconds` / `idle_timeout_seconds` 作为读期限 |
| 连接洪水 | `max_connections` 上限，超出者收到明确拒绝 |
| 单连接 panic 拖垮进程 | 每个连接的处理器带 `recover`，panic 只关掉那条连接 |
| 短写导致数据截断 | 所有写入检查返回值；帧写入用单次 `Write` + 互斥锁 |
| 慢客户端拖住三层转发 | 每个对端有独立队列，队列满即丢包并计数，不阻塞设备读循环 |
| UDP 会话堆积 | 空闲超过 `IdleTimeout`（默认 2 分钟）的会话被回收 |

## 13. 不提供的能力

以下能力在本版本中不存在，请不要按它们规划防护。需要其中任何一项，请自行实现或改用别的
工具；本文件不会把它们列为已有能力：

- 移动端应用（没有 iOS/Android 工程）
- Windows 与 macOS 上的三层隧道（只有 Linux 设备绑定）
- 基于学习的路由（`adaptive` 是移动平均延迟加失败惩罚的策略，不是模型）
- 区块链、代币、链上结算
- WebRTC 数据通道
- zk-SNARK / zk-STARK 证明（`nizk` 是 Schnorr 证明，不是简洁证明系统）
- 模拟完整 TLS 会话（`disguise` 只写记录头，不做握手）
- DHT 记录签名、自动封禁、按目标地址的访问控制

## 14. 报告问题

请通过 GitHub Issues 报告（附版本号、复现步骤、以及 `--version` 的输出）。
`--version` 会打印版本、协议号、构建时间与 commit，定位问题时请一并提供。
