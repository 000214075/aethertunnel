# 配置参考 · Configuration reference

**English summary.** Every key this version reads is listed below. Any other key is reported at
startup and by `--check`; pass `--reject-unknown-keys` to make such a key an error instead, which
is how the repository validates the example files. Three credentials may also
come from the environment and take precedence over the file: `AETHERTUNNEL_AUTH_TOKEN`,
`AETHERTUNNEL_DASHBOARD_TOKEN`, `AETHERTUNNEL_ENCRYPTION_PASSPHRASE`. The example files in the repository root are checked this way.

Windows 编辑器写出的配置文件可以直接用：CRLF 行尾与 UTF-8 BOM 都按原样解析，不会把回车
带进任何值里（`pkg/config` 有测试锁定这一行为）。

用 `--check` 校验而不启动（未知键与来自环境变量的凭据都会打印出来；文件因为别的问题被拒时
也会先打印，所以一次运行就能看到全部问题，校验失败时退出码为 1）。**从 frp 粘过来的键会额外
给出对应写法**：frp 用驼峰键名（`bindPort`、`localIP`、`customDomains`），旧版本的客户端设置还
放在 `[common]` 段里，加载器认出这些名字后在未知键列表后面写 `frp names some of these
differently: serverAddr → client.server_addr, customDomains → domains, …`；frp 有而本项目没有的
（`kcpBindPort`、`quicBindPort`、`virtualNet`）也会明说而不是给一个同样用不了的键名。完整对照见
[`docs/VS-FRP.md`](VS-FRP.md) 的「从 frp 迁移」一节：

```bash
aethertunnel-server --config server.toml --check
aethertunnel-client --config client.toml --check

# 把未知键从警告变成错误：仓库就是这样校验两个示例文件的，
# 示例里多打一个键会当场让这一步失败而不是只打一行警告
aethertunnel-server --config server.toml --check --reject-unknown-keys
```

**哪些检查与段的 `enabled` 有关**：对**单个数值**的范围检查与它无关——负数、上下限（如 `mtu` 的
576–9000、`pad_to` 不得超过帧上限、`announce_ttl_seconds` 最少 2 秒、以秒计的握手／读／心跳／重连／
空闲等时长不得超过 1152921504 秒、`visitor.fallback_timeout_ms` 不得超过 9223372036854）说的是文件本身写错了，所以
另有几族按键更紧的上限（`max_reconnect_seconds`、DHT 的四个时长与 `health_check` 的间隔／超时都不得超过
86400，`obfuscation.jitter_millis` 不得超过 60000——各键的表格行里写了），同样是文件本身写错。
段写着 `enabled = false` 也照样会被 `--check` 拒绝，而不是等你把它打开才发现。需要该段真的在用的
检查只在启用时执行：要能解析的地址与密钥，以及**两个值之间的关系**——`republish_seconds` 必须短于
`announce_ttl_seconds`，而段未启用时 `republish_seconds = 0` 表示"按 TTL 推导"，此时无从比较。

## 顶层 `includes`（客户端配置拆分）

客户端配置可以在**根级**写 `includes = ["fragments/*.toml"]`（相对主文件目录，支持通配），
每个片段里的 `[[proxies]]` 与 `[[visitors]]` 会按文件名顺序追加进主配置：名字必须全局唯一，
嵌套的 `includes` 与匹配不到文件的通配都会被拒绝。拆开写的好处是每类隧道一个文件、
改动互不干扰；SIGHUP 热重载会连同片段一起重读。

## `[server]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `bind_addr` | string | 必填 | 监听地址。`0.0.0.0` 表示所有 IPv4 网卡，`::` 表示所有 IPv6 网卡，`::1` 只回环 |
| `bind_port` | int | 必填 | 监听端口（1–65535）。控制连接与数据连接共用。同一个进程里两个监听器不能绑在同一个地址上：与 `http_port`/`https_port`/`dashboard.port` 冲突时 `--check` 直接拒绝 |
| `proxy_bind_addr` | string | 同 `bind_addr` | 已发布端口的监听地址：`http_port`、`https_port`、`tcpmux_port`、`https_passthrough_port` 以及每一条远程端口都绑在它上面。控制端口不动，因此「控制面在一个网段、发布的服务在另一个网段」只需这一项——对应 frp 服务端的 `proxyBindAddr`。必须是 IP 字面量；监听冲突检测按这个地址判断 |
| `tcp_keepalive_seconds` | int | 0 | 客户端到本服务器的连接上 TCP keep-alive 探测间隔；0 用 Go 默认（15 秒）。对端在故障 NAT 后消失时，内核靠它发现并让会话尽快重建——对应 frp 服务端的 `transport.tcpKeepalive`（frp 默认 7200）。负数被拒绝 |
| `user_conn_timeout_seconds` | int | 0 | 已发布端口上的连接向客户端索要数据连接后，等待对方交回连接的上限；0 沿用 `dial_timeout_seconds`。对应 frp 服务端的 `userConnTimeout`（frp 默认 10）：两者是**不同的等待**——`dial_timeout_seconds` 管“客户端拨到本地服务”，本项管“客户端把数据连接交回来”，客户端网络慢时先撞的是后者。负数被拒绝 |
| `auth_token` | string | 必填 | 与客户端共享的密钥。少于 16 位或形如占位符会**警告** |
| `auth_token_file` | string | 空 | 从文件读取共享密钥，配置文件里就不必写它——对应 frp 的 `auth.tokenSource`（type `file`）。行尾空白与末尾换行会去掉（编辑器留下的换行不算密钥的一部分）；文件读不到或只有空白是**硬错误**，不会退化成空令牌；同时写了 `auth_token` 时以文件为准并给一条警告。**服务端只在启动时读它一次**：服务端进程没有 SIGHUP 热重载，轮换密钥 = 写好新文件 + 重启服务端。（客户端侧的 `auth_token_file` 会被客户端自身的 SIGHUP 重载重读，见下文 `[client]` 表。） |
| `max_connections` | int | 512 | 同时在线客户端上限，超出的会被明确拒绝。负数被拒绝 |
| `max_pool_count` | int | 64 | 一个客户端会话最多能在服务端**停放**多少条数据连接，对应 frp 服务端的 `maxPoolCount`（默认同为 64）：客户端 `pool_count` 超过它时，多出的停放连接被按名拒绝（计入 `/metrics` 的 `aethertunnel_pool_refused_total`），客户端回退为按流拨号——**调低它损失的是一次拨号，不是一条流**。负数被拒绝；设得比客户端的 `pool_count` 大没有额外效果 |
| `handshake_timeout_seconds` | int | 10 | 连接建立后必须在此时限内发出第一帧。负数被拒绝 |
| `read_timeout_seconds` | int | 120 | 隧道流的空闲上限。控制连接不归它管：服务端按 `heartbeat_seconds` 的三倍判定控制连接空闲并断开。负数被拒绝 |
| `heartbeat_seconds` | int | 30 | 期望客户端的心跳间隔；连续 3 次未收到即断开。负数被拒绝 |
| `dial_timeout_seconds` | int | 10 | 访问者到来后，等待客户端回拨数据连接的时限。负数被拒绝 |
| `graceful_shutdown_seconds` | int | 5 | 收到停止信号后，先停止接受新连接，并给正在传输的流最多这么多秒完成，之后才断开客户端。0 视为默认值，负数被拒绝 |
| `allow_cidrs` | []string | 空 | CIDR 白名单。非空时只有匹配的来源可以连接 |
| `deny_cidrs` | []string | 空 | CIDR 黑名单，优先级高于白名单 |
| `rate_limit_per_second` | float | 0 | 按来源地址的连接速率（每秒），0 表示关闭 |
| `rate_limit_burst` | int | 20 | 令牌桶容量。负数被拒绝（此前会静默当作 1） |
| `ban_after_failures` | int | 0 | 同一来源认证失败多少次后封禁该来源，0 表示关闭 |
| `ban_seconds` | int | 300 | 首次封禁的时长；再次封禁时按倍数增长 |
| `ban_max_seconds` | int | 3600（且不小于 `ban_seconds`） | 封禁时长的上限 |
| `ban_ignore_cidrs` | []string | 空 | 永不封禁的来源，负载均衡后面或监控主机需要填 |
| `allow_ports` | []string | 空 | 客户端可注册的远端端口范围，如 `["6000-6999", "8000"]`；注册到范围外的端口会被拒绝并记入审计。留空表示不限 |
| `http_port` | int | 0 | `http` 代理的共享监听端口，0 表示不启用。与 `bind_port` 同端口时，只有在 `proxy_bind_addr` 也等于 `bind_addr` 时才被拒绝：它绑的是 `server.proxy_bind_addr`（默认取 `bind_addr`），所以两个端口落在同一张网卡时才是冲突 |
| `https_port` | int | 0 | `https` 代理的共享监听端口；非 0 时下面两项必须同时设置 |
| `https_cert_file` | string | 空 | 共享 HTTPS 监听的证书 |
| `https_key_file` | string | 空 | 共享 HTTPS 监听的私钥 |
| `subdomain_host` | string | 空 | 子域名托管的域名后缀。设置后，没有显式 `domains` 的 `http`/`https` 代理以 `<代理名>.<该值>` 注册；客户端用 `subdomain` 声明标签时以 `<标签>.<该值>` 注册；为空时这类代理在注册阶段被服务端拒绝 |
| `tcpmux_passthrough` | bool | false | 开启后 `tcpmux_port` 上收到的 CONNECT **不再由服务器应答**，而是连同请求一起原样交给隧道——对应 frp 的 `tcpmuxPassthrough`：隧道后端自己会说 CONNECT（例如隧道后面就是一台 HTTP 代理）时用它。服务器仍读请求头以按 authority 路由；关闭时（默认）由服务器回 200 并把请求之后的字节交给隧道 |
| `tcpmux_port` | int | 0 | `tcpmux` 代理的共享监听端口，0 表示不启用。访问者发一条 `CONNECT 主机名:端口`，服务器按主机名选中隧道并回 200，之后的字节都属于这条隧道；一个端口就能发布任意多条 TCP 服务 |
| `https_passthrough_port` | int | 0 | https 代理的 TLS **透传**监听端口，0 表示不启用。声明了 `tls_passthrough = true` 的 https 代理骑在这上面：服务器只嗅探 ClientHello 的 SNI 选中隧道，访客的 TLS 会话原样中继到客户端，证书由客户端逐域名提供。与 `https_port` 配成同一个端口会被拒绝：一个中继 TLS、一个终结 TLS，绑在一起必有一个失效 |
| `custom_404_page` | string | 空 | 一个文件的路径：http 共享监听上没有人发布的域名来访时，用它回答 404，代替内置的一句话。按请求时读取，改了文件无需重启；文件读不到时回退内置回答 |
| `vhost_http_timeout` | int | 0 | 走终结路径的 http/https 代理等待本地服务回**响应头**的上限（秒），超时回 502 并在日志记录原因；0 表示不设这条界，沿用拨号超时。对应 frp 服务端的 `vhost_http_timeout`（frp 默认 60，机制相同：反代 Transport 的响应头超时）——本地服务卡住时，先于访客一侧的超时给出明确答复 |
| `udp_packet_size` | int | 0 | 服务端侧 udp / sudp 代理上允许中继的单条数据报载荷上限（字节），超限的数据报被丢弃、计入 `/metrics` 的 `aethertunnel_udp_oversize_dropped_total`，而不是截断后交给本地服务。0 表示最大 UDP 载荷 65535；frp 同名键 `udpPacketSize` 默认 1500 |
| `max_ports_per_client` | int | 0 | 一个客户端会话最多能注册的公共端口数，超出按名拒绝；0 表示不限制。private 代理不占名额 |
| `detailed_errors_to_client` | bool | true | 拒绝客户端的答复是否带上完整理由，对应 frp 服务端的 `detailedErrorsToClient`（默认同为 true）。关闭后，措辞依赖**服务端自身配置或运行时状态**的拒绝只回一句概述：注册失败回 `cannot register the proxy "<名字>"`、身份校验失败回 `identity check failed`（`identity_required` 标记照旧发送，客户端据此继续给出可操作的提示）、隧道请求被拒回 `the tunnelled connection was refused`、首帧不合法回 `the first frame is not a valid request`。服务端日志与审计记录**始终**保留完整理由，所以关掉它不影响排障，只是不把"哪个端口被占、子域名需要哪个地址、名字是否已存在"告诉一个尚未证明自己的客户端。请求本身格式错误（JSON 解析失败）这类不涉及服务端配置的答复不受影响 |
| `p2p_port` | int | 0 | `xtcp` 打洞的 UDP 会合端口，0 表示不支持打洞（xtcp 走中继）。与 `dht.listen_addr` 撞在同一个 UDP 地址上时被拒绝 |
| `feature_gates` | 表 | 空 | 关掉本程序的某项能力（frp 的 `featureGates`，方向相反：frp 用它**启用**分级特性，这里的能力默认全开，这段是**关闭**它们的开关）。可写：`WebRTC`（访客的 WebRTC 数据路径）与 `Snark`（Groth16 访客证明），设为 `false` 后服务端**直接拒绝**走这条路径的访客连接并在审计记录原因，客户端自己的配置校验也会拒绝这样的 `[[visitors]]`。`VirtualNet` 是 frp 的唯一门名，接受但不起作用（虚拟网络未复现，会警告）；其它名字报错并列出已知门名。写在错误的段里（服务端配置里的 `[client] feature_gates`）给警告而不是静默无效 |
| `load_balance` | string | `round-robin` | 代理池策略：`round-robin` `random` `latency` `failover` `adaptive` `bandit`。只对声明了 `group` 的代理有影响。`bandit` 是**在线学习**的多臂老虎机（UCB1）：每条流按应答速度记奖励（立即回答记 1，越慢越小），据此估计各成员的平均奖励并加一个探索项来选择成员；没有离线训练、没有模型文件，学习只来自这个池子实际服务过的流。每第 20 次选择会去测观测最少的成员，因此一度很慢的成员在恢复后仍会被重新测量 |

