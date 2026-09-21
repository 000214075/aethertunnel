# 更新日志 · Changelog

本文件记录每个版本改了什么。

> **关于 v3.1.0 之前的条目**：旧版本宣称 WebRTC、DHT、区块链、抗量子加密、AI 路由等
> 20 项功能，并声称在 14 个平台上产出 28 个二进制。对全部 Go 源码检索这些关键词，
> 出现次数为 0，构建脚本指向的是不存在的目录。v3.1.0 之前的详细功能清单已删除，
> 只保留版本号与日期。v0.1.x 到 v3.0.0 期间完成的是配置、协议与面板的骨架，
> 核心链路在本版本之前未跑通。

---

## [3.5.0] — 2026-09-21

本版本把三层隧道放到真实 tun 设备上跑，修掉了它和客户端退出路径上的两个缺陷，
并让打洞结果、代理移除与面板操作有了真实来源。配置键与线协议都没有破坏性变化。

### 修复

- **Linux 上的 `[vpn] enabled = true` 起不来。** 服务端给自己那张 tun 设备配地址时，
  把整个 `sockaddr_in` 交给只接受 4 字节地址的 `SetInet4Addr`：它返回 `EINVAL`，
  代码没有检查这个返回值，于是内核收到一个空地址请求并再次以 `EINVAL` 拒绝，
  服务端随即拒绝启动。实测报错：`vpn: assigning 192.168.99.1 to at0: ... invalid argument`。
  只在真实设备上出现，用假设备的单测看不到。现在传入 4 字节地址并检查返回值。
- **客户端收到停止信号后最长要等一个心跳周期才退出。** 会话循环阻塞在读控制帧上，
  取消只在读到下一帧后才会被看到，而自发到达的帧只有心跳应答（默认 30 秒一次）。
  现在取消会直接关闭控制连接，`Ctrl-C` 与 `SIGTERM` 立刻结束会话。实测：修复前
  `kill` 之后 10 秒进程仍在，修复后立刻退出。

### 变更

- **打洞结果有了真实来源。** 走直连的访客不再使用控制连接、也不再发任何帧，服务器只能看到
  连接消失——这和访客中途放弃是同一个现象。现在访客在直连建立后向会合端口回报路径
  （新增数据报格式 `ATP3" | token:32 | path:1`），服务器在控制连接消失后最多再等 2 秒；
  收到 `'D'` 才计入 `aethertunnel_p2p_direct_total` 并写 `p2p_direct` 审计，
  什么都没收到时写新的 `p2p_abandoned`，不把未知当成功。这个数据报是新增的，
  旧服务端会把它当作无效的会合请求丢弃；旧客户端不发它，那些尝试记为"未知"。
- **`proxy_removed` 现在真的会写出来。** 之前 `ProxyGroup.remove` 只在"池子清空"时返回真，
  因此代理池少一个成员不会记录。现在成员被移除就记录，`detail` 带原因；
  端点与 DHT 通告仍然只在整个名字不再由任何人提供时撤回。
- **`dashboard_action` 现在真的会写出来。** 从面板断开客户端会留下一条审计记录，
  之前这个事件名只出现在文档里。
- **新增 `GET /api/vpn`**，并把同一份摘要并入 `GET /api/config` 的 `vpn` 段；
  面板配置页新增"三层隧道"一栏（中英双语），显示接口、地址池与包计数。
  之前 `[vpn]` 的运行时状态在面板上完全看不到。

### 测试

- `pkg/vpn` 新增 Linux 专用测试 `TestTunDeviceTakesAnAddress` 与
  `TestTunDeviceRefusesAddressesItCannotUse`：打开真实 tun 设备、配置地址并从内核回读。
  第一个测试在修复前会以 `invalid argument` 失败。
- `pkg/server/strategy_test.go`：补上此前完全没有测试的两个调度策略——`random`（40 次请求
  必须触达每个成员）与 `latency`（时延移动平均更低的成员持续被选中，未测量的成员仍会被试用）。
- `pkg/server/punch_test.go`：四项，覆盖"访客回报直连 → 计入直连并写 `p2p_direct`"、
  "访客什么都没回报 → 记为未知、不计直连"、"要求中继 → 计入中继"、"未知 token 的回报无影响"。
