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
| `[obfuscation] default_type` | 已删除。它从未被任何代码读取，`--check` 现在会把它当未知键报出来 |
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

## 4. 从 v3.2.1 到 v3.3.0

不需要改动任何既有配置：协议版本仍是 4，没有删除或改名任何键，旧配置照旧可用。
新增的都是可选项，默认值保持既有行为。

| 新增 | 位置 | 默认 | 打开后发生什么 |
|---|---|---|---|
| `[[proxies]] type = "socks5"` | 客户端 | 不使用 | 新增一种代理类型：访客指定目标，客户端拨号；`remote_port` 与 `allow_targets` 都是必填 |
| `[[proxies]] allow_cidrs` / `deny_cidrs` | 客户端 | 空 | 该代理只服务名单内的访客来源；空 = 不限（与升级前一致） |
| `[server] ban_after_failures` | 服务端 | 0 | 0 = 不封禁；设置后同一来源认证失败达到次数即被拒 |
| `[server] ban_seconds` / `ban_max_seconds` / `ban_ignore_cidrs` | 服务端 | 300 / 3600 / 空 | 封禁时长、上限与豁免地址 |
| `[dht] signing_key_file` | 服务端 | 空 | 空 = 通告不签名（并把这件事写进启动警告）；设置后每条通告带 Ed25519 签名 |
| `[dht] require_signed` / `trusted_keys` | 客户端与查询端 | false / 空 | 拒绝无签名通告，或只接受指定公钥签发的通告 |
| `--dht-key` | 服务端命令行 | — | 打印通告签名公钥 |

升级后如果要让客户端只接受签名通告，顺序是：先在服务端设置 `[dht] signing_key_file`
并重启，用 `--dht-key` 读出公钥，再把它填进客户端的 `[dht] trusted_keys`（可同时设
`require_signed = true`）。反过来先给客户端设 `trusted_keys` 会让它拒绝升级前发布的所有记录。

封禁按**来源地址**记账，因此部署在负载均衡或反向代理后面、或由监控系统探测的地址，必须写进
`ban_ignore_cidrs`，否则它们会与攻击者共用同一个来源地址。

## 4.1 从 v3.3.0 到 v3.4.0

- **删除 `[obfuscation] default_type`**：它从未被读取，配置里写了它不会报错也不会有任何效果。
  现在会被当作未知键，`--check` 会失败并指出该行；删掉这一行即可。
- `[server] graceful_shutdown_seconds` 从"写了没用"变成真实行为：收到停止信号后先停止接受新连接，
  给正在传输的流最多这么多秒完成，然后才断开客户端。默认 5 秒；这个值现在也参与校验，负数会被拒绝。
- 面板与 `/api/clients` 里每个客户端的 `active_streams` 与 `total_streams` 之前恒为 0，
  现在是真的计数；同一张表里的 `active conns` 之前只增不减，现在流结束就归零。
  升级后如果看到这两个数字发生变化，那是在读真实状态，不是在读旧版本的常量。

## 4.2 从 v3.4.0 到 v3.5.0

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，
两端的旧配置可以直接用。变的是几件"一直没做到"的事：

- **Linux 上的 `[vpn] enabled = true` 之前根本起不来。** 服务端给自己的 tun 设备配地址时
  把整个 `sockaddr_in` 交给了只接受 4 字节地址的接口，内核以 `EINVAL` 拒绝，
  服务端随后拒绝启动。这个缺陷只在真实设备上出现，用假设备的单测看不到。
  现在 Linux 上这条路径已在真实 tun 设备上跑通（`scripts/vpn-linux-test.sh`），
  `pkg/vpn` 也补上了打开真设备并回读地址的测试。
- **客户端收到停止信号后不会立刻退出。** 会话循环阻塞在读控制帧上，只有心跳应答到达时
  才会重新检查取消状态，因此 `Ctrl-C` 之后最长要等一个 `heartbeat_seconds`（默认 30 秒）。
  现在取消会直接关闭控制连接，会话立即结束。
- **打洞结果有了真实来源。** 访客在直连建立后通过会合端口回报路径（`ATP3` 数据报）。
  v3.4.0 及更早的客户端不会发这个数据报，因此这类会话的打洞结果记为 `p2p_abandoned`
  （未知），`aethertunnel_p2p_direct_total` 对它们保持为 0；升级客户端后才会出现直连计数。
  服务端不认识这个数据报也不会出错：旧服务端会把它当作无效的会合请求丢弃。
- **`proxy_removed` 与 `dashboard_action` 现在真的会写出来。** 代理池少一个成员也会记录，
  从面板断开客户端也会记录。之前这两个事件名只出现在文档里。
- **新增只读接口 `GET /api/vpn`**，并把它并入 `GET /api/config` 的 `vpn` 段；
  面板的"三层隧道"一栏读的就是它。之前 `[vpn]` 的运行时状态在面板上完全看不到。

## 4.3 从 v3.5.0 到 v3.6.0

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，
v3.5.0 的配置可以直接用。变的是两件事：

- **不带 `domains` 的 `http`/`https` 代理现在能加载了。** 之前客户端把它当作配置错误
  直接拒绝（`a http tunnel needs at least one entry in domains`），因此服务端的
  `subdomain_host` 约定虽然写在文档与配置示例里，却没有任何配置能触发它，服务端那段
  注册逻辑不可达。现在客户端只给出一条警告，注册时由服务端决定：`server.subdomain_host`
  有值就按 `<代理名>.<该值>` 发布，为空则拒绝这个代理并在错误里点出该设置。
  如果你之前为了让配置通过校验而随手填了一个占位域名，现在可以删掉它并改用
  `subdomain_host`；已经填了真实域名的配置行为完全不变。
