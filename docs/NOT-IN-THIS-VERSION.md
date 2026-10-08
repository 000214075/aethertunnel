# 设计边界 · Scope and Non-Goals

本版本有两项能力是**有意不做**的。它们是设计取舍、不是待办清单——每一节都写了同样目的下
现在就能用的做法。除此之外，README 能力清单里的每一项都已完成并通过检查。

## 移动端：商店应用与真机验证

客户端不只是命令行程序：实现整体在 `pkg/clientlib`（`Run(ctx, cfg, logger)` 一个入口），
`pkg/mobile` 以 `Run(configTOML)` / `Stop()` / `RunVPN(config, shell)` 把它交给 gomobile
绑定，配置可以直接以 TOML 字符串内嵌（`config.LoadString`）。

发布的移动端产物有三样：android/arm64 的**完整客户端可执行文件**、**绑定包
`aethertunnel-mobile-android.aar`**（放进自己的 Android 工程就能驱动完整客户端），以及
`mobile/android` 里最小 App 构建出的 **debug APK**（Kotlin、无 androidx：一个 TOML 配置框、
Start/Stop/VPN 三个按钮、一块日志区——隧道本身全是 `pkg/clientlib` 的 Go 代码）。
设备级 VPN 也已落地：`RunVPN` 在会话已建立、地址已知的那一刻向壳要 tun 接口，
`ProtectSocket` 保证隧道自己的套接字不被自己喂的接口捕获；模拟器上验证过从设备经隧道
ping 服务端的逐包路径。iOS/arm64 编到**库级**——iOS 的可执行文件按平台规则必须用 Apple 的
cgo 工具链（gomobile/Xcode），那是 app 工程自己的步骤。

**有意不做的只剩一件：上架商店的第一方应用。** App Store 与 Google Play 的签名、审核与
开发者账号不是代码能替的，物理 arm64 设备上的触摸屏与蜂窝网络行为也未被核实。
debug APK 侧载到自己的 arm64 设备上试用即可；想要自己的入口，用 AAR 包一个壳。

不装任何东西的手机今天就能用本程序：在服务端发布一个 `socks5` 出口（`remote_port` +
`allow_targets`），任意 SOCKS5 客户端指向 `<服务器>:<remote_port>` 即可；`http`/`https`
代理也可以被系统代理设置直接使用。

## 区块链、代币或激励

带宽账本是一条 Ed25519 签名的哈希链：能证明某段用量没有被改动或替换，没有共识、没有货币、
没有矿工、没有分布式账本——这是有意的选择。它提供的是**可审计性**（`--verify-ledger` 与
`--ledger-proof`），不是去中心化。

---

## English

Two capabilities are **deliberately out of scope** for this release. They are design
decisions, not a to-do list — each entry says what this program offers instead for the same
purpose. Every other capability in the README is complete and covered by checks.

**Mobile: a first-party store app, and real-device verification.** The client implementation
is a library (`pkg/clientlib` with a single `Run` entry point); `pkg/mobile` exposes it to
gomobile as `Run(configTOML)` / `Stop()` / `RunVPN(config, shell)`, with the configuration
embeddable as a TOML string. The published mobile artifacts are a full android/arm64 client
executable, the binding package `aethertunnel-mobile-android.aar` (drop it into your own
Android project), and a debug APK built from the minimal app in `mobile/android`. The
on-device VPN path has landed and the packet path is verified on an emulator; iOS builds at
library level (a linked iOS executable needs Apple's cgo toolchain, an app-project step).
What no code can replace — store signing, review and developer accounts, and a physical
arm64 device's touchscreen and cellular behaviour — is what stays out of scope: sideload the
debug APK onto your own device, or wrap the AAR in a shell of your own. An unmodified phone
can use this program today: publish a `socks5` exit (`remote_port` + `allow_targets`) on the
server and point any phone's SOCKS5 client at `<server>:<remote_port>`, or use the
`http`/`https` proxies through the system proxy settings.

**Blockchain, tokens, incentives.** The bandwidth ledger is a signed hash chain by design:
no consensus, no currency, no miners. It gives auditability (`--verify-ledger` and
`--ledger-proof`), not decentralization.
