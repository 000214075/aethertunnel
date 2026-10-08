# pkg/snarkauth

零知识访客认证：992 约束的 MiMC 电路，用 Groth16 证明访客知道私有代理的密钥而不泄露它。
电路与证明/验证密钥由 `go:embed` 内置，[`tools/snarksetup`](../../tools/snarksetup)
可以重新生成；证明绑定代理名与服务器给出的挑战，换一个上下文即失效。

Zero-knowledge visitor authentication: a 992-constraint MiMC circuit proved with Groth16,
showing knowledge of a private proxy's secret without revealing it. The circuit and keys
are embedded and `tools/snarksetup` regenerates them; a proof is bound to the proxy name
and to the server's challenge, so it fails in any other context.

用到它的两处：

- 客户端 [`pkg/clientlib`](../clientlib) 的 `visitorProof`：访客的 `auth_method = "snark"`
  时生成证明。
- 服务端 [`pkg/server`](../server) 的 `verifyVisitorProof`：按代理注册时声明的方法校验。

两端构造的上下文都是 `protocol.VisitorProofContext(代理名, 服务端 nonce)`，所以证明
只对这一次挑战有效、无法重放；访客声明的方法与代理不一致（例如代理写 `nizk`）会被
拒绝，不会静默降级成明文比对。功能套件里有一项让真实的客户端生成证明、真实的服务端
校验，持错密钥或另一种方法的访客都拿不到数据。

Both ends build the context with `protocol.VisitorProofContext(proxy, server nonce)`, so
a proof answers exactly one challenge and cannot be replayed; a visitor whose method
differs from the proxy's is refused rather than silently downgraded.
