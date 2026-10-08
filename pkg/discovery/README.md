# pkg/discovery

在 [`pkg/dht`](../dht) 之上发布与解析代理名：客户端只需要知道代理名，不必配置服务端
地址；服务端把自己的代理写进 DHT，并在代理下线时撤回。读取侧可以要求通告必须签名，也可以
只认指定的几把公钥。

Publishes and resolves proxy names on top of [`pkg/dht`](../dht): a client needs only the
proxy's name, not the server's address; a server writes its proxies into the DHT and
withdraws them when they go down. A reader can require signed announcements and trust
only the keys it names.

| 文件 | 做什么 |
| --- | --- |
| `discovery.go` | 记录的形状、发布/解析/撤回、通告签名与读取侧的信任策略（`require_signed` 拒绝无签名记录，`trusted_keys` 只认指定公钥） |

DHT 本身不保证写入者是谁，所以服务端用 Ed25519 给每条通告签名，读取端据此判断
"这条记录是不是我信的那台服务器写的"。改一个字段或换一把密钥都会校验失败——
这正是 `scripts/functional-linux.sh` 里目录那几节核对的事，也是服务端的
`--dht-key` 存在的理由（把公钥交给客户端填进 `trusted_keys`）。

The table cannot say who wrote a key, so the server signs every announcement with
Ed25519 and a reader decides whether it believes the writer. One altered field or a
different key fails verification — which is what the directory checks in the functional
suite exercise.
