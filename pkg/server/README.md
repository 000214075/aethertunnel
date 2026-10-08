# pkg/server

服务端实现。`server.go` 是控制连接的入口，其余文件各管一条能力：会话与注册、代理池与
负载均衡、访客请求（含 WebRTC 数据路径）、虚拟主机与 tcpmux 的共享端口路由、SSH 隧道网关、
打洞会合、面板 API、指标、审计与带宽账本写入。

The server. `server.go` is the control connection's entry point and every other file
owns one capability: sessions and registration, the proxy pool with load balancing,
visitor requests (including the WebRTC data path), shared-port vhost and tcpmux routing,
the SSH tunnel gateway, hole-punch rendezvous, the panel API, metrics, the audit log and
the ledger.