## `[server.ssh_tunnel_gateway]`（服务端）

一台只肯开 ssh 的机器也能发布服务：服务端自己就是 SSH 服务器，客户端的 `ssh -R` 远程转发被
当作隧道发布，对应 frp 的 `sshTunnelGateway`。整段不写、或写了但 `bind_port = 0`，即关闭。

```bash
ssh -R 127.0.0.1:0:127.0.0.1:8080 v0@server -p 2200 tcp \
    --proxy_name web --remote_port 18080 --token "$TOKEN"
```

`-R` 的目标是**本地服务**（客户端自己去连它，与 OpenSSH 的 `-R` 规则一致），发布出去的端口由
命令行的 `--remote_port` 给。命令行旗标与 frp 的 sshTunnelGateway 相同：`--proxy_name`、
`--remote_port`、`--local_ip`、`--local_port`、`--custom_domain` / `--custom_domains`、
`--subdomain`、`--secret_key`、`--auth_method`、`--group`、`--token`、`--allow_users`
（`--bandwidth` 照收不改变行为），`--help` 打印全部旗标。发布走的是**与客户端 `[[proxies]]`
完全相同的那条路**：`server.allow_ports`、按名字的 `[[proxies]]` 策略、`max_connections`、
`max_ports_per_client`、审计、面板、指标与带宽账本都照旧适用；网关连接本身也走与控制端口相同的准入，
`allow_cidrs`/`deny_cidrs`、按来源的连接限速与封禁阶梯（`ban_after_failures`）同样适用，
口令或 `--token` 的每一次失败都计入认证失败并喂给封禁阶梯（已建立的会话不受影响）；隧道在面板上和一个客户端发布的
没有区别，会话的 `metas` 里带着 `ssh_user`。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `bind_addr` | string | 同 `server.bind_addr` | SSH 监听地址 |
| `bind_port` | int | 0（关闭） | SSH 监听端口。与其他监听器一起做占用检查，冲突在启动时报出来 |
| `key_file` | string | 空 | 主机私钥，OpenSSH 或 PEM 格式。留空时首次启动生成一把并写到 `auto_gen_key_file` |
| `auto_gen_key_file` | string | `aethertunnel_ssh_gateway_key` | 自动生成的主机私钥写到哪（frp 的 `autoGenPrivateKeyPath` 形态）。生成一次后重启沿用，固定过主机公钥的客户端不会被换钥匙打断 |
| `authorized_keys_file` | string | 空 | 允许开转发的公钥列表（authorized_keys 格式，一行一把）。给了文件就只认文件里的密钥。留空表示**没有登记任何密钥**：此时只有在下述 `user`/`password` 也为空时才接受任意通过认证的密钥（给「前面已经有一层访问控制」的网关用）；配了口令而没有文件时，密钥一律不接受，只认口令。文件读不出来时网关不启动，而不是退回到"谁都能连" |
| `user` / `password` | string | 空 | 一把共享口令，给没有装密钥的客户端用。`password` 为空即不接受口令登录（只配密钥的网关不会被口令绕开）；只配了 `password` 没配 `user` 在启动时报错。配了口令之后，哪怕没有 `authorized_keys_file`，**密钥也不会被接受**——"要口令"的网关不会顺带放进任何一把送上门的钥匙；两者都配时，文件里的密钥与口令都可以用 |
| `max_proxies` | int | 0（不限） | 一条 SSH 会话最多发布几条隧道，超过时命令行收到拒绝原因，会话与已有隧道留着 |
| `idle_timeout_seconds` | int | 0（不超时） | 空闲这么久就关掉这条 SSH 会话；0 表示等客户端自己结束 |

SSH 层认证（密钥或口令）与隧道层 `--token` 是**两道**：`--token` 不对时命令输出
`authentication failed: --token does not match the server's auth_token`，而 SSH 会话与它的远程
转发仍然留着，可以改了命令再试一次——`ssh` 的退出码与输出因此能直接用在脚本里。
`authorized_keys_file` 读不出来、或里面没有一把可用的公钥时，网关**不启动**（服务器带着这条
错误退出），而不是退回到"谁都能连"。客户端忘了 `-R` 时发布照样成功，但访客连上来会得到
`the ssh client did not request a remote forwarding`：转发规则就是数据路径，没有它就没有回程。

## `[[http_plugins]]`（服务端，可重复）

frp 服务端 httpPlugins webhook 的对应物：服务器在**会话登录**、**代理注册**与**每个访客
连接**时，把一个 JSON 信封 POST 到插件端点并读取 JSON 应答。应答 `reject = true`（或
省略 `reject_reason` 时的默认理由）会以与内置检查完全相同的方式拒绝：登录被拒的客户端
拿到认证应答里的理由，注册被拒的代理拿到注册拒绝，访客连接被直接关掉。**插件够不着时
同样拒绝**——把门禁接到服务器前面的人，不希望它宕机时门被绕开。

