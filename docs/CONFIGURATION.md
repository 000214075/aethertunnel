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
| `bind_port` | int | 必填 | 监听端口（1–65535）。控制连接与数据连接共用。同一个进程里两个监听器不能绑在同一个地址上：与 `http_port`/`https_port`/`dashboard.port` 冲突时 `--check` 直接拒绝 |
| `auth_token` | string | 必填 | 与客户端共享的密钥。少于 16 位或形如占位符会**警告** |
| `max_connections` | int | 512 | 同时在线客户端上限，超出的会被明确拒绝。负数被拒绝 |
| `handshake_timeout_seconds` | int | 10 | 连接建立后必须在此时限内发出第一帧。负数被拒绝 |
| `read_timeout_seconds` | int | 120 | 隧道流的空闲上限，同时也是控制连接的空闲判据。负数被拒绝 |
| `heartbeat_seconds` | int | 30 | 期望客户端的心跳间隔；连续 3 次未收到即断开。负数被拒绝 |
| `dial_timeout_seconds` | int | 10 | 访问者到来后，等待客户端回拨数据连接的时限。负数被拒绝 |
| `graceful_shutdown_seconds` | int | 5 | 收到停止信号后，先停止接受新连接，并给正在传输的流最多这么多秒完成，之后才断开客户端。0 视为默认值，负数被拒绝 |
| `allow_cidrs` | []string | 空 | CIDR 白名单。非空时只有匹配的来源可以连接 |
| `deny_cidrs` | []string | 空 | CIDR 黑名单，优先级高于白名单 |
| `rate_limit_per_second` | float | 0 | 按来源地址的连接速率（每秒），0 表示关闭 |
| `rate_limit_burst` | int | 20 | 令牌桶容量。负数被拒绝（此前会静默当作 1） |
| `ban_after_failures` | int | 0 | 同一来源认证失败多少次后封禁该来源，0 表示关闭 |
| `ban_seconds` | int | 300 | 首次封禁的时长；再次封禁时按倍数增长 |
| `ban_max_seconds` | int | 3600 | 封禁时长的上限 |
| `ban_ignore_cidrs` | []string | 空 | 永不封禁的来源，负载均衡后面或监控主机需要填 |
| `http_port` | int | 0 | `http` 代理的共享监听端口，0 表示不启用。与 `bind_port` 同端口时被拒绝：它绑的是 `server.bind_addr`，所以这个组合必然是同一个地址，而不是分到两张网卡上 |
| `https_port` | int | 0 | `https` 代理的共享监听端口；非 0 时下面两项必须同时设置 |
| `https_cert_file` | string | 空 | 共享 HTTPS 监听的证书 |
| `https_key_file` | string | 空 | 共享 HTTPS 监听的私钥 |
| `subdomain_host` | string | 空 | 设置后，没有显式 `domains` 的 `http`/`https` 代理以 `<代理名>.<该值>` 注册；为空时这类代理在注册阶段被服务端拒绝 |
| `p2p_port` | int | 0 | `xtcp` 打洞的 UDP 会合端口，0 表示不支持打洞（xtcp 走中继）。与 `dht.listen_addr` 撞在同一个 UDP 地址上时被拒绝 |
| `load_balance` | string | `round-robin` | 代理池策略：`round-robin` `random` `latency` `failover` `adaptive` `bandit`。只对声明了 `group` 的代理有影响。`bandit` 是**在线学习**的多臂老虎机（UCB1）：每条流按应答速度记奖励（立即回答记 1，越慢越小），据此估计各成员的平均奖励并加一个探索项来选择成员；没有离线训练、没有模型文件，学习只来自这个池子实际服务过的流。每第 20 次选择会去测观测最少的成员，因此曾经很慢的成员在恢复后仍会被重新测量 |

