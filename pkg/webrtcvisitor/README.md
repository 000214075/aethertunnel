# pkg/webrtcvisitor

WebRTC 访客数据路径：私有代理的流量走 DTLS 加密、ICE 选路的 DataChannel，
信令走已认证的控制连接。

The WebRTC visitor data path: a private proxy's traffic rides a DTLS-encrypted,
ICE-routed DataChannel; the signalling rides the authenticated control connection.