- `pkg/protocol/rendezvous_test.go`：`ATP3` 的编解码、与 `ATP1` 的区分、畸形输入与非法入参。

### 运维测试

`scripts/smoke-test.ps1` 从 59 项扩到 66 项，新增的一节用两个真实客户端组成代理池：

- 两个客户端发布同一个名字、共用一个端口，成员数为 2；
- 六个请求全部被正确应答，且两个成员都各自服务过请求；
- `multipath = 3` 的 UDP 代理：一次数据报会话确实开了 3 条数据连接（读
  `aethertunnel_data_connections_total` 的增量）；
- 打洞结果与访客自己日志里报的路径一致，审计里恰好只有一种结果事件；
- 停掉其中一个成员后池子继续服务，且移除被写进审计。

新增 `scripts/vpn-linux-test.sh`：在真实 tun 设备上跑三层隧道。两端分别处于两个网络命名空间，
因此隧道地址不是对端的本地地址，ICMP 必须真的穿过设备；脚本核对双向 ping、两侧
`ip -s link` 的收发包计数、`GET /api/vpn`、`GET /api/config` 的 `vpn` 段、
审计里的地址分配，以及客户端退出后地址回到池中。缺 root、`/dev/net/tun` 或网络命名空间时
打印原因并以 0 退出。该脚本已接入 CI 的 ubuntu 任务。

### 文档

README 的能力表与运维一节、`docs/CONFIGURATION.md`（`[vpn]`、指标序列、审计事件）、
`docs/SECURITY.md`（审计事件）、`docs/ARCHITECTURE.md`（新增 5.1 打洞结果上报）、
`docs/MIGRATION.md`（新增 v3.4.0 → v3.5.0）、`web/dashboard/README.md`（三层隧道一栏）。

---

## [3.4.0] — 2026-09-21

本版本处理了两个"写了没用"的配置键——一个补上真实行为、一个删除，并修好了面板上两个
长期不动的数字。

### 修复

- **活动连接数只增不减。** 访问者流的两条服务路径（通用代理与 socks5 出口）在流结束时
  没有释放记账，于是 `aethertunnel_streams_active`、按隧道的活动流与面板的
  `active_connections` 会随流量一直往上加。实测：三条**已完成**的请求之后指标读到 3、
  `/api/proxies` 显示 `active_connections: 3`，而不是 0。现在两条路径都释放，
  且 `release` 只生效一次（`sync.OnceFunc`），失败路径与成功路径重复调用也不会重复扣减。
  新增回归测试 `TestStreamCountersReturnToZeroWhenAStreamIsServed`、
  `TestStreamCountersReturnToZeroForASocks5Stream`、`TestStreamCountersReturnToZeroForAnHTTPRequest`，
  以及确认"流在跑的时候能被看见"的 `TestAnOpenStreamIsCountedWhileItRuns`。
- **每个客户端的 `active_streams` 与 `total_streams` 恒为 0。** 这两个字段在会话上存在、
  面板也在读，但没有任何地方写入过。现在在流打开与结束处记账；
  smoke test 的 `the clients view counts the streams the session carried` 会核对实际次数。

### 变更

- `[server] graceful_shutdown_seconds` **之前完全无效**：服务器收到停止信号就把所有客户端
  断开。现在按它的字面含义工作：先关闭监听、停止接受新访客，给正在传输的流最多这么多秒完成，
  然后才断开；期间到达的访客被立即拒绝并计入
  `aethertunnel_streams_refused_while_draining_total`；没有流在传时立刻退出，不会空等。
  日志会写明是 `every stream finished within` 还是 `stream(s) were still running after`。
  负数现在会被 `--check` 拒绝。
- **删除 `[obfuscation] default_type`。** 它从未被任何代码读取，文档也只写着"保留键"。
  配置里再写它会被当作未知键报出来。

### 运维测试

`scripts/smoke-test.ps1` 从 54 项扩到 59 项：