## `[client]`（客户端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `server_addr` | string | 条件必填 | `host:port`。为空时必须设置 `[dht] discover`，否则报错 |
| `auth_token` | string | 必填 | 与服务端一致 |
| `reconnect_seconds` | int | 3 | 重连退避的起始值。负数被拒绝 |
| `max_reconnect_seconds` | int | 60 | 退避上限，不得小于 `reconnect_seconds`；退避带 ±20% 抖动。负数被拒绝 |
| `heartbeat_seconds` | int | 30 | 心跳间隔的**回退值**：服务端在会话建立时把自己的 `[server].heartbeat_seconds` 下发下来，客户端按它发心跳（服务端才是"连续 3 次未收到即断开"的一方），因此正常会话里本项不生效；服务端没有下发时（旧版或第三方服务端）才用它。与下发的值不同时会在连接时报告一行。负数被拒绝 |
| `dial_timeout_seconds` | int | 10 | 连接服务端、等待 `DataOpenAck`、连接本地服务的超时。负数被拒绝（此前负值等于"立即超时"，客户端永远连不上） |
| `idle_timeout_seconds` | int | 300 | 单条隧道流的空闲上限：超过这段时间没有字节流动就断开。适用于服务端转发的流、访客的本地监听连接，以及打洞后的直连路径。负数被拒绝 |

## `[[proxies]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 代理名，同一客户端内不可重复；同一 `group` 内多个客户端可以同名 |
| `type` | string | `tcp` | `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5` |
| `local_ip` | string | `127.0.0.1` | 本地服务地址 |
| `local_port` | int | 必填 | 本地服务端口（1–65535） |
| `remote_port` | int | 0 | 服务器上对外开放的端口。`tcp`/`udp` 用它；`http`/`https` 与私有类型必须为 0。同一客户端里两个**同协议**的代理不能请求同一个端口（先注册的绑住它，第二个会被拒绝），`tcp` 与 `udp` 用同一个端口号是允许的：它们绑的是不同协议的两个套接字 |
| `domains` | []string | 空 | `http`/`https` 的访问域名：精确域名、`*.通配`，或留空后由服务端 `subdomain_host` 拼出 `<代理名>.<该值>`。留空时客户端只给出警告，因为该设置属于服务端 |
| `secret_key` | string | 空 | 私有类型必填，也是访客侧的凭据 |
| `auth_method` | string | `secret` | `secret` 直接比对；`nizk` 用 Schnorr 证明，secret 不出现在线上 |
| `group` | string | 空 | 填入同一名字的多个客户端组成代理池 |
| `multipath` | int | 0 | 数据报代理最多使用的数据连接数（0–8）。字节流代理上会给出警告 |
| `allow_targets` | []string | 空 | 仅 `socks5`：本客户端允许拨号的地址范围（CIDR）。**必填**，缺失即拒绝注册 |
| `allow_cidrs` | []string | 空 | 只有匹配的来源地址可以访问该代理；留空表示不限 |
| `deny_cidrs` | []string | 空 | 拒绝的来源地址，优先级高于 `allow_cidrs` |

`socks5` 没有本地服务，因此 `local_ip` 与 `local_port` 会被忽略并给出警告，必须设置
`remote_port`。`allow_targets` 按 IP 范围匹配，域名先解析再匹配；不在范围里的目标会以
SOCKS5 回复码 `0x02`（not allowed）拒绝。`allow_cidrs` / `deny_cidrs` 在服务器接受连接之后、
建立隧道之前执行，被拒绝的访客会留下 `proxy_visitor_denied` 审计记录。

## `[[proxies]]`（服务端，可重复）

服务端本身不发布代理：发布什么由连上来的客户端决定。服务端配置里的 `[[proxies]]` 是
**针对某个代理名的策略**，任何客户端注册这个名字都按它执行；没有条目的名字不受策略约束，
与升级前一致。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 策略针对的代理名 |
| `type` | string | 空 | 该名字允许发布的类型；留空表示任意类型 |
| `remote_port` | int | 0 | 该名字必须使用的对外端口；0 表示由客户端决定 |
| `allow_cidrs` | []string | 空 | 允许访问该名字的来源地址；留空表示这里不限 |
| `deny_cidrs` | []string | 空 | 拒绝的来源地址，优先级高于 `allow_cidrs` |

