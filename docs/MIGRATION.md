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
  Release 建好后另有两个作业把发布页上的产物下载下来再验一遍——Windows 上核对
  `SHA256SUMS`、版本与示例配置，并用那两个已发布的二进制重跑整套运维脚本；Linux 上跑
  `scripts/verify-release-linux.sh`（服务端起得来、`/healthz` 200、审计日志被 `mv` 走之后
  按配置路径重建、`audit.keep` 的代数、`SIGTERM` 后退出）。自己部署前想复现这些检查，
  直接跑同一个脚本即可（`-ServerExe` / `-ClientExe` 可以指向自己手上的二进制，不重新构建）。

## 4.6 从 v3.7.1 到 v3.7.2

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，v3.7.1 的配置
可以直接用。这一版没有改动服务端行为，变的是面板与检查：

- **面板多了三处显示**：代理表的「负载均衡」列（池当前使用的策略）、客户端表的
  「客户端版本」列、配置页的「发布的代理」行。三者都是 `/api/proxies`、`/api/clients`、
  `/api/config` 早就在返回的字段，只是此前没有显示出来。
- **检查更多**：`scripts/smoke-test.ps1` 从 86 项到 89 项（认证失败、被拒绝的注册、面板
  断开各自留下审计记录），`scripts/vpn-linux-test.sh` 从 17 项到 20 项（服务的
  `vpn.require = true` 拒绝一个没有 `[vpn]` 段的会话，并留下 `vpn_address_rejected`）。
  这四条审计记录此前没有任何检查读过。

## 4.7 从 v3.7.2 到 v3.7.3

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有删除任何键，v3.7.2 的配置
可以直接用。变的是服务端配置里 `[[proxies]]` 的含义：

- **服务端 `[[proxies]]` 从"解析并计数"变成"按代理名的策略"**。升级前写在服务端配置里的
  条目会被校验、会出现在 `/api/config` 的 `proxies_configured` 里，但服务端对注册与访客
  不做任何比对。现在：
  - 条目里写了 `type` 或 `remote_port` 时，注册该名字的客户端必须一致，否则注册被拒
    （`proxy_rejected`，客户端收到服务端期望的值）。此前写错的客户端能照常发布。
  - 条目里写了 `allow_cidrs` / `deny_cidrs` 时，访客来源要先过这份名单，再过客户端为该
    代理声明的名单。此前服务端名单不生效，只有客户端的名单生效。
  - `type` 留空表示任意类型；服务端条目不再被默认成 `tcp`，因此只写 `name` 的条目仍然
    接受任意类型、任意端口。
- **服务端条目不再要求 `local_port`**。升级前，服务端配置里的一个 `[[proxies]]` 条目若没写
  `local_port`（1–65535）会以 `local_port must be 1-65535` 报错；现在它是策略，不描述本地
  服务，`local_ip`、`local_port`、`group`、`multipath`、`secret_key`、`auth_method`、
  `allow_targets`、`domains` 都只是加载并给出 `--check` 警告。
- **`GET /api/status` 新增 `connections.authenticated`**（完成握手的连接数），面板概览页
  多一格；配置页那一行改名为「代理策略」，值仍是 `proxies_configured`。

**升级检查清单**（只影响把 `[[proxies]]` 写在服务端配置里的部署）：

1. 如果服务端配置里有 `[[proxies]]` 条目，确认它的 `type` 与 `remote_port` 就是客户端会
   注册的值；写错会让该客户端注册失败，错误信息里给出服务端期望的值。
2. 如果条目里写了 `allow_cidrs` / `deny_cidrs`，确认这些来源确实是你允许访问该代理的：
   升级后它们开始生效，会把此前能访问的访客拒掉（审计记录 `proxy_visitor_denied`，
   `detail` 指出是服务端策略拒绝的）。
3. 用 `--check` 看一遍警告：服务端配置里描述客户端服务的键现在都会被逐条列出来。
4. 不想要任何策略就把服务端配置里的 `[[proxies]]` 整段删掉：没有条目的名字不受约束。

## 4.8 从 v3.7.3 到 v3.7.4

