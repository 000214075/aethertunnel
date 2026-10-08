# pkg/reliable

在 UDP 上承载一条**有序、可靠**的字节流，供 xtcp 打洞成功后的直连使用。它存在的原因很
具体：两个 NAT 后的端直连时只有 UDP 可用，而隧道里的 TLS、HTTP、ssh 需要的是字节流。

An ordered, reliable byte stream over UDP, used by the direct path after an xtcp punch
succeeds. Its reason to exist is concrete: two NATed peers have only UDP, while TLS, HTTP
and ssh inside the tunnel want a byte stream.

| 文件 | 做什么 |
| --- | --- |
| `packet.go` | 线上的数据包与序号编码 |
| `exchange.go` | 与会合服务器交换地址：在将来承载流的那条 socket 上完成，NAT 映射才对得上 |
| `conn.go` | 面向连接的层：读写、超时、重传、关闭，以及重组、乱序与去重 |
| `reliable.go` | socket 与同时打开：`Punch`/`Accept` 握手，双方按 HMAC-SHA256 的约定互发、确认对端身份后定序 |

`stale_ooo_test.go` 专门钉住一个容易被写错的点：迟到的乱序包不能把重组窗口顶坏——
这正是打洞路径上"看起来通了、偶尔卡住"的来源。`fuzz_test.go` 覆盖包头解析。

`stale_ooo_test.go` pins down the case that is easy to get wrong on a punched path:
a stale out-of-order packet must not damage the reassembly window. `fuzz_test.go`
covers packet-header parsing.
