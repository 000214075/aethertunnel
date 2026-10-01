# tools/snarksetup

重新生成 SNARK 电路与密钥：`go run ./tools/snarksetup`。产物 `r1cs.bin`、
`proving.key`、`verifying.key` 写入 `pkg/snarkauth/` 并随源码入库。

Regenerates the SNARK circuit and keys: `go run ./tools/snarksetup`. The outputs
(`r1cs.bin`, `proving.key`, `verifying.key`) land in `pkg/snarkauth/` and are
committed with the source.