注册阶段先比对 `type` 与 `remote_port`：不匹配时不建立隧道，客户端收到说明服务端期望的类型
或端口，服务端记录 `proxy_rejected`。访客阶段先过服务端策略的 `allow_cidrs` / `deny_cidrs`，
再过客户端为该代理声明的名单，两边都通过才建立隧道；被服务端策略拒绝的访客记入
`proxy_visitor_denied` 与 `aethertunnel_visitors_denied_by_proxy_total`，审计记录的 `detail`
指出是服务端策略拒绝的。

描述客户端自身服务的键（`local_ip`、`local_port`、`group`、`multipath`、`secret_key`、
`auth_method`、`allow_targets`、`domains`）在服务端配置里会加载成功但不生效，`--check` 会逐条
输出警告。因此同一份 `[[proxies]]` 列表可以放在两种角色的配置里，只是含义不同。

## `[[visitors]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 本机监听器的名字 |
| `type` | string | `stcp` | `stcp` `sudp` `xtcp`。要与服务端上那个代理的**形状**一致：`stcp` 与 `xtcp` 是**字节流**访客（`xtcp` 仍然是字节流，它的本地监听器是 TCP，打洞之后也走字节流），`sudp` 是**数据报**访客。不一致时服务端拒绝并说明原因：服务端按数据报转发、客户端按字节流直通，本地服务会收到帧头，访客什么也收不到 |
| `server_name` | string | 必填 | 服务端上对应的私有代理名 |
| `secret_key` | string | 必填 | 必须与代理侧一致 |
| `auth_method` | string | `secret` | `secret` 或 `nizk` |
| `bind_addr` | string | `127.0.0.1` | 本机监听地址。非回环地址时会**警告**：这个监听器自身没有认证（能连上端口的人就能用这条隧道），而服务端的 `[[proxies]]` 名单判断的是**这个客户端**的地址、不是它后面的用户，所以绑到别的地址等于把私有代理交给那个网络 |
| `bind_port` | int | 必填 | 本机监听端口（1–65535）。两个 visitor 绑同一个地址时被拒绝 |

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

指标序列：控制连接、被拒绝的控制连接、认证失败、ACL 拒绝、限流拒绝、被封禁的来源、
因封禁被拒的连接、按代理拒绝的访客、socks5 请求数、因服务器关闭而拒绝的流、
数据连接、活动与累计流数、双向字节、UDP 数据报与活动会话、共享监听上的 HTTP 请求数、
打洞尝试数与结果（直连/中继），审计日志的写入失败数，以及按 `tunnel` 标签的活动流、
累计流、双向字节与 HTTP 请求数。
每一行都直接读自运行中的计数器，没有估算值；`aethertunnel_p2p_direct_total` 只在访客
用 `ATP3` 数据报回报了直连路径时才增加，访客没回报的尝试不会计入直连。

序列名逐条列出（值都是自进程启动以来的累计数，除标为 gauge 的）：

