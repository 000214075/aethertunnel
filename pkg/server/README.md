# pkg/server

服务端实现：会话与认证、代理池与负载均衡、访客请求（含 WebRTC 数据路径）、
虚拟主机与 tcpmux 共享端口路由（`vhost.go`、`tcpmux.go`）、面板 API、审计日志
与带宽账本写入。

The server: sessions and authentication, the proxy pool with load balancing,
visitor requests (including the WebRTC data path), the shared vhost and tcpmux
routing (`vhost.go`, `tcpmux.go`), the panel API, the audit log and the ledger
writes.
