# 架构 · Architecture

**English summary.** AetherTunnel listens on one TCP port per server. A client keeps a
long-lived *control connection* on it (auth, heartbeat, tunnel registration, IP packets when
`[vpn]` is on) and opens a short *data connection* per visiting connection, which carries
either raw bytes or `TypeUDPPacket` frames. A published proxy is a `ProxyGroup`: several
clients may register the same name, the group owns the public endpoint, and one strategy picks
the member that serves each visitor. The crypto stack is layered — frame AEAD, then TLS, then
the obfuscation wrapper — and a DHT publishes one record per proxy so a client can resolve a
server address by name. Sessions, tunnels, leases and counters live in managers that release
them on disconnect; every number the dashboard shows is read from those managers.

---

## 1. 角色与连接

```
                        ┌──────────────────── 服务器 ────────────────────┐
内网机器                  │                                               │
┌──────────────┐  控制连接 │  :7001  ┌──────────────────────────────┐      │
│ aethertunnel │ ────────►│ ┌─────► │ SessionManager / TunnelManager│      │
│   -client    │          │ │       └──────────────────────────────┘      │
│              │  数据连接 │ │                                             │
│  ┌────────┐  │ ────────►│ │  tunnel "ssh" :6022 ┌─────────────────┐     │
│  │ 本地服务│◄─┼──────────┼─┘◄────────────────────│ 访问者           │     │
│  │ :22    │  │          │                        └─────────────────┘     │
│  └────────┘  │          │                                              │
└──────────────┘          └──────────────────────────────────────────────┘
```

- **控制连接**：客户端启动后建立，发送 `AuthRequest`，之后保持长连接用于心跳、注册代理、
  接收 `DataRequest`，以及（启用 `[vpn]` 时）收发 `TypeVPNPacket` 帧。一个客户端只有一条。
- **数据连接**：每当有访问者连到某条隧道的公网端口，服务器通过控制连接发一个
  `DataRequest{proxy, stream_id}`，客户端**新拨一条** TCP 连接回来，发
  `DataOpen{session, proxy, stream_id}`；服务器把这条连接与正在等待的访问者配对，
  此后双方按代理类型搬运字节或数据报。
- 所有角色复用同一个端口，防火墙只需放行一个端口。

## 2. 数据面方向：客户端回拨

客户端通常在 NAT 后面，服务器无法主动连它。因此数据面必须由客户端发起：服务器只负责
"告诉客户端有人来了"和"把两条连接对接起来"。`xtcp` 是唯一例外：它先让两侧互发 UDP
打洞，成功则直连，失败仍回到这条回拨路径。

## 3. 帧格式

```
+--------+--------+------------------+-------------------+
| type:1 | flags:1| length:4 (BE)    | payload:length    |
+--------+--------+------------------+-------------------+
```

- `length` 允许为 0（心跳），读取前先与上限（默认 1 MiB）比较，**不会**按对端声明的长度
  直接分配内存。
- `flags` bit0 = 已加密，bit1 = 已填充。发送端先加密再补齐，接收端先按标志去补齐再解密。
- 写入用单次 `Write` 提交整帧，并由互斥锁串行化，因此心跳线程与数据请求线程不会交叉写坏。

## 4. 代理类型与数据面

| 类型 | 公网端点 | 数据面 |
|---|---|---|
| `tcp` | 独立端口 `remote_port` | 一条数据连接，原始字节 + 半关闭 |
| `udp` | 独立 UDP 端口 | 一条数据连接，`TypeUDPPacket` 帧，按来源地址分会话 |
| `http` | 共享监听 `http_port` | 反向代理，按 Host 头选隧道，仍经一条数据连接回源 |
| `https` | 共享监听 `https_port` | 同上，入口为 TLS |
| `stcp` | 无 | 访客连服务器控制端口，按名字配对，字节流 |
| `sudp` | 无 | 同上，数据报 |
| `xtcp` | 无 | 打洞直连或中继回退，两者都是字节流 |

