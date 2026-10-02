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

- **frp 的 use_compression**：按代理压缩需要协议协商，混跑新旧两端时语义复杂；
  AetherTunnel 的隧道默认全量加密，压缩的收益场景（明文文本）留给后续带版本协商的实现。
- **frp 的 tcpmux / http2https 等插件型入口**：尚未实现；`static_file` 与
  `unix_domain_socket` 是第一批落地的插件。
- **frp 的 use_compression 之外的热重载（SIGHUP）**：尚未实现；改配置仍需重启客户端。
  进程内已具备按名注销/重注册的协议原语，热重载在其上做差异增删即可。
- **OIDC 认证**：未实现；`auth_method` 的三种零知识/签名方案覆盖了 frp 用 token+sk
  覆盖的场景。

## 一句话 · In one sentence

frp 做到的转发与治理能力，AetherTunnel 基本都有；在此之上它把默认姿态从"能通"换成
"默认加密、默认可审计、访客可以不交秘密、客户端可以进手机"。
