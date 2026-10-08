# 与 frp 的对照 · Against frp

[fatedier/frp](https://github.com/fatedier/frp) 是这个领域最广为人知的工具，AetherTunnel
的配置词汇（`[[proxies]]`、`remote_port`、访客、`stcp`/`xtcp`、按代理限速）有意与它相似，
让从 frp 迁移过来的使用者能对上号。本页如实对照两者的能力：相同处、超出处，以及
AetherTunnel 有意不抄的地方。每一项"有"都对应仓库里可执行的检查，不是宣传词。

[fatedier/frp](https://github.com/fatedier/frp) is the best-known tool in this space.
AetherTunnel's configuration vocabulary is deliberately close to frp's so that moving
between them is easy. This page compares the two honestly: where they match, where
AetherTunnel goes further, and where it deliberately differs. Every "yes" points at
executable checks in this repository, not at marketing.

## frp 有、AetherTunnel 也有 · In both

| 能力 | frp | AetherTunnel | 备注 |
| --- | --- | --- | --- |
| tcp/udp 端口转发 | ✅ | ✅ | 功能套件逐次验证 |
| http/https 虚拟主机（按域名路由，共享端口） | ✅ | ✅ | `domains` 支持通配；vhost 检查 |
| stcp/sudp 私有代理 + 访客 | ✅ | ✅ | 访客认证见下 |
| xtcp P2P 打洞 | ✅ | ✅ | 可靠 UDP 承载，见 `pkg/reliable` |
| 跨客户端负载均衡（同名 group） | ✅ | ✅ | `group` + `[server].load_balance` |
| 带宽限速 | ✅ 每代理 | ✅ 每代理 | `bandwidth = "5KB"`，限速在客户端数据路径 |
| 端口段映射（remote_port 区间展开） | ✅ | ✅ | `remote_ports = "6000-6002"` |
| PROXY protocol 传真实访客 IP | ✅ v1/v2 | ✅ v1/v2 | `proxy_protocol = "v1"` 是文本行，`"v2"` 是二进制头，功能套件两种都验 |
| allow_ports（服务端端口白名单） | ✅ | ✅ | 注册时拒绝，审计留痕 |
| 健康检查（本地服务探活） | ✅ 失败自动摘除 | ✅ 失败自动摘除 | 摘除撤下公开端点，恢复后重新发布；旧服务端回退为拨号拒绝 |
| 子域名托管 | ✅ subdomain_host | ✅ `subdomain` + `[server].subdomain_host` | 客户端只声明一个 DNS 标签，服务端拼全名并按域名路由 |
| 经代理连服务器 | ✅ transport.dialServerProxy | ✅ `dial_via` | socks5/socks5h/http/https；隧道自身的 TLS 加在代理之上 |
| http/https 基本认证 | ✅ httpUser/httpPassword | ✅ `http_user`/`http_password` | 服务端在转发前检查，失败回 401 并带上挑战头 |
| tcpmux（HTTP CONNECT 多路复用） | ✅ | ✅ | 一个端口按 CONNECT 的主机名分流任意多条 TCP 隧道 |
| 热重载 | ✅ 客户端管理 API（`GET /api/reload`），无信号触发 | ✅ SIGHUP + 客户端管理 API（`POST /api/reload`） | 重读配置并按名增删代理、重绑访客、重新按 `start` 筛选；改不动的会话级小节明确提示"重启后生效"。本项目多一条信号触发：容器里只剩信号可用时也能改配置。两侧的服务端都没有重载 |
| 按代理压缩 | ✅ transport.useCompression | ✅ `use_compression` | snappy 流式压缩加在加密层外侧；服务器在 DataRequest 里回声确认，新旧混跑自动退化为不压缩 |
| https2http 插件 | ✅ | ✅ | 客户端用自己的证书终结访客的 TLS，明文转发给 local_port 的 HTTP 服务；可走专用 tcp 端口，也可叠加 `tls_passthrough` 骑在共享透传端口上 |
| https SNI 透传 | ✅ | ✅ | `tls_passthrough = true` + `[server].https_passthrough_port`：按 ClientHello 的 SNI 分流，访客的 TLS 会话原样中继到客户端，证书由客户端逐域名提供 |
| 客户端管理 API | ✅ webServer | ✅ `[client.admin]` | `/healthz` 免认证，`/api/status`、`/api/reload` 可选 basic auth |
| 服务端 httpPlugins webhook | ✅ | ✅ | `[[http_plugins]]`：login / newProxy / newUserConn 三个操作，JSON 信封与应答契约与 frp 的 webhook 兼容；newProxy 的应答可改写可发布字段；信封刻意不含令牌与私有代理的 secret |
| 代理 HTTP 头注入 | ✅ requestHeaders/responseHeaders | ✅ `request_headers`/`response_headers` | 服务端终结 HTTP 的路径按代理注入请求头与应答头 |
| vhost 按路径分流 locations | ✅ | ✅ `locations` | 一个主机名可被多个 http/https 代理按路径前缀拆分，最长前缀胜出，无 locations 的代理兜底；`tls_passthrough` 不解析路径 |
| http 代理 hostHeaderRewrite | ✅ | ✅ `host_header_rewrite` | 终结路径转发给本地服务的 Host 改写为指定值 |
| 按认证用户分流 routeByHTTPUser | ✅ | ✅ `route_by_http_user` | vhost 第三路由维度：同域名同前缀下按访客呈现的 basic-auth 用户名选路，命名路由优先、未声明用户的代理兜底；路由先于代理自身的凭据核对 |
| 客户端 metas | ✅ | ✅ `[client.metas]` | 随认证请求上报，服务端交给 webhook 并记入日志 |
| tcpmuxPassthrough | ✅ | ✅ `server.tcpmux_passthrough` | 开启后 CONNECT 不由服务器应答，原样交给隧道后端；路由仍按 authority |
| 逐条标签 annotations | ✅ | ✅ `[[proxies]] annotations` | 自由键值，只在 dashboard 展示 |
| 逐条开关 enabled | ✅ | ✅ `enabled`（代理与访客各一） | 条目留在文件里但不启动/不监听；与 `start` 叠加，两者都满足才运行 |
| `[log]` 写到文件 | ✅ `log.to` + `maxDays`（默认 3）+ `level` + `disablePrintColor` | ✅ `to`、`max_days`（默认 0）、`level`（默认 info）、`disable_print_color`、`disable_timestamp` | 同为按天归档 + 保留窗口（frp 是 `RotateFileModeDaily` + `MaxDays`）。差异：frp 的 `to` 默认 `"console"`（stdout），本项目留空写 stderr——沿用两个入口既有行为，也是监管进程采集的那个流；frp 的 `maxDays` 默认 3，本项目默认 0（一个只增大的文件），从 frp 迁过来要显式写 `max_days = 3`；`level` 与 `disablePrintColor` 已复现：本项目把**出错、被拒绝、被丢弃、超时**的行记在 warn、把 panic 与落盘失败记在 error、其余记在 info，所以 `level = "warn"` 是"只留坏消息"而不是空白；颜色只加在终端上（写文件时永不着色）。`disable_timestamp` 是本项目多出来的键（当前 frp 没有） |
| SSH 隧道网关 | ✅ sshTunnelGateway | ✅ `[server.ssh_tunnel_gateway]` | 一台只肯开 ssh 的机器用 `ssh -R` 就能发布服务，命令行旗标与 frp 相同（`--proxy_name`/`--remote_port`/`--custom_domain(s)`/`--subdomain`/`--secret_key`/`--auth_method`/`--group`/`--token`/`--allow_users`）。发布走与客户端 `[[proxies]]` 同一条路：`allow_ports`、按名字的策略、`max_ports_per_client`、审计、面板、账本都适用。差异：主机密钥可自动生成并留存（`auto_gen_key_file`），`authorized_keys_file` 与共享 `password` 可单用或同用（没有文件而配了口令时只认口令，密钥一律拒绝；都没有时任何密钥都收下）；`--bandwidth` 只收下不改变行为 |
| 客户端 store（运行期条目） | ✅ `client.store` | ✅ `[store] path` + 管理 API | 不在配置文件里写、通过管理 API 增删的代理与访客，持久化到 JSON 文件并在重启后生效；同名运行期条目压过配置文件里的定义。`client_id` 已复现为随认证上报的稳定名字（服务端日志、审计、面板与账本都用它称呼客户端）；差异只在 frp 的 store 还保存 mux/quic 会话的连续性状态，而本项目的数据连接不复用、会话从零建，不需要那份状态 |
| 逐代理 useEncryption | ✅ transport.useEncryption（逐条） | ✅ `[[proxies]] use_encryption` | 只给这一条代理的中继字节流加一层 AEAD，密钥由 `auth_token` 与代理名经 HKDF 导出、两端各自算出，所以整会话不开 `[encryption]` 时也能单独保护一条代理。差异：本项目把它叠在会话密钥之内（`[encryption] enabled = true` 时给警告），且只对字节流生效——数据报路径不受影响 |
| transport.wireProtocol | ✅ v1/v2 | ✅ `wire_protocol`（`v2` 或留空） | 本项目只有一套线协议、版本在握手内协商，`v1` 被明确拒绝而不是静默接受 |
| transport.tcpMux | ✅ 默认 true | ✅ `tcp_mux`（写 `true` 时警告） | 本项目的数据连接从不复用（一条流一条连接），正是 frp `tcpMux = false` 的形状，所以两种取值下数据路径相同；只有显式写 `true` 才提示 |
| transport.additionalScopes | ✅ HeartBeats/NewWorkConns | ✅ `additional_scopes` | 本项目的心跳帧与 data-open 本就不带凭据，两个名字照收并给说明，别的名字报错 |
| 按名启动 start | ✅ | ✅ `[client] start` | 只启动列出的代理与访客，其余保持定义；空数组即全部。重载重新筛选，不再列出的隧道注销 |
| UDP 数据报上限 udpPacketSize | ✅（默认 1500，超限截断） | ✅ `udp_packet_size`（默认 0=最大载荷，超限丢弃并计数） | 服务端与客户端各一个键；本项目的实现丢弃而不是截断，本地服务不会收到半条数据报 |
| 发布端口绑定地址 proxyBindAddr | ✅（默认同 bindAddr） | ✅ `proxy_bind_addr`（默认同 `bind_addr`） | 已发布端口（http/https/tcpmux/透传/远程端口）绑到它，控制端口不动；监听冲突检测按这个地址判断 |
| TCP keep-alive | ✅ transport.tcpKeepalive / dialServerKeepalive | ✅ `tcp_keepalive_seconds`（服务端与客户端各一个） | 内核级探测间隔；0 为 Go 默认 15 秒，frp 默认 7200。与应用层心跳互补 |
| 工作连接等待上限 userConnTimeout | ✅（默认 10s） | ✅ `user_conn_timeout_seconds`（默认 0=沿用拨号超时） | 已发布连接等待客户端交回数据连接的上限，与「客户端拨本地服务」的 `dial_timeout_seconds` 分开 |
| 客户端 DNS 服务器 dnsServer | ✅ | ✅ `dns_server`（默认空=系统解析器） | 只用于解析 `server_addr` 的主机名；要求 `host:port` |
| 令牌来源 tokenSource（file） | ✅ file / exec | ✅ `auth_token_file`（两端各一个；另有环境变量 `AETHERTUNNEL_AUTH_TOKEN`） | 优先级：环境变量 > 令牌文件 > 配置文件里的 `auth_token`；文件读不到或为空是硬错误 |
| 客户端源地址 connectServerLocalIP | ✅ | ✅ `connect_server_local_ip` | 连服务器时绑定的本机源地址；必须是 IP 字面量 |
| 私有代理身份名单 allowUsers | ✅（空=仅同 user） | ✅ `[client] user` + `[[proxies]] allow_users`（空=仅发布者身份） | 语义与 frp 一致：密钥之外再按身份限定谁能访问私有代理。差别是本项目在**密钥证明之后**才核对身份，frp 先核对，因而 frp 会在证明密钥前泄露名单 |
| 访客回退 fallbackTo / fallbackTimeoutMs | ✅（frp 用于 xtcp） | ✅ `fallback_to` / `fallback_timeout_ms`（用于字节流访客） | 隧道打不开时把调用方直连到该地址；本项目对 stcp/xtcp 都适用，数据报访客不使用 |
| vhost 响应头超时 vhostHTTPTimeout | ✅（默认 60） | ✅ `vhost_http_timeout`（默认 0，沿用拨号超时） | 终结路径等待本地服务响应头的上限，超时回 502；机制同为反代 Transport 的 ResponseHeaderTimeout |
| 首登失败退出 loginFailExit | ✅（默认 true） | ✅ `login_fail_exit`（默认 false，保持旧的重试行为） | 只看第一次登录：被拒即退出并写明原因（注册被拒不算——端口名额、名字或端口冲突只记日志并按退避重试）；登录成功过的会话掉线照常重连 |
| 心跳 heartbeatInterval / heartbeatTimeout | ✅ 客户端 30 秒发一次、90 秒内没有应答即断（可设负数关闭） | ✅ `heartbeat_seconds`（默认 30，服务端决定、客户端跟随；客户端上这个键只是服务端没给值时的兜底） | 行为等价：本项目服务端按**三个间隔**（默认 30×3 = 90 秒）没有收到心跳就断开会话，正是 frp 的默认 timeout；客户端的值不会覆盖服务端的，两者不一致时客户端日志会说明这个键对本会话无效 |
| 指标端点 enablePrometheus | ✅ 服务端 `enablePrometheus`，在 **webServer 地址**上导出 `/metrics`，默认 false | ✅ `[metrics] enabled`（默认 false），导出在**面板监听器**上 | 机制相同：显式开启才暴露、默认都不暴露、都跟在服务端的管理监听器后面；差异都在开关之外——本项目可以把它单独放到另一个地址上（`[metrics] bind_addr`/`port`，frp 没有第二个地址），并且可以用 `[metrics] token` 单独要求一个 scrape 令牌（frp 只有 webServer 的 user/password 管着整个监听器） |
| 工作连接池 poolCount | ✅ `transport.poolCount` | ✅ `[client] pool_count`（0–64，默认 0） | 客户端预先在服务端停放若干数据连接，访客到达时服务器直接把手边这条交出去，省掉一次拨号（开 TLS 时就是一次握手）。差异：frp 的池是全局队列，停放连接被用过就丢弃再补；本项目按会话停放、断开即释放，一条停放连接只承载一条流，并且客户端**在同一条连接上回一条 data-open**——因此停放连接若已被客户端丢掉，服务器在用它之前就会发现，改走控制连接要一条新连接，访客不会因为一条死连接而断流 |
| 错误明细开关 detailedErrorsToClient | ✅（默认 true） | ✅ `detailed_errors_to_client`（默认 true） | 措辞依赖服务端配置或运行时状态的拒绝（注册失败、身份校验、隧道请求、首帧不合法）在关闭后只回一句概述；服务端日志与审计始终留全。frp 只把它用在注册与 ping 的应答上，本项目多覆盖认证阶段的拒绝 |
| OIDC 认证 | ✅ auth.oidc | ✅ `[oidc]` | 服务端经发现文档取 JWKS 并验签（RS/PS/ES 系列）、查 iss/aud/exp；客户端以 client-credentials 取 token。差异：本项目的 `[oidc]` 是**加一把**凭据——静态令牌在配置了 `[oidc]` 的服务器上仍然有效；JWS 自实现（无 go-oidc 依赖） |
| 传输层 websocket | ✅ transport.protocol | ✅ `transport.protocol = "websocket"` | 到服务器的每条连接包进 RFC 6455 二进制帧；服务端同一端口按连接嗅探升级，无需配置；TLS 仍在外侧（wss 分层）；客户端启动日志写明当前形态（普通流不写），配置是否真的生效一眼可见 |
| 客户端插件：socks5 | ✅ | ✅ | 访客对公共端口说 SOCKS5，客户端从自己的网络拨目标；`allow_targets` 必填（比 frp 多一层圈界），`plugin_user`/`plugin_password` 可选认证 |
| 客户端插件：https2https / tls2raw | ✅ | ✅ | 终结访客 TLS 后分别转本地 HTTPS（两腿加密）与本地 TCP 明文 |
| 客户端插件：static_file | ✅ | ✅ | 含 HTTP basic auth |
| 客户端插件：unix_domain_socket | ✅ | ✅ | |
| socks5 出口 | ✅ 客户端插件 | ✅ 服务端代理类型 + allow_targets | 语义不同，见下 |
| 面板 / 审计 / 指标 | ✅ | ✅ | 面板中英双语；审计 JSONL；Prometheus 指标 |
| 流量加密 | ✅ 可选 | ✅ 可选（默认关闭） | 设 `[encryption] enabled = true` 或按代理 `use_encryption` 后，控制消息与隧道字节都走 AEAD，见 `docs/CONFIGURATION.md` |

## AetherTunnel 超出处 · Where AetherTunnel goes further

| 能力 | 说明 |
| --- | --- |
| 身份认证 | Ed25519 客户端身份 + 可选的后量子 ML-KEM 混合加密 |
| 访客零知识认证 | `auth_method = "nizk"`（Schnorr）与 `"snark"`（Groth16，992 约束电路）：证明知道 secret 而不发送它 |
| 流量伪装 | `disguise = "tls-session"`：每连接真实的 TLS 握手 + 现签匿名自签证书 |
| 去中心目录发现 | DHT：客户端只需代理名，无需知道服务器地址 |
| 三层 VPN | Linux/Windows(Wintun)/macOS(utun) 原生 tun；Android 上 App 的 VpnService 交 fd，模拟器上逐包验证 |
| WebRTC 数据通道 | `transport = "webrtc"`：私有代理流量走 DTLS DataChannel，信令走控制连接；服务端记 `data channel established`，客户端记 `direct true` |
| 带宽账本 | Ed25519 签名哈希链，仅凭公钥离线核验，可导出单条证明 |
| 客户端即库 | `pkg/clientlib` 一个入口 + gomobile 绑定（AAR）+ 最小 Android App（模拟器上过包验证） |
| 多路数据报 | `multipath`：一条 udp/sudp 会话并行多条数据连接 |
| 信号触发的热重载 | SIGHUP 或客户端管理 API 的 `POST /api/reload` 重读配置文件：代理与访客上线/下线、`start` 重新筛选，改不动的小节会明确提示"重启后生效"。frp 的 frpc 也能重载，但只能通过它的管理 API（`GET /api/reload`）触发 |

## 有意不抄的地方 · Deliberate differences

- **https 的默认形态**：本项目 https 代理默认由服务端共享监听统一终结 TLS（一份证书
  服务多个域名）；需要"证书挂在客户端、按域名各归各家"的形态时开 `tls_passthrough`，
  行为与 frp 一致。
- **frp 的 kcp/quic 传输层**：未实现；本项目走 TCP 加自有加密帧。

## 功能对照清单 · Feature-by-feature audit

逐项对照 frp（克隆源码于提交 d20a232：`conf/frpc_full_example.toml`、
`conf/frps_full_example.toml`、`server/proxy/`、`client/`），每一行"已复现"都指向
本项目里承载它的文件。此后新增的行（`[log]`、`pool_count`、指标端点的开关）是再取一份
upstream 源码逐个核对的，取自 `pkg/config/v1/`、`pkg/util/log/` 与 `server/`。

### 代理类型 · Proxy types

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| tcp / udp 公共端口代理 | ✅ | `pkg/server/group.go`（bind、acceptLoop）、`pkg/clientlib/client.go`（serveStream） |
| http / https 虚拟主机 | ✅ | `pkg/server/vhost.go` |
| vhostHTTPTimeout 响应头超时 | ✅ `vhost_http_timeout` | `pkg/server/group.go` responseHeaderTimeout |
| udpPacketSize 数据报上限 | ✅ `udp_packet_size` | `pkg/net/dgram.go` DatagramPump.MaxDatagram（服务端与客户端的 udp/sudp 路径共用） |
| stcp / sudp 私有代理 | ✅ | `pkg/config` 私有类型 + `pkg/server/visitor.go` |
| xtcp 打洞 | ✅ | `pkg/server/p2p*.go`、`pkg/clientlib/visitor.go`（openVisitorPath） |
| socks5 出口代理 | ✅（形态不同：入口在服务端） | `pkg/socks`、`pkg/server/group.go`（serveVisit） |
| tcpmux HTTP CONNECT 多路复用 | ✅ | `pkg/server/tcpmux.go` |
| https SNI 透传 | ✅ | `pkg/server/sniset.go`（嗅探 ClientHello 的 SNI 路由，原始字节经 helloRecorder 重放后原样中继） |

### 代理治理 · Proxy governance

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| transport.bandwidthLimit | ✅ | `pkg/config` ParseBandwidth、`pkg/clientlib/ratelimit.go` |
| transport.useCompression | ✅ | `pkg/net/compress.go`（snappy）、DataRequest 回声协商 |
| transport.proxyURL | ✅ `dial_via` | `pkg/clientlib/dialvia.go` |
| transport.proxyProtocolVersion v1/v2 | ✅ | `pkg/server/proxyproto.go` |
| remotePort / 端口段展开 | ✅ | `pkg/config` ExpandRemotePorts |
| loadBalancer.group/strategy | ✅ | `pkg/server/group.go`（pick/pickExcluding + 延迟与 bandit 策略） |
| healthCheck 探活 | ✅ 且更进一步：失败注销公开端点 | `pkg/clientlib/health.go`、`pkg/clientlib/withdraw.go`、协议 24/25；`healthCheck.httpHeaders` 也已复现为 `http_headers`（探活请求带的头）。判定规则差异：本项目要求 http 探活得到 **2xx**（frp 只认 200，重定向两边都跟随），`max_failed` 默认 3（frp 默认 1） |
| httpUser/httpPassword | ✅ | `pkg/server/group.go` serveHTTP |
| subdomain / customDomains | ✅ | `pkg/server/vhost.go`、`pkg/server/server.go` 注册处理 |
| plugin static_file / unix_domain_socket | ✅ | `pkg/clientlib/plugins.go` |
| plugin https2http | ✅（专用 tcp 端口形态） | `pkg/clientlib/plugins.go` serveHTTPS2HTTP |
| plugin http_proxy | ✅（且多一层 allow_targets 圈定可拨范围） | `pkg/clientlib/plugins.go` httpProxyHandler |
| plugin socks5 | ✅（且 allow_targets 必填圈界） | `pkg/clientlib/plugins.go` serveSocks5、`pkg/socks/server.go` ServeConn |
| plugin https2https / tls2raw | ✅ | `pkg/clientlib/plugins.go` serveHTTPS2HTTPS、serveHTTPS2HTTP |
| transport.protocol websocket | ✅ | `pkg/net/websocket.go`（RFC 6455 二进制帧）、服务端同一端口嗅探升级 |
| 自定义 404 页 custom404Page | ✅ `custom_404_page` | `pkg/server/vhost.go` serveNotFound |
| plugin http2https | ✅ | `pkg/clientlib/plugins.go` http2HTTPSHandler |
| maxPortsPerClient | ✅ | `pkg/config` MaxPortsPerClient、`pkg/server/tunnel.go` Register |
| includes 配置拆分 | ✅ | `pkg/config/config.go` mergeIncludes |

### 客户端 · Client

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| 重连退避、心跳、空闲断开 | ✅ | `pkg/clientlib/client.go`（run/jitter、idle_timeout） |
| dialServerKeepalive TCP keep-alive | ✅ `tcp_keepalive_seconds` | `pkg/clientlib/client.go` dialer()；服务端侧同一键用于监听器 |
| 客户端 DNS 服务器 dnsServer | ✅ `dns_server` | `pkg/clientlib/client.go` dialer()（net.Dialer.Resolver） |
| 令牌文件 tokenSource | ✅ `auth_token_file` | `pkg/config` applyTokenFiles()（解码阶段读取，热重载重新读取） |
| connectServerLocalIP 源地址 | ✅ `connect_server_local_ip` | `pkg/clientlib/client.go` dialer()（net.Dialer.LocalAddr） |
| fallbackTo 访客回退 | ✅ `fallback_to` | `pkg/clientlib/visitor.go` serveVisitorFallback() |
| allowUsers 身份名单 | ✅ `user` + `allow_users` | `pkg/server/group.go` visitorIdentityAllow()（`pkg/server/visitor.go` 在密钥证明后调用） |
| 首登失败退出 loginFailExit | ✅ `login_fail_exit` | `pkg/clientlib/client.go`（sessionEstablished、giveUp） |
| enabled 逐条开关 | ✅ `enabled` | `pkg/config` IsEnabled()、`pkg/clientlib/reload.go` selectByStart() |
| 按名启动 start | ✅ `[client] start` | `pkg/clientlib/reload.go` selectByStart（启动与热重载同一条筛选路径） |
| SIGHUP / 管理 API 热重载 | ✅ | `pkg/clientlib/reload.go`（SIGHUP）、`pkg/clientlib/admin.go`（`[client.admin]`：/healthz、/api/status、/api/reload） |
| stcp/sudp/xtcp visitor | ✅ | `pkg/clientlib/visitor.go`、`pkg/config` VisitorConfig |
| token 认证 | ✅ | `pkg/crypto` EqualTokens、control 握手 |
| 客户端即库 + 移动绑定 | ✅（frp 没有的） | `pkg/clientlib` Run、`pkg/mobile`（gomobile AAR） |

### 服务端 · Server

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| 端口白名单 allowPorts | ✅ | `pkg/config` PortSet、`pkg/server` policies |
| 虚拟主机端口 http/https/tcpmux | ✅ | `pkg/server/vhost.go`、`pkg/server/tcpmux.go` |
| userConnTimeout 工作连接等待 | ✅ `user_conn_timeout_seconds` | `pkg/server/tunnel.go` streamWait()（Tunnel.userConnTimeout） |
| proxyBindAddr 发布端口绑定地址 | ✅ `proxy_bind_addr` | `pkg/config` ProxyHost()、`pkg/server/group.go` 与 `tcpmux.go`/`sniset.go` |
| 仪表盘/指标 | ✅ | `pkg/server/metrics*.go`、面板 API、Android App |
| 日志/审计 | ✅ 且更进一步 | `pkg/server/audit*.go`（结构化审计事件） |
| 管理 API 的 reload | ✅ | `pkg/clientlib/reload.go`（SIGHUP 同路径）、`pkg/clientlib/admin.go`（客户端 `POST /api/reload`） |
| httpPlugins webhook | ✅（信封更省：不带令牌与 secret） | `pkg/server/httpplugin.go`（login 在注册会话前、newProxy 在注册入口且可改写、newUserConn 在每个访客连接） |
| featureGates 特性开关 | ✅（方向相反：这里默认全开，门是关闭开关） | `pkg/config` 的 `feature_gates` 校验、`pkg/server/visitor.go` 的运行期拒绝；可关 `WebRTC` 与 `Snark`，frp 的 `VirtualNet` 门名接受但不起作用 |

### 尚未复现 · Not reproduced yet

- （认证差距已闭合：OIDC 见对照表，`auth_method` 的 nizk/snark 已接上运行时。）
- （`client_id` 已闭合：它是随认证上报的稳定名字，服务端日志、审计、面板与账本都用它称呼
  客户端，见对照表的 store 一行。剩下的是 frp 用 store 保存的 mux/quic 会话连续性状态：
  本项目的数据连接不复用、会话从零建，识别客户端另有可选的 Ed25519 身份（`[identity]`），
  不需要那份状态。）
- frp 的 kcp/quic 传输层（本项目走 TCP + 自有加密帧；websocket 形态已实现）。
- frp 的管理端 Web UI 形态（本项目面板另有实现）。
- **打洞的分析数据（`nat_hole_stun_server`、`nathole_analysis_data_reserve_hours`）**：
  frp 向外部 STUN 服务器询问自己的公网地址，分析 NAT 行为、预测端口，并把分析结果保留
  N 小时以提高后续打洞成功率。本项目只让会合服务器记录双方数据报的实际来源地址，不依赖
  第三方 STUN、不保留分析数据——少一个外部依赖，代价是同一 NAT 映射规律无法跨会话复利。
- **`virtual_net`**：frp 的虚拟网络功能未复现。
- ~~**`feature_gates`**~~：已复现，方向相反——frp 用它**启用**分级特性，本项目的能力默认全开，它用来**关闭**（`WebRTC`、`Snark`；frp 的 `VirtualNet` 门名接受但不起作用），见对照表。

## 从 frp 迁移 · Migrating from frp

frp 的键名是驼峰式（`bindPort`、`localIP`、`customDomains`），旧版本的客户端设置还统一放在
`[common]` 段里，而本项目的键名是下划线式、客户端设置放在 `[client]`。别人粘过来的
`frpc.toml`/`frps.toml` 会以未知键的形式报出来——加载器认得 frp 的名字，因此**除了列出未知键，
还会写出每一项该改成什么**，frp 有而本项目没有的（`kcpBindPort`、`quicBindPort`、`virtualNet`）
也会明说：

```
config frpc.toml contains 11 key(s) this version does not understand: common, common.serverAddr,
common.serverPort, common.token, …; frp names some of these differently: common → [client] or
[server], whichever holds the setting, serverAddr → client.server_addr, serverPort →
client.server_addr (host:port), token → auth_token, loginFailExit → client.login_fail_exit, …
```

常用对照（加载器认得的完整集合在 `pkg/config/config.go` 的 `frpKeyEquivalents`）：

| frp（含旧版写法） | 本项目 |
|---|---|
| `[common]` | `[client]`（服务端设置放 `[server]`） |
| `serverAddr` / `server_addr`、`serverPort` | `client.server_addr = "host:port"` |
| `token` / `authentication_token` / `auth.token` | `auth_token`（两端各一） |
| `bindAddr`/`bind_addr`、`bindPort`/`bind_port` | `[server]` 的 `bind_addr`、`bind_port` |
| `vhostHTTPPort`、`vhostHTTPSPort` | `[server] http_port`、`https_port` |
| `subDomainHost` / `subdomain_host` | `[server] subdomain_host` |
| `allowPorts`、`maxPortsPerClient`、`userConnTimeout` | `[server] allow_ports`、`max_ports_per_client`、`user_conn_timeout_seconds` |
| `transport.tls.enable` / `tls_enable` | `[transport] enable_tls` |
| `transport.tls.certFile`/`keyFile`/`trustedCaFile` | `[transport] cert_file`/`key_file`/`ca_file` |
| `transport.poolCount` / `poolCount` / `pool_count` | `[client] pool_count` |
| `transport.heartbeatInterval` / `heartbeatInterval` | `[client] heartbeat_seconds`（服务端用 `[server] heartbeat_seconds`） |
| `transport.tcpMux` / `tcp_mux` | `[transport] tcp_mux` |
| `log.to`、`log.level`、`log.maxDays` | `[log] to`、`level`、`max_days` |
| `webServer.addr`/`port`/`password`（客户端为管理 API） | `[client.admin] bind_addr`/`port`/`password` |
| `webServer.addr`/`port`/`password`（服务端为面板） | `[dashboard] bind_addr`/`port`/`token` |
| `[[proxies]]` 的 `localIP`/`localPort`/`remotePort`/`customDomains`/`secretKey` | `local_ip`/`local_port`/`remote_port`/`domains`/`secret_key` |
| `[[proxies]]` 的 `transport.useEncryption`/`useCompression` | `use_encryption`/`use_compression` |
| `kcpBindPort`/`quicBindPort`、`virtualNet` | 未复现（消息里明说，见上） |

## 一句话 · In one sentence

frp 做到的转发与治理能力，AetherTunnel 基本都有。两者的默认值其实很接近：加密、审计在两边都要
显式打开（本仓库的示例配置里加密与审计同样是关的），面板与指标同样是显式开关。不同在于每一项都配齐了
能真正用起来的那一半，并且都在 `scripts/` 的运行脚本里有独立检查：加密可加抗量子密钥协商、审计有
丢失计数与自动重开、访客认证可以既不交秘密（Schnorr 证明）也不交零知识证明之外的东西（zk-SNARK）、
客户端能进手机。
