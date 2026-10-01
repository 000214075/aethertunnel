# pkg/snarkauth

零知识访客认证：992 约束的 MiMC 电路，用 Groth16 证明访客知道私有代理的 secret
而不泄露它；电路与证明/验证密钥由 `go:embed` 内置，`tools/snarksetup` 可重新生成。

Zero-knowledge visitor authentication: a 992-constraint MiMC circuit proved with
Groth16, showing knowledge of a private proxy's secret without revealing it. The
circuit and keys are embedded; `tools/snarksetup` regenerates them.