| 序列 | 类型 | 含义 |
|---|---|---|
| `aethertunnel_uptime_seconds` | gauge | 进程运行秒数 |
| `aethertunnel_control_connections_total` | counter | 被接受的（已通过握手的）控制连接 |
| `aethertunnel_control_rejected_total` | counter | 在握手前被拒绝的控制连接，原因是容量、ACL、限流或封禁中的任意一种 |
| `aethertunnel_auth_failures_total` | counter | 凭据错误的认证尝试 |
| `aethertunnel_connections_denied_by_acl_total` | counter | 被允许/拒绝名单挡下的连接 |
| `aethertunnel_connections_rate_limited_total` | counter | 被按来源令牌桶挡下的连接 |
| `aethertunnel_sources_banned_total` | counter | 因反复认证失败被封禁的来源 |
| `aethertunnel_banned_connections_refused_total` | counter | 因来源处于封禁期而被拒绝的连接 |
| `aethertunnel_visitors_denied_by_proxy_total` | counter | 被单个代理自己的名单拒绝的访客 |
| `aethertunnel_streams_refused_while_draining_total` | counter | 因服务器正在关闭而被拒绝的流 |
| `aethertunnel_socks5_requests_total` | counter | 通过 socks5 出口发出的 CONNECT 请求 |
| `aethertunnel_data_connections_total` | counter | 客户端打开的数据连接 |
| `aethertunnel_streams_active` | gauge | 当前打开的隧道流 |
| `aethertunnel_streams_total` | counter | 已结束的隧道流 |
| `aethertunnel_bytes_from_clients_total` | counter | 从客户端收到的字节 |
| `aethertunnel_bytes_to_clients_total` | counter | 发往客户端的字节 |
| `aethertunnel_udp_datagrams_total` | counter | 转发的 UDP 数据报 |
| `aethertunnel_udp_sessions_active` | gauge | 当前跟踪的 UDP 访客会话（按来源地址计） |
| `aethertunnel_http_requests_total` | counter | 共享虚拟主机监听上服务的请求 |
| `aethertunnel_p2p_punches_total` | counter | 为 xtcp 代理发起的打洞尝试 |
| `aethertunnel_p2p_direct_total` | counter | 得到直连路径的打洞尝试 |
| `aethertunnel_p2p_relayed_total` | counter | 回退到中继的打洞尝试 |
| `aethertunnel_audit_write_failures_total` | counter | 首次写入即失败的审计记录数 |
| `aethertunnel_audit_records_lost_total` | counter | 重开文件后仍然没能写下的审计记录数 |
| `aethertunnel_audit_records_recovered_total` | counter | 重开文件后补写成功的审计记录数 |
| `aethertunnel_tunnel_streams_active{tunnel}` | gauge | 按隧道的当前活动流 |
| `aethertunnel_tunnel_streams_total{tunnel}` | counter | 按隧道的累计流 |
| `aethertunnel_tunnel_bytes_total{tunnel,direction}` | counter | 按隧道与方向的字节 |
| `aethertunnel_tunnel_http_requests_total{tunnel}` | counter | 按隧道服务的 HTTP 请求 |

三条审计序列只在 `[audit] enabled = true` 时出现。审计关闭时不输出它们，因为恒为 0 的
"丢失 0 条"会被读成"审计正常"，而实际上根本没有任何日志。

UDP 数据报会话的字节在**会话释放时**计入两个字节计数器与按隧道的计数，即该来源地址安静
`server.read_timeout_seconds`（默认 120）秒之后，与它从 `aethertunnel_udp_sessions_active`
消失的时刻相同；每收到一个数据报就增加的是 `aethertunnel_udp_datagrams_total`。

## `[audit]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否写审计日志 |
| `path` | string | `aethertunnel-audit.jsonl` | 日志路径 |
| `max_bytes` | int | 33554432 | 超过该大小后轮转，旧文件保留为 `<path>.1` |
| `keep` | int | 1 | 轮转时保留多少代，范围 1–100；`<path>.1` 是刚轮转走的那一代，`<path>.2` 是它之前的那一代。写 0 与不写一样按默认 1 处理，超出范围直接报错 |

轮转时各代按序号往后挪一位（`.1` → `.2`，…），最老的一代（`<path>.<keep>`）被丢弃：
文件数量由 `keep` 决定，不会随运行时间增长。一次轮转把当前文件改名为 `<path>.1`，
紧接着在 `path` 上新建一个——这两步之间 `path` 短暂不存在，和所有日志轮转工具一样，
所以盯着这个路径的采集程序要容忍一次读不到。文件名被别处占着而改不动时（Windows 上另一个
进程打开着这个文件就是这种情况），这次轮转推迟到写入下一条记录时再试，文件在那之前继续变大，
服务器日志写出一行说明。这里不切短文件：那会把还没人读过的记录丢掉，而审计日志的第一条要求
是不要丢记录。

