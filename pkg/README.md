# pkg/

AetherTunnel 的全部实现。三个入口共用这里的代码：仓库根目录的服务端命令、
[`client/`](../client) 的客户端命令，以及嵌入者调的 [`clientlib`](clientlib) 库
（移动端经 [`mobile`](mobile) 走同一条路）。包按「只做一件事」切开：`protocol` 定词汇、
`crypto` 管机密性、`net` 搬字节、`clientlib` 与 `server` 是两端本体，其余各包只服务一个
具体能力。每个子包都有自己的 README，写它实际做的事，也写它不负责什么。

Every implementation of AetherTunnel. Three entry points share this code: the server
command at the repository root, the client command in [`client/`](../client), and the
embeddable [`clientlib`](clientlib) library that the mobile bindings in
[`mobile`](mobile) drive as well. The packages are cut so each does one thing —
`protocol` fixes the vocabulary, `crypto` holds confidentiality, `net` moves bytes,
`clientlib` and `server` are the two ends — and every subpackage README says what it
does and what it deliberately leaves to someone else.

| 包 | 负责什么 |
| --- | --- |
| [`config`](config) | TOML 的解析、默认值、按角色校验、未知键报告、`includes` 合并、环境变量模板与密钥文件 |
| [`protocol`](protocol) | 线上词汇：帧封装、消息类型与结构体、协议版本 |
| [`crypto`](crypto) | 帧与字节流的 AEAD、Ed25519 身份、Schnorr 证明、X25519+ML-KEM-768 混合协商、按代理的流密钥 |
| [`clientlib`](clientlib) | 客户端本体：会话循环、数据路径、访客、插件、热重载、管理 API、运行时条目（store） |
| [`server`](server) | 服务端本体：会话与注册、代理池与负载均衡、虚拟主机、tcpmux、访客、打洞、SSH 网关、面板、指标、审计与账本 |
| [`net`](net) | 客户端与服务端共享的搬运助手：管道、数据报泵、监听、压缩、websocket 帧 |
| [`obfs`](obfs) | 流量伪装：长度补齐、抖动、record 伪装与真实 TLS 会话伪装 |
| [`socks`](socks) | SOCKS5 的解析与应答（CONNECT 与 UDP ASSOCIATE）、目标圈界与拨号 |
| [`reliable`](reliable) | UDP 上的有序字节流：xtcp 打洞后的数据路径 |
| [`vpn`](vpn) | 三层隧道：tun 设备与 fd 设备、IP 包校验、路由、会话与计数 |
| [`dht`](dht) | UDP 上的 Kademlia：路由表、存储、迭代查找 |
| [`discovery`](discovery) | 在 DHT 之上发布与解析代理名，含通告签名校验 |
| [`ledger`](ledger) | 可校验的带宽账本：哈希链、Ed25519 签名、离线核验与单条证明 |
| [`snarkauth`](snarkauth) | 零知识访客认证：Groth16 电路与证明/验证 |
| [`webrtcvisitor`](webrtcvisitor) | WebRTC DataChannel 上的访客数据路径 |
| [`oidc`](oidc) | OIDC：服务端经 JWKS 验签，客户端取 token |
| [`logging`](logging) | `[log]` 段：文件、按天归档、保留窗口、级别与颜色 |
| [`mobile`](mobile) | gomobile 绑定的胶水层（`Run`/`Stop`/`RunVPN`） |

依赖方向是单向的：`config` →（加密、发现、伪装等），`clientlib` 与 `server` 在最上层，
`logging`、`protocol`、`crypto` 在下面。没有包反向依赖 `clientlib` 或 `server`，
所以嵌入者只为自己用到的东西付编译时间。

Dependencies point one way: `clientlib` and `server` sit on top, `config` above the
shared primitives, and nothing depends back on the two big ones — so an embedder
compiles only what it uses.
