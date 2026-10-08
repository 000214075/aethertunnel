# tools/snarksetup

重新生成 zk-SNARK 的电路与密钥。只有需要改动电路（`pkg/snarkauth` 里的约束）时才跑它；
产物已经在仓库里并随源码入库，平时不必运行。

Regenerates the zk-SNARK circuit and keys. Run it only when the circuit in
`pkg/snarkauth` changes; the outputs are already in the repository and are committed with
the source.

```bash
go run ./tools/snarksetup
```

产物写入 [`pkg/snarkauth/`](../../pkg/snarkauth) 并随源码入库，由 `go:embed` 内置：

| 文件 | 内容 |
| --- | --- |
| `r1cs.bin` | 编译后的 992 约束 MiMC 电路 |
| `proving.key` | 客户端的证明密钥 |
| `verifying.key` | 服务端的验证密钥 |

密钥与电路必须配套：换了电路就要重新生成两边，否则证明会验证失败——这是
`pkg/snarkauth` 的测试会立刻报出来的那种失败，不会拖到线上。

The circuit and the keys must be regenerated together; a mismatched pair fails
verification, which `pkg/snarkauth`'s tests report immediately.