- 优雅关闭三项：信号之后仍在传输的流继续可用；最后一个流结束后服务器立即退出并在日志里
  写明 drained；超过宽限期的流被断开且日志写明放弃。这三项在一台独立服务器上由真实的停止
  信号驱动（Windows 用不带 `/F` 的 `taskkill`，Linux/macOS 用 `kill -TERM`），
  并校验流在宽限期内确实还能交互、宽限期耗尽后确实被断开。
- 活动流计数归零与客户端流计数两项：读 `/metrics` 与 `/api/clients`。

### 文档

`docs/CONFIGURATION.md` 改写 `graceful_shutdown_seconds` 的含义并删除 `default_type`，
指标一节补上新增序列；`docs/MIGRATION.md` 新增 v3.3.0 → v3.4.0 一节；
`server.toml.example`、`README.md` 同步。

---


## [3.3.0] — 2026-09-21

本版本新增一个代理类型（`socks5`）、三项策略能力（按代理的访客 ACL、认证失败自动封禁、
DHT 通告签名），并把它们接进 `scripts/smoke-test.ps1` 的真实运维检查。

### 新增

**代理类型：`socks5` 出口**

- `[[proxies]] type = "socks5"`：访客用标准 SOCKS5（RFC 1928 CONNECT，无认证）指定目标，
  客户端拨号，字节流经隧道转发。没有本地服务，因此 `local_ip` / `local_port` 会被忽略并
  给出警告，`remote_port` 必填。
- `allow_targets` 是**必填**项，列出客户端允许拨号的 CIDR；缺失时注册被拒绝
  （`a socks5 tunnel needs allow_targets`），不会退化成"什么都能连"。不在名单里的目标用
  SOCKS5 回复码 `0x02`（not allowed）拒绝。
- 服务器在收到 CONNECT 之前先完成协商，因此目标不可达时访客看到的是 SOCKS5 错误码而不是
  连接被直接关闭。新增 `aethertunnel_socks5_requests_total` 指标。

**按代理的访客 ACL**

- `[[proxies]] allow_cidrs` / `deny_cidrs`：服务器整体接受连接之后、建立隧道之前，再按该
  代理自己的名单判断来源地址；deny 优先，名单非空而来源无法解析时按拒绝处理。
- 拒绝会写审计记录 `proxy_visitor_denied`（带代理名）并计入
  `aethertunnel_visitors_denied_by_proxy_total`。http/https 代理对被拒访客返回 403。

**认证失败自动封禁**

- `[server] ban_after_failures` / `ban_seconds` / `ban_max_seconds` / `ban_ignore_cidrs`。
  同一来源在 10 分钟窗口内认证失败达到次数后封禁该来源，封禁期间**任何凭据**都先被拒绝、
  不进入握手；认证成功清零计数；重复被封时长翻倍直到上限。
- 新增指标 `aethertunnel_sources_banned_total`、`aethertunnel_banned_connections_refused_total`，
  审计事件 `source_banned` 与 `ban_refused`。

**DHT 通告签名**

- `[dht] signing_key_file`（服务端）：每条通告用 Ed25519 签名，密钥首次使用时生成到该文件
  （0600），公钥可从启动日志、`GET /api/dht` 的 `signing_key` 或新增的 `--dht-key` 读出。
- `[dht] require_signed` / `trusted_keys`（客户端与查询端）：拒绝无签名记录，或只接受指定
  公钥签发的记录。校验在有效期判断之前进行，伪造的记录报 `signature does not verify`
  而不是被当成过期。
- `--dht-lookup` 与 `--discover` 的输出现在会写明记录是否经过签名校验及其公钥。

### 运维测试

`scripts/smoke-test.ps1` 从 38 项扩到 54 项：新增 socks5 出口（到达指定目标、拒绝名单外目标、
面板记账）、按代理 ACL（名单外拒绝、名单内放行、审计记录）、DHT 签名（公钥发布、查询报告
签名者、可信读取端成功、异钥读取端拒绝），以及一处独立的封禁服务器（失败达次数即封禁、
审计与指标、被封来源上有效凭据同样被拒、到期自动解除）。全部用真实二进制与 curl 跑通。
面板另在 Chrome 里验证：列出 socks5 出口的类型与端口、真实 SOCKS5 请求的计数、
名单外目标被拒、中英切换、360 px 布局与断开按钮。

