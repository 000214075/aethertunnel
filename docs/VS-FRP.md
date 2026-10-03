# 与 frp 的对照 · Against frp

[fatedier/frp](https://github.com/fatedier/frp) 是这个领域最广为人知的工具，AetherTunnel
的配置词汇（`[[proxies]]`、`remote_port`、访客、`stcp`/`xtcp`、连接池）有意与它相似，
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
| PROXY protocol 传真实访客 IP | ✅ v1/v2 | ✅ v1 | `proxy_protocol = "v1"`；v2 未做，见下 |
| allow_ports（服务端端口白名单） | ✅ | ✅ | 注册时拒绝，审计留痕 |
| 健康检查（本地服务探活） | ✅ 失败自动摘除 | ✅ 失败自动摘除 | 摘除撤下公开端点，恢复后重新发布；旧服务端回退为拨号拒绝 |
| 子域名托管 | ✅ subdomain_host | ✅ `subdomain` + `[server].subdomain_host` | 客户端只声明一个 DNS 标签，服务端拼全名并按域名路由 |
| 经代理连服务器 | ✅ transport.dialServerProxy | ✅ `dial_via` | socks5/socks5h/http/https；隧道自身的 TLS 加在代理之上 |
| http/https 基本认证 | ✅ httpUser/httpPassword | ✅ `http_user`/`http_password` | 服务端在转发前检查，失败回 401 并带上挑战头 |
| tcpmux（HTTP CONNECT 多路复用） | ✅ | ✅ | 一个端口按 CONNECT 的主机名分流任意多条 TCP 隧道 |
| 热重载 | ✅ 管理 API / SIGHUP | ✅ SIGHUP | 重读配置并按名增删代理、重绑访客；会话级设置改动提示重启后生效 |
| 按代理压缩 | ✅ transport.useCompression | ✅ `use_compression` | snappy 流式压缩加在加密层外侧；服务器在 DataRequest 里回声确认，新旧混跑自动退化为不压缩 |
| https2http 插件 | ✅ | ✅ | 客户端用自己的证书终结访客的 TLS，明文转发给 local_port 的 HTTP 服务；可走专用 tcp 端口，也可叠加 `tls_passthrough` 骑在共享透传端口上 |
| https SNI 透传 | ✅ | ✅ | `tls_passthrough = true` + `[server].https_passthrough_port`：按 ClientHello 的 SNI 分流，访客的 TLS 会话原样中继到客户端，证书由客户端逐域名提供 |
| 客户端管理 API | ✅ webServer | ✅ `[client.admin]` | `/healthz` 免认证，`/api/status`、`/api/reload` 可选 basic auth |
| 服务端 httpPlugins webhook | ✅ | ✅ | `[[http_plugins]]`：login / newProxy / newUserConn 三个操作，JSON 信封与应答契约与 frp 的 webhook 兼容；newProxy 的应答可改写可发布字段；信封刻意不含令牌与私有代理的 secret |
| 代理 HTTP 头注入 | ✅ requestHeaders/responseHeaders | ✅ `request_headers`/`response_headers` | 服务端终结 HTTP 的路径按代理注入请求头与应答头 |
| vhost 按路径分流 locations | ✅ | ✅ `locations` | 一个主机名可被多个 http/https 代理按路径前缀拆分，最长前缀胜出，无 locations 的代理兜底；`tls_passthrough` 不解析路径 |
| http 代理 hostHeaderRewrite | ✅ | ✅ `host_header_rewrite` | 终结路径转发给本地服务的 Host 改写为指定值 |
| 客户端 metas | ✅ | ✅ `[client.metas]` | 随认证请求上报，服务端交给 webhook 并记入日志 |
| OIDC 认证 | ✅ auth.oidc | ✅ `[oidc]` | 服务端经发现文档取 JWKS 并验签（RS/PS/ES 系列）、查 iss/aud/exp；客户端以 client-credentials 取 token。差异：本项目的 `[oidc]` 是**加一把**凭据——静态令牌在配置了 `[oidc]` 的服务器上仍然有效；JWS 自实现（无 go-oidc 依赖） |
| 传输层 websocket | ✅ transport.protocol | ✅ `transport.protocol = "websocket"` | 到服务器的每条连接包进 RFC 6455 二进制帧；服务端同一端口按连接嗅探升级，无需配置；TLS 仍在外侧（wss 分层） |
| 客户端插件：socks5 | ✅ | ✅ | 访客对公共端口说 SOCKS5，客户端从自己的网络拨目标；`allow_targets` 必填（比 frp 多一层圈界），`plugin_user`/`plugin_password` 可选认证 |
| 客户端插件：https2https / tls2raw | ✅ | ✅ | 终结访客 TLS 后分别转本地 HTTPS（两腿加密）与本地 TCP 明文 |
| 客户端插件：static_file | ✅ | ✅ | 含 HTTP basic auth |
| 客户端插件：unix_domain_socket | ✅ | ✅ | |
| socks5 出口 | ✅ 客户端插件 | ✅ 服务端代理类型 + allow_targets | 语义不同，见下 |
| 面板 / 审计 / 指标 | ✅ | ✅ | 面板中英双语；审计 JSONL；Prometheus 指标 |
| 流量加密 | ✅ 可选 | ✅ 默认 | AetherTunnel 的隧道默认全量 AEAD |

## AetherTunnel 超出处 · Where AetherTunnel goes further

| 能力 | 说明 |
| --- | --- |
| 身份认证 | Ed25519 客户端身份 + 可选的后量子 ML-KEM 混合加密 |
| 访客零知识认证 | `auth_method = "nizk"`（Schnorr）与 `"snark"`（Groth16，992 约束电路）：证明知道 secret 而不发送它 |
| 流量伪装 | `disguise = "tls-session"`：每连接真实的 TLS 握手 + 现签匿名自签证书 |
| 去中心目录发现 | DHT：客户端只需代理名，无需知道服务器地址 |
| 三层 VPN | Linux/Windows(Wintun)/macOS(utun) 原生 tun；Android 上 App 的 VpnService 交 fd，模拟器上逐包验证 |
| WebRTC 数据通道 | `transport = "webrtc"`：私有代理流量走 DTLS DataChannel |
| 带宽账本 | Ed25519 签名哈希链，仅凭公钥离线核验，可导出单条证明 |
| 客户端即库 | `pkg/clientlib` 一个入口 + gomobile 绑定（AAR）+ 最小 Android App（CI 在模拟器上过包验证） |
| 多路数据报 | `multipath`：一条 udp/sudp 会话并行多条数据连接 |

## 有意不抄的地方 · Deliberate differences

- **https 的默认形态**：本项目 https 代理默认由服务端共享监听统一终结 TLS（一份证书
  服务多个域名）；需要"证书挂在客户端、按域名各归各家"的形态时开 `tls_passthrough`，
  行为与 frp 一致。
- **frp 的 OIDC 认证**：未实现；`auth_method` 的三种零知识/签名方案覆盖了 frp 用
  token+sk 覆盖的场景。
- **frp 的 kcp/quic 传输层**：未实现；本项目走 TCP 加自有加密帧。

## 功能对照清单 · Feature-by-feature audit

逐项对照 frp（克隆源码于提交 d20a232：`conf/frpc_full_example.toml`、
`conf/frps_full_example.toml`、`server/proxy/`、`client/`），每一行"已复现"都指向
本项目里承载它的文件。

### 代理类型 · Proxy types

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| tcp / udp 公共端口代理 | ✅ | `pkg/server/group.go`（bind、acceptLoop）、`pkg/clientlib/client.go`（serveStream） |
| http / https 虚拟主机 | ✅ | `pkg/server/vhost.go` |
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
| healthCheck 探活 | ✅ 且更进一步：失败注销公开端点 | `pkg/clientlib/health.go`、`pkg/clientlib/withdraw.go`、协议 24/25 |
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
| SIGHUP / 管理 API 热重载 | ✅ | `pkg/clientlib/reload.go`（SIGHUP）、`pkg/clientlib/admin.go`（`[client.admin]`：/healthz、/api/status、/api/reload） |
| stcp/sudp/xtcp visitor | ✅ | `pkg/clientlib/visitor.go`、`pkg/config` VisitorConfig |
| token 认证 | ✅ | `pkg/crypto` EqualTokens、control 握手 |
| 客户端即库 + 移动绑定 | ✅（frp 没有的） | `pkg/clientlib` Run、`pkg/mobile`（gomobile AAR） |

### 服务端 · Server

| frp 能力 | 本项目状态 | 落点 |
|---|---|---|
| 端口白名单 allowPorts | ✅ | `pkg/config` PortSet、`pkg/server` policies |
| 虚拟主机端口 http/https/tcpmux | ✅ | `pkg/server/vhost.go`、`pkg/server/tcpmux.go` |
| 仪表盘/指标 | ✅ | `pkg/server/metrics*.go`、面板 API、Android App |
| 日志/审计 | ✅ 且更进一步 | `pkg/server/audit*.go`（结构化审计事件） |
| 管理 API 的 reload | ✅ | `pkg/clientlib/reload.go`（SIGHUP 同路径）、`pkg/clientlib/admin.go`（客户端 `POST /api/reload`） |
| httpPlugins webhook | ✅（信封更省：不带令牌与 secret） | `pkg/server/httpplugin.go`（login 在注册会话前、newProxy 在注册入口且可改写、newUserConn 在每个访客连接） |

### 尚未复现 · Not reproduced yet

- （认证差距已闭合：OIDC 见对照表；`auth_method` 的零知识/签名方案仍在。）
- frp 的 kcp/quic 传输层（本项目走 TCP + 自有加密帧；websocket 形态已实现）。
- frp 的管理端 Web UI 形态（本项目面板另有实现）。
- frp xtcp 访客的 fallback_to 回退（本项目的 xtcp 打洞失败本就走服务器中继）。

## 一句话 · In one sentence

frp 做到的转发与治理能力，AetherTunnel 基本都有；在此之上它把默认姿态从"能通"换成
"默认加密、默认可审计、访客可以不交秘密、客户端可以进手机"。
