# pkg/dht

UDP 上的 Kademlia 分布式哈希表。服务端用它发布"哪个节点能服务某个具名代理"，
客户端据此在没有中心目录的情况下找到彼此。

A Kademlia distributed hash table over UDP: servers advertise which nodes can
serve a named proxy, and peers find each other without a central directory.