信封与 frp 的插件契约同形（`{version, op, content}` 进、`{reject, reject_reason,
unchange, content}` 出），已有的 frp webhook 可以直接应答。内容刻意比 frp 收窄：认证
令牌与私有代理的 secret 永远不出服务器。`newProxy` 应答的 `unchange = false` 携带改写
后的可发布字段（name、type、remote_port、domains、subdomain、group、multipath、
http_user），服务器按改写后的值继续注册。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 插件名，日志与拒绝理由里用它 |
| `addr` | string | 必填 | 插件端点：`http://host:port` 或 `https://host:port`；裸 `host:port` 按 http 处理 |
| `path` | string | 空 | 追加到 `addr` 后；非空必须以 `/` 开头 |
| `ops` | []string | 必填 | 本插件关心哪些操作：`login`、`newProxy`、`newUserConn` |
| `timeout_secs` | int | 5 | 每次调用的时限，0 取默认，负数被拒 |
| `tls_verify` | bool | false | https 插件端点是否校验证书 |

## `[oidc]`（两端；服务端填 issuer 侧，客户端填 token 端点侧）

frp `auth.oidc` 的对应物。配置了 `[oidc]` 的**服务器**在静态令牌之外再接受一把凭据：
客户端送来的 OIDC 访问令牌（JWT），服务器经发行方的发现文档取 JWKS、按 kid 验签
（RS256/384/512、PS256/384/512、ES256/384/512），并核对 `iss`、`aud`、`exp`/`nbf` 声明。
配置了 `[oidc]` 的**客户端**在每次（重）连接时用 client-credentials 授权向令牌端点换取
访问令牌，放进认证请求的令牌字段——有过期时间的令牌缓存到临期前 30 秒，没有过期时间的
每次连接都重新取（与 frp 的非缓存 token source 同行为）。访客连接（`[[visitors]]`）与
控制连接同源取凭据：服务端对访客沿用同一套核对（静态令牌或验签通过的访问令牌），
所以 `[oidc]` 客户端的私有代理访客在占位符 `auth_token` 下照常工作。服务端需与客户端
同步升级：旧服务器把 JWT 当静态令牌比对，必然失配。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `issuer` | string | 空 | 服务端：令牌的发行方 URL；发现文档取自 `<issuer>/.well-known/openid-configuration`，其值必须与令牌的 `iss` 声明一致 |
| `audience` | string | 空 | 两端共用：客户端向提供方申请的 aud，服务端核对 `aud` 声明；留空跳过 aud 核对（与 frp 相同） |
| `jwks_url` | string | 空 | 服务端：覆盖发现文档给出的 JWKS 地址，给不发 well-known 文档的发行方用 |
| `skip_expiry_check` / `skip_issuer_check` | bool | false | 服务端：为声明不合规矩的发行方关闭对应检查（与 frp 的 skipExpiryCheck/skipIssuerCheck 相同） |
| `timeout_secs` | int | 10 | 服务端：发现与密钥集取数的时限；发现失败会让服务器拒绝启动 |
| `token_endpoint_url` | string | 空 | 客户端：以 client-credentials 换取访问令牌的端点 |
| `client_id` / `client_secret` | string | 空 | 客户端：令牌请求的凭据，必须成对出现（HTTP Basic 认证头） |
| `scope` | string | 空 | 客户端：令牌请求附带的作用域 |

## `[client]`（客户端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `server_addr` | string | 条件必填 | `host:port`。为空时必须设置 `[dht] discover` 且 `[dht] enabled = true`，否则报错 |
| `auth_token` | string | 必填 | 与服务端一致 |
| `auth_token_file` | string | 空 | 从文件读取共享密钥，配置文件里就不必写它——对应 frp 的 `auth.tokenSource`（type `file`）。行尾空白与末尾换行会去掉；文件读不到或只有空白是**硬错误**；同时写了 `auth_token` 时以文件为准并给一条警告。令牌也参与派生加密口令（未单独设口令时），因此这一项在密钥派生之前生效；热重载会重新读取该文件 |
| `reconnect_seconds` | int | 3 | 重连退避的起始值。负数被拒绝 |
| `max_reconnect_seconds` | int | 60（且不小于 `reconnect_seconds`） | 退避上限，不得小于 `reconnect_seconds`，也不得超过 86400（24 小时）；退避带 ±20% 抖动。负数被拒绝 |
| `heartbeat_seconds` | int | 30 | 心跳间隔的**回退值**：服务端在会话建立时把自己的 `[server].heartbeat_seconds` 下发下来，客户端按它发心跳（服务端才是"连续 3 次未收到即断开"的一方），因此正常会话里本项不生效；服务端没有下发时（第三方服务端）才用它。与下发的值不同时会在连接时报告一行。负数被拒绝 |
| `tcp_keepalive_seconds` | int | 0 | 本客户端到服务器连接上的 TCP keep-alive 探测间隔；0 用 Go 默认（15 秒）。应用层心跳之外再加一层内核探测，对端静默掉线（故障 NAT、机器断电）时会话更快被判死并重建——对应 frp 客户端的 `transport.dialServerKeepalive`（frp 默认 7200）。负数被拒绝 |
| `dns_server` | string | 空 | 解析 `server_addr` 里的主机名时使用的 DNS 服务器，写成 `host:port`（如 `10.0.0.53:53`）；空表示用系统解析器。对应 frp 客户端的 `dnsServer`：内网域名只有内网解析器认得时用它。端口必须写，写错不会被静默回退到系统解析器 |
| `user` | string | 空 | 本客户端的**身份名**——对应 frp 客户端的 `user`：私有代理的 `allow_users` 按它放行；没有 `allow_users` 的私有代理只允许**发布者本人这一身份**（同一 operator 的其它客户端照常可用）。不设置即空身份，未设置过的部署彼此之间的行为与以前完全一致 |
| `connect_server_local_ip` | string | 空 | 本客户端连服务器时绑定的**源地址**，必须是本机的 IP 字面量——对应 frp 客户端的 `connectServerLocalIP`：多网卡机器用它决定服务端看到的来源地址，防火墙规则也可以按它来写。地址不在本机时在拨号时失败并给出内核的原始报错；域名形式会被配置校验直接拒绝 |
| `dial_timeout_seconds` | int | 10 | 连接服务端、等待 `DataOpenAck`、连接本地服务的超时。负数被拒绝（此前负值等于"立即超时"，客户端永远连不上） |
| `idle_timeout_seconds` | int | 300 | 单条隧道流的空闲上限：超过这段时间没有字节流动就断开。适用于服务端转发的流、访客的本地监听连接，以及打洞后的直连路径。负数被拒绝 |
| `dial_via` | string | 空 | 经一个中转代理连接服务器：`socks5://user:pass@10.0.0.2:1080`、`socks5h://proxy.lan`、`http://proxy.lan:3128`、`https://…`。`socks5`/`socks5h` 的端口可以省（默认 1080），`http`/`https` 必须写出端口。直连被封或计费时用它；隧道自身的 TLS 与加密全部加在中转之上，中转只看到不透明的 TLS 形状流量。留空直连 |
| `metas` | 表 | 空 | 操作者自选的键值对（`[client.metas]` 下），随认证请求上报：服务端在日志里记录，并原样交给 `[[http_plugins]]` 的 webhook |
| `client_id` | string | 主机名 | 本客户端的**稳定名字**，对应 frp 客户端的 `clientID`。它随认证请求上报，服务端在**日志的 `client_id` 字段、审计记录的 `client_id`、面板的客户端与代理池成员、以及带宽账本条目**里都用它称呼这个客户端，而不是每个会话随机生成的标识；同名客户端重连后仍是同一个名字。客户端不上报它时，服务端退回用会话标识。最长 128 字节：账本的每客户端合计以它为键、永久保存，服务端拒收更长的一个（认证时拒绝），`--check` 同样拒绝。留空默认取主机名 |
| `login_fail_exit` | bool | false | 设为 true 时，**第一次**登录就失败的客户端直接退出并在日志里给出原因，不再无限重试——连接不上服务器、认证被拒（token 不对、被拉黑）、或向 IdP 取访问令牌失败都算；已经成功登录过一次的会话掉线后照常重连。**注册被拒不算登录失败**：端口名额用尽、名字或端口冲突都发生在认证通过之后，客户端记一行日志并按退避重试——这些拒绝可能是瞬时的，首次会话遇到就杀掉客户端会让部署变脆。对应 frp 客户端的 `loginFailExit`（首次登录任何一步失败即退出）——frp 默认 true，本客户端默认 false 保持旧行为 |
| `start` | 字符串数组 | 空 | 只启动其中列出的代理与访客，文件里其余条目保持定义但不启动——对应 frp 客户端的 `start`。空数组表示全部启动；写了没有任何条目对应的名字只在启动时给一条警告，不影响其余条目。热重载会重新按它筛选：新列出的隧道上线，不再列出的隧道注销并关闭公开端口 |
| `feature_gates` | 表 | 空 | 本客户端自己不使用被关掉的能力：`Snark = false` 让含 `auth_method = "snark"` 的代理/访客条目在配置校验时被拒，`WebRTC = false` 拒绝 `transport = "webrtc"` 的访客。见 `[server] feature_gates` 的完整说明（服务端 gate 是对访客连接的运行期拒绝，客户端 gate 是对自己的配置校验） |
| `udp_packet_size` | int | 0 | 客户端侧 udp / sudp 隧道上允许中继的单条数据报载荷上限（字节），超限的数据报被丢弃且不截断（否则本地服务会收到半条数据报）。0 表示最大 UDP 载荷 65535；frp 同名键 `udpPacketSize` 默认 1500，为不改动既有行为本客户端默认不设限 |
| `pool_count` | int | 0 | 预先在服务端**停放**多少条数据连接（0–64），对应 frp 的 `transport.poolCount`（默认同为 0，即每条流按需拨一条连接）。访客到达时服务器直接把手边已停放、已完成确认的连接交给它，省掉一次拨号（开了 TLS 时就是一次完整握手）；一条停放连接只承载一条流，用过之后客户端才再停放一条，所以有流在用时会暂时少几条空闲连接，流结束后补回。**关闭它不损失任何功能**：没有停放连接、或某条停放连接已经被客户端丢掉时，服务器改回在控制连接上要一条新连接。上限 64 与服务端每会话的停放假额是同一个数，超过直接报错而不是让多出来的连接被服务端拒绝。三种数据路径都实测走通过停放连接：公网端口的 `tcp`、`udp`（数据报会话），以及私有代理的访客路径（`stcp`）；xtcp 打洞成功后的直连路径本来是两端直连、不经服务器，因此也不经池。指标见 `aethertunnel_pool_*` 四项 |