```
访问者连接 ──► ProxyGroup.acceptLoop ──► pick() 选出成员 ──► 分配 streamID
                                                        │
              控制连接 ───────────────────────────────┘  DataRequest
                                                        │
客户端新拨数据连接 ──► handleData ──► 按 streamID 找到等待中的访问者
                                                        │
                                                        └─► flynet.Pipe 或 relayDatagrams
```

`flynet.Pipe` 的要点：

- 两个方向各自独立，用 `CloseWrite` 半关闭，所以 HTTP/1.0、SSH 这类"先关一个方向"的
  协议不会被截断。
- 双向复制各带空闲超时（读超时），防止对端静默时永久占用 goroutine 与 fd。
- 32 KiB 缓冲来自 `sync.Pool`。

## 5. 代理池与调度

同名代理由 `ProxyGroup` 持有端点，每个注册它的客户端是它的一个 `Tunnel` 成员。
`pickExcluding` 先过滤出健康成员（会话未关闭且在 `SessionManager` 中），再按策略选择：

| 策略 | 选择方式 |
|---|---|
| `round-robin` | 依次轮转 |
| `random` | 均匀随机 |
| `latency` | 时延移动平均最小者 |
| `failover` | 注册顺序第一个健康成员 |
| `adaptive` | 代价最小者，代价 = 时延移动平均 × (1 + 2 × 连续失败数)，失败数上限 4 |

成员每次回答一条流时更新移动平均（新值 = 旧值 − 旧值/4 + 本次/4，单位纳秒），
失败时连续失败数加一、成功时清零。未测量过的成员代价为 0，因此总会被优先尝试。

## 6. 加密、传输与伪装

三层互相独立，按需开启：

```
应用数据（帧）
  └─ [encryption] 帧级 AEAD（XChaCha20-Poly1305 或 AES-256-GCM）
       └─ post_quantum：会话密钥 = HKDF(X25519 共享秘密, ML-KEM-768 共享秘密)
            每条数据连接再派生 HKDF(session_key, stream_id)
       └─ [transport] TLS（可选，服务器证书认证）
            └─ [obfuscation] disguise：TLS 记录头封装（可选，仅改变外观）
```

层次顺序固定：伪装必须是最外层，否则观察者看不到它。TLS 与伪装同时开启时，连接外观是
"TLS 记录包住 TLS 记录"。

主机身份是正交的一层：客户端用 Ed25519 对 `nonce|timestamp` 签名，服务器校验签名、
时间窗口与 nonce 缓存（防重放），并按 `allowed_keys` 白名单决定是否接受。控制连接、
数据连接与访客连接都会校验。

## 7. 目录（DHT）

`pkg/dht` 是一个 Kademlia 表：160 位标识、k 桶（k=20）、α=3 的迭代查找，
值与提供者记录分属两个命名空间，记录按 TTL 过期，本节点拥有的记录会被定期重发。

`pkg/discovery` 在其上定义隧道自己的记录格式：键为 `<namespace>/proxy/<name>`，
值是 JSON（名字、类型、地址、域名、更新时间、过期时间）。服务器在某个代理第一次出现时
写入记录，最后一个成员离开时删除本地记录（`Forget`）；记录自带有效期，读取端把过期
记录当作不存在，因此服务端下掉代理后，对端已复制的副本会在一个通告周期内失效。

## 8. 三层隧道

```
客户端                                 服务器
tun 设备 ──► vpn.Tunnel ──► 控制连接 ──► Router ──► tun 设备
                       TypeVPNPacket              │
                                           地址池：每个会话一个地址
```

- 客户端是点对点：`vpn.Tunnel` 在两个方向搬运 IP 包，写入设备前校验版本、长度与 MTU。
- 服务器是路由器：`vpn.Router` 读一次共享设备，按目的地址查表找出应接收的会话，
  入队后由该成员的发送协程写出；每个成员只能以自己领到的源地址发包。
