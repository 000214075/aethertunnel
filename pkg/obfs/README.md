# pkg/obfs

流量伪装。record 伪装逐字节改写、去掉帧头特征；`tls-session` 伪装做**真实的
TLS 握手**，证书为每连接现签的匿名自签证书——建模 TLS 会话的探测器看到的就是一个会话。

Traffic disguise. The record transform strips the frame header's signature; the
tls-session wrapper performs a real TLS handshake with a freshly minted anonymous
self-signed certificate per connection.