## `[client.admin]`（客户端管理 API）

对应 frp 客户端的 webServer：一个只给本机操作者用的小 HTTP 端口，回答运行状态并执行
配置重读。`/healthz` 供进程监管探活，不要求凭据；`/api/status` 返回会话与每个代理的
配置端口、确认状态（服务器最近一次代理名单里有它）与健康状态；`POST /api/reload`
（GET 也可以）走与 SIGHUP 完全相同的重读路径，按名增删隧道。没有配置文件可读时
（客户端从字符串启动，如移动端绑定）reload 回 400 并说明原因。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否启动管理 API |
| `bind_addr` | string | `127.0.0.1` | 监听地址。默认回环：管理面是给本机操作者的。留空按默认回环处理（不回警告），非回环地址才警告 |
| `port` | int | 0 | 监听端口；负数或超过 65535 一律报错，与 `enabled` 无关（与其他端口键同一条总则）；`enabled = true` 时还必须是可用的端口，即 1–65535 |
| `user` / `password` | string | 空 | basic auth 凭据，必须成对出现；留空表示 `/api/status` 与 `/api/reload` 也不设防——此时把监听开在回环地址之外会给警告。监听失败（端口被占）会让客户端启动失败，因为它是一项被明确要求的配置 |

## `[[proxies]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 代理名，同一客户端内不可重复；同一 `group` 内多个客户端可以同名 |
| `annotations` | 表 | 空 | 自由标签（`[proxies.annotations]` 下），只被 dashboard 展示、不影响行为——对应 frp 的 `annotations`：把 `env`、`owner`、工单号挂在隧道上，名字不必承担这些信息 |
| `enabled` | bool | true | 设为 false 时这条代理保持定义但**不启动**——对应 frp 的 `enabled`：条目留在文件里（评审记录、开关都方便），服务端完全看不到它。与 `[client] start` 叠加时两者都满足才运行；`start` 指向一条已禁用的条目会给警告 |
| `type` | string | `tcp` | `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5` `tcpmux` |
| `local_ip` | string | `127.0.0.1` | 本地服务地址；IPv6 地址同样可用（如 `::1`） |
| `local_port` | int | 必填 | 本地服务端口（1–65535） |
| `remote_port` | int | 0 | 服务器上对外开放的端口。`tcp`/`udp` 用它；`http`/`https` 与私有类型必须为 0。同一客户端里两个**同协议**的代理不能请求同一个端口（先注册的绑住它，第二个会被拒绝），`tcp` 与 `udp` 用同一个端口号是允许的：它们绑的是不同协议的两个套接字 |
| `domains` | []string | 空 | `http`/`https` 的访问域名：精确域名、`*.通配`，或留空后由服务端 `subdomain_host` 拼出 `<代理名>.<该值>`。通配同时拥有裸域本身——`*.example.com` 也应答 `example.com`，与 tcpmux 和 TLS 透传的匹配一致；裸域被另一条精确条目占有时仍由精确条目优先。服务端会拒绝不是合法主机名（或 `*.后缀` 通配）的条目，所以不经本仓库客户端校验的注册方也发不进带空白或空标签的域名。留空时客户端只给出警告，因为该设置属于服务端 |
| `subdomain` | string | 空 | 子域名托管：客户端只声明一个 DNS 标签（小写字母/数字/连字符，1–63 字符），服务端把它拼成 `<标签>.<subdomain_host>` 并按该域名路由。声明了 `subdomain` 的代理，代理名不再参与域名拼接。服务端没有 `subdomain_host` 时拒绝注册 |
| `http_user` / `http_password` | string | 空 | 仅 `http`/`https`：服务器在转发之前检查访问者的 Authorization 头，不匹配回 401 并带 `WWW-Authenticate: Basic` 挑战；两者任一非空即启用。二者都填空才是不设防的主机名 |
| `multiplexer` | string | 空 | 仅 `tcpmux`，目前唯一取值 `"httpconnect"`：访问者对该隧道发 `CONNECT`，以主机名选中 |
| `use_compression` | bool | false | 字节流在客户端与服务器之间用 snappy 流式压缩（加在加密层外侧，先压缩后加密）。服务器在每条 DataRequest 里回声确认，旧服务端不回声就自动退化为不压缩，混合部署不会坏流。对数据报隧道无效果（给出警告），打洞成功的 xtcp 直连路径不经过它 |
| `use_encryption` | bool | false | 只给**这一条代理**的中继字节流加一层 AEAD：密钥由本端的 `auth_token` 与代理名经 HKDF 导出，两端各自算出，不需要再协商——所以整会话不开 `[encryption]` 时也能单独保护一条代理。服务器在 DataRequest 里回声这个标志，客户端据此决定是否加层；`[encryption] enabled = true` 时整会话已有密钥，此时**不再叠这一层**，只在配置校验时给出一条告警（会话密钥已经覆盖这条流）。只对字节流生效：数据报（udp/sudp）与 socks5 的 UDP 走帧而不是字节流，不受影响。两端各自从本次会话认证**实际提交的凭据**导出这层密钥（静态令牌认证用 `auth_token`，`[oidc]` 认证用访问令牌），所以 `[oidc]` 客户端的 `auth_token` 写占位符也无碍；不使用 `[oidc]` 访问令牌时，`auth_token` 必须写成服务端的同一个值 |
| `request_headers` / `response_headers` | 表 | 空 | 仅 `http`/`https`（终结形态）：服务端在转发的 HTTP 请求上 `Set` 这些请求头、在回答上 `Set` 这些应答头。`tls_passthrough` 的透传不解析 HTTP，忽略这两项 |
| `route_by_http_user` | string | 空 | 仅 `http`/`https`（终结形态）：vhost 的第三路由维度——同域名同前缀下，声明了用户的代理只接受呈现该用户名的请求，未声明的代理接住其余所有访客；路由发生在代理自身的凭据核对**之前**（核对用的是完整用户名+密码）。`tls_passthrough` 不解析 HTTP，忽略此项 |
| `locations` | []string | 空 | 仅 `http`/`https`（终结形态）：按路径前缀拆分主机名——多个代理可共用一个域名，各自声明如 `["/api", "/v2"]`，服务器把每个请求交给**最长匹配前缀**的代理，没有声明 locations 的代理兜底该主机名其余的路径。前缀按字节匹配（`/api` 也匹配 `/apiv2`，与 frp 相同）。`tls_passthrough` 不解析路径，忽略此项 |
| `tls_passthrough` | bool | false | 仅 `https`：隧道改骑 `[server].https_passthrough_port` 的透传监听——服务器按 ClientHello 的 SNI 选中隧道后把访客的 TLS 会话原样中继过来，访客看到的是客户端自己的证书（每个域名各归各家）。本地服务必须自己说 TLS，或配 `https2http` 插件替它终结。默认 false：https 在共享监听上由服务器统一终结 TLS |
| `secret_key` | string | 空 | 私有类型必填，也是访客侧的凭据 |
| `auth_method` | string | `secret` | `secret` 直接比对；`nizk` 用 Schnorr 证明，secret 不出现在线上；`snark` 用 Groth16 的 zk-SNARK 证明（电路与可信设置说明见 `pkg/snarkauth`），secret 不出现在线上且证明绑定本次挑战，单次证明约几十毫秒。**访客侧的 `auth_method` 必须与代理侧一致**，服务端按代理注册的方法校验；三种不一致的结局都写明，不会降级成明文比对：访客写 `secret` 而代理是 `nizk`/`snark` 时，访客会把明文 secret 放进自己的 VisitorConnect（发不发由它自己的配置决定，服务端无从提前知道），服务端看到 secret 即按"访客的 auth_method 与代理不一致"拒绝，不再用挑战去接它；两端都是证明法但方法不同（`nizk` 对 `snark`）时，证明验不过，拒绝理由是 `the proof does not verify`；访客用证明法而代理是 `secret` 时，它根本没带 secret，服务端以 `invalid secret key` 拒绝 |
| `group` | string | 空 | 填入同一名字的多个客户端组成代理池 |
| `multipath` | int | 0 | 数据报代理最多使用的数据连接数（0–8）。字节流代理上写大于 1 的值会给出警告（1 与 0 同为单路径，是无操作） |
| `allow_targets` | []string | 空 | 仅 `socks5`：本客户端允许拨号的地址范围（CIDR）。**必填**，缺失即拒绝注册 |
| `allow_cidrs` | []string | 空 | 只有匹配的来源地址可以访问该代理；留空表示不限 |
| `deny_cidrs` | []string | 空 | 拒绝的来源地址，优先级高于 `allow_cidrs` |
| `allow_users` | 字符串数组 | 空 | 允许访问本**私有**代理（stcp/sudp/xtcp）的客户端身份名，对应 frp 的 `allow_users`；空表示只允许发布者自己 `[client].user` 的身份。密钥只是共享的那一半，这一项决定"谁家的客户端可以用它"。写在非私有类型上不起作用，会得到一条警告；重复项也会被指出 |
| `bandwidth` | string | 空 | 本代理在客户端侧的限速，双向合计，十进制字节每秒：`1MB`、`500KB`；留空不限速。`socks5` 代理的每条 UDP 关联也按它限速，`udp`/`sudp` 数据报代理的本地套接字也按它限速 |
| `proxy_protocol` | string | 空 | `v1` 或 `v2`：服务器把带访客真实地址的 PROXY protocol 头插到本地服务收到的流最前面——`v1` 是文本行，`v2` 是二进制头（含签名与命令块）；留空不插。服务端不认识该设置时只是不发头 |
| `remote_ports` | string | 空 | 端口段展开：`"6000-6002"` 生成 `remote_port` 6000/6001/6002 的三个代理，名字分别为 `名-6000`、`名-6001`、`名-6002`；条目的其余设置对每个副本生效。最多 256 个端口。`client.start` 里写展开前的名字会跟着展开成这三个名字 |
| `plugin` | string | 空 | 用客户端自带的组件代替本地服务：`static_file`（把 `plugin_local_path` 目录以 HTTP 发布）、`unix_domain_socket`（拨 `plugin_local_path` 的套接字）、`https2http`（用 `plugin_cert_file`/`plugin_key_file` 在客户端终结访客的 TLS，把明文转发给 `local_port` 的 HTTP 服务——配一条 `tcp` 代理即可在专用端口发布 HTTPS，或声明 `type = "https"` 加 `tls_passthrough = true` 骑在透传端口上）、`https2https`（同上终结访客的 TLS，再以 TLS 拨 `local_port` 的本地 HTTPS 服务——两腿都加密）、`tls2raw`（终结访客的 TLS 后把明文字节原样转给 `local_port` 的 TCP 服务，适合说自有协议的后端）、`http_proxy`（客户端运行 HTTP 正向代理：访客用绝对地址请求与 CONNECT 出客户端网络，`allow_targets` 圈定可拨范围，`plugin_http_user`/`plugin_http_password` 可选保护）、`socks5`（访客对公共端口说 SOCKS5，客户端从自己的网络拨目标；`allow_targets` **必填**圈定可拨范围，`plugin_user`/`plugin_password` 可选认证；UDP ASSOCIATE 以"命令不支持"拒绝）、`http2https`（明文进、以 TLS 出到 `local_port` 的本地 HTTPS 服务，SNI 取请求主机名） |
| `plugin_local_path` | string | 空 | `static_file` 与 `unix_domain_socket` 用的本地路径 |
| `plugin_user` / `plugin_password` | string | 空 | 仅 `socks5` 插件：SOCKS5 用户名/密码认证（RFC 1929），两者必须成对出现——只设其中一个的配置会被 `--check` 拒绝；留空表示任何访客无需凭据 |
| `plugin_cert_file` / `plugin_key_file` | string | 空 | `https2http` 插件终结 TLS 用的证书与私钥；证书按代理名缓存，连接不再读文件 |
| `host_header_rewrite` | string | 空 | 本地服务看到的 Host 头替换成这一项：`http2https` 插件发往本地服务时用它，服务端终结的虚拟主机路径在 http/https 代理上转发时也用它；留空保留访客的 Host。原始主机名仍用于本地 TLS 的 SNI |
| `plugin_http_user` / `plugin_http_password` | string | 空 | `static_file` 的 HTTP basic auth；两者任一非空即启用 |
| `health_check` | 表 | 空 | 本地服务健康检查：`type = "tcp"` 或 `"http"`，`interval_s`、`timeout_s`、`max_failed`、`path`、`http_headers` 五个子键列在下面紧接着的几行里。**判定规则**：`tcp` 能连上就算健康；`http` 要求最终应答是 **2xx**（frp 只认 200，这里放宽到整个 2xx——204 也是健康应答；重定向会被跟随，与 frp 相同，判定的是链尾的状态码）。连续失败达到 `max_failed` 后，客户端向服务端注销该代理（协议消息 `ProxyWithdraw`），公开端点随之关闭；探活恢复后自动重新注册。服务端不认识注销消息时，客户端回退为拒绝拨号、代理保持注册，见 `docs/VS-FRP.md`。注意 `max_failed` 默认 3，frp 默认 1——本项目的探活更保守，避免一次抖动就摘掉公开端点。`health_check` 只对真正拨号到本地服务的代理类型有意义，写在 `socks5` 代理、`udp`／`sudp` 代理或 `static_file`／`unix_socket`／`socks5`／`http_proxy` 这些**进程内插件**上会被 `--check` 拒绝：`socks5` 一个代理服务多个目标、探活无从判定，`udp`／`sudp` 的本地服务是 UDP 端口而探活走 TCP，进程内插件根本不拨本地端口——三种情况下探活都会次次失败，最终把一条本来可用的隧道永久注销 |
| `interval_s` | int | 10 | `health_check` 的探活间隔秒数，不得超过 86400 |
| `timeout_s` | int | 3 | `health_check` 的单次探活超时秒数，同样不得超过 86400 |
| `max_failed` | int | 3 | `health_check` 连续失败达到该次数后注销该代理，判定规则见上面 `health_check` 一行（frp 默认 1，这里更保守） |
| `path` | string | 空 | `health_check` 仅 http：探活的 URL 路径，对应 frp 的 `healthCheck.path`；留空即请求 `http://<local_addr>`（路径为 `/`） |
| `http_headers` | 表 | 空 | `health_check` 仅 http：探活请求带的头，对应 frp 的 `healthCheck.httpHeaders`——探活端点要凭据时靠它 |