- **`idle_timeout_seconds`、`audit.max_bytes`、`dht.ttl_seconds` 三个键的语义没有变化，
  但它们此前从未被任何测试或脚本设置过**（一直是默认值在生效），现在都补上了真实验证，
  细节见 CHANGELOG。使用上的两点提醒：`dht.ttl_seconds` 必须不小于
  `dht.announce_ttl_seconds`，否则配置校验直接报错；`audit.max_bytes` 的大小是在写入
  下一条记录之前检查的，因此上一代文件最多比该值多一条记录。

## 4.4 从 v3.6.0 到 v3.7.0

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，
v3.6.0 的配置可以直接用。变的是审计日志写不进去时的行为，以及几处此前没有任何验证
覆盖的接口、指标与命令行参数：

- **审计日志写不进去时会重新打开路径并重试，仍然失败才计入丢失。** 在此之前
  `Auditor.Record` 丢弃写入错误，而注释声称这个错误会在面板上浮现——没有任何代码
  读过它。两种情况都会命中：文件被外部轮转改名后，服务器手上的句柄指向旧文件，
  操作者查看的路径从此不再有新记录；重开失败则把句柄置空，此后每条记录都被丢掉，
  而 `GET /healthz` 仍是 200。现在每条记录写入前会核对配置路径是否仍指向手上这个
  文件，路径被换掉或整个消失都会重开（消失时重建），写入失败会重开并重试一次，
  仍然失败才计入 `aethertunnel_audit_records_lost_total`。
  写不进去**不会**让服务器停止服务，这是有意的：否则一个只读的日志目录就能让隧道下线。
  升级后请确认 `GET /api/status` 的 `audit.writable` 为 `true`。
- **`aethertunnel_control_rejected_total` 现在把 ACL 拒绝与限流也算进去。** 它此前只
  统计容量与封禁，说明文字却写着"容量、ACL、限流或封禁"。两个计数器故意重叠：
  汇总的回答"握手前一共挡掉多少"，更具体的回答"为什么"。
- **`GET /api/status` 的 `traffic` 改为自服务器启动起累计**，与 `/metrics` 的两个字节
  计数器一致。它此前等于"当前注册的这些隧道各自累计了多少"，最后一个发布该代理的
  客户端断开后归零。按隧道的数字仍随注册重置，留在 `/api/proxies` 里。
- **UDP 数据报会话的字节计入全局与按隧道的字节计数**，此前它只写在账本里。
  数据报不计为"流"，`streams_total` 不受影响。这些字节在会话释放时一次性计入，
  即该来源地址安静 `server.read_timeout_seconds`（默认 120）秒之后；按数据报即时增加的
  只有 `aethertunnel_udp_datagrams_total`。把这个值调小（例如 2）可以让面板更快显示出来。
- **`GET /api/status` 新增 `audit` 段**（`enabled`、`writable`、`path`、`max_bytes`、
  `bytes_written`、`write_failures`、`records_lost`、`recovered`、`last_error`），
  面板的"服务器状态"栏新增"审计日志"一行与丢失横幅，中英双语。配置了审计但当前写不进去
  时报告为 `enabled: true` 且 `writable: false`，而不是 `enabled: false`——这两种情况对
  运维意味着完全不同的东西。

## 4.5 从 v3.7.0 到 v3.7.1

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，v3.7.0 的配置
可以直接用。变的是两件事：

- **新增 `[audit] keep`**：轮转时保留多少代（`<path>.1` … `<path>.<keep>`），范围 1–100，
  默认 1，与 v3.7.0 的行为完全相同（只保留 `<path>.1`）。要更长的保留窗口就把它调大；
  各代按序号往后挪一位，最老的一代被覆盖丢弃，保留的文件数量始终等于 `keep`。
- **端到端运维脚本现在跑在流水线里**：CI 的 Windows 作业每次推送都执行
  `scripts/smoke-test.ps1`，发布流程也在标签上执行同一个脚本，通过之后才创建 Release；
  Release 建好后再有一个作业把发布页上的产物下载下来，核对 `SHA256SUMS`、版本与示例配置，
  并用那两个已发布的二进制把整套脚本再跑一遍。自己部署前想复现这一轮检查，直接跑同一个
  脚本即可（`-ServerExe` / `-ClientExe` 可以指向自己手上的二进制，不重新构建）。

## 5. 升级检查清单

1. 两端一起换成 v3.2.0 的二进制。
2. 用 `--check` 校验新配置，把报出的未知键逐个处理掉；`[vpn]` 的旧键必须删掉。
3. 决定是否开启 `[encryption]` 与 `[transport]`；加密的两端必须填写**完全相同**的
   `algorithm`、`salt` 与 `passphrase`，TLS 的客户端必须能校验服务端证书。
4. 若面板需要对外访问，设置 `[dashboard].token`；对外暴露 `/metrics` 时设置 `[metrics].token`。
5. 启动后确认：客户端日志出现 `connected ... as session <id>` 与
   `server confirms N tunnel(s)`；面板 `/api/status` 的连接数与隧道数符合预期。
6. 用真实客户端做一次访问（例如 `ssh -p <remote_port> ...`），确认数据真的通。
7. 用到的新功能各自验证一次：`--dht-lookup`、`--verify-ledger`、`--discover`、`--dht-key`，
   或直接跑 `scripts/smoke-test.ps1`（86 项检查，覆盖全部代理类型、签名通告、按代理 ACL、
   服务端级拒绝与限流、审计轮转与保留代数、审计写不进去与恢复、空闲超时、自动封禁、
   宽限期内的拒绝与探针、代理池、多路径与打洞结果）。Linux 上再用
   `sudo scripts/vpn-linux-test.sh bin/aethertunnel-server bin/aethertunnel-client`
   验一次三层隧道。
