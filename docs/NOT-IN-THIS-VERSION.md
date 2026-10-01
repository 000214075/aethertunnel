# 设计边界 · Scope and Non-Goals

本文件记录本版本**有意不做**的两项能力：每一项写明设计上的考虑、同样目的下**本程序真正可用**
的做法，以及若要实现大致需要什么。它们是设计取舍，不是待办清单——表格里的每一项 ✅ 能力
都是已完成并通过检查的。（曾经的「TLS 会话模拟」在本版本里以 `disguise = "tls-session"` 实现，
曾经的「Windows 与 macOS 的 tun 设备」以 Wintun 与 utun 实现，曾经的「zk-SNARK」以
`auth_method = "snark"` 的 Groth16 电路实现，曾经的「WebRTC」以 `transport = "webrtc"` 的
访客数据通道实现——四者都移进了 README 的能力表；曾经的「移动端 App」也部分收进了代码：
客户端实现已是库 `pkg/clientlib`，移动绑定在 `pkg/mobile`，android 的完整客户端、绑定 AAR
与最小 App 的 debug APK 由 CI 逐次产出，iOS 编到库级；剩下有意不做的收窄为「商店应用」与
「设备级 VPN」，见下文。）

命名规则：文档里能力的名称按实际能力书写，做到什么写什么。历史背景（v3.1.0 之前宣称过
未实现的功能）记录在 [`CHANGELOG.md`](../CHANGELOG.md) 的开篇说明里。

---

## 移动端：商店应用与设备级 VPN

客户端不再是只能在命令行里运行的程序：实现整体在 `pkg/clientlib`（`Run(ctx, cfg, logger)`
一个入口），`pkg/mobile` 以 `Run(configTOML)` / `Stop()` 两个函数把它交给 gomobile 这样的
绑定工具；配置可以直接以 TOML 字符串内嵌（`config.LoadString`）。CI 的 mobile 作业逐次产出
三样东西：android/arm64 的**完整客户端可执行文件**（pion 依赖的接口枚举助手用 go:linkname，
按其官方解法加 `-checklinkname=0`）、**绑定包 `aethertunnel-mobile-android-arm64.aar`**
（放进自己的 Android 工程就能驱动完整客户端）、以及 **`mobile/android` 里最小 App 构建出的
debug APK**（Kotlin、无 androidx：一个 TOML 配置框、Start/Stop 按钮、一块日志区——隧道
本身全是 `pkg/clientlib` 的 Go 代码）。iOS/arm64 编到**库级**——iOS 的可执行文件按平台规则
必须用 Apple 的 cgo 工具链（gomobile/Xcode），那是 app 工程自己的步骤。

在此之上，**有意不做**的仍是两件：

- **上架商店的第一方应用**。App Store 与 Google Play 的签名、审核与开发者账号不是代码能替
  的；本仓库也没有可以在其上验证 UI 的设备或模拟器。debug APK 侧载到自己的 arm64 设备上
  试用即可；想要自己的入口，用 AAR 包一个壳。
- **设备级 VPN**。Android 的 VpnService 与 iOS 的 NetworkExtension 各要一套 JNI/Swift 壳，
  并且逐包路径必须在真机上验证；本仓库的验证方式（真实进程、真实套接字、真实内核）在没有
  设备的情况下无法覆盖。`[vpn]` 在这两个平台上的运行时行为沿用 `device_other.go`：明确报
  “此构建没有 tun 实现”。不装任何东西的手机今天就能用本程序：在服务端发布一个 `socks5`
  出口（`remote_port` + `allow_targets`），任意 SOCKS5 客户端指向 `<服务器>:<remote_port>`
  即可；`http`/`https` 代理也可以被系统代理设置直接使用。smoke test 里的
  `socks5: a visitor reaches the address it asks for` 与
  `socks5: a target outside allow_targets is refused` 两项，就是用真实二进制与 curl 跑通
  这条路径的。

## 区块链、代币或激励

带宽账本是一条 Ed25519 签名的哈希链：能证明某段用量没有被改动或替换，没有共识、没有货币、
没有矿工、没有分布式账本——这是有意的选择。

它提供的是**可审计性**（`--verify-ledger` 与 `--ledger-proof`），不是去中心化。

---

## English

This file records two capabilities that are **deliberately out of scope** for this release. Each
entry gives the design reasoning, what this program offers instead for the same purpose, and what
an implementation would take. They are design decisions, not a to-do list — every ✅ capability in
the README is complete and covered by checks. Earlier entries left this list as they were
implemented: TLS session emulation became `disguise = "tls-session"`, the tun devices on Windows
and macOS became real Wintun and utun devices, the zk-SNARK became `auth_method = "snark"`, and
WebRTC became the `transport = "webrtc"` visitor data path — all moved to the README's capability
table. The mobile entry narrowed the same way: the client implementation is now a library
(`pkg/clientlib`) with gomobile bindings (`pkg/mobile`); CI produces a full client executable for
android/arm64, the binding AAR, and the debug APK of the minimal app in `mobile/android`, and the
library level builds for ios/arm64 — what remains deliberately out of scope is the
store-distributed app and the on-device VPN, described below.

Naming follows what the code actually does. The historical background (features claimed before
v3.1.0 that were never implemented) is recorded at the top of [`CHANGELOG.md`](../CHANGELOG.md).

| Out of scope (by design) | The design decision | What to use instead |
| --- | --- | --- |
| mobile: a first-party store app, and an on-device VPN | the client is an embeddable library (`pkg/clientlib`, `pkg/mobile`); CI produces a full android/arm64 executable, the gomobile binding AAR, and the debug APK of the minimal Kotlin app in `mobile/android`, and the library level builds for ios/arm64 — but there is no App Store/Play project, no signing or review identity, and no VpnService/NetworkExtension shell, each needing a platform toolchain and real-device verification this repository cannot run | sideload the debug APK onto your own arm64 device, or call `Mobile.run(configTOML)` / `Mobile.stop()` from your own app; on an unmodified phone, publish a `socks5` exit (`remote_port` + `allow_targets`) and point any phone SOCKS5 client at it, or use `http`/`https` proxies through the system proxy settings; `[vpn]` on these platforms reports that this build has no tun implementation |
| blockchain, tokens, incentives | the ledger is a signed hash chain by design: no consensus, no currency, no miners | `--verify-ledger` and `--ledger-proof` give auditability without a distributed ledger |
