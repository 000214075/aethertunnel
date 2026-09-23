# AetherTunnel

**把内网服务发布到公网的隧道工具 · A tunnel that publishes a service behind NAT.**
服务端 + 客户端 + 内置 Web 面板，纯 Go，无 CGO，六个平台开箱可用。
Server, client and a built-in web panel. Pure Go, no CGO, cross-compiled for six platforms.

八种代理类型（`tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5`）、代理池与负载均衡、
可选的后量子加密、Prometheus 指标与 JSONL 审计日志；三层隧道**只在 Linux 上实现**，其他平台
启动 `vpn.enabled = true` 会明确报错退出。
Eight proxy types (`tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5`), pooling with load
balancing, optional post-quantum encryption, Prometheus metrics and a JSONL audit log; the
layer-3 tunnel is **Linux only** and every other platform refuses to start with it enabled.

[![CI](https://github.com/000214075/aethertunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/000214075/aethertunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/tag/000214075/aethertunnel?label=release)](https://github.com/000214075/aethertunnel/releases)
[![License](https://img.shields.io/github/license/000214075/aethertunnel?label=license)](LICENSE)

---

## 中文说明

### 这是什么

AetherTunnel 让一台没有公网 IP 的机器（家里的 NAS、公司的开发机、树莓派）把本地服务
发布到一台有公网 IP 的服务器上。访问者连服务器的端口，流量通过隧道回到你的本地服务。

```
访问者 ──► 服务器公网端口 ──► [隧道] ──► 客户端 ──► 本地服务 127.0.0.1:22
```

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
| 代理类型 | ✅ 可用 | `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5`；http/https 走服务器的共享监听并按 Host 头选择隧道（精确域名、`*.通配`，或服务端 `subdomain_host` 拼出的 `<代理名>.<该值>`），stcp/sudp/xtcp 为私有隧道，socks5 是出口代理（访客指定目标，客户端拨号，`allow_targets` 限定可达范围） |
| XTCP 直连 | ✅ 可用 | 自研 UDP 打洞（HMAC-SHA256 同时打开 + 可靠有序字节流），打洞失败自动回退到服务器中继；访客把实际走通的路径回报给服务器（`ATP3` 数据报），因此指标与审计记录的是真实结果而不是猜测 |
| TCP/UDP 转发 | ✅ 可用 | 访问者 → 服务器端口 → 客户端 → 本地服务；TCP 保留半关闭，UDP 按来源地址分会话 |
| 负载均衡 | ✅ 可用 | 同名代理可由多个客户端组成代理池，策略：`round-robin` `random` `latency` `failover` `adaptive`，以及**在线学习**的 `bandit`（UCB1 多臂老虎机：按每条流的应答速度记奖励，估计各成员并保留探索，所以曾经慢过的成员在恢复后仍会被重新测量；没有离线训练与模型文件）。池只拥有一个端点（取第一个成员的端口），后到的成员即使请求了别的端口也不会另开一个，服务端会把**实际生效的端口**回给客户端，客户端日志按它显示；`scripts/smoke-test.ps1` 用两个真实客户端验到池成员都被分流、掉一个成员后仍继续服务 |
| 多路径 | ✅ 可用 | 数据报代理可把流量分散到最多 8 条数据连接（`multipath`），单条故障不影响整体；运维脚本用一次数据报会话验到确实开了 3 条数据连接 |
| 控制连接与会话 | ✅ 可用 | 认证、心跳、断线自动重连（指数退避 + 抖动）、连接数上限 |
| 可选的数据包加密 | ✅ 可用 | XChaCha20-Poly1305 或 AES-256-GCM，默认**关闭**；控制消息与隧道字节都加密 |
| 密钥派生 | ✅ 可用 | 口令经 HKDF-SHA256 派生 32 字节密钥；口令留空则用 auth_token |
| TLS 传输层 | ✅ 可用 | `[transport] enable_tls`，控制端口与数据连接都走 TLS，可用 `ca_file` 做证书校验 |
| 主机身份 | ✅ 可用 | Ed25519 身份签名，服务端可用 `allowed_keys` 白名单与 `require_identity` 强制；对访客连接同样生效 |
| 抗量子密钥协商 | ✅ 可用 | `encryption.post_quantum`：X25519 与 ML-KEM-768 混合（HKDF 同时纳入两者），每条数据连接单独派生密钥 |
| 零知识证明 | ✅ 可用 | `auth_method = "nizk"`：访客用 P-256 上的 Schnorr 证明自己知道 secret_key，过程中不发送该值 |
| 带宽账本 | ✅ 可用 | Ed25519 签名、哈希链式追加的用量记录（JSONL），`GET /api/ledger` 发布公钥、链头与条目，`--verify-ledger` 可离线校验，`--ledger-proof <文件> --proof-index <n>` 导出到第 n 条为止的前缀（只凭公钥即可校验，不必交出整条链）；改一个字节、换一串公钥、调换条目顺序或用别条的签名都会失败 |
| 去中心化目录 | ✅ 可用 | 基于 Kademlia 的 DHT（160 位、k 桶、迭代查找）；服务端把已发布的代理写成记录，客户端可用 `dht.discover` 按名字找服务器，运维可用 `--dht-lookup` / `--discover` / `--dht-key` |
| 通告签名 | ✅ 可用 | DHT 上任何节点都能写同一个键，所以服务端用 Ed25519 给每条通告签名；读取端 `require_signed` 拒绝无签名记录，`trusted_keys` 只认指定公钥。改一个字段或换一把key都会失败 |
| 三层隧道 | ⚠️ 仅 Linux | `[vpn]`：客户端从服务端领取地址，IP 包经控制连接转发；服务端是共享一张网卡的路由器。Linux 上打开或创建 tun 设备；其它平台**明确拒绝启动**并说明缺少什么，不会静默降级。Linux 路径已在真实 tun 设备上跑通（`scripts/vpn-linux-test.sh`，两端放在不同网络命名空间里互相 ping），`GET /api/vpn` 与面板的"三层隧道"一栏显示接口、地址池与包计数 |
| 流量混淆 | ✅ 可用 | `pad_to` 补齐帧长度、`jitter_millis` 加抖动；`disguise = "tls-record"` 把每个写入包进 TLS 1.2 应用数据记录 |
| 访问控制 | ✅ 可用 | `allow_cidrs` / `deny_cidrs` 在握手前执行；无法解析的来源在存在规则时按拒绝处理。单个代理还能再限定自己的访客来源（`[[proxies]]` 的 `allow_cidrs` / `deny_cidrs`）。服务端配置里的 `[[proxies]]` 是**按代理名的策略**：限定该名字能以什么类型、哪个对外端口发布，以及哪些来源可以访问，客户端注册时对不上就被拒。运维脚本用一台独立服务器把服务端级规则单独验了一遍：被拒来源在握手前断开、且不计入接入连接数 |
| 连接限流 | ✅ 可用 | 按来源地址的令牌桶，在握手前执行；空闲桶会被回收。运维脚本分别验了桶内请求被放行、超出后被拒并写进审计与日志 |
| 自动封禁 | ✅ 可用 | 同一来源认证失败 `ban_after_failures` 次后，在握手前拒绝该来源 `ban_seconds` 秒，期间任何凭据都被拒；再次违规时长翻倍，上限 `ban_max_seconds`；`ban_ignore_cidrs` 排除负载均衡与监控地址 |
| 审计日志 | ✅ 可用 | JSON Lines，记录接入/拒绝、认证失败、上下线、代理注册/移除与拒绝、面板断连、封禁与被封拒绝、按代理拒绝的访客、访客接受与拒绝、打洞结果（直连/中继/未知）、隧道地址分配；`max_bytes` 到量后按大小轮转，保留 `audit.keep` 代（默认 1，范围 1–100）。写不进去时重开文件并重试该条记录，仍然失败的计入 `aethertunnel_audit_records_lost_total`，`GET /api/status` 的 `audit` 段与面板会把它显示出来——审计日志停下来是没有别的痕迹的 |
| Prometheus 指标 | ✅ 可用 | `GET /metrics`（文本格式 0.0.4），含连接、认证失败、拒绝、封禁、按代理拒绝的访客、socks5 请求数、因关闭被拒的流、流、双向字节、打洞结果与按隧道的序列；每个数字都读自运行中的计数器 |
| 优雅关闭 | ✅ 可用 | 收到停止信号后停止接受新连接，给正在传输的流最多 `server.graceful_shutdown_seconds`（默认 5）秒完成再断开客户端；期间到达的访客被立即拒绝并计入指标；没有流在传时立刻退出，不会空等 |
| 健康探针 | ✅ 可用 | `GET /healthz` 恒 200；`GET /readyz` 在监听器未就绪或正在关闭时返回 503 |
| Web 面板 | ✅ 可用 | 单页、自带资源（编译进二进制）、中英双语、手机可用；含 `/api/ledger`、`/api/dht` 与 `/api/vpn`；代理池在代理表格里逐成员列出，每行是该客户端测得的延迟与连续失败次数（尚未应答过的成员延迟显示 `—`）——`latency`、`failover`、`adaptive` 三种策略依据的就是这两个数 |
| 容器与编排 | ✅ 可用 | `Dockerfile`（多阶段 → distroless）与 `deploy/kubernetes/` 清单；凭据可用环境变量提供，不必写进 ConfigMap |
| 多平台 | ✅ 可用 | linux/darwin/windows × amd64/arm64，`scripts/build-release.*` 一键出 12 个产物 + SHA256 |
| 配置校验 | ✅ 可用 | 未知配置项会**报出来**而不是静默忽略；`--check` 只校验不启动 |
| 单元 + 端到端测试 | ✅ 可用 | 含"访问者→隧道→本地服务"的真实回环测试，明文与加密两种模式；另有 101 项检查的运维脚本 `scripts/smoke-test.ps1`（CI 的 Windows 作业与发布流程都会跑它，发布前不通过就不出 Release；发布后还会把发布页上的二进制下载下来重跑一遍），以及真实 tun 设备上的 `scripts/vpn-linux-test.sh`（20 项）与发布后在真实内核上跑 `scripts/verify-release-linux.sh`（18 项） |
| 逐平台功能验证 | ✅ 可用 | 同一套 69 项功能检查（八种代理类型、共享监听、三个访问者、面板、指标、审计、命令行子命令、两种加密算法、带证书校验的 TLS、伪装、身份认证、DHT 解析、通配域名、socks5 域名目标、客户端日志里流的来源）在可执行的平台上真跑：原生 linux/amd64 **69/69**、qemu 下的 linux/arm64 **69/69**；Wine 下的 windows/amd64（真实 PE）在 67 项那一套上是 **66/66**，其后新增的两项**尚未在 Windows 上执行**（原因见 [`docs/PLATFORMS.md`](docs/PLATFORMS.md) 第 3.1 节）。另有**跨系统矩阵**——两端来自不同平台，六对组合各 24 项隧道检查全过，五对组合各 6 项四层安全全过。 |

### 这一版有意不做的能力

**移动端 App**、**Windows/macOS 的 tun 设备**、**区块链/代币**、**WebRTC**、**zk-SNARK**、
**TLS 会话模拟**这六项本版不做。每一条都写清楚了缺的具体是什么、以及同样目的下
本程序真正可用并且已被检查覆盖的做法：见 [`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md)。

清单之所以存在：本仓库在 v3.1.0 之前宣称过 WebRTC、区块链、抗量子加密、AI 路由等 20 项功能，
而对全部 Go 源码检索这些关键词，出现次数为 0。现在的规则是**名称按实际能力书写**——
做到什么写什么，没做的写在上面那份文件里，而不是从文档里消失。

### 加密怎么开

两端必须配置一致，否则连接会在握手阶段报明确的 `encryption mismatch`：

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # 或 "aes-256-gcm"
passphrase = ""                     # 留空 = 用 auth_token 派生
salt = "aethertunnel"               # 两端必须相同
post_quantum = true                 # 再用 X25519 + ML-KEM-768 协商每条连接的密钥
```

密钥不是 auth_token 本身，而是 `HKDF-SHA256(passphrase, salt)` 派生的 32 字节；
每个数据包（控制帧）与每条隧道记录都有独立随机 nonce，篡改会被 AEAD 拒绝并断开连接。
开启 `post_quantum` 后，会话密钥由 X25519 与 ML-KEM-768 两个共享秘密共同派生，
每条数据连接再用 `HKDF(session_key, stream_id)` 单独派生一把密钥。

### 面板

- 资源用 `go:embed` 打进二进制，**不需要**在二进制旁边放 `web/` 目录。
- 默认只监听 `127.0.0.1`。要让外部访问，请设置 `[dashboard].token`，之后 `/api/*` 需要
  `Authorization: Bearer <token>`；`/api/health`、`/healthz`、`/readyz` 始终公开且不含敏感信息。
- 面板轮询 `/api/status`、`/api/clients`、`/api/proxies`，**屏幕上每个数字都来自服务器实时状态**；
  没有客户端时显示空状态。
- `/api/ledger` 返回公钥、链头、最近的账本条目与按客户端汇总的用量；条目本身已签名，
  拿到响应的人可以独立校验。
- `/api/dht` 返回 DHT 节点标识、绑定地址、已知节点数、对外通告的主机名、当前已通告的代理名，
  以及 `signing_key`（通告用的 Ed25519 公钥；未签名部署为空串）。

### 运维

```bash
# 校验配置
./aethertunnel-server --config server.toml --check

# 按名字查一条代理记录（用服务端配置即可，查询节点自己绑临时端口）
./aethertunnel-server --config server.toml --dht-lookup ssh

# 读出通告签名公钥，填进客户端的 dht.trusted_keys
./aethertunnel-server --config server.toml --dht-key

# 从客户端角色解析同一个名字
./aethertunnel-client --config client.toml --discover ssh

# 读出本客户端的身份公钥，填进服务端的 identity.allowed_keys
./aethertunnel-client --config client.toml --identity

# 通过一个 socks5 出口访问内网地址
curl --socks5-hostname 服务器IP:6100 http://10.0.0.5:8080/

# 离线核对带宽账本，只需要公钥
./aethertunnel-server --verify-ledger ledger.jsonl --ledger-key <64 位十六进制公钥>

# 导出到第 41 条为止的账本前缀：给审计方这一段用量，不必交出整条链
./aethertunnel-server --ledger-proof ledger.jsonl --proof-index 41 > proof.jsonl
```

封禁与按代理 ACL 都不需要额外命令，但它们留下的痕迹可以这样看：

```bash
curl -s http://127.0.0.1:7500/metrics | grep -E 'sources_banned|banned_connections_refused|visitors_denied_by_proxy|socks5_requests'
grep -E 'source_banned|ban_refused|proxy_visitor_denied' aethertunnel-audit.jsonl
```

`scripts/smoke-test.ps1` 会构建两个二进制、起一个本地服务集合（TCP/UDP/HTTP 回显）、
拉起服务端与两个客户端、逐项验证上表里的能力，最后打印通过数与失败项。它会绑定回环端口，
运行结束后清理临时目录；加 `-Keep` 保留现场，加 `-ProgressLog <path>` 实时记录进度。

其中三项用真实的停止信号驱动一台独立服务器，验证优雅关闭：信号之后仍在传输的流继续可用、
最后一个流结束后服务器立即退出并在日志里写明 drained、超过宽限期的流被断开。
Windows 上发信号用不带 `/F` 的 `taskkill`，Linux 与 macOS 上等价于 `kill -TERM`；
`Stop-Process` 是硬杀，会跳过整个排空过程，因此脚本不用它来测这一项。

另外三项用两个真实客户端组成代理池，验证负载均衡：两个客户端都把成员数报成 2、
六个请求确实分到两个成员、停掉其中一个成员后池子仍继续服务且移除被写进审计。
多路径一项从 `/metrics` 读 `aethertunnel_data_connections_total` 的增量，确认一次 UDP 会话
真的开了 3 条数据连接。打洞一项把服务器上的 `aethertunnel_p2p_direct_total` /
`aethertunnel_p2p_relayed_total` 与访客自己日志里报的路径对照，两边必须一致。

`idle_timeout_seconds` 一项把访客客户端的空闲上限设成 2 秒：先做一次回显确认会话可用，
然后一个字都不发，连接必须在几秒内自己结束；紧跟的第二项在同一段时间里持续来回发字节，
连接必须活着。两项一起把"超时针对静默、而不是针对连接时长"钉住。

最后两节各起一台独立服务器，把与来源地址有关、在主服务器上会互相干扰的规则单独跑一遍。
一节是服务端级的 `deny_cidrs` 与令牌桶限流：被拒来源在握手前断开、不计入
`aethertunnel_control_connections_total`，两种拒绝分别出现在 `/metrics`、审计与日志里。
另一节把 `[audit] max_bytes` 设成 1024 并制造足够多的拒绝记录，核对轮转真的发生：
上一代文件存在且每一行都能被 JSON 解析，当前文件已经重新从小尺寸开始增长。

```bash
# 手工做一次同样的验证：起服务、保持一条流、发信号、看日志
taskkill /PID <服务器PID>            # Windows，不带 /F
kill -TERM <服务器PID>               # Linux 与 macOS
# 日志里应出现 shutting down: 以及 graceful shutdown: ...（一行）
```

三层隧道没法在 Windows 上验，脚本 `scripts/vpn-linux-test.sh` 在 Linux 上做这件事：
它把两端放进两个网络命名空间（否则内核会把隧道地址当成自己的地址，绕开隧道直接应答），
在真实的 tun 设备上互相 ping，然后核对两个接口的收发包计数、`GET /api/vpn`、
`GET /api/config` 的 `[vpn]` 段以及审计日志里的地址分配。需要 root、`/dev/net/tun`
和网络命名空间；缺任何一样就打印原因并以 0 退出，不会把 CI 弄红。

```bash
sudo scripts/vpn-linux-test.sh bin/aethertunnel-server bin/aethertunnel-client
# 关键行：PASS  服务端与客户端跨隧道互相 ping 通；两侧接口双向都有包
```

### 构建与测试

```bash
make build          # 本机两个二进制 → bin/
make test           # 单元 + 端到端测试
make test-race      # 同上，带竞态检测（需要 CGO 与 C 编译器）
make vet
make cross          # 12 个产物 + dist/SHA256SUMS
make check          # 校验示例配置
```

Windows 无 make 时：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1 -Version v3.7.0
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1
```

### 从旧版本升级

旧版命令行参数与配置键有一部分已经不同，请读 [`docs/MIGRATION.md`](docs/MIGRATION.md)。
一句话版本：`auth_token`、`bind_port`、`[[proxies]]` 的写法保持不变；`[vpn]` 段已按实现重写，
旧键 `bind_addr`/`port`/`auth_token`/`protocol` 不再被接受。

### 安全模型

- 认证可以是共享密钥（`auth_token`）或 Ed25519 身份，两者都不是证书；token 用常数时间比较，
  失败时**不会**把 token 写进日志。
- `[encryption]` 保护负载，`[transport] enable_tls` 提供传输层加密与服务器证书认证，
  `[obfuscation] disguise` 只改变外观、不提供任何机密性。
- 面板 API 只有 Bearer token 一种保护，没有登录会话、没有多用户。
- 私有隧道（stcp/sudp/xtcp）的 secret_key 用 `auth_method = "nizk"` 时不会出现在线上，
  但代理的元数据（名字、类型、地址）会出现在 DHT 里，除非把 `[dht]` 关掉；
  启用 `[dht] signing_key_file` 后这些记录带签名，读取端可以据此拒绝改写的记录。
- 自动封禁按**来源地址**记账：部署在负载均衡、反向代理或监控系统后面时，那些地址必须写进
  `ban_ignore_cidrs`，否则它们会与攻击者共享同一个来源地址并被一起拒绝。

详见 [`docs/SECURITY.md`](docs/SECURITY.md)。

---

## English

### What it is

AetherTunnel publishes a service that sits behind NAT onto a machine with a public
address. A visitor connects to the server's port; the bytes travel back through the
tunnel to the service running on your own machine.

```
visitor ──► server public port ──► [tunnel] ──► client ──► local service 127.0.0.1:22
```

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
| Proxy types | ✅ works | `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5`; http and https use one shared listener and are selected by the Host header; stcp, sudp and xtcp are private; socks5 is an exit, where the visitor names the target and `allow_targets` bounds what may be reached |
| XTCP direct path | ✅ works | its own UDP hole punching (HMAC-SHA256 simultaneous open over a reliable ordered byte stream) with an automatic fall back to the server's relay; the visitor reports the path it actually took with an `ATP3` datagram, so the metrics and the audit log record the outcome rather than a guess |
| TCP and UDP forwarding | ✅ works | visitor → server port → client → local service; TCP preserves half-close, UDP keeps one session per source address |
| Load balancing | ✅ works | several clients may publish one name as a pool; strategies `round-robin`, `random`, `latency`, `failover`, `adaptive` and `bandit` — a UCB1 multi-armed bandit that learns online which member answers fastest (each stream rewards its speed, and the least-observed member is still sampled so one that recovers is found again; no offline training, no model file). A pool owns one endpoint (the first member's port), so a member that arrives later and asks for another port does not get a second one; the server reports the port that is actually in effect and the client's log shows that one. `scripts/smoke-test.ps1` runs two real clients and checks that both members serve, and that the pool keeps serving after one of them leaves |
| Multipath | ✅ works | a datagram proxy spreads traffic over up to 8 data connections (`multipath`), and the loss of one does not stop the rest; the operations script checks that one datagram session really opens three connections |
| Control session | ✅ works | auth, heartbeat, exponential-backoff reconnect with jitter, connection limit |
| Optional packet encryption | ✅ works | XChaCha20-Poly1305 or AES-256-GCM, **off** by default, covers control frames and tunnelled bytes |
| Key derivation | ✅ works | HKDF-SHA256 over the passphrase; an empty passphrase falls back to `auth_token` |
| TLS transport | ✅ works | `[transport] enable_tls` wraps the control port and every data connection; `ca_file` pins the server certificate |
| Host identity | ✅ works | Ed25519 assertions, with `allowed_keys` and `require_identity` on the server, checked on data connections from visitors as well |
| Post-quantum key agreement | ✅ works | `encryption.post_quantum`: X25519 together with ML-KEM-768, both folded into one HKDF, and a separate key per data connection |
| Proof of token knowledge | ✅ works | `auth_method = "nizk"` proves knowledge of `secret_key` with a Schnorr proof over P-256; the secret itself is never sent |
| Bandwidth ledger | ✅ works | Ed25519-signed, hash-chained usage records (JSONL); `GET /api/ledger` publishes the public key, the chain head and the entries, `--verify-ledger` checks them offline, and `--ledger-proof <file> --proof-index <n>` writes the prefix up to entry n, which verifies on its own so one period's usage can be shown without handing over the rest of the chain; one altered byte, a different key, two entries swapped or another entry's signature all fail |
| Decentralised directory | ✅ works | a Kademlia DHT (160-bit, k-buckets, iterative lookup); the server publishes one record per proxy, a client resolves a name with `dht.discover`, and operators use `--dht-lookup`, `--discover` or `--dht-key` |
| Signed announcements | ✅ works | any node can write to a proxy's key, so the server signs every record with Ed25519; a reader sets `require_signed` to refuse unsigned records and `trusted_keys` to believe named keys only. One altered field or a different key fails |
| Layer-3 tunnel | ⚠️ Linux only | `[vpn]`: a client is given an address and its IP packets travel on the control connection; the server is a router over one shared interface. Linux opens or creates a tun device; every other platform **refuses to start** and says what is missing instead of degrading silently. The Linux path runs on a real tun device in `scripts/vpn-linux-test.sh`, with the two ends in separate network namespaces, and `GET /api/vpn` plus the dashboard's layer-3 panel report the interface, the pool and the packet counters |
| Traffic obfuscation | ✅ works | `pad_to` rounds frame lengths, `jitter_millis` adds delay, and `disguise = "tls-record"` puts every write inside TLS 1.2 application-data records |
| Access control | ✅ works | `allow_cidrs` / `deny_cidrs` before the handshake; an unparseable source is refused when any rule exists. A single proxy can restrict its own visitors further with `allow_cidrs` / `deny_cidrs` in its `[[proxies]]` block |
| Rate limiting | ✅ works | a per-source token bucket before the handshake, with idle buckets reclaimed |
| Automatic ban | ✅ works | after `ban_after_failures` failed authentications one source is refused before the handshake for `ban_seconds`, and every attempt from it is refused in the meantime whatever credential it carries; the window doubles for a repeat offender up to `ban_max_seconds`, and `ban_ignore_cidrs` exempts load balancers and monitors |
| Audit log | ✅ works | JSON Lines for accepted and refused connections, authentication failures, disconnects, proxy registrations and refusals, bans and ban refusals, per-proxy visitor refusals, proxy removals, dashboard disconnects, visitor outcomes, punch results (direct, relayed or unknown) and tunnel address assignments; rotates by size, keeping `audit.keep` generations (1 by default, up to 100). A record that cannot be written reopens the file and is retried; one that still cannot be written moves `aethertunnel_audit_records_lost_total` and shows up in the `audit` section of `GET /api/status` and on the panel, because a log that stopped recording leaves no other trace |
| Prometheus metrics | ✅ works | `GET /metrics` in the text format 0.0.4: connections, authentication failures, refusals, bans, per-proxy visitor refusals, socks5 requests, streams refused while shutting down, streams, bytes both ways and per-tunnel series; every number is read from a live counter |
| Graceful shutdown | ✅ works | on a stop signal the server stops accepting, gives the streams already running up to `server.graceful_shutdown_seconds` (5 by default) to finish, and only then disconnects the clients; a visitor that arrives meanwhile is refused and counted, and an idle server exits at once instead of sitting out the grace period |
| Health probes | ✅ works | `GET /healthz` is always 200; `GET /readyz` is 503 before the listener is up and while shutting down |
| Web dashboard | ✅ works | one self-contained embedded page, English + 简体中文, usable on a phone, including `/api/ledger` and `/api/dht`; a pool is listed member by member in the proxies table, each line carrying the latency that client measured and its consecutive failures (a member that has not answered yet shows `—` for latency) — the two numbers `latency`, `failover` and `adaptive` act on |
| Containers and orchestration | ✅ works | a multi-stage `Dockerfile` ending in distroless, and manifests under `deploy/kubernetes/`; credentials can come from environment variables instead of the ConfigMap |
| Platforms | ✅ works | linux/darwin/windows × amd64/arm64; `scripts/build-release.*` produces 12 binaries + SHA256 |
| Config validation | ✅ works | unknown keys are **reported**, not ignored; `--check` validates without starting |
| Tests | ✅ works | unit tests, a real end-to-end tunnel test in cleartext and encrypted modes, a 101-check operations script `scripts/smoke-test.ps1` (run by CI, required before a release is created, and run again against the published binaries afterwards), a 20-check layer-3 run on real tun devices in `scripts/vpn-linux-test.sh`, and `scripts/verify-release-linux.sh` (18 checks), which runs the published Linux binaries on a real kernel after they are uploaded |
| Per-platform checks | ✅ works | the same 69 checks (eight proxy types, shared listeners, three visitor kinds, dashboard, metrics, audit, command-line subcommands, both ciphers, TLS with certificate verification, the disguise, identity authentication, DHT resolution, wildcard domains, a hostname socks5 target, and how a stream reached the client, read out of its log) run on every platform this machine can execute: **69/69** natively on linux/amd64 and **69/69** on linux/arm64 under qemu-aarch64. The windows/amd64 build (real PE) stood at **66/66** on the set of 67; the two checks added since have **not run on Windows** — see section 3.1 of [`docs/PLATFORMS.md`](docs/PLATFORMS.md) for why; plus a **cross-system matrix** — the two ends on different platforms — six platform pairs at 24 tunnel checks each and five pairs at 6 security-layer checks each, all passing. Which targets these checks cannot execute, and why, are in [`docs/PLATFORMS.md`](docs/PLATFORMS.md) |

### What this release deliberately does not do

Six capabilities are out of scope: a **mobile app**, **tun devices on Windows and macOS**, a
**blockchain**, **WebRTC**, **zk-SNARKs** and **TLS session emulation**. Each entry in [`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md) says what
exactly is missing and what this program offers instead for the same purpose, with the checks that
cover the alternative.

The list exists because before v3.1.0 this repository claimed twenty features — WebRTC, a
blockchain, post-quantum encryption, AI routing among them — that a search of the entire Go source
turned up **zero** times. Names are now written to match what the code does: what is implemented is
described as such, and what is not is in that file rather than missing from the documentation.

### Encryption

Both ends must agree, or the handshake fails with a clear `encryption mismatch`:

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # or "aes-256-gcm"
passphrase = ""                     # empty = derive from auth_token
salt = "aethertunnel"               # must match on both ends
post_quantum = true                 # agree per-connection keys with X25519 + ML-KEM-768
```

The key is `HKDF-SHA256(passphrase, salt)`, not the token itself. Every control frame and
every stream record carries a fresh random nonce, and a tampered record is rejected by the
AEAD and drops the connection. With `post_quantum` on, the session key is derived from both
an X25519 and an ML-KEM-768 secret, and every data connection derives its own key with
`HKDF(session_key, stream_id)`.

### Dashboard

Assets are embedded with `go:embed`, so a released binary serves its own UI with nothing next
to it. It listens on loopback by default; set `[dashboard].token` to expose it, after which
`/api/*` needs `Authorization: Bearer <token>` (`/api/health`, `/healthz` and `/readyz` stay
public and carry no secrets). Every number on screen comes from live server state; empty lists
say they are empty.

- `/api/ledger` returns the signing public key, the chain head, the most recent entries and the
  per-client totals. The entries are already signed, so a reader can check them independently.
- `/api/dht` returns the DHT node identity, its bound address, the number of known peers, the
  host it advertises and the proxy names it currently announces.

### Operations

```bash
# validate a configuration without starting
./aethertunnel-server --config server.toml --check

# resolve a proxy record by name (the server's own configuration works: the query node binds
# an ephemeral port)
./aethertunnel-server --config server.toml --dht-lookup ssh

# the same from a client-role configuration
./aethertunnel-client --config client.toml --discover ssh

# print this client's public identity key, to put into the server's identity.allowed_keys
./aethertunnel-client --config client.toml --identity

# check a bandwidth ledger offline; only the public key is needed
./aethertunnel-server --verify-ledger ledger.jsonl --ledger-key <64 hex characters>

# write the ledger prefix up to entry 41: hand an auditor that period only
./aethertunnel-server --ledger-proof ledger.jsonl --proof-index 41 > proof.jsonl
```

`scripts/smoke-test.ps1` builds both binaries, starts a set of local services (TCP, UDP and
HTTP echoes), starts the server and two clients, exercises every capability in the table above
and prints the number of checks that passed and any that failed. It binds loopback ports and
removes its working directory when it finishes; `-Keep` preserves the directory and
`-ProgressLog <path>` appends each step and verdict as it happens.

Three of its checks drive the graceful shutdown with a real stop signal against a server of
their own, and three more run two clients as a pool to check load balancing, multipath and the
hole-punch outcome. The layer-3 tunnel cannot be checked on Windows: `scripts/vpn-linux-test.sh`
does that on Linux, with the two ends in separate network namespaces so that the kernel cannot
answer for a tunnel address itself, and checks the ping, both interfaces' packet counters,
`GET /api/vpn`, the `[vpn]` section of `GET /api/config` and the address assignment in the audit
log. It needs root, `/dev/net/tun` and a network namespace; when one of those is missing it says
which and exits 0 rather than turning a CI run red.

### Build and test

```bash
make build     # both binaries into bin/
make test      # unit + end-to-end tests
make test-race # the same under the race detector (needs CGO and a C compiler)
make vet
make cross     # 12 artifacts + dist/SHA256SUMS
make check     # validate the example configs
```

### Upgrading from an older release

Some flags and config keys changed; see [`docs/MIGRATION.md`](docs/MIGRATION.md). In short:
`auth_token`, `bind_port` and the `[[proxies]]` blocks keep their meaning, the `[vpn]` section
has been rewritten around what is implemented, and the old keys `bind_addr`, `port`,
`auth_token` and `protocol` in that section are no longer accepted.

### Security model, stated plainly

- Authentication is a shared secret (`auth_token`) or an Ed25519 identity, and neither is a
  certificate. The token comparison is constant-time and a failed attempt never writes the
  offered token to the log.
- `[encryption]` protects the payload, `[transport] enable_tls` provides transport encryption
  and server authentication, and `[obfuscation] disguise` changes only the appearance: it
  provides no confidentiality.
- The dashboard has a single Bearer token: no user accounts, no sessions.
- A private tunnel's `secret_key` is not sent on the wire when `auth_method = "nizk"`, but the
  proxy's metadata (name, type, address) is published to the DHT unless `[dht]` is disabled.
  With `[dht] signing_key_file` those records carry a signature, which a reader uses to refuse
  a rewritten one.
- The automatic ban counts by **source address**: behind a load balancer, a reverse proxy or a
  monitoring host, those addresses belong in `ban_ignore_cidrs`, or they share the source
  address of an attacker and are refused along with it.

See [`docs/SECURITY.md`](docs/SECURITY.md) for the full list, including what an attacker on
the path can still learn.

### License

MIT — see [LICENSE](LICENSE).
