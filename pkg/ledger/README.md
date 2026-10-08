# pkg/ledger

篡改可见的带宽账本：记录服务端为某个客户端搬了多少字节，条目构成哈希链并逐条 Ed25519
签名，仅凭公钥就能离线核验，还可以只导出到第 n 条的前缀单独交给审计方。账本只做可审计性，
没有共识、没有货币。

A tamper-evident usage ledger: what the server carried for a client, chained by hash and
signed entry by entry, verifiable offline against the public key alone — and a prefix up
to entry n can be handed to an auditor on its own. It provides auditability; there is no
consensus and no currency.

| 能力 | 命令/入口 |
| --- | --- |
| 追加与落盘 | `[ledger]` 段，JSONL，`GET /api/ledger` 发布公钥、链头与条目 |
| 离线核验 | `--verify-ledger <文件> --ledger-key <公钥>` |
| 单条前缀证明 | `--ledger-proof <文件> --proof-index <n>` |

改一个字节、换一串公钥、调换条目顺序或用别条的签名都会校验失败——这四种都在
`scripts/functional-linux.sh` 的账本一节里端到端跑过。**记的是服务端搬过的字节**：
`xtcp` 打洞成功后两端直连，那段流量不经过服务端，因此不在账本里；这一条也写进了
[`docs/SECURITY.md`](../../docs/SECURITY.md)。

没有任何共识、货币或激励：账本提供的是**可审计性**，不是去中心化，理由写在
[`docs/NOT-IN-THIS-VERSION.md`](../../docs/NOT-IN-THIS-VERSION.md)。

No consensus, no currency, no incentives: the ledger provides auditability, not
decentralisation, and the reasoning is in `docs/NOT-IN-THIS-VERSION.md`.