`socks5` 没有本地服务，因此 `local_ip` 与 `local_port` 会被忽略并给出警告，必须设置
`remote_port`。它支持 TCP `CONNECT` 与 UDP `ASSOCIATE`：前者按访客指定的目标拨号，后者在
服务端开一个 UDP 中继、按每个数据报头里写的目标拨号并把应答原样带回。`allow_targets` 对两者
都生效，按 IP 范围匹配，域名先解析再匹配；不在范围里的目标会以
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
`auth_method`、`allow_targets`、`domains`、`subdomain`、`http_user`、`http_password`、`multiplexer`、`use_compression`、`use_encryption`、`tls_passthrough`、`request_headers`、`response_headers`、
`bandwidth`、`proxy_protocol`、`remote_ports`、`plugin`、`host_header_rewrite`、`health_check`）在服务端配置里会加载成功但不生效，`--check` 会逐条
输出警告。因此同一份 `[[proxies]]` 列表可以放在两种角色的配置里，只是含义不同。

## `[[visitors]]`（客户端，可重复）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 本机监听器的名字 |
| `enabled` | bool | true | 设为 false 时这条访客保持定义但**不监听**——对应 frp 访客的 `enabled`；`start` 指向一条已禁用的条目会给警告 |
| `type` | string | `stcp` | `stcp` `sudp` `xtcp`。要与服务端上那个代理的**形状**一致：`stcp` 与 `xtcp` 是**字节流**访客（`xtcp` 仍然是字节流，它的本地监听器是 TCP，打洞之后也走字节流），`sudp` 是**数据报**访客。不一致时服务端拒绝并说明原因：服务端按数据报转发、客户端按字节流直通，本地服务会收到帧头，访客什么也收不到 |
| `server_name` | string | 必填 | 服务端上对应的私有代理名 |
| `secret_key` | string | 必填 | 必须与代理侧一致 |
| `auth_method` | string | `secret` | `secret`、`nizk` 或 `snark`，且必须与代理侧的 `auth_method` 一致。写 `secret` 时本端会把明文 secret 放进 VisitorConnect，所以对 `nizk`/`snark` 的代理会被服务端以"访客的 auth_method 与代理不一致"拒绝；本端写证明法而代理用的是另一种证明法时，证明验不过，拒绝理由为 `the proof does not verify`。方法不一致不会被降级成明文比对 |
| `transport` | string | 空（中继） | `webrtc`：数据路径走 WebRTC DataChannel（DTLS 加密、ICE 选路），信令走控制连接；仅字节流私有代理（`stcp`，以及声明了它的 `xtcp`——`xtcp` 访客声明后不再打洞，直接由通道承载）。服务端在通道打开时记一行 `data channel established`，客户端把这条会话记成 `direct true`——两侧都能看出流量走的是通道而不是中继 |
| `bind_addr` | string | `127.0.0.1` | 本机监听地址。非回环地址时会**警告**：这个监听器自身没有认证（能连上端口的人就能用这条隧道），而服务端的 `[[proxies]]` 名单判断的是**这个客户端**的地址、不是它后面的用户，所以绑到别的地址等于把私有代理交给那个网络 |
| `bind_port` | int | 必填 | 本机监听端口（1–65535）。两个 visitor 绑同一个地址时被拒绝 |
| `fallback_to` | string | 空 | 隧道打不开时把来访的连接**直接**接到这个本地地址（`host:port`）——对应 frp 访客的 `fallbackTo`：服务端或隧道不可用时私有代理的调用方仍能用，代价是绕过隧道直连。空表示不设回退，调用方像以前一样被拒。地址非法（缺端口/端口越界）直接拒绝；仅字节流访客（stcp/xtcp），数据报访客不使用它 |
| `fallback_timeout_ms` | int | 0 | 隧道尝试的时间上限（毫秒），到点即用回退——对应 frp 访客的 `fallbackTimeoutMs`；0 表示由隧道自身的超时决定（例如拨号超时）。没有 `fallback_to` 时只给一条警告；负数被拒绝，上限 9223372036854——再大换算成毫秒就溢出，回退会立刻生效而不是等满窗口 |