写不进去的记录不会被丢掉不管：写入失败时会重新打开 `path` 并重试该条记录一次，
因此外部的日志轮转或一次瞬时错误不会造成空洞。每条记录写入前还会核对配置路径是否仍指向
手上这个文件：路径被换成新文件（logrotate 的默认模式）会重开，**路径整个消失也会重建**——
`mv` 到别处而不补新文件、或者为了腾空间 `rm` 掉日志，都会让句柄写进一个再也没有名字指向它的
inode，不重建的话路径到重启为止都是空的。仍然写不下去的记录计入
`aethertunnel_audit_records_lost_total`，同时 `GET /api/status` 的 `audit` 段报告
`enabled`、`writable`、`path`、`max_bytes`、`bytes_written`、`write_failures`、
`records_lost`、`recovered` 与 `last_error`，面板的"服务器状态"栏会显示这一行，
有记录丢失时还会顶出一条横幅。服务器本身不受影响：审计写不进去不会让隧道停下来。
日志关闭之后到达的记录同样计入 `records_lost`，并在服务器日志里写出是哪一条事件：
关闭之后再写一条，说明有写记录的东西活过了关闭，不能就这么算了。

每行一个 JSON 对象，字段为 `time`、`event`、`client_id`、`remote`、`proxy`、`detail`、`outcome`；
`event` 取值为 `control_accepted`、`control_rejected`、`auth_failed`、`client_disconnected`、
`proxy_registered`、`proxy_rejected`、`proxy_removed`、`acl_denied`、`rate_limited`、
`source_banned`、`ban_refused`、`proxy_visitor_denied`、
`dashboard_action`、`visitor_accepted`、`visitor_rejected`、
`p2p_direct`、`p2p_relayed`、`p2p_abandoned`、
`vpn_address_assigned`、`vpn_address_rejected`。

`proxy_removed` 在成员真正被移除时记录：代理池里少一个成员也记录，`detail` 是移除原因，
`proxy` 是代理名。记录写在移除发生的那一处，所以客户端自己断开与**服务端关闭会话**（优雅关闭
走的就是这条）两种情形都会写，且只写一条。`dashboard_action` 记录从面板发起的操作（目前是断开客户端）。
`p2p_abandoned` 表示访客的控制连接消失了，既没有要求中继、也没有回报直连路径，
服务器因此不知道那条尝试的结果，不会把它记成直连。

## `[ledger]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否记录带宽账本 |
| `path` | string | `aethertunnel-ledger.jsonl` | 账本文件，每行一条 JSON 记录 |
| `signing_key_file` | string | `aethertunnel-ledger.key` | Ed25519 私钥种子（32 字节十六进制），首次使用时生成。**Unix 上以 0600 创建**；Windows 没有 POSIX 权限位，`0600` 只是 Go 的请求，实际保护来自文件所在目录继承的 ACL（用户配置目录默认只授予本人、SYSTEM 与 Administrators） |

每条记录包含序号、时间、客户端、代理、双向字节、上一条的哈希、本条哈希与签名。
`GET /api/ledger` 发布公钥、链头、最近条目与按客户端汇总。
`aethertunnel-server --verify-ledger <文件> --ledger-key <公钥或私钥文件>` 可以离线校验，
校验只需要公钥。
`aethertunnel-server --ledger-proof <文件> --proof-index <n>` 把到第 n 条为止的前缀按同样的
JSONL 格式写到标准输出（索引从 0 开始，越界或缺失时报错）：这段前缀用 `--verify-ledger`
单独可校验，链头就是整条链在第 n 条的哈希，因此可以把某一段用量交给审计方而不交出整条链。
截断不会被链本身发现（后面的条目没了，前面的仍然自洽），**要发现截断必须把链头另外发布并在
校验时比对**，`GET /api/ledger` 与 `--ledger-proof` 的输出都会给出链头。

## `[dht]`（两端）

