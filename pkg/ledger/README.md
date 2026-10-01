# pkg/ledger

篡改可见的带宽账本：条目构成哈希链、逐条 Ed25519 签名，仅凭公钥即可离线校验，
并可导出单条证明。不提供共识、货币或激励——那是有意的设计边界。

A tamper-evident, append-only bandwidth ledger: hash-chained entries, each
Ed25519-signed, verifiable offline with the public key alone. No consensus, no
currency, no incentives — by design (see docs/NOT-IN-THIS-VERSION.md).
