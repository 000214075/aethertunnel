# pkg/socks

SOCKS5 的解析与应答，两处用它：服务端的 `socks5` 代理类型（访客说 SOCKS5，客户端从自己
的网络拨目标），以及手机上不装任何东西就能用的出口——系统代理设置指向服务器即可。

SOCKS5 parsing and replies. Two callers: the server's `socks5` proxy type, where the
visitor speaks SOCKS5 and the client dials the target from its own network, and the exit
a phone can use without installing anything by pointing its system proxy settings at the
server.

| 文件 | 做什么 |
| --- | --- |
| `socks5.go` | 握手与请求的解析：方法协商（无认证或用户名/口令）、`CONNECT` 的目标地址解析（IPv4、域名、IPv6）、`UDP ASSOCIATE`、应答码 |
| `server.go` | 把解析结果变成会话：认证、按目标拨号、把字节接起来 |
| `target.go` | `allow_targets` 的圈界检查与拨号策略——`socks5` 代理没有它就拒绝注册，所以客户端不会无意中发布一个通吃出口 |

`allow_targets` 检查发生在拨号之前，域名目标按解析出的地址逐个判定；
`fuzz_test.go` 覆盖 `CONNECT` 与 `UDP ASSOCIATE` 两条解析路径，跑出的失败输入留在
`testdata/fuzz/` 作为语料。

The `allow_targets` check runs before dialling, and a hostname target is judged on the
addresses it resolves to. The two parsers are fuzzed; the seeds that found failures are
kept in `testdata/fuzz/`.