**配置与线协议都没有破坏性变化**：`ProtocolVersion` 仍是 4，没有增删任何配置键，
v3.7.3 的配置与二进制可以直接升级。这一版改面板、服务端回给客户端的端口、自动封禁的时长
倍数、客户端心跳间隔的取用，以及三处版本号：

- **代理池在面板里逐成员列出**。此前代理表的「成员」一格只写"几个成员、几个可用"；
  现在成员数大于 1 的池会在这一格里为每个成员再列一行，内容是**客户端 ID**、该客户端
  测得的延迟、以及它连续失败的次数。`/api/proxies` 在每个成员上返回 `latency_ms` 与
  `consecutive_failures`（池状态字段与这几种均衡策略都是 v3.2.0 加的），是 `latency`、
  `failover`、`adaptive` 三种 `load_balance` 策略的判断依据，此前没有任何界面显示它们。
  - 成员行的标识是客户端 ID，不是本地地址：一个池里的成员通常都转发到同一台服务的同一个
    端口，本地地址区分不开它们。只有当某个成员的本地地址与整行显示的不同时，才把它跟在
    客户端 ID 后面的括号里。
  - **只有一个成员的代理显示不变**，与 v3.7.3 完全一致。
  - 某个成员在第一次成功应答之前延迟显示 `—`（服务端此时把它记为 0），不是"0.0 毫秒"；
    连续失败次数照常显示。
  - 这一格因此会比以前宽一些：窄屏下会折行，过长的客户端 ID 会断行，但数值与单位
    （`ms` / `毫秒`）始终留在同一行。
- **`deploy/kubernetes/` 的凭据注入方式改了，照旧方式部署的 Pod 起不来**。此前 Deployment
  用 `envFrom: secretRef`，但 `envFrom` 把 Secret 的键名原样当作变量名（这里是 `auth-token`、
  `dashboard-token`），服务端读的是 `AETHERTUNNEL_AUTH_TOKEN`、`AETHERTUNNEL_DASHBOARD_TOKEN`，
  凭据传不进去，Pod 会以 `server.auth_token is required` 退出。现在 Deployment 用显式的
  `env`/`valueFrom` 映射，**Secret 无需改动**，`kubectl apply -k deploy/kubernetes` 重新应用
  Deployment 即可。另外部署文档补上一条：`ConfigMap` 打开了 `[obfuscation]` 的
  `disguise = "tls-record"`，**客户端必须配同样的伪装**才能连上。
- **`docs/PLATFORMS.md` 新增**：逐目标写明被什么执行、证明了什么、什么没证明。发布与运维
  方式不受影响；如果你的部署依赖 darwin/amd64 或 windows/arm64，注意这两个目标**至今没有被
  任何地方执行过**（CI 也只交叉编译它们）。
- **`[dht] discover` 现在真的能用**（此前不能）。若你曾按文档把 `client.server_addr` 留空、
  只写 `[dht] discover`，客户端过去会在启动时直接以 `key not found` 退出；现在它会先引导 DHT
  节点再解析，并在 30 秒内重试，成功则连上。要用这个功能，**`discover` 指向的名字应当是私有
  代理**（`stcp`/`sudp`/`xtcp`），因为只有私有代理的记录写的是控制端口；指向 `tcp`/`udp`/
  `http`/`https` 的名字会解析到"访问者到达代理的地址"，客户端会警告这一点。既有的
  `server_addr` 用法不受影响。
- **文档结构调整（不影响软件行为）**：README 里那段"这一版没有什么"移入
  [`docs/NOT-IN-THIS-VERSION.md`](NOT-IN-THIS-VERSION.md)，README 只留一行指针。内容一条未删，
  每条仍写明缺什么与可用的替代；README 的能力表新增一行"逐平台功能验证"，列出三个平台与
  跨系统矩阵的实测结果。若你的文档或脚本引用了 README 的旧小节标题，请改指新文件。
- **新增 `load_balance = "bandit"`**（可选，默认不变）：UCB1 多臂老虎机式的池成员选择，按实测
  应答速度在线学习。既有部署不受影响——不写这一项时策略仍是 `round-robin`，其余策略行为未变。
  若你的池里有一个成员长期不应答，`bandit` 会比 `round-robin` 少浪费大量尝试（实测 60 次访问里
  30 次对 6 次），而访问本身在两种策略下都不会丢。