### 修复（由新测试发现）

- 钉住 socks5 与 ACL 的端到端测试根本不覆盖被测路径：`pkg/server` 的测试代理在没有
  `handlers` 表项时直接丢弃数据请求，socks5 测试因此既不拨号也不回应，最终以访客侧超时
  失败（`reply: read tcp ...: i/o timeout`）。现在带目标的请求不再查表。
- 封禁与按代理 ACL 的测试用 127.0.0.2 当"另一个来源地址"，而 macOS 的 lo0 只分配
  127.0.0.1，绑定同网段的其它地址会失败，CI 的 macOS 任务因此有 7 个测试报错
  （Linux 与 Windows 正常，二者在整个 127.0.0.0/8 上都有响应）。规则本身改用单个地址配合
  是否落在名单里的范围验证，真正需要两个来源地址才成立的两条断言（封禁不影响其它地址、
  名单只放行其中一个地址）改为先探测本机能否绑定 127.0.0.2，不能则跳过并写明原因。
  `scripts/smoke-test.ps1` 的同类检查也做了同样的探测。
- `scripts/smoke-test.ps1` 的 `Read-Log` 在文件为空时返回 `$null`，而
  `if ($null -notmatch '模式')` 恒为假，导致**所有读日志的断言都在不校验任何内容的情况下通过**。
  现在它同时读取标准输出与标准错误两个文件，并始终返回字符串。

---


## [3.2.1] — 2026-09-20

### 修复

- **`http` 与 `https` 代理的流量从未被记账。** 反向代理这条路径只更新了组级计数，
  而面板、会话计数与带宽账本读的是成员计数，因此一个 http 代理无论搬运多少字节，
  面板都显示 `0 B` / `0 次请求`，客户端断开时写进账本的也是 `bytes_in=0 bytes_out=0`
  （实测：请求正常返回 200，面板仍是 `total=0 bytes_in=0 bytes_out=0`，
  账本条目同样为 0）。现在每个完成的请求按成员数平均计入各成员，与数据报会话在多路径上的
  记法一致；那三个从未被任何地方读取的组级计数已删除。
  新增两个回归测试：`TestHTTPProxyTrafficIsRecorded`（面板所用的 `Totals()` 与成员摘要）
  与 `TestLedgerRecordsHTTPProxyTraffic`（账本条目），修复前分别失败于
  「the group counted 0 requests, want 1」与「bytes_in is 0, want 30」。
- 文档同步：`docs/SECURITY.md` 的账本一节改为按代理类型说明记账口径，
  `web/dashboard/README.md` 说明 http 代理按请求计数、池内按比例计入成员。

---

## [3.2.0] — 2026-09-20

本版本实现了 v3.1.0 文档中列为"尚未实现"的全部条目（移动端应用与 Windows/macOS 的 tun
设备除外，见文末），并补齐 Kubernetes 清单与容器镜像。

### 新增

**运维与策略层**

- `[server] allow_cidrs` / `deny_cidrs`：按 CIDR 的访问控制，在握手之前执行；
  先匹配 deny，再匹配 allow（allow 为空表示允许全部来源），无法解析的来源地址在存在规则时按拒绝处理。
- `[server] rate_limit_per_second` / `rate_limit_burst`：按来源地址的令牌桶限流，
  同样在握手之前执行；空闲桶会被回收，不在内存中按历史来源地址无限增长。
- `[audit]`：JSON Lines 审计日志，记录接入/拒绝、认证失败、客户端上下线、代理注册与拒绝、
  ACL 与限流拒绝、访客接受与拒绝、打洞结果与隧道地址分配；按 `max_bytes` 轮转，保留一份历史文件。
- `[metrics]`：`GET /metrics` 输出 Prometheus 文本格式（版本 0.0.4），
  含控制连接、认证失败、ACL 拒绝、限流拒绝、数据连接、流数量与并发、双向字节、UDP 数据报与活动会话，
  以及按隧道标签的流与字节序列。可设置 `[metrics] token`，该 token 与面板 token 任一可用。
