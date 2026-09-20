# 更新日志 · Changelog

本文件记录每个版本**实际**改了什么。

> **关于 v3.1.0 之前的条目**：旧版本曾宣称 WebRTC、DHT、区块链、抗量子加密、AI 路由等
> 20 项功能，并声称在 14 个平台上产出 28 个二进制。这些描述与代码不符——那些关键词在 Go
> 源码中的出现次数为 0，构建脚本指向的是不存在的目录。为避免继续误导，v3.1.0 之前的
> 详细功能清单已删除，只保留版本号与日期。真实的历史是：v0.1.x 到 v3.0.0 期间完成的是
> **脚手架**（配置、协议与面板的骨架），核心链路从未真正跑通。

---

## [3.1.0] — 2026-09-20

这一版的目标不是"加功能"，而是**让核心链路第一次真正可用**，并把说明书改成与代码一致。

### 修复（都是会导致功能完全不可用的缺陷）

- **服务端无法启动**：`main.go` 对同一个 `bind_addr:bind_port` 调用了两次 `net.Listen`，
  第二次必然失败并 `log.Fatalf`，进程启动即退出。现在只监听一个端口，控制连接与数据连接
  复用它。
- **客户端与服务端协议不兼容**：客户端发送 ASCII 文本 `AUTH:<token>`，服务端读取 8 字节
  二进制帧头，认证永远不可能成功；服务端回包客户端也解析不了。现在两端使用同一套
  带版本号的帧格式（`pkg/protocol`），并在握手时交换协议版本与加密算法。
- **心跳消息无法表示**：`NewHeartbeatMessage` 造出空负载，而读取端把 `payloadLen == 0`
  判为错误，于是每次心跳都会踢掉连接。现在空负载合法。
- **隧道数据面是死的**：`HandleConnection` 只读一条消息就返回，连接随后被关闭；目标地址
  被硬编码为服务器自己的 `127.0.0.1:<remote_port>`；服务器从不监听 `remote_port`。
  现在服务器为每条隧道真正绑定公网端口，访问者到来时通过控制连接请客户端回拨一条数据
  连接，两端用 `io.Copy` 半关闭双向搬运（带 32 KiB 缓冲池与空闲超时）。
- **未认证即可开隧道**：旧代码在第一条消息就是 `Proxy` 时直接转发，不检查认证。现在数据
  连接必须携带有效会话 ID 与未使用的流 ID。
- **连接表永久泄漏**：`RemoveConnection` 关闭 socket 却从不 `delete` 表项，超过 100 次
  连接后服务器永久拒绝所有客户端；隧道也不会随会话释放。现在会话与隧道在断开时都会被
  注销，释放公网端口。
- **隧道名在重连后无法复用**：`Session.Close` 清空了隧道表，导致随后注销时什么都没删掉，
  重连的客户端拿到 "a tunnel with that name is already registered" 而失去隧道。现在
  隧道归属会被正确注销，且管理器会把"所属会话已死"的陈旧条目判为可替换。
- **加密完全不可用**：密钥直接取口令字节，而 XChaCha20-Poly1305 要求恰好 32 字节，任何
  普通长度的 token 都会报 `bad key length`。现在用 HKDF-SHA256 派生，任意长度口令可用，
  并支持选择 XChaCha20-Poly1305 或 AES-256-GCM。
- **混淆模块返回被丢弃的明文**：`ObfuscatePacket` 加密后返回的是另一个变量；接收端又在
  已 base64 的数据上再编码一次，任何数据包都解不开。该模块已删除（未实现的功能不再假装
  存在）。
- **WebSocket / HTTP / SCTP 传输一被调用就 panic**：把 `http.ResponseWriter` 断言成
  `net.Conn`、`Close()` 时关闭仍有发送者的 channel、读写锁被用于阻塞的 socket 读导致
  连接无法关闭。这些传输从没有被 `main.go` 启动过，已删除。
- **VPN 包整体不可调用**：`performance` 接口声明 `Disable() bool` 而实现返回 `void`，
  接口无法被满足，调用必 panic；统计模块在同一把非可重入锁上自锁死。已删除。
- **凭据泄漏进日志**：认证失败时把用户提交的完整 token 打进日志。现在只记录来源地址。
- **面板按相对路径读文件**：文件服务器用 `../../web/dashboard`，页面处理器用
  `web/dashboard/...`，两者不可能同时成立，且发布的压缩包里根本没有这些文件。现在用
  `go:embed` 打进二进制。
- **面板显示假数据**：三个页面没有任何网络请求，连接数/带宽/客户端列表/日志/图表分别是
  字面量与 `Math.random()`；`server.html` 因为 CSS 里没有 `.hidden` 规则而根本无法切换
  页面。已重写为单页真实面板（详见"新增"）。