| 键 | 类型 | 默认 | 端 | 说明 |
|---|---|---|---|---|
| `enabled` | bool | false | 两端 | 是否启动 DHT 节点 |
| `listen_addr` | string | `0.0.0.0:7001` | 两端 | 节点绑定的 UDP 地址。服务端上它与 `server.p2p_port` 撞同一个地址时被拒绝 |
| `bootstrap` | []string | 空 | 两端 | 启动时要联系的其它节点，`host:port`。留空表示自成一个单节点网络 |
| `node_id` | string | 空 | 两端 | 40 位十六进制标识；留空则随机生成，每次重启都会变 |
| `namespace` | string | `aethertunnel` | 两端 | 键前缀，两套部署可以共用一个 DHT |
| `ttl_seconds` | int | 3600 | 两端 | 记录在 DHT 中的存活时间，不能短于 `announce_ttl_seconds` |
| `announce_ttl_seconds` | int | 90 | 两端 | 一条通告对读取端有效的时长，最少 2 秒：通告必须在失效前被重写，而间隔以整秒计 |
| `republish_seconds` | int | `announce_ttl_seconds / 3`，至少 1 秒 | 两端 | 重新发布已有通告的间隔，必须短于 `announce_ttl_seconds` |
| `lookup_timeout_seconds` | int | 5 | 两端 | 单次解析的时限。负数被拒绝 |
| `advertise_host` | string | 空 | 服务端 | 通告里写给客户端的主机名，**不含端口**（端口按代理类型取）。留空取 `server.bind_addr`，广播地址会**警告** |
| `discover` | string | 空 | 客户端 | `client.server_addr` 为空时要解析的代理名（两边都设置时以 `server_addr` 为准，客户端不会启动解析：DHT 上的记录除 `trusted_keys` 之外没有签名，跟着它走等于让能应答这个名字的人改掉你的服务器地址）。**要用来找服务器地址时，这个名字必须是私有代理**（`stcp`/`sudp`/`xtcp`）：只有私有代理的记录写的是控制端口；`tcp`/`udp` 的记录写的是该代理的公网端口，`http`/`https` 是共享监听端口，那些是访问者到达代理的地方。客户端解析到公网类型的记录会给出警告 |
| `signing_key_file` | string | 空 | 服务端 | 通告签名用的 Ed25519 种子，首次使用时生成。留空则发布未签名通告并**警告** |
| `require_signed` | bool | false | 客户端 / 查询端 | 拒绝任何没有签名的通告 |
| `trusted_keys` | []string | 空 | 客户端 / 查询端 | 只接受这些公钥（十六进制）签发的通告；非空时未签名的通告同样被拒绝 |

签名覆盖记录里读取端会使用的每个字段（名字、类型、地址、域名、发布时间、失效时间）
以及签名公钥本身，因此改掉其中任何一个都会导致校验失败。校验在有效期判断之前进行，
所以伪造的记录会以 `signature does not verify` 报告，而不是被当成过期。
不满足策略时 `--dht-lookup` 与 `--discover` 会失败，错误分别是
`the announcement carries no signature`、`the announcement's signature does not verify`
与 `the announcement is signed by a key that is not trusted`。
服务端的签名公钥可以从 `--dht-key`、`GET /api/dht` 的 `signing_key` 字段或启动日志里读到。

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

运行状态可以这样看：`GET /api/vpn` 返回接口名、服务端地址、子网、掩码、MTU、地址池大小与
已分配数、对端数，以及从设备读到的包、送入设备的包、已投递、无法路由、丢弃与出错计数；
`GET /api/config` 里也带同一份摘要，面板的"三层隧道"一栏读的就是它。这些数字由服务端进程
自己维护，设备在进程内部，面板看不到网卡本身。

Linux 上的这条路径由 `scripts/vpn-linux-test.sh` 在真实 tun 设备上验证：两端处于不同的
网络命名空间，因此内核不会把隧道地址当成自己的地址直接应答，ICMP 必须真的穿过隧道；
脚本同时核对 `ip -s link` 的收发包计数与上面的接口。具体步骤见 README 的"运维"一节。

## `[obfuscation]`（两端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后下列各项才会生效 |
| `pad_to` | int | 256 | 帧负载补齐到该值的整数倍；0 表示不补齐。不得超过帧上限 1048576 字节：补齐发生在写出之前，超过上限的帧会被发送端自己拒收（两端不必取同一个值，但都受同一个上限约束） |
| `jitter_millis` | int | 0 | 每次写入前插入 `[0, 该值)` 毫秒的随机延迟 |
| `disguise` | string | `none` | `none` 原样写出；`tls-record` 把每个写入包进 TLS 1.2 应用数据记录，两端必须一致 |

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
