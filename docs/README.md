# docs/

项目的长文档。根目录的 [`README.md`](../README.md) 负责「这是什么、怎么跑起来」，这里
回答剩下的问题：内部怎么搭、每个配置键是什么意思、在哪些平台真的验过、与 frp 差在哪里、
安全边界到哪为止、哪些是有意不做。

Long-form documentation. The root [`README.md`](../README.md) answers "what is this and
how do I run it"; these files answer everything after that: how the internals fit
together, what every configuration key means, which platforms were actually exercised,
where this differs from frp, where the security boundary lies, and what is deliberately
out of scope.

| 文档 | 回答的问题 | 什么时候读 |
| --- | --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | 控制连接、数据连接、代理池、访客与打洞在代码里怎么连起来；一条字节从访客到本地服务经过哪些函数 | 你要改代码，或想知道某个能力为什么这样实现 |
| [`CONFIGURATION.md`](CONFIGURATION.md) | 每一个配置键的类型、默认值、作用与配错时会怎样 | 你在写 `server.toml` / `client.toml` |
| [`PLATFORMS.md`](PLATFORMS.md) | 六个发布目标各自被什么执行过、平台差异与已知边界 | 你准备部署到某个平台，或怀疑某个能力只是纸上写着 |
| [`SECURITY.md`](SECURITY.md) | 威胁模型：认证是什么、加密保护什么、伪装不保护什么、路上的人还能学到什么 | 你在评估能不能用它承载敏感流量 |
| [`VS-FRP.md`](VS-FRP.md) | 与 fatedier/frp 的逐项对照：相同、超出、有意不同、尚未复现 | 你从 frp 过来，想知道哪一步会踩空 |
| [`NOT-IN-THIS-VERSION.md`](NOT-IN-THIS-VERSION.md) | 有意不做的能力、为什么不做、同样目的下现在能用什么 | 你在找某个 frp 或别处的功能，想知道是不是漏了 |

文档里的每个“可用”都对应仓库里可执行的检查，不是宣传词；做不到的不写进“可用”。

Every "works" in these files corresponds to an executable check in this repository rather
than to a marketing claim.