## `[store]`（客户端）

运行期条目：不在配置文件里写、而是通过客户端管理 API 增删的代理与访客。对应 frp 的
`client.store`。写进去的条目持久化到一个 JSON 文件，重启后读回并生效；**同名的运行期条目
压过配置文件里的条目**，因此不动配置文件也能改一条已发布隧道的行为（删掉运行期条目即回到
配置文件里的定义）。启动时会校验整个文件：过不了校验的运行期条目（端口越界、私有代理缺 `secret_key`、监听地址冲突……）
会被**丢弃并各记一行日志**（写明文件名与原因），客户端照常启动，而不是"加载一半"——一个连解析都过不了的条目（例如它带着本版本不认识的键）同样按条丢弃，不让整个客户端起不来——同一名字冲突时按名字排序取
第一条，所以重启后留下的还是同一条；只有配置文件**本身**过不了校验时，客户端才带着文件名与原因退出，因为那不是
丢掉某条运行期条目就能解决的问题。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `path` | string | 空 | 运行期条目持久化到的 JSON 文件；留空即**关闭** store：管理 API 上的 `/api/store` 一律 404，也不会读写任何文件——没要求运行期改动的部署不可能"顺手"改动 |

`path` 需要 `[client.admin] enabled = true`：条目只能从管理 API 进来，没有 API 的文件永远是
空的，配置里两个键并存会在校验时被拒绝。管理 API 的形状与 frp 的客户端 API 一致：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/store` | 每个条目的 JSON 形式，按名字分组 |
| PUT | `/api/store/proxy/{name}` / `/api/store/visitor/{name}` | 用请求体里的 JSON 对象创建或替换同名条目；名字必须与路径一致，条目按与配置文件相同的解码与校验走一遍（不认识的键名直接拒绝），写盘后**立即生效**——发布与下线的动作走的就是热重载那套代码。跨条目的冲突（两条条目争同一个公开端口）在写入时就用**整份合并后的条目**跑一遍校验，**当场以 400 拒绝**，条目不会落盘；只有手工改文件造成的冲突才会在下次启动的整份校验里被丢弃并各记一行日志 |
| GET | `/api/store/proxy/{name}` | 该条目的 JSON 形式，没有则 404 |
| DELETE | `/api/store/proxy/{name}` / `/api/store/visitor/{name}` | 删除并撤销（公开端点随之关闭）；没有则 404 |

所有 `/api/store*` 端点都要管理 API 的 basic auth（与 `/api/status`、`/api/reload` 相同）。
请求体是**代理条目的 JSON 形式**（键名与配置文件里的键一致，例如 `{"type":"tcp","local_ip":
"127.0.0.1","local_port":8080,"remote_port":6022}`），回复里的 `proxies` 是填好默认值之后的
条目，也就是客户端接下来真正发布的东西。

## `[dashboard]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否启动面板。写了 `token` 却忘了把它打开时会**警告**（面板不会出现；只有另开的 `[metrics]` 监听器才会用上这把 token） |
| `bind_addr` | string | `127.0.0.1` | 面板监听地址。留空按默认回环处理；非回环地址且未设 `token` 时会**警告** |
| `port` | int | 7500 | 面板端口 |
| `token` | string | 空 | 设置后 `/api/*` 需要 `Authorization: Bearer <token>`；`/api/health`、`/healthz`、`/readyz` 始终公开。它同时是 `[metrics]` 独立监听器接受的凭据之一 |

## `[encryption]`（两端，必须一致）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 打开后控制帧与隧道记录都会被 AEAD 保护 |
| `algorithm` | string | `xchacha20-poly1305` | 也可用 `aes-256-gcm`；`enabled = true` 时其他值会被拒绝 |
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
| `protocol` | string | 空 | 客户端 | 到服务器的每条连接（控制、数据、访客）的承载方式：留空是普通流；`websocket` 把连接包进 RFC 6455 二进制帧，让隧道穿过只转发规整 websocket 会话的中间设备（frp 的 `transport.protocol = "websocket"` 形态）。服务端无需任何设置：它在同一端口上按连接嗅探升级请求，其余连接原样放行。TLS（`enable_tls`）仍在 websocket 之外，即 wss 的分层。形态会写进客户端启动日志——普通流不写，`websocket` 写一行 RFC 6455，所以「配置写了但没生效」一眼可见 |
| `wire_protocol` | string | 空 | 客户端 | frp 的 `transport.wireProtocol`。本项目只有一种线上格式，它的修订号在握手内协商，所以接受的值是 `v2`（frp 给同一种格式的名字）与留空，`v1` 直接报错——静默接受等于承诺一种不存在的格式 |
| `tcp_mux` | bool | 未写即 true | 客户端 | frp 的 `transport.tcpMux`。frp 把客户端的所有逻辑连接复用在一个 yamux 会话上；本项目每条数据连接只承载一条流、从不复用，也就是 frp `tcp_mux = false` 的形状。写 `true` 会给一条警告说明数据路径两种取值下相同；**没写不提示**，因为 frp 的默认值就是 true，提示它只会让每个从 frp 来的配置都多一句废话 |
| `additional_scopes` | 字符串数组 | 空 | 客户端 | frp 的 `transport.additionalScopes`：允许省掉凭据的消息名（frp 认 `HeartBeats` 与 `NewWorkConns`）。本项目这两个帧上本来就没有凭据可省——心跳是空帧，data-open 带的是会话标识与每流密钥——所以两个名字照收并给一条说明，别的名字直接报错 |

`端` 列只是说明这一项在**哪一端生效**：写在另一端（服务端写 `ca_file`／`server_name`／`insecure_skip_verify`，客户端写 `cert_file`／`key_file`）会加载成功但不生效，`--check` 会逐条警告——写得像启用 TLS 的客户端配置却什么都没做，是发布前就该看见的错。

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
| `enabled` | bool | false | 是否提供 `GET /metrics`（Prometheus 文本格式 0.0.4）。端点出现在面板监听器上（面板开着的时候），以及下面 `port` 指的那个独立监听器上。两者都没有时会**警告**——开关是开的，却没有任何监听器回答它 |
| `token` | string | 空 | 该 token 与面板 token 任一可用；两者都未设置时 `/metrics` 不需要鉴权 |
| `bind_addr` | string | `127.0.0.1` | `port` 非 0 时独立指标监听器的绑定地址。默认回环：它不带凭据时是本机的事。留空按默认回环处理，不会为此给出警告 |
| `port` | int | 0 | 在自己的地址上另开一个只回答 `GET /metrics` 的监听器（其余路径 404），对应 frp 的 `metrics.addr`——**面板不开也能被采集**：`[dashboard] enabled = false` 时面板那个监听器根本不存在，指标从这一个端口取。需要 `enabled = true`（先有端点才谈得上在哪儿提供，否则两个键互相矛盾，校验直接报错）。地址参与端口冲突检查，与 `dashboard.port` 撞车会报出来 |

独立监听器的凭据规则与面板上的 `/metrics` 完全相同（`metricsCredentialAccepted` 一处实现）：`token`
设置时只有它或面板 token 可用，两者都没设置时不需要凭据——所以默认只听回环，绑到非回环地址而没设
token 会在启动时给出告警。端口被占用时服务器记一条日志并继续服务隧道，与面板端口被占时一样。

指标序列：控制连接、被拒绝的控制连接、认证失败、ACL 拒绝、限流拒绝、被封禁的来源、
因封禁被拒的连接、按代理拒绝的访客、socks5 请求数、因服务器关闭而拒绝的流、
数据连接、活动与累计流数、双向字节、UDP 数据报与活动会话、共享监听上的 HTTP 请求数、
打洞尝试数与结果（直连/中继），停放连接（停放数、被用掉的流数、落空的次数、因会话已满被拒
的次数），审计日志的写入失败数，以及按 `tunnel` 标签的活动流、
累计流、双向字节与 HTTP 请求数。
每一行都直接读自运行中的计数器，没有估算值；`aethertunnel_p2p_direct_total` 只在访客
用 `ATP3` 数据报回报了直连路径时才增加，访客没回报的尝试不会计入直连。

