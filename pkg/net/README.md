# pkg/net

两端共用的搬运助手：半关闭感知的双向管道、按来源地址分会话的数据报泵、绑定失败的原因
归类、snappy 压缩与 websocket 帧。它们只搬字节、不碰协议语义，所以两端引用的理由
一致。

Byte-movement helpers both ends share: a half-close-aware pipe, a datagram pump that
splits sessions by source address, the diagnosis of a failed bind, snappy
compression and websocket framing. They move bytes and know nothing about the protocol,
so both ends use them for the same reasons.

| 文件 | 做什么 |
| --- | --- |
| `pipe.go` | `Pipe`：一条双向拷贝，处理半关闭（一端 `CloseWrite` 后另一端仍能收完应答）并统计双向字节，是每条隧道流的实际搬运者 |
| `dgram.go` | 数据报泵：按来源地址分会话、`MaxDatagram` 上限（超限丢弃而不是截断）、多路径分发 |
| `listen.go` | 绑定失败时的原因归类：`ListenCause` 把 `EADDRINUSE` / `EACCES` / `EADDRNOTAVAIL`（Windows 上是同义的 WSA 码）翻成可读原因，`ListenError` 用它包装 `net` 的原始错误并带上地址 |
| `compress.go` | snappy 流式压缩包装，加在加密层外侧，两端由 `use_compression` 协商 |
| `websocket.go` | RFC 6455 二进制帧：把一条连接包成 websocket，让隧道穿过只放行合法 websocket 的中间设备 |

半关闭、压缩与 websocket 三条路径都有真实回环测试；`dgram.go` 与 `listen.go` 的边界
（超限数据报、空的来源、监听冲突）也各有测试。

Half-close, compression and the websocket framing each have real loopback tests, and
the datagram and listener edge cases are covered too.