- `GET /healthz` 与 `GET /readyz`：前者恒为 200，后者在监听器未就绪或正在关闭时返回 503；
  两者都不需要令牌。

**代理类型与调度**

- `udp`：每个访问者来源地址一个会话，服务器用一个 UDP 套接字承载全部会话。
- `http` / `https`：服务器上一套共享监听，按请求的 Host 头选择隧道，支持精确域名、
  `*.通配` 与 `subdomain_host` 后缀匹配；作为反向代理转发并保留流式响应。
- `stcp` / `sudp` / `xtcp`：私有隧道，只对知道 `secret_key` 的访客开放，不开放公网端口。
- `xtcp` 打洞：自研 UDP 打洞（HMAC-SHA256 同时打开 + 可靠有序字节流 `pkg/reliable`），
  失败时经 `[server] p2p_port` 的会合服务回退到中继，两种情况都记入日志与审计。
- 代理池与 `[server] load_balance`：同名代理可由多个客户端组成池，策略为
  `round-robin`、`random`、`latency`、`failover`、`adaptive`（时延移动平均 × 连续失败惩罚）。
- `multipath`：数据报代理最多用 8 条数据连接承载，单条路径故障不影响整个会话。
- `GET /api/proxies` 增加 `member_count`、`members`、`healthy`、`consecutive_failures` 等池状态字段；
  控制台的代理表格新增「成员」列，显示成员数与其中可用（healthy）的个数。

**加密与身份**

- `[transport] enable_tls`：控制端口与所有数据连接使用 TLS，客户端可用 `ca_file` 校验证书。
- `[identity]`：Ed25519 身份签名，服务端可用 `allowed_keys` 白名单、`require_identity` 强制；
  数据连接与访客连接同样校验，不再只有控制连接校验。
- `[encryption] post_quantum`：X25519 与 ML-KEM-768 混合密钥协商，每个会话派生会话密钥，
  每条数据连接再用 `HKDF(session_key, stream_id)` 派生独立密钥。
- `auth_method = "nizk"`：访客用 P-256 上的 Schnorr 证明自己知道 `secret_key`，不发送该值。

**网络与目录**

- `[ledger]`：Ed25519 签名、哈希链式追加的带宽账本（JSONL）。客户端断开时按代理写入一条，
  `GET /api/ledger` 发布公钥、链头、最近条目与按客户端汇总；`--verify-ledger` 离线校验，
  篡改任何一字节或换一串公钥都会失败。
- `[dht]`：Kademlia DHT（160 位标识、k 桶、迭代查找、值/提供者两个命名空间）。服务端把每个
  已发布的代理写成记录（记录里带地址与有效期），客户端可用 `dht.discover` 按名字解析服务器地址，
  并在每次重连前重新解析；运维可用 `--dht-lookup`（服务端配置）与 `--discover`（客户端配置）。
  通告自带有效期，服务端下掉代理后记录会在一个通告周期内失效。
- `[vpn]`：三层隧道。客户端向服务端申请地址，IP 包作为 `TypeVPNPacket` 帧在控制连接上传输；
  服务端用 `pkg/vpn` 的地址池与路由器在多个客户端之间分发。Linux 上打开或创建 tun 设备
  （`device` 为空则向内核申请名字，MTU 写入网卡）；其它平台启动即报错并说明缺少什么。
  地址池不会分配网络地址、广播地址与服务端自用地址；非本客户端的源地址会被丢弃。

**混淆与交付**

- `[obfuscation] disguise = "tls-record"`：把每个写入包进 TLS 1.2 应用数据记录，
  超过 16384 字节的写入按记录上限拆分，读侧透明重组。它不做握手，只改变外观。
- `Dockerfile`（多阶段构建 → distroless 非 root）与 `deploy/kubernetes/`（Namespace、
  ConfigMap、Secret 示例、Deployment、Service、kustomization）。
- `AETHERTUNNEL_AUTH_TOKEN`、`AETHERTUNNEL_DASHBOARD_TOKEN`、
  `AETHERTUNNEL_ENCRYPTION_PASSPHRASE`：凭据可由环境变量提供，并在校验之前生效，
  因此配置文件里可以完全不放密钥。
