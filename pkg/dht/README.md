# pkg/dht

UDP 上的 Kademlia 分布式哈希表（160 位、k 桶、迭代查找）。服务端把「哪个节点能服务某个
具名代理」写成记录，客户端据此在没有中心目录时找到服务器；记录可以签名，读取侧可以要求
签名并只认指定公钥。

A Kademlia distributed hash table over UDP (160-bit, k-buckets, iterative lookup).
Servers write "which node serves this named proxy" as a record and clients use it to
find a server with no central directory; records can be signed, and a reader can require
that and trust only named keys.

| 文件 | 做什么 |
| --- | --- |
| `table.go` / `routing.go` | 路由表与 k 桶：节点的新鲜度、固定 160 个桶（按共同前缀长度定址，不做拆分）、桶满时的替换缓存与淘汰 |
| `store.go` | 记录存储：按键存值、TTL、容量上限 |
| `lookup.go` | 迭代查找：并行询问最近节点、收敛、超时 |
| `wire.go` | UDP 上的报文编解码 |
| `dht.go` | 节点本体：绑定端口、处理请求、对外接口 |

DHT 上的任何节点都能写同一个键，所以"记录是真的"这件事由
[`pkg/discovery`](../discovery) 的 Ed25519 通告签名负责，而不是靠 DHT 本身——
这个包只保证"能存能取"，不假装它是可信的。`wire.go` 的解析有 fuzz 目标，因为它直接
吃来自任意节点的 UDP 字节。

Any node can write the same key, so "is this record real" is answered by the Ed25519
announcement signature in [`pkg/discovery`](../discovery) rather than by the table
itself. The UDP parser is fuzzed, since its input comes from arbitrary nodes.
