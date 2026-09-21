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
| `socks5` | 独立端口 `remote_port` | 访客先做 SOCKS5 协商并指定目标，服务器把目标放进 `DataRequest.target`，客户端拨号后回拨；`allow_targets` 之外的目标以回复码 `0x02` 拒绝 |

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

## 5.1 打洞与结果上报

xtcp 的私有通道走 UDP 打洞：服务器在 `server.p2p_port` 上做会合，两端各发一条请求数据报，
服务器把对方的 NAT 映射地址回给双方，之后两端用 `pkg/reliable` 的可靠有序字节流直连。

```
请求     "ATP1" | role:1 | token:32
应答     "ATP2" | token:32 | length:1 | "ip:port"
结果     "ATP3" | token:32 | path:1      ('D' 直连，'R' 中继)
```

结果数据报是打洞结果**唯一**的可信来源。走直连的访客不再使用控制连接，也不再发任何帧，
服务器只能看到那条连接消失；同一个动作也可能是访客中途放弃。因此访客在直连建立后先给
会合端口发一次结果，服务器在控制连接消失后再等最多 2 秒（`punchReportWait`），
收到 `'D'` 才计入 `aethertunnel_p2p_direct_total` 并写 `p2p_direct` 审计；
什么都没收到时写 `p2p_abandoned`，不猜测、不计数。旧版服务端不认识 `ATP3`：魔法前缀不同，
会被当作无效的会合请求丢掉。

结果数据报不重传也不确认，访客连发三份；丢失的代价只是那条尝试记为"未知"。

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
值是 JSON（名字、类型、地址、域名、更新时间、过期时间，以及可选签名公钥与签名）。
服务器在某个代理第一次出现时写入记录，最后一个成员离开时删除本地记录（`Forget`）；
记录自带有效期，读取端把过期记录当作不存在，因此服务端下掉代理后，对端已复制的副本会在
一个通告周期内失效。

DHT 的值没有写入权限控制：任何节点都能写同一个键。因此服务器可以为记录签名
（`[dht] signing_key_file`，Ed25519），签名覆盖读取端会使用的每个字段；读取端按
`require_signed` 与 `trusted_keys` 决定接受什么，并在判断有效期之前先校验签名，
使被改写的记录报"签名不通过"而不是"已过期"。

## 7.1 准入与策略

每条被接受的连接先过 `Server.admit`：封禁名单、`allow_cidrs` / `deny_cidrs`、按来源的令牌桶，
全部在读取任何帧之前执行。认证失败由 `recordAuthFailure` 记账，同一来源在十分钟窗口内失败
达到 `ban_after_failures` 次即写入封禁名单（时长按倍数增长到 `ban_max_seconds`），
成功认证清零；封禁按来源地址生效，不区分客户端。

代理自身的 `allow_cidrs` / `deny_cidrs` 是第二层：在服务器整体接受连接之后、分配流之前按
代理判断访客来源，拒绝会写 `proxy_visitor_denied` 审计记录。`socks5` 的 `allow_targets`
是唯一按**目标地址**判断的名单，在客户端拨号前生效。

## 7.2 关闭顺序

`Server.Shutdown` 的顺序是固定的，每一步都为下一步创造条件：

1. 关闭控制监听，并停掉 http/https 共享监听与打洞会合端口——不再有新的控制连接与访客；
2. 把每个会话标记为 draining：已经在传的流继续，新的流请求以
   `errServerDraining` 立刻失败（计入 `aethertunnel_streams_refused_while_draining_total`），
   而不是让访客等到拨号超时；
3. 等待所有会话的活动流计数归零，最多 `graceful_shutdown_seconds` 秒（25 毫秒轮询一次）；
   空闲的连接不参与等待，因为控制连接只由客户端自己决定何时结束；
4. 到点或提前完成后 `CloseAll`：断开客户端，连接处理器随之收尾（写账本、从 DHT 撤回通告），
   `Run` 等所有处理器退出后才关闭存储。

`Session` 上的活动流与累计流计数就是第 3 步的依据，也是 `/api/clients` 里
`active_streams` 与 `total_streams` 的来源；两者在流的打开与结束处由 `Tunnel.openStreamFor`
的释放函数维护，且只会生效一次。

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
scripts/smoke-test.ps1  端到端运维脚本（83 项检查）
scripts/vpn-linux-test.sh  真实 tun 设备上的三层隧道检查（两端各在一个网络命名空间）
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