- 运维子命令：`--dht-lookup`、`--verify-ledger`（服务端），`--discover`（客户端）。
- `scripts/smoke-test.ps1` 扩到 38 项检查：新增目录（发布、两种查询、未知名）、
  账本（记账、校验、篡改检测、异钥拒绝）、隧道设备缺失时的拒绝，以及全程开启的连接伪装；
  新增 `-ProgressLog` 参数与 `-Keep` 保留现场。
- CI 升到 Go 1.24（`crypto/mlkem` 需要），新增六个目标的交叉编译检查与 Linux 上的 `-race` 任务。

### 修复（由新测试发现）

- `pkg/vpn` 的路由器会把读取缓冲区切片排进发送队列，缓冲区被下一次读取复用后，
  排在队列里的包内容会被改写。现在入队前复制，测试用两个不同长度的包复现过该问题。
- `Session.framer` 在会话生命周期中会被抗量子握手替换，而隧道协程同时读取它，
  存在数据竞争；改为加锁访问的 `Framer()` / `SetFramer()`。
- `Server.listener` 由 `Run` 写入、由面板与测试读取，同样存在数据竞争；改为加锁访问。
- `dht.Table` 缺少删除本地记录的方法，导致服务端下掉代理后仍会继续重新发布；
  新增 `Forget`。
- `-dht-lookup` 使用服务端自身配置时会去绑定服务端已经占用的 DHT 端口而失败；
  查询节点现在改绑临时端口，且在没有配置 `bootstrap` 时向 `listen_addr` 指向的节点查询。
- 客户端 `--discover` 与 `dht.discover` 未等待加入 DHT 就查询，首次必然失败；
  现在先完成 bootstrap 再解析。
- 帧长度填充的抖动与补齐在 `[obfuscation] enabled = false` 时也会生效；
  现在 disguise 未启用时会给出警告。
- `obfs` 的 TLS 记录头校验只检查了主版本号，`0x0301`（TLS 1.0）会被当成合法记录；
  现在要求完整的 `0303`。
- IPv6 包的长度校验把"负载长度"当成"整包长度"，导致所有 IPv6 包被判为畸形。
- 测试端口分配在 Windows 上会取到被系统保留的端口，UDP 绑定随即失败；
  UDP 代理测试改用 UDP 端口探测，服务端测试与运维脚本同时探测 TCP 与 UDP。
- `Server.setListener` 递归调用自己，在非可重入互斥锁上自锁死，服务端一启动就卡住；
  现在直接赋值。

### 兼容性

- **线协议升到 4**（`ProtocolVersion = 4`）：本版本新增了帧填充标志位与 16–18 号消息类型，
  3 版对端不认识它们——它会把填充帧里的长度前缀当成数据而拒绝该帧。因此两端必须一起升级。
  版本不一致时握手仍然成功，但两端都会在日志里报出 `protocol mismatch`，
  这一条比"看起来能连上、部分功能静默失效"更容易排查。
- 配置文件向后兼容 v3.1.0：`[server]`、`[client]`、`[[proxies]]` 的既有键含义未变，
  新增段都是可选的，默认关闭。

### 仍未实现

- **移动端应用**：本仓库只产出服务端与客户端两个可执行程序，没有 iOS/Android 工程。
- **Windows 与 macOS 的 tun 设备**：三层隧道只在 Linux 上打开设备；Windows 需要 Wintun 驱动，
  macOS 需要 utun 控制套接字，本程序都不提供。Linux 路径每次 CI 交叉编译，但未在真实 tun
  设备上运行过。

---

## [3.1.0] — 2026-09-20

本版本修复了使核心链路不可用的缺陷，并新增配置校验、可选加密、面板 API、测试与跨平台构建。

### 修复（都是会导致功能完全不可用的缺陷）

- **服务端无法启动**：`main.go` 对同一个 `bind_addr:bind_port` 调用了两次 `net.Listen`，
  第二次必然失败并 `log.Fatalf`，进程启动即退出。现在只监听一个端口，控制连接与数据连接
  复用它。