- **新增 `--ledger-proof`**（可选，不影响既有部署）：`--ledger-proof <文件> --proof-index <n>`
  把到第 n 条为止的账本前缀写到标准输出，`--verify-ledger` 可以直接校验它，链头就是整条链在
  第 n 条的哈希。用于只证明某一段用量、不必交出整条链的场合。
- **`[client].heartbeat_seconds` 开始有意义**。心跳间隔一直由服务端下发、客户端照办（这是
  对的：判定"三次收不到就断开"的是服务端），但这个键此前被完全忽略，改它没有任何效果。
  现在它在服务端没有下发间隔时作为回退值生效；与服务端下发的值不同时，客户端会打印一行
  说明配置值不生效。**正常会话里行为不变**，因为在跑的服务器都会下发自己的间隔。
- **自动封禁的时长倍数开始真正增长**。`ban_seconds`、2 倍、4 倍……到 `ban_max_seconds`
  封顶——这套倍数增长此前从未生效：封禁期间来源的连接在握手前就被拒，不可能再累积失败，
  而服务端又会把"封禁已过期、失败次数为 0"的条目删掉，于是每次再犯都从 `ban_seconds`
  重新开始，`ban_max_seconds` 是一段死配置。现在被封禁过的来源在封禁结束后仍被记住一个
  10 分钟窗口。**这会改变受影响来源被封的时长**：反复失败且每次都服满封禁的地址，第二次是
  2 倍、第三次是 4 倍……直到上限；只失败一两次、之后正常使用的地址不受影响（成功认证清零，
  窗口过后记忆被回收）。
- **服务端回给客户端的端口改成实际生效的那个**。代理池只拥有一个端点，端口取自第一个
  成员；后到的成员请求了别的端口也会被并入池中，请求不被采纳。此前 `TypeProxyList` 里填的
  是这个成员**请求**的端口，于是客户端日志写着一个服务端并没有监听的端口。现在池成员报池的
  端口，不在池里的代理报自己的，客户端日志相应变成
  `server confirms 2 tunnel(s): pooled (tcp) on port 6022 shared by 2 clients`——每行多带
  代理类型，共享同一个名字时还带上成员数。线协议版本不变。
- **`TypeProxyList` 只剩下客户端会读的四项**（`name`、`type`、`remote_port`、`group_members`），
  `AuthRequest` 里的 `encryption_salt` 删除。删掉的字段要么是这条连接自己的会话号，要么是服务端
  面板与指标已经在读的计数，客户端收到也只能复述；`encryption_salt` 则是客户端从来没有发过、
  服务端从来没有读过的键（两端都按各自 `[encryption]` 的 `salt` 派生密钥）。**对兼容性没有
  影响**：JSON 解码忽略多出来的键、缺失的键读作零值，自己实现的客户端不需要改。
- **客户端日志开始区分一条流是访客来的还是走公网端口的**
  （`stream for "x" (from a visitor) finished ...`）。这只影响日志文本，不影响行为。
- **访客的传输必须与代理的形状一致**（`stcp`↔`stcp`、`sudp`↔`sudp`、`xtcp` 两者皆可）。不一致的组合此前会静默失败：服务端按数据报转发、客户端按字节流直通，帧头被灌进本地服务，访客什么也收不到。现在这类访客会被拒绝，错误里写明是哪两个形状撞在一起，并留下 `visitor_rejected` 审计记录。**若你的部署依赖这种错配**（唯一能让它看起来能用的情形是本地服务把收到的字节原样回显），请把访客或代理的 `type` 改成一致的那一个。
- **半关闭现在真的生效**。此前 `flynet.Pipe` 的 `CloseWrite` 在三种包装上都会退化成“整条关闭”（加密记录层、伪装层、xtcp 直连的可靠流），所以**“服务端读完请求才回答”的协议**（HTTP/1.0、若干数据库协议、任何先 `shutdown(SHUT_WR)` 再等响应的客户端）会在响应返回之前被断开。以隧道为跳板时若你观察到过“请求发出去了但没有响应”的现象，这一版修的就是它。反过来，如果某处依赖了旧行为（把半关闭当成整条关闭），行为会变化——请检查那条链路的另一端是否需要显式关闭。
- **访问者绑到非回环地址时会警告**（行为不变，只是说出来）。该监听器自身没有认证，服务端
  `[[proxies]]` 的名单也看不到它后面的用户，所以绑到 `0.0.0.0` 或某个内网地址等于把私有代理
  交给那个网络。如果你的部署本来就是这样（例如访问者跑在一台只对本网段开放的跳板机上），
  这条警告可以忽略；如果只是顺手写了 `0.0.0.0`，建议改回 `127.0.0.1`。
