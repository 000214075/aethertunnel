# pkg/protocol

客户端与服务端对话的全部词汇：帧怎么封（6 字节头、类型与标志、单帧上限）、每条消息带
哪些字段、协议版本怎么协商。改这里等于改线上格式，两端必须一起改。

The whole vocabulary the client and the server speak: how a frame is laid out (a
six-byte header, a type, flags, one frame's ceiling), what each message carries, and how
the revision is agreed. Changing it changes the wire format, so both ends move together.

| 文件 | 做什么 |
| --- | --- |
| `message.go` | 帧头（类型 + 标志 + 长度）、`Framer` 的读写、加密与补齐选项，以及每个消息结构体：认证、心跳、代理注册、数据请求/打开/应答、访客握手与证明、打洞准备与回退、VPN 包、撤销 |
| `rendezvous.go` | 会合端口上的三条报文（打洞请求、对端地址应答、打洞结果路径上报）的编解码与校验 |

帧头的标志位是协议的一部分：加密与补齐各自一位，接收端据此还原，所以两端不需要
就 `obfuscation.pad_to` 达成一致也能互通。版本号在认证应答里交换，不一致时两端各
记一条 `protocol mismatch` 告警并继续——这是有意的兼容选择：旧对端不会被拒，但流量
若有异常应先怀疑版本差。

`message_test.go` 与 `fuzz_test.go` 覆盖字节级边界：截断的帧、超长的负载、加密位与
补齐位的组合、会合请求的畸形输入。

The frame flags are part of the protocol: encryption and padding are announced per
frame, so the two ends do not have to agree on `pad_to` to interoperate. The revision
is exchanged in the auth response, and a mismatch logs a warning on both ends and the
session continues — an intentional compatibility choice, so an old peer is not refused.