两个字节计数器与按隧道的字节只统计**服务端搬过**的字节：`xtcp` 打洞成功后两端直连，那个方向
的流量不再经过服务端，因此既不在这些计数里，也不在带宽账本里；服务端能记下的是这次尝试与它
报告的结果（`aethertunnel_p2p_*_total` 与对应的审计事件）。

序列名逐条列出（值都是自进程启动以来的累计数，除标为 gauge 的）：

| 序列 | 类型 | 含义 |
|---|---|---|
| `aethertunnel_uptime_seconds` | gauge | 进程运行秒数 |
| `aethertunnel_control_connections_total` | counter | 被接受的（已通过握手的）控制连接 |
| `aethertunnel_control_rejected_total` | counter | 被拒绝的控制连接：容量、ACL、限流、封禁、首帧读不出来、首帧不是一个可用请求，以及凭据不通过（访客入口的拒绝也算在这里）；各项另有单独的计数。其中有些是不回答直接关闭的，所以这一条是"盯着端口有没有人在敲"该看的汇总 |
| `aethertunnel_auth_failures_total` | counter | 凭据错误的认证尝试 |
| `aethertunnel_connections_denied_by_acl_total` | counter | 被允许/拒绝名单挡下的连接 |
| `aethertunnel_connections_rate_limited_total` | counter | 被按来源令牌桶挡下的连接 |
| `aethertunnel_sources_banned_total` | counter | 因反复认证失败被封禁的来源 |
| `aethertunnel_banned_connections_refused_total` | counter | 因来源处于封禁期而被拒绝的连接 |
| `aethertunnel_visitors_denied_by_proxy_total` | counter | 被拒绝的访客：既包括被服务端为该名字定的策略拒绝的，也包括被代理自己的名单拒绝的（审计记录的 `detail` 指出是哪一边） |
| `aethertunnel_unusable_request_frames_total` | counter | 首帧不是一个可用请求的连接：载荷无法解析的请求帧（认证请求、访客连接或数据打开），或一个不能用来开局面的帧类型。每条都会先回答再关闭；控制端口上那几种和别的拒绝一样计入汇总，数据打开那一帧不计入汇总（它没有成为控制会话）；单列是为了让"有人在拿垃圾帧扫端口"不被读成策略拒绝 |
| `aethertunnel_handshake_failures_total` | counter | 连首帧都读不出来、因而被**不回答直接关闭**的连接：伪装、加密或 TLS 两端设置不一致导致字节解不开，或者对端连上什么都不发就离开（TCP 健康检查也是这样）。和上一条的区别在于"字节到了但不可用"还是"根本没有可读的帧"，两者都会计入 `control_rejected`；这一条单列是因为它此前只写日志：一边改了 `[encryption]` 口令的机群、或拿垃圾扫控制端口的人，在指标和审计里都是零 |
| `aethertunnel_dashboard_unauthorized_total` | counter | 面板监听器或独立的 metrics 监听器上因令牌缺失或错误被拒的请求：需要令牌的每个 `/api` 端点，以及没带对令牌的 `/metrics`（两个监听器共用这一条，独立 metrics 监听器上的拒绝也计入）。这两个监听器**自己都没有任何限流**，所以这条是"有东西在够一处它没有凭据的监听器"的唯一痕迹；它只计数不写审计，是因为面板本身每隔几秒轮询一次，一次请求一行会让一个拿着过期令牌的客户端把痕迹埋掉（控制端口那边每条一次是因为一条连接只对应一次拒绝） |
| `aethertunnel_streams_refused_while_draining_total` | counter | 因服务器正在关闭而被拒绝的流 |
| `aethertunnel_socks5_requests_total` | counter | 通过 socks5 出口发出的 CONNECT 请求 |
| `aethertunnel_socks5_udp_associations_total` | counter | socks5 出口接受的 UDP ASSOCIATE 关联 |
| `aethertunnel_socks5_udp_datagrams_total` | counter | socks5 出口转发的 UDP 数据报（双向各计一次） |
| `aethertunnel_socks5_malformed_requests_total` | counter | socks5 出口上收到的、不是一个可用 SOCKS5 请求的连接（与上面的"被名单拒绝"分开计数，便于区分探测与策略拒绝） |
| `aethertunnel_data_connections_total` | counter | 客户端打开的数据连接（能解析出 `DataOpen` 的那些，包括后面没能配对的） |
| `aethertunnel_pool_parked_total` | counter | 客户端按 `pool_count` 预先停放的数据连接数 |
| `aethertunnel_pool_used_total` | counter | 在停放连接上直接服务掉的流数（每条停放连接只用一次） |
| `aethertunnel_pool_missed_total` | counter | 需要流时发现停放连接已不可用的次数（客户端被重启过就会出现），这些流改走控制连接要新连接 |
| `aethertunnel_pool_refused_total` | counter | 因会话里停放连接已满而被拒绝的停放请求 |
| `aethertunnel_data_connections_unmatched_total` | counter | 数据连接的 `DataOpen` 指向了服务端没在等的会话或流：会话未知或已过期，或者要这条流的访客已经走了。这两类都在任何认证之前就能到达，此前只留一行日志。**连不上自己本地服务的客户端不算在内**——它在帧里自己说明了原因，等这条流的访客也会收到错误，那是正常结果而不是配不上的连接 |
| `aethertunnel_streams_active` | gauge | 当前打开的隧道流 |
| `aethertunnel_streams_total` | counter | 已结束的隧道流 |
| `aethertunnel_bytes_from_clients_total` | counter | 从客户端收到的字节 |
| `aethertunnel_bytes_to_clients_total` | counter | 发往客户端的字节 |
| `aethertunnel_udp_datagrams_total` | counter | `udp` 与 `sudp` 代理转发的 UDP 数据报（双向各计一次）。socks5 出口的数据报走它自己那条序列 |
| `aethertunnel_udp_sessions_active` | gauge | 当前跟踪的 UDP 访客会话（`udp` 代理按来源地址各算一个；`sudp` 的访客是一条独立的流，不进这个数） |
| `aethertunnel_udp_oversize_dropped_total` | counter | 超过 `server.udp_packet_size` 而被丢弃的 UDP 数据报 |
| `aethertunnel_udp_sessions_limit_dropped_total` | counter | 因该代理已跟踪到会话上限而被丢弃的数据报。一条隧道最多跟踪 4096 个来源地址（每个地址占一个协程和一条通往客户端流，而来源地址是访客可以伪造的）；上限之内的来源照常收发 |
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

UDP 数据报会话的字节在**会话释放时**计入两个字节计数器与按隧道的计数：`udp` 代理是
该来源地址安静 `server.read_timeout_seconds`（默认 120）秒之后，`sudp` 与 socks5 出口是那条
流结束时，前者同时从 `aethertunnel_udp_sessions_active` 消失；每转一个数据报就增加的是
`aethertunnel_udp_datagrams_total`（`udp` 与 `sudp`）或
`aethertunnel_socks5_udp_datagrams_total`（socks5 出口），都是双向各计一次。

一个 `udp` 代理同时跟踪的来源地址上限是 4096 个，超出的新来源被丢弃并计入
`aethertunnel_udp_sessions_limit_dropped_total`：每个被跟踪的地址都要占一个协程和一条发往
客户端的流，而 UDP 的来源地址是发送方可以伪造的，公共端口上的这个数字必须有界。上限之内的
来源地址不受影响，`aethertunnel_udp_sessions_active` 到了上限就会停在那里。

## `[log]`（两端）

本进程自己的日志写到哪、写到多细。这是 frp 的 `[log]` 段：文件与按天归档（`to`、
`max_days`）、级别（`level`）、时间戳与着色开关（`disable_timestamp`、`disable_print_color`）。

**级别怎么落到行上**：本项目把每一行标上 trace/debug/info/warn/error 中的一个，`level` 是
最低要写出的那一档。绝大多数行是 info；**出错、被拒绝、被丢弃、超时**这些「事情不顺利但进程
继续跑」的行是 warn（来源被拒、认证失败、限流、封禁、健康检查失败与摘除、插件/网关失败、
帧不合法、重连与退避、写到一半失败的告警）；**panic、审计/ledger/store 落盘失败、启动失败**
是 error。因此 `level = "warn"` 得到的是「只留坏消息」，而不是一片空白：写 `warn` 时
`listening on ...`、`connected as ...` 这类行不写，`authentication failed for ...` 照写。

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `to` | string | 空 | 日志追加到该文件；留空写标准错误（`stderr`），也就是交给 systemd/journald、容器运行时或监管进程采集。目录必须已存在：写不进去时进程带着这条错误退出，而不是把日志丢进虚空 |
| `max_days` | int | 0 | 保留多少天的轮转文件，范围 0–3650。**0 表示不轮转**：`to` 就是一个只增大的文件。正值表示：某天第一次写日志时，若该文件里没有当天的内容，就按它持有的日期改名为 `<to>.<YYYY-MM-DD>`，并删掉窗口之外的同名归档。小于 0 会被拒绝；正值用在没有 `to` 的配置里也会被拒绝——没有文件就没有可轮转的东西 |
| `level` | string | `info` | `trace` `debug` `info` `warn` `error` 之一（对应 frp 的 `log.level`，默认同为 info）。比它轻的行不写：`warn` 只留 warn 与 error，`trace` 全留。别的值在启动时报错并列出行 |
| `disable_print_color` | bool | false | 关掉按级别着色（对应 frp 的 `log.disablePrintColor`）。**只有终端**会被着色：`to` 指向文件时永不着色，标准错误不是终端时（被重定向、被 systemd 采集）也不着色，所以这个键在已经着色的场合才有意义 |
| `disable_timestamp` | bool | false | 去掉每行的日期与时间，只留消息本身（已经有采集程序打时间戳时用）。frp 当前版本没有这个键，它多出来的一项 |

