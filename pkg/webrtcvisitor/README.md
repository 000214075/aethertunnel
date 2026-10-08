# pkg/webrtcvisitor

WebRTC 的访客数据路径：私有代理的流量走 DTLS 加密、ICE 选路的 DataChannel，信令走已经
认证过的控制连接——新增的这条数据路径因此没有引入新的信任假设，也不发布新的监听端口。

The WebRTC visitor data path: a private proxy's traffic rides a DTLS-encrypted, ICE-routed
DataChannel while the signalling rides the already-authenticated control connection, so
the new data path adds no new trust assumption and opens no new listening port.

| 文件 | 做什么 |
| --- | --- |
| `webrtcvisitor.go` | 建 PeerConnection、开 DataChannel，并把 offer/answer 交给控制连接。ICE 只用 host candidate：不配任何 STUN/TURN 服务器（`ICEServers` 是空表），也没有对应的配置键——写 `[webrtc]` 会被当成未知键 |
| `conn.go` | 把 DataChannel 包成 `net.Conn`，让上层的搬运代码不用知道下面是什么 |

客户端用 `transport = "webrtc"` 选择这条路径。功能套件里有一项是访客真的把数据走
DataChannel：服务端日志记 `data channel established`，客户端记 `direct true`，
调用方拿到自己的回显——三者必须同时对得上。

A visitor selects this path with `transport = "webrtc"`. The functional suite checks
that a visitor really moves its data over the DataChannel: the server logs
`data channel established`, the client logs `direct true`, and the caller gets its
echo back.