- **`client.server_addr` 与 `[dht] discover` 同时设置时，现在以 `server_addr` 为准**。
  此前客户端会启动 DHT 解析并**跟着记录走**（尽管它同时打印的警告说不会），也就是说
  一个已经写死服务器地址的部署仍然可以被 DHT 上任何人写的无签名记录改写到别处，并把
  `auth_token` 交给那台服务器。受影响的部署：如果你正依赖这种行为跟随服务端迁移，请把
  `server_addr` 留空（这正是文档一直描述的做法）；如果只是两种配置都留着，行为现在与文档一致。
- **同一客户端里两个同协议的代理不能再请求同一个 `remote_port`**。此前这种配置会被接受，
  运行时只有先注册的那个发布成功，第二个在服务端与客户端各留一行 `address already in use`，
  而消息里不会说端口被这个客户端自己的另一个代理占着。现在 `--check` 会拒绝并指出两个代理名。
  `tcp` 与 `udp` 用同一个端口号仍然合法（两个不同协议的套接字）。
- **同一个进程里的两个监听器不能再配成同一个地址**。`http_port`、`https_port`、
  `dashboard.port`、`server.bind_port` 之间（以及客户端两个 visitor 的 `bind_port` 之间、
  服务端 `p2p_port` 与 `dht.listen_addr` 之间）撞在同一个地址上的配置，现在会被 `--check`
  拒绝并指出是哪两个键。此前它会先绑上一个、打印“listening”，再以
  `bind: address already in use` 退出（客户端那一侧更糟：进程继续运行，其中一个 visitor
  永远不工作）。**同一个端口号用在不同地址上仍然合法**（例如控制端口在 `127.0.0.1`、面板
  在另一张网卡上）。
- **`obfuscation.pad_to` 有了上限**。补齐发生在写出之前，而超过帧上限（1 MiB）的帧会被发送端
  自己拒收，所以 `pad_to` 大于上限的配置一条帧都发不出去（客户端连认证请求都发不出，服务端
  只看到 `handshake failed: EOF`）。现在这类配置会被 `--check` 拒绝并给出上限；`pad_to` 取到
  上限本身仍然可用，两端不必取同一个值。
- **负数的时长与计数不再被接受**。`[client].dial_timeout_seconds = -1` 此前等于"立即超时"，
  客户端因此永远连不上；`reconnect_seconds = -1` 等于"每秒重试一次、退避不再增长"；服务端一侧
  的负值等于把对应的限制关掉（心跳窗口、流的空闲上限、握手时限、连接数上限，
  `rate_limit_burst` 则被静默当作 1）。升级前跑一次 `--check`：配置里若有这类值（多半是手误或
  占位符），现在会被明确拒绝并指出是哪一个键；`max_reconnect_seconds` 小于
  `reconnect_seconds` 也会被拒绝。**0 的含义不变**，仍是"取默认值"。
- **审计日志不再漏掉"会话结束"的记录**。`proxy_removed` 此前只在客户端自己断开时写：服务端
  自己关闭会话（优雅关闭就是这条路径）时，成员已经先被摘掉，写记录的代码找不到它，于是一条
  都不留；`client_disconnected` 也会因为在拆除完成之前关闭日志而丢掉。停下来的服务器在审计
  里看起来就像还有客户端连着。升级后这两类记录都会出现（`proxy_removed` 的 `detail` 是关闭
  原因，例如 `server shutting down`）——**若你有脚本按 `proxy_removed` 统计下线，注意现在会
  多出这些行**；反过来，如果你的合规要求是"每次下线都有记录"，这一版之前并不成立。另外，
  日志关闭之后到达的记录不再静默丢弃，会计入 `records_lost` 并在服务器日志里写出是哪一条。
