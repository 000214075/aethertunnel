# pkg/vpn

三层隧道：tun 设备（Linux 的 /dev/net/tun、Windows 的 Wintun、macOS 的 utun，
以及 `NewFromFD` 包装平台壳交来的描述符）、IP 包校验与路由、隧道会话与统计。

The layer-3 tunnel: tun devices (Linux, Windows Wintun, macOS utun, and
`NewFromFD` for a descriptor the platform shell hands over), packet validation
and routing, the tunnel session and its stats.