- **客户端与服务端协议不兼容**：客户端发送 ASCII 文本 `AUTH:<token>`，服务端读取 8 字节
  二进制帧头，认证永远不可能成功；服务端回包客户端也解析不了。现在两端使用同一套
  带版本号的帧格式（`pkg/protocol`），并在握手时交换协议版本与加密算法。
- **心跳消息无法表示**：`NewHeartbeatMessage` 造出空负载，而读取端把 `payloadLen == 0`
  判为错误，于是每次心跳都会踢掉连接。现在空负载合法。
- **隧道数据面是死的**：`HandleConnection` 只读一条消息就返回，连接随后被关闭；目标地址
  被硬编码为服务器自己的 `127.0.0.1:<remote_port>`；服务器从不监听 `remote_port`。
  现在服务器为每条隧道真正绑定公网端口，访问者到来时通过控制连接请客户端回拨一条数据
  连接，两端用 `io.Copy` 半关闭双向搬运（带 32 KiB 缓冲池与空闲超时）。
- **未认证即可开隧道**：旧代码在第一条消息就是 `Proxy` 时直接转发，不检查认证。现在数据
  连接必须携带有效会话 ID 与未使用的流 ID。
- **连接表永久泄漏**：`RemoveConnection` 关闭 socket 却从不 `delete` 表项，超过 100 次
  连接后服务器永久拒绝所有客户端；隧道也不会随会话释放。现在会话与隧道在断开时都会被
  注销，释放公网端口。
- **隧道名在重连后无法复用**：`Session.Close` 清空了隧道表，导致随后注销时什么都没删掉，
  重连的客户端拿到 "a tunnel with that name is already registered" 而失去隧道。现在
  隧道归属会被正确注销，且管理器会把"所属会话已死"的陈旧条目判为可替换。
- **加密完全不可用**：密钥直接取口令字节，而 XChaCha20-Poly1305 要求恰好 32 字节，任何
  普通长度的 token 都会报 `bad key length`。现在用 HKDF-SHA256 派生，任意长度口令可用，
  并支持选择 XChaCha20-Poly1305 或 AES-256-GCM。
- **混淆模块返回被丢弃的明文**：`ObfuscatePacket` 加密后返回的是另一个变量；接收端又在
  已 base64 的数据上再编码一次，任何数据包都解不开。该模块已删除（未实现的功能不再假装
  存在）。
- **WebSocket / HTTP / SCTP 传输一被调用就 panic**：把 `http.ResponseWriter` 断言成
  `net.Conn`、`Close()` 时关闭仍有发送者的 channel、读写锁被用于阻塞的 socket 读导致
  连接无法关闭。这些传输从没有被 `main.go` 启动过，已删除。
- **VPN 包整体不可调用**：`performance` 接口声明 `Disable() bool` 而实现返回 `void`，
  接口无法被满足，调用必 panic；统计模块在同一把非可重入锁上自锁死。已删除。
- **凭据泄漏进日志**：认证失败时把用户提交的完整 token 打进日志。现在只记录来源地址。
- **面板按相对路径读文件**：文件服务器用 `../../web/dashboard`，页面处理器用
  `web/dashboard/...`，两者不可能同时成立，且发布的压缩包里根本没有这些文件。现在用
  `go:embed` 打进二进制。
- **面板显示假数据**：三个页面没有任何网络请求，连接数/带宽/客户端列表/日志/图表分别是
  字面量与 `Math.random()`；`server.html` 因为 CSS 里没有 `.hidden` 规则而根本无法切换
  页面。已重写为单页真实面板（详见"新增"）。
- **`/api/config` 返回示例配置**，包含一个假的 auth token，且与真实配置无关。现在返回
  脱敏后的真实设置。
- 构建脚本与 CI 全部指向不存在的路径（`./server`、`./main_minimal.go`），Docker 构建把
  Go 1.21 与要求 1.22.2 的模块放在一起，CI 的产物路径与 `download-artifact@v4` 不符，
  两个工作流还会向同一个 release 上传同名附件。已重写。
- 版本号被打成 `-X main.Version` 而变量名是小写 `version`，链接器静默忽略，于是二进制永远
  报硬编码的旧版本号。现在 `--version` 能报出真实构建信息。