- **`[dht]` 的两个键现在会被校验得更紧**。`republish_seconds` 的派生默认值（
  `announce_ttl_seconds / 3`）有了 1 秒下限：短于 3 秒的 TTL 之前会把派生值算成 0，而 0 被
  DHT 节点读作"没设置"，回退到它自己的 30 秒默认值——**通告会在失效之前不被重写**，一个仍在
  运行的服务端的名字会停止解析。另外 `announce_ttl_seconds` 小于 2 秒的配置现在会被
  `--check` 拒绝：间隔以整秒计，1 秒的 TTL 没有可能在失效前被重写。**若你的配置里
  `announce_ttl_seconds` 是 1，升级前先把它改成 2 或更大**（默认值 90 不受影响）；只写
  `republish_seconds` 且它小于 TTL 的配置一律照常。
- **`server.toml.example`、`client.toml.example` 首行的版本号**从 `v3.3.0` 改为当前版本，
  `deploy/kubernetes/kustomization.yaml` 的 `newTag` 从 `v3.2.0` 改为当前版本。这三个文件
  随 Release 一起发布或随仓库分发，此前写着旧版本，容易让人以为它们描述的是旧版配置。

**升级检查清单**：

1. 用的是 `deploy/kubernetes/` 时：把 `kustomization.yaml` 的 `newTag` 改成你要部署的版本
   （此前它把镜像钉在 `v3.2.0`），并重新 `kubectl apply -k deploy/kubernetes` 应用修好的
   Deployment——旧清单注入凭据的方式是错的，Pod 会以 `server.auth_token is required` 退出。
   正在运行的部署不受影响，直到你重新应用清单。
2. 有代理池（多个客户端用同一个 `name` 与同一个 `group` 发布）时，打开面板的「代理」页
   确认每行的成员数与你部署的客户端数一致；某成员显示 `—` 表示它还没有成功服务过一次
   连接，因此还没有测到延迟。
3. 如果某个池成员的配置里 `remote_port` 与其他成员不同，注意它一直是以**第一个成员的
   端口**对外服务的（这一点没有变），现在客户端日志会照实写出那个端口而不是它自己请求的
   值；要让两端一致就把配置改成同一个端口。
4. 开了 `ban_after_failures` 时，确认 `ban_max_seconds` 是你真正想要的上限：它此前不生效，
   现在会生效。反复失败又每次服满封禁的地址会按 2 倍递增到该上限，间隔较久的偶发失败不受
   影响。负载均衡、健康检查与监控地址记得写进 `ban_ignore_cidrs`。
5. 用 `[dht]` 时，确认 `announce_ttl_seconds` 不小于 2（默认 90 不用动）：小于 2 的配置现在
   会被 `--check` 拒绝，而不写 `republish_seconds` 时派生出的间隔现在有 1 秒下限。若你的部署
   依赖"短 TTL 让名字很快消失"的旧行为，注意那是派生值失效造成的副作用——名字消失的原因是
   通告没有被重写，而不是 TTL 到了。
6. 没有代理池、也没用 k8s 清单、也没开自动封禁、也没用 `[dht]` 时，这一版不需要做任何事。

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
   或直接跑 `scripts/smoke-test.ps1`（101 项检查，覆盖全部代理类型、签名通告、按代理 ACL、
   服务端按代理名的策略、服务端级拒绝与限流、审计轮转与保留代数、审计写不进去与恢复、
   空闲超时、自动封禁、宽限期内的拒绝与探针、代理池、多路径与打洞结果、面板与指标的一致
   性、客户端在服务端重启后的恢复）。Linux 上再用
   `sudo scripts/vpn-linux-test.sh bin/aethertunnel-server bin/aethertunnel-client`
   验一次三层隧道。