- **`/api/config` 返回示例配置**，包含一个假的 auth token，且与真实配置无关。现在返回
  脱敏后的真实设置。
- 构建脚本与 CI 全部指向不存在的路径（`./server`、`./main_minimal.go`），Docker 构建把
  Go 1.21 与要求 1.22.2 的模块放在一起，CI 的产物路径与 `download-artifact@v4` 不符，
  两个工作流还会向同一个 release 上传同名附件。已重写。
- 版本号被打成 `-X main.Version` 而变量名是小写 `version`，链接器静默忽略，于是二进制永远
  报硬编码的旧版本号。现在 `--version` 能报出真实构建信息。

### 新增

- **`--config` / `--check` / `--version` 命令行参数**（旧版把第一个参数当配置文件路径，
  `--version` 会被当成文件名）。
- **配置校验**：未知配置键会被列出来，`--check` 只校验不启动；端口、代理名重复、不支持的
  代理类型、弱 token、对外暴露却无 token 的面板都会给出明确提示。
- **客户端重连**：指数退避到 `max_reconnect_seconds`，带 ±20% 抖动；断线后自动重新注册
  全部隧道。
- **可选的负载加密**：控制帧与隧道记录都用 AEAD；两端配置不一致时给出
  `encryption mismatch` 而不是无限认证失败。
- **面板 API 与 Bearer token**：`/api/health`（公开）、`/api/status`、`/api/clients`、
  `/api/proxies`、`/api/config`（脱敏）、`DELETE /api/clients/{id}` 断开指定客户端。
- **新面板**：单页、内嵌资源、中英双语（两个语言包键完全对齐）、有真正的移动端导航抽屉、
  空状态与错误提示如实显示、GET/PUT 数据全部用 `textContent` 插入（无 `innerHTML`）。
- **测试**：`pkg/config`、`pkg/crypto`、`pkg/protocol`、`pkg/server` 四组测试，其中
  `pkg/server` 包含真实的端到端测试——启动服务器、注册隧道、访问者连接、数据穿过隧道
  回到本地回声服务，明文与加密各跑一遍；另有会话释放、名字复用、连接上限、垃圾输入
  不致命等回归测试。
- **跨平台构建**：`scripts/build-release.sh`（POSIX）与 `scripts/build-release.ps1`
  （Windows）产出 6 平台 × 2 个二进制 + `SHA256SUMS`；CI 在 Linux/Windows/macOS 上
  跑 `gofmt`/`vet`/`test`/构建；发布工作流按 tag 触发并校验产物格式。
- **文档**：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)、
  [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)、
  [`docs/SECURITY.md`](docs/SECURITY.md)、
  [`docs/MIGRATION.md`](docs/MIGRATION.md)，以及双语 README。

### 移除

- 所有描述不存在功能的文档（共 27 个 md 文件），包括 `PROJECT_SUMMARY.md`、
  `QUALITY_REPORT.md`、`DELIVERY_CHECKLIST.md`、三个安全审计报告与 `docs/` 下 19 个文件。
  需要对照旧说法请查本文件的顶部说明。
- 不可用且未被调用的模块：`pkg/vpn`、`pkg/obfuscation`、`pkg/protocol/{http,websocket,sctp}.go`、
  `pkg/net/mux.go`、`pkg/interfaces`、`sctp-fake`（一个把 `libp2p/go-sctp` 替换成空壳的
  本地模块）、`release/{linux,darwin,windows}-amd64`（三份重复的客户端源码副本）。
- 与产品无关的 Python 编排脚本（`auto_trigger_system.py` 等 4 个）与 `__pycache__`。
- 依赖 `gorilla/websocket` 与 `libp2p/go-sctp`；现在只有 `BurntSushi/toml` 与
  `golang.org/x/crypto`。

### 兼容性

- 配置文件：`server.bind_addr`、`server.bind_port`、`server.auth_token`、`client.server_addr`、
  `client.auth_token`、`[[proxies]]` 的字段含义不变。`[dashboard]` 新增 `bind_addr`/`token`，
  `[encryption]` 是新增段。旧配置里的 `enable_tls`、`[vpn]`、`[obfuscation]`、`[webrtc]`
  等键不再被使用：前者会作为未知键被报出来，后两者会解析但被忽略并给出警告。
- 协议：与旧版本**不兼容**，两端必须一起升级（`ProtocolVersion = 3`）。

---

## [3.0.0] — 2026-02

标签存在，但核心链路未跑通；详细功能清单已按本文件顶部说明删除。

## [2.0.1] / [2.0.0] — 2026-01

同上。

## [1.0.x] / [0.1.1-alpha] — 2025-12 ~ 2026-01

同上。
