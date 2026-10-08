# tools/

开发期工具。它们不属于任何一个二进制，只在需要重新生成派生产物时运行；出问题只影响
重新生成这件事，不影响已经编译好的隧道。

Development-time tools. They ship in no binary; they run only when a derived artifact has
to be regenerated, so a problem here can only affect that, never a compiled tunnel.

| 目录 | 作用 |
| --- | --- |
| [`snarksetup/`](snarksetup/) | 重新生成 zk-SNARK 电路（992 约束的 MiMC）与 Groth16 的证明/验证密钥；产物写回 `pkg/snarkauth/` 并随源码入库，由 `go:embed` 内置进客户端与服务端 |

`tools/` 下的代码不参与运行时的任何路径，所以它们出问题只会影响"重新生成密钥"这一件事，
不会影响已经编译好的隧道。

Nothing under `tools/` is on a runtime path: a problem here can only affect
regenerating the keys, never a running tunnel.
