# pkg/reliable

在 UDP 上承载有序字节流，让两个 NAT 后的端在约会服务器互相告知地址后直连
（xtcp 打洞的数据路径）。

An ordered byte stream over UDP, so two NATed peers can tunnel directly once a
rendezvous server has told them each other's address — the data path of xtcp.