### 新增

- **`--config` / `--check` / `--version` 命令行参数**（旧版把第一个参数当配置文件路径，
  `--version` 会被当成文件名）。
- **配置校验**：未知配置键会被列出来，`--check` 只校验不启动；端口、代理名重复、不支持的
  代理类型、弱 token、对外暴露却无 token 的面板都会给出明确提示。
- **客户端重连**：指数退避到 `max_reconnect_seconds`，带 ±20% 抖动；断线后自动重新注册
  全部隧道。
- **可选的负载加密**：控制帧与隧道记录都用 AEAD；两端配置不一致时给出
  `encryption mismatch` 而不是无限认证失败。
- **面板 API 与 Bearer token**：`/api/health`（公开）、`/api/status`、`/api/clients`、
  `/api/proxies`、`/api/config`（脱敏）、`DELETE /api/clients/{id}` 断开指定客户端。
- **新面板**：单页、内嵌资源、中英双语（两个语言包键完全对齐）、有真正的移动端导航抽屉、
  空状态与错误提示按实际数据显示、GET/PUT 数据全部用 `textContent` 插入（无 `innerHTML`）。
- **测试**：`pkg/config`、`pkg/crypto`、`pkg/protocol`、`pkg/server` 四组测试，其中
  `pkg/server` 包含真实的端到端测试——启动服务器、注册隧道、访问者连接、数据穿过隧道
  回到本地回声服务，明文与加密各跑一遍；另有会话释放、名字复用、连接上限、垃圾输入
  不致命等回归测试。
- **跨平台构建**：`scripts/build-release.sh`（POSIX）与 `scripts/build-release.ps1`
  （Windows）产出 6 平台 × 2 个二进制 + `SHA256SUMS`；CI 在 Linux/Windows/macOS 上
  跑 `gofmt`/`vet`/`test`/构建；发布工作流按 tag 触发并校验产物格式。
- **文档**：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)、
  [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)、
  [`docs/SECURITY.md`](docs/SECURITY.md)、
  [`docs/MIGRATION.md`](docs/MIGRATION.md)，以及双语 README。

### 移除

- 所有描述不存在功能的文档（共 27 个 md 文件），包括 `PROJECT_SUMMARY.md`、
  `QUALITY_REPORT.md`、`DELIVERY_CHECKLIST.md`、三个安全审计报告与 `docs/` 下 19 个文件。
  需要对照旧说法请查本文件的顶部说明。
- 不可用且未被调用的模块：`pkg/vpn`、`pkg/obfuscation`、`pkg/protocol/{http,websocket,sctp}.go`、
  `pkg/net/mux.go`、`pkg/interfaces`、`sctp-fake`（一个把 `libp2p/go-sctp` 替换成空壳的
  本地模块）、`release/{linux,darwin,windows}-amd64`（三份重复的客户端源码副本）。
- 与产品无关的 Python 编排脚本（`auto_trigger_system.py` 等 4 个）与 `__pycache__`。
- 依赖 `gorilla/websocket` 与 `libp2p/go-sctp`；现在只有 `BurntSushi/toml` 与
  `golang.org/x/crypto`。

### 兼容性

- 配置文件：`server.bind_addr`、`server.bind_port`、`server.auth_token`、`client.server_addr`、
  `client.auth_token`、`[[proxies]]` 的字段含义不变。`[dashboard]` 新增 `bind_addr`/`token`，
  `[encryption]` 是新增段。旧配置里的 `enable_tls`、`[vpn]`、`[obfuscation]`、`[webrtc]`
  等键不再被使用：前者会作为未知键被报出来，后两者会解析但被忽略并给出警告。
- 协议：与旧版本**不兼容**，两端必须一起升级（`ProtocolVersion = 3`）。

---

## [3.0.0] — 2026-02

标签存在，但核心链路未跑通；详细功能清单已按本文件顶部说明删除。

## [2.0.1] / [2.0.0] — 2026-01

同上。

## [1.0.x] / [0.1.1-alpha] — 2025-12 ~ 2026-01

同上。