`debug` 目前只有一处调用点（xtcp 打洞遇到数据报代理时那句"走不了直连"的说明），`trace` 还没有：
本项目把运行状态写在 info 上，这两档是给将来的诊断行留的位置。

`[log]` 与其他段一样在启动时读入，热重载不会改变级别或去向（改了这一节会提示"重启后生效"）；
过滤发生在每次写出的那一刻，所以一个只写标准错误的进程在重定向之后仍会遵守 `level`。

与 frp 的两处默认值不同，迁移时留意：frp 的 `to` 默认是 `"console"`（写到 stdout），本项目
留空表示 stderr——两个入口在有这个键之前就一直写 stderr，而 stderr 正是 systemd、容器运行时
采集的那个流；frp 的 `maxDays` 默认 3 天，本项目默认 `max_days = 0`（一个只增大的文件），
要从 frp 拿到同样的行为请显式写 `max_days = 3`。

轮转发生在**写出新一天第一行时**，不是定时器：整夜安静的服务进程不必为了轮转醒来，忙碌的
进程则把当天第一行写进新文件。判决依据是文件的修改时间，也就是唯一能在重启后保留下来、
说明"这个文件里是哪天的内容"的证据；跨多天停机后，归档名是文件真正持有的那天，而不是停机
前的最后一刻。目录里符合 `<to>.<日期>` 之外的文件（例如 `server.log.1`）不属于本包，
不会被删。

两个入口只在**真正开始运行**时按 `[log]` 打开文件：`--check`、`--identity`、`--discover`、
`--dht-key`、`--dht-lookup`、`--ledger-proof`、`--verify-ledger` 这些问答式命令仍写标准错误，
一次校验不会顺手创建或追加它所校验的日志文件。移动端绑定同样读 `[log]`：Android 上标准错误
已经被平台钩子接到 logcat，`to` 留空即保持这一行为，填路径则把日志留在应用自己的沙箱里。

## `[audit]`（服务端）

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | false | 是否写审计日志 |
| `path` | string | `aethertunnel-audit.jsonl` | 日志路径 |
| `max_bytes` | int | 33554432 | 超过该大小后轮转，旧文件保留为 `<path>.1`；写 0 与不写一样按 32 MiB 处理——轮转是审计日志不无限增长的方式，只有从代码里构造配置才能表达"不轮转" |
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
`event` 取值为 `control_accepted`、`control_rejected`、`handshake_failed`、`auth_failed`、`client_disconnected`、
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
| `path` | string | `aethertunnel-ledger.jsonl` | 账本文件，每行一条 JSON 记录。同一账本文件同时只允许一个在服务的实例：第二个实例启动时会拒绝并报"已由另一个实例写入"（文件上持有排他锁） |
| `signing_key_file` | string | `aethertunnel-ledger.key` | Ed25519 私钥种子（32 字节十六进制），首次使用时生成。**Unix 上以 0600 创建**；Windows 没有 POSIX 权限位，`0600` 只是 Go 的请求，实际保护来自文件所在目录继承的 ACL（用户配置目录默认只授予本人、SYSTEM 与 Administrators） |

每条记录包含序号、时间、客户端、代理、双向字节、上一条的哈希、本条哈希与签名。这里的**双向字节
只是服务端自己搬过的那部分**：`xtcp` 打洞成功后两端直接对话，那条会话的条目就是 0 字节（它没有
因此变成"免费额度"，而是根本没经过这里，详见 `docs/SECURITY.md` 第 9 节）。条目在**会话结束时**
追加，所以被强杀的进程不会留下这一条——这也是容器里那张账本有时是空的原因。
`GET /api/ledger` 发布公钥、链头、最近条目与按客户端汇总；面板的「账本」页渲染的就是这份响应。
按客户端汇总最多为 1024 个 `client_id` 各自保留一行（`client_id` 由客户端自选，服务端只限长度），
其后出现的客户端并入名为 `other` 的一行，因此这张表不随客户端数量增长，而字节总数仍然完整。
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
| `ttl_seconds` | int | 3600 | 两端 | 记录在 DHT 中的存活时间，不能短于 `announce_ttl_seconds`，也不得超过 86400；留空按 3600 补上，与 DHT 自身的默认一致 |
| `announce_ttl_seconds` | int | 90 | 两端 | 一条通告在被读取端采信的时长，最少 2 秒、最多 86400：该值由**发布方**写进记录，读取端以此判定记录新鲜度；读取端自己的这项配置不缩短它读到的记录。通告必须在失效前被重写，而间隔以整秒计 |
| `republish_seconds` | int | `announce_ttl_seconds / 3`，至少 1 秒 | 两端 | 重新发布已有通告的间隔，必须短于 `announce_ttl_seconds`，也不得超过 86400 |
| `lookup_timeout_seconds` | int | 5 | 两端 | 单次解析的时限。负数被拒绝，也不得超过 86400 |
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
服务端的签名公钥可以从 `--dht-key`、`GET /api/dht` 的 `signing_key` 字段或启动日志里读到；
面板的「目录」页把这份响应连同正在通告的名字一起显示出来（`--dht-key` 打印的必须是同一个值）。

## `[vpn]`（三层隧道）

| 键 | 类型 | 默认 | 端 | 说明 |
|---|---|---|---|---|
| `enabled` | bool | false | 两端 | 是否启用三层隧道 |
| `device` | string | 空 | 两端 | 网卡名。服务端是读写的网卡，客户端是要创建的网卡；留空时 Linux 由内核分配，Windows 叫 `AetherTunnel`，macOS 叫 `utun9` |
| `address` | string | 空 | 服务端 | 分配给客户端的 IPv4 子网（CIDR），服务端占用第一个可用地址。必填 |
| `mtu` | int | 0 | 两端 | 覆盖网卡 MTU（576–9000）；0 表示用网卡自身的值 |
| `require` | bool | false | 服务端 | 拒绝没有申请隧道地址的会话 |

三个平台各用各的设备：Linux 打开 `/dev/net/tun`；Windows 加载官方 `wintun.dll`（从 wintun.net
获取，放在可执行文件旁边或用环境变量 `WINTUN_DLL` 指向，**本程序不安装驱动**；创建适配器与
配置地址需要管理员权限）；macOS 打开 utun 控制套接字（打开不需要权限，配置地址需要 root）。
某个平台的设备打不开时**明确拒绝启动**并说明原因，不会静默降级。

地址分配与路由是两个独立动作：本程序只分配地址，**不会**改动主机路由表。
服务端需要自行加上指向该子网的路由，例如 `ip route add 10.7.0.0/24 dev tun0`。

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
| `pad_to` | int | 256（仅在写了 `enabled = true` 时；键不写才取这个默认） | 帧负载补齐到该值的整数倍；显式写 `0` 表示不补齐。不得超过帧上限 1048576 字节：补齐发生在写出之前，超过上限的帧会被发送端自己拒收（两端不必取同一个值，但都受同一个上限约束） |
| `jitter_millis` | int | 0 | 每次写入前插入 `[0, 该值)` 毫秒的随机延迟；不得超过 60000（60 秒）——延迟发生在每次写出之前，更大的值是停顿而不是伪装 |
| `disguise` | string | `none` | `none` 原样写出；`tls-record` 把每个写入包进 TLS 1.2 应用数据记录；`tls-session` 让每条连接做一次真实的 TLS 握手（服务端每连接现签一张匿名自签证书），会话本身就是真实加密。三种值两端都必须一致 |

`tls-record` 的补齐在加密之后进行，帧内保留真实长度前缀；接收端仅凭帧标志位去补齐，两端不需要配置一致。
伪装是最外层：`[transport] enable_tls` 打开时，TLS 会话骑在伪装里面，观察者先看到的是伪装。
`tls-record` 不做握手：它能骗过只看首字节的识别器，骗不过会建模 TLS 会话的识别器；
`tls-session` 有真握手，建模 TLS 会话的探测器看到的就是一个会话。`tls-session` 的半关闭由 TLS
的 `close_notify` 保留，需要对端半关闭的场合它同样可用。

## 环境变量

| 变量 | 覆盖 | 生效时机 |
|---|---|---|
| `AETHERTUNNEL_AUTH_TOKEN` | `[server] auth_token` 与 `[client] auth_token` | 校验之前，因此配置文件里可以不放密钥 |
| `AETHERTUNNEL_DASHBOARD_TOKEN` | `[dashboard] token` | 同上 |
| `AETHERTUNNEL_ENCRYPTION_PASSPHRASE` | `[encryption] passphrase` | 同上 |

覆盖发生时会在启动日志里列出被覆盖的变量名。

## 完整示例

见仓库根目录的 [`server.toml.example`](../server.toml.example) 与
[`client.toml.example`](../client.toml.example)——`--check` 会校验它们，
所以它们始终是可用的。Kubernetes 下的用法见
[`deploy/kubernetes/README.md`](../deploy/kubernetes/README.md)。
