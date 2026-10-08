# AetherTunnel 1.0.0

**把内网服务发布到公网，也把客户端嵌进你自己的程序 · Publish a service behind NAT — and embed the client in your own program.**

一份纯 Go、无 CGO 的实现，同时是服务端、客户端与内置 Web 面板：命令行程序、可嵌入的库
（`pkg/clientlib`）与 Android App（gomobile 绑定 + VpnService 壳）三种形态，六个平台交叉
编译即可用。

One pure-Go, CGO-free codebase that is a server, a client and a web panel at once — a
command line program, an embeddable library (`pkg/clientlib`) and an Android app (gomobile
binding + VpnService shell), cross-compiled for six platforms.

## 转发与治理 · Forwarding and governance

- 九种代理类型：`tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5` `tcpmux`。
- 跨客户端代理池与五种负载均衡策略（`round-robin` `random` `latency` `failover`
  `adaptive`），加上按每条流应答速度在线学习的 `bandit`。
- 按代理限速（`bandwidth`）、`remote_ports` 端口段展开、`proxy_protocol = "v1"`、
  `subdomain` 泛子域路由、`tcpmux_port` 按 CONNECT 主机名分流、`http_user`/`http_password`
  基本认证、`use_compression`（snappy，混合部署自动退化）、`transport.protocol = "websocket"`
  与普通客户端共用控制端口。
- 插件：`static_file`（把目录以 HTTP 发布，支持 basic auth）、`unix_domain_socket`；
  `health_check`（tcp/http 探活，失败关端口、恢复自动重发布）。
- 服务端治理：`allow_ports`、`deny_cidrs`、令牌桶连接限流、`ban_after_failures` 与
  `ban_ignore_cidrs`、`max_connections`、`max_ports_per_client`、`max_pool_count`、
  `feature_gates`。
- `ssh -R`：OpenSSH 客户端加一条发布命令就能用；`SIGHUP` 热重载让新增隧道即刻上线。

## 加密与身份 · Encryption and identity

- 可选端到端加密：X25519 + ML-KEM 后量子混合，会话密钥随握手更新。
- Ed25519 身份公钥或共享密钥认证；token 常数时间比较、失败不落日志。
- `stcp`/`xtcp` 私有代理可选 `auth_method = "snark"`（Groth16 证明由客户端生成、服务端校验）；
  `transport = "webrtc"` 的访客走 WebRTC DataChannel。

## 三层隧道 · Layer-3 tunnel

- Linux（TUN）、Windows（Wintun）、macOS（utun）打开真实设备；Android 由 App 的
  VpnService 提供接口，带 protect 钩子与全隧道路由；没有设备可用时明确报错退出。

## 去中心化目录与可观测 · Decentralized directory and observability

- 内置 DHT：仅凭公钥在去中心化目录解析名字，通告带签名、有 TTL 与撤回；`xtcp` NAT
  打洞成功后两端直连。
- 带宽账本：Ed25519 签名 + 哈希链，`--verify-ledger` 离线校验、`--ledger-proof` 把前缀
  导出给审计方。
- JSONL 审计日志（按大小轮转、保留代数）、Prometheus 指标、优雅退出。
- 内置 Web 面板：中英双语、账本页与目录页、窄屏版式与无障碍支持。

## 质量 · Quality

- 每一项能力都有仓库里可复跑的检查：单元与端到端测试（含 fuzz），以及面向真实二进制、
  真实浏览器与真实设备的功能套件；出错时点名是哪一项。

## 交付物 · Artifacts

- `aethertunnel-server-*` / `aethertunnel-client-*`：Windows / Linux / macOS × amd64 / arm64
  共 12 个静态二进制。
- `aethertunnel-mobile-android.aar`：嵌进你自己的 Android 工程的绑定库（`Mobile.run(config)` /
  `Mobile.stop()`，`RunVPN` 支持设备级隧道）；`aethertunnel-app-android-arm64-debug.apk`：
  最小 App。
- `server.toml.example` / `client.toml.example`：带注释的最小配置。
- `SHA256SUMS`：上面 12 个二进制的校验和（移动端产物与示例配置不在其中），下载后
  `sha256sum -c SHA256SUMS` 核对。

## 文档 · Docs

- [README](https://github.com/000214075/aethertunnel/blob/v1.0.0/README.md)：完整导览与 30 秒上手。
- [docs/VS-FRP.md](https://github.com/000214075/aethertunnel/blob/v1.0.0/docs/VS-FRP.md)：与 frp 的逐项对照。
- [docs/CONFIGURATION.md](https://github.com/000214075/aethertunnel/blob/v1.0.0/docs/CONFIGURATION.md)：每个配置键的说明（守卫测试强制）。
- [docs/PLATFORMS.md](https://github.com/000214075/aethertunnel/blob/v1.0.0/docs/PLATFORMS.md)：各平台验证过什么、没验证什么。
- [docs/SECURITY.md](https://github.com/000214075/aethertunnel/blob/v1.0.0/docs/SECURITY.md)：安全模型与漏洞私下报告。
- [docs/NOT-IN-THIS-VERSION.md](https://github.com/000214075/aethertunnel/blob/v1.0.0/docs/NOT-IN-THIS-VERSION.md)：有意不做的功能。
