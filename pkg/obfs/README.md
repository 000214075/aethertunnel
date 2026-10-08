# pkg/obfs

流量伪装：改的不是加密强度，而是**一次连接看起来像什么**。它不提供机密性——机密性归
[`pkg/crypto`](../crypto)；这里的每一层只影响外观：TLS record 分层，以及一次真实握手的
会话伪装。帧长度补齐与写入抖动不在这里，它们是帧层的 `FramerOptions.PadTo` / `Jitter`
（[`pkg/protocol`](../protocol/message.go)），由 `[obfuscation]` 的 `pad_to` /
`jitter_millis` 配置。

Traffic disguise changes what a connection looks like, not how strong its secrecy is.
Confidentiality belongs to [`pkg/crypto`](../crypto); each layer here only shapes an
appearance — TLS record framing, and a disguise built from a real handshake. Frame
padding and write jitter are not here: they are the frame layer's
`FramerOptions.PadTo` / `Jitter` ([`pkg/protocol`](../protocol/message.go)), configured
by `[obfuscation]`'s `pad_to` / `jitter_millis`.

| 手段 | 做什么 | 骗得过谁 |
| --- | --- | --- |
| `pad_to` / `jitter_millis` | 帧长度补齐到固定倍数、写入前加随机延迟；实现在帧层（`pkg/protocol` 的 `FramerOptions`），本包不参与 | 只看包长与间隔的统计 |
| `disguise = "tls-record"` | 把每次写入包进 TLS 1.2 应用数据记录 | 读首字节判断协议的识别器 |
| `disguise = "tls-session"` | 每条连接做一次**真实的 TLS 握手**，证书是每连接现签的匿名自签证书 | 建模 TLS 会话（看握手序列）的探测器 |

`tls-session` 是真加密：它用标准库的 TLS 完成握手与记录层，只是证书不指向任何身份。
`obfs_test.go` 与 `fuzz_test.go` 覆盖记录的读写与畸形输入；伪装后的字节流
由 `scripts/functional-linux.sh` 在真实隧道上验证。

`tls-session` is real TLS — the standard library does the handshake and the record
layer; the certificate simply names no identity. The disguised byte stream is
exercised on a real tunnel by `scripts/functional-linux.sh`.