- 地址池不分配网络地址、广播地址与服务器自留地址；`Release` 会忽略不属于池的地址。

## 9. 状态与生命周期

| 对象 | 谁持有 | 何时释放 |
|---|---|---|
| `Session` | `SessionManager`（按 id） | 控制连接结束、心跳超时、面板断开 |
| `ProxyGroup` / `Tunnel` | `TunnelManager`（按名字）+ 所属 `Session` | 最后一个成员离开时销毁端点；成员随会话结束注销 |
| 等待中的访问者连接 | `Session.pending`（按 streamID） | 被数据连接认领、超时、或会话关闭时全部失败关闭 |
| 隧道地址 | `vpnService.leases` + 地址池 | 会话结束时归还 |
| 账本条目 | `pkg/ledger` 的哈希链 | 会话结束时按代理追加，进程退出前落盘 |

释放路径只有一条：`Session.Close`（幂等）→ 关闭 pending → 关闭成员端点（仅当它是最后
一个）→ 关 socket → 归还隧道地址 → 写账本条目。随后容器关闭：`Run` 在最后一个连接
处理协程返回后才关闭账本与 DHT 节点，避免关掉仍在写入的对象。

## 10. 目录结构

```
main.go                 服务端入口（flag、配置、面板、信号处理、--dht-lookup、--verify-ledger）
client/main.go          客户端入口（会话循环、重连、按流转发、DHT 解析、隧道设备）
client/punch.go         xtcp 打洞与中继回退
client/visitor.go       访客监听器（stcp/sudp/xtcp）
pkg/config              配置加载、默认值、校验（未知键会被报告）、环境变量凭据
pkg/protocol            帧格式、消息类型、JSON 负载结构、会合协议
pkg/crypto              HKDF 派生、AEAD 封装、记录层、Ed25519 身份、Schnorr 证明、混合密钥协商
pkg/net                 双向复制（半关闭、超时、缓冲池）、数据报泵
pkg/reliable            UDP 之上的可靠有序字节流（xtcp 直连用）
pkg/ledger              Ed25519 签名、哈希链式带宽账本
pkg/dht                 Kademlia 表、路由、存储、线协议
pkg/discovery           DHT 上的代理通告与解析
pkg/obfs                TLS 记录封装
pkg/vpn                 三层隧道：IP 包处理、地址池、点对点隧道、路由器、平台设备绑定
pkg/server              Server / SessionManager / ProxyGroup / TunnelManager / Dashboard /
                        vhost / p2p / visitor / directory / vpn / audit / acl / metrics
web/embed.go            go:embed 的入口
web/dashboard           面板单页（内嵌进二进制）
deploy/kubernetes        Namespace、ConfigMap、Secret 示例、Deployment、Service、kustomization
Dockerfile              多阶段构建 → distroless
scripts/build-release.* 跨平台构建与校验和
scripts/smoke-test.ps1  端到端运维脚本（38 项检查）
```

## 11. 扩展点

- **新的代理类型**：在 `config.ProxyTypes` 加入名字，在 `ProxyGroup` 中实现端点开关与
  搬运方式，在 `serverAddrFor` 中说明 DHT 记录应指向哪个地址。
- **新的数据面传输**：`handleData` 收到 `DataOpen` 后把连接交给配对逻辑，替换这一步即可
  接入其它承载。
- **新的调度策略**：在 `pkg/server/group.go` 的 `pickExcluding` 中加一个分支，
  在 `config` 的接受列表与文档中加入名字。
- **新的账本后端**：`pkg/ledger` 只依赖 `Append`/`Verify`/`Proof`，换存储只需替换文件写入。
- **指标输出**：计数都在各对象的原子字段里，`dashboard.go` 与 `metrics.go` 是唯一读取方。
