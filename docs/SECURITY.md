# 安全说明 · Security

**English summary.** Authentication is a shared secret compared in constant time and never
logged. Payloads can be protected with an AEAD (off by default), keyed by
`HKDF-SHA256(passphrase, salt)`; there is no transport TLS, no per-tunnel access control and
no user accounts on the dashboard. This document lists what is protected, what is not, and
the concrete limitations you should assume when deploying it.

---

## 1. 认证

- 客户端用 `AuthRequest{token, client_version, protocol, encryption}` 打开会话；服务端用
  `crypto.EqualTokens`（`crypto/subtle.ConstantTimeCompare`）比较，**不会**因为提前返回
  而泄漏前缀。
- 认证失败只记录来源地址与客户端自报版本，**不记录**客户端提交的 token。
  旧版本会把完整 token 打进日志（`log.Printf("Invalid auth token: %s", token)`），已修复。
- 认证成功后服务端返回随机会话 ID（8 字节），数据连接必须携带它，且只能认领**尚未被认领**
  的流 ID。旧版本允许任何人在未认证的连接上直接开隧道到服务器的回环地址，已修复。

**限制**：这是共享密钥，不是双向证书。token 泄漏 = 任何人可以开隧道。
请用至少 16 位随机字符（`openssl rand -hex 32`），并把它当成密码管理。

## 2. 加密

| 项 | 状态 |
|---|---|
| 算法 | XChaCha20-Poly1305（默认）或 AES-256-GCM |
| 密钥派生 | HKDF-SHA256，32 字节输出；口令任意长度，留空则用 `auth_token` |
| nonce | 每条记录独立随机，随密文一起传输 |
| 保护范围 | 控制帧负载 + 隧道流的每条记录 |
| 篡改 | AEAD 校验失败 → `ErrAuthFailed` → 断开该连接 |
| 默认 | **关闭** |

**没有传输层 TLS。** 打开 `[encryption]` 保护的是**负载**，不改变协议外观：
帧头（类型、长度）仍是明文，所以观察者依然能看出：
连接方向、包大小分布、时序、连接持续时间、以及双方 IP。
需要抗流量分析请叠加别的工具（例如把隧道放进 TLS、WireGuard 或 SSH 里）。

**密钥与凭据耦合**：`passphrase` 留空时，能通过认证的人就能解密流量。
如果需要分离，把 `passphrase` 单独设成另一串随机值并只分发给需要解密的一端。

## 3. 资源与稳健性

| 攻击 | 防护 |
|---|---|
| 用超大长度字段骗内存 | 帧长度先与上限（1 MiB）比较，超限直接拒绝，不分配 |
| 连接后不发数据 | `handshake_timeout_seconds` 内的读期限 |
| 心跳静默 | 连续 3 个心跳周期未收到心跳即断开 |
| 隧道流挂死 | `read_timeout_seconds` / `idle_timeout_seconds` 作为读期限 |
| 连接洪水 | `max_connections` 上限，超出者收到明确拒绝 |
| 单连接 panic 拖垮进程 | 每个连接的处理器带 `recover`，panic 只关掉那条连接 |
| 短写导致数据截断 | 所有写入检查返回值；帧写入用单次 `Write` + 互斥锁 |

## 4. 面板

- 默认只监听 `127.0.0.1`。绑定非回环地址而未设置 `token` 时，启动会**警告**。
- 设置 `token` 后，`/api/*` 需要 `Authorization: Bearer <token>`（常数时间比较）。
  `/api/health` 公开，只返回状态、版本、协议号与运行时长。
- `/api/config` 返回**脱敏**后的设置：不含 `auth_token`、不含面板 token、不含加密口令。
- 面板只读 + 一个 `DELETE /api/clients/{id}`（断开客户端）。没有登录会话、没有多用户、
  没有审计日志、没有 CSRF token —— 因此**不要把面板暴露到公网**，用 SSH 端口转发或
  WireGuard 访问更合适。

## 5. 隧道本身

- 只支持 TCP。
- **没有每连接的访问控制**：任何能连到 `remote_port` 的人都能使用该隧道。
  需要限制来源时，请用防火墙或反向代理把 `remote_port` 保护起来。
- 服务器不会主动连接客户端以外的目标：数据面的目标由客户端自己决定（它在自己的机器上
  连 `local_ip:local_port`）。因此**获得 token 的客户端可以让服务器的公网端口指向它自己
  网络里的任意服务**——这是工具的固有性质，请只把 token 发给可信的人。

## 6. 访问控制、限流与审计

| 能力 | 配置 | 行为 |
|---|---|---|
| 来源白名单 | `[server] allow_cidrs` | 非空时只有匹配的来源可以建立连接 |
| 来源黑名单 | `[server] deny_cidrs` | 优先级高于白名单；先匹配 deny，再匹配 allow |
| 连接限流 | `[server] rate_limit_per_second` / `rate_limit_burst` | 按来源地址的令牌桶，在握手前执行；空闲桶会被回收 |
| 审计日志 | `[audit]` | JSON Lines，记录接入/拒绝、认证失败、上下线、代理注册与拒绝、ACL 与限流拒绝 |

规则在握手**之前**执行，被拒绝的连接不会消耗会话槽位，也不会读取任何帧。
当白名单或黑名单非空时，来源地址无法解析的连接按拒绝处理。

## 7. 已知未实现的安全相关能力

以下能力在旧版文档中被声称存在，实际没有（关键词在 Go 源码中零出现）：
TLS 1.3 传输层、Ed25519 签名认证、自动封禁、时间戳防重放、HMAC 心跳签名、
零知识证明、抗量子密钥交换（Kyber/Dilithium）。

需要其中任何一项，请自行实现或改用别的工具；本文件不会把它们列为已有能力。

## 8. 报告问题

请通过 GitHub Issues 报告（附版本号、复现步骤、以及 `--version` 的输出）。
`--version` 会打印版本、协议号、构建时间与 commit，定位问题时请一并提供。
