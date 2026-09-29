# 设计边界 · Scope and Non-Goals

本文件记录本版本**有意不做**的四项能力：每一项写明设计上的考虑、同样目的下**本程序真正可用**
的做法，以及若要实现大致需要什么。它们是设计取舍，不是待办清单——表格里的每一项 ✅ 能力
都是已完成并通过检查的。（曾经的「TLS 会话模拟」在本版本里以 `disguise = "tls-session"`
实现，曾经的「Windows 与 macOS 的 tun 设备」以 Wintun 与 utun 实现：两者都移进了 README
的能力表。）

命名规则：文档里能力的名称按实际能力书写，做到什么写什么。历史背景（v3.1.0 之前宣称过
未实现的功能）记录在 [`CHANGELOG.md`](../CHANGELOG.md) 的开篇说明里。

---

## 移动端 App

本版本的范围是服务端与客户端两个可执行程序；iOS/Android 工程与移动端构建产物不在其中。

**可用的替代**：在服务端发布一个 `socks5` 出口（`remote_port` + `allow_targets`），手机上的任意
SOCKS5 客户端指向 `<服务器>:<remote_port>` 即可使用；`http`/`https` 代理也可以被系统代理设置
直接使用。smoke test 里的 `socks5: a visitor reaches the address it asks for` 与
`socks5: a target outside allow_targets is refused` 两项，就是用真实二进制与 curl 跑通这条路径的。

若要纳入范围，需要 iOS/Android 工具链（Xcode、Android SDK）与可在其上验证的设备；
本仓库的验证方式（真实进程、真实套接字、真实内核）在没有这些的情况下无法覆盖移动端。

## 区块链、代币或激励

带宽账本是一条 Ed25519 签名的哈希链：能证明某段用量没有被改动或替换，没有共识、没有货币、
没有矿工、没有分布式账本——这是有意的选择。

它提供的是**可审计性**（`--verify-ledger` 与 `--ledger-proof`），不是去中心化。

## WebRTC

XTCP 用的是自己的 UDP 打洞实现（HMAC-SHA256 同时打开 + 可靠有序字节流），不依赖 WebRTC 协议栈，
也不需要 ICE/DTLS/SCTP 中任何一项。

打洞成功走直连、失败自动经服务端中继，访客会回报实际走的是哪条路径，指标与审计记录的是实测结果
而不是猜测。

## zk-SNARK

`nizk` 是 P-256 上的 Schnorr 证明：它能证明"知道秘密"而不在线上泄露秘密（服务端先发 32 字节
nonce，访客用 nonce 与代理名组成上下文做证明）。它按设计**不具备**简洁证明、可信设置等性质，
因此不叫 zk-SNARK。

需要密钥不出现在线上时用 `auth_method = "nizk"`，并同时开启 `[encryption]` 或 `[transport]`，
其余环节就不再有明文密钥。

---

## English

This file records four capabilities that are **deliberately out of scope** for this release. Each
entry gives the design reasoning, what this program offers instead for the same purpose, and what
an implementation would take. They are design decisions, not a to-do list — every ✅ capability in
the README is complete and covered by checks. Two earlier entries left this list in this release:
TLS session emulation became `disguise = "tls-session"`, and the tun devices on Windows and macOS
became real Wintun and utun devices — both moved to the README's capability table.

Naming follows what the code actually does. The historical background (features claimed before
v3.1.0 that were never implemented) is recorded at the top of [`CHANGELOG.md`](../CHANGELOG.md).

| Out of scope (by design) | The design decision | What to use instead |
| --- | --- | --- |
| Mobile app | the release ships two executables, a server and a client; no iOS/Android project or mobile build artifacts | publish a `socks5` exit (`remote_port` + `allow_targets`) and point any phone SOCKS5 client at it; `http`/`https` proxies work with the system proxy settings |
| blockchain, tokens, incentives | the ledger is a signed hash chain by design: no consensus, no currency, no miners | `--verify-ledger` and `--ledger-proof` give auditability without a distributed ledger |
| WebRTC | XTCP is its own UDP hole punching (HMAC-SHA256 simultaneous open over a reliable byte stream); no ICE, DTLS or SCTP | a failed punch falls back to the server's relay automatically, and the visitor reports which path it actually took |
| zk-SNARK | `nizk` is a Schnorr proof over P-256 — knowledge of a secret, deliberately without succinctness or a trusted setup | use `auth_method = "nizk"` when the key must not appear on the wire, with `[encryption]` or `[transport]` on |
