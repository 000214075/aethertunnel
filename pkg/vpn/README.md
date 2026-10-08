# pkg/vpn

三层隧道：客户端从服务端领取一个地址，IP 包经控制连接转发；服务端是共享一张网卡的路由器，
而不是每条会话一块虚拟网卡。设备按平台打开（Linux 的 tun、Windows 的 Wintun、macOS 的
utun、平台壳交来的 fd），开不了就明确拒绝启动。

The layer-3 tunnel: a client is given an address and its IP packets travel on the control
connection; the server routes over one shared interface rather than a virtual adapter per
session. Devices open per platform (Linux tun, Windows Wintun, macOS utun, a
shell-provided fd), and the program refuses to start where none can be opened.

| 文件 | 做什么 |
| --- | --- |
| `device_linux.go` / `device_wintun.go` / `device_utun.go` / `device_other.go` | 各平台的设备打开：`/dev/net/tun`、官方 `wintun.dll`、utun 控制套接字；没有可用设备的平台**明确拒绝启动** |
| `device_fd.go` | `NewFromFD`：包装平台壳（Android 的 VpnService、iOS 的 NetworkExtension）交来的描述符 |
| `ip.go` | IP 包的校验：版本、长度、头部校验和——进隧道前就拦掉畸形的包 |
| `router.go` | 地址池与虚拟网卡之间的路由：谁拥有哪个地址、包该发给谁 |
| `tunnel.go` | 一条会话的隧道状态：队列、统计、关闭 |
| `pool.go` | 地址分配与回收 |
| `transport.go` | 把包写进控制连接、从控制连接读出 |

`ip.go` 与整体输入路径有 fuzz 目标：进隧道前解析的每一个字段都吃不可信字节。
Linux 上的逐包路径由 `scripts/vpn-linux-test.sh` 在真实 tun 设备、两个网络命名空间里
证明（互相 ping、核对两侧接口计数）；Windows 与 macOS 验证设备创建、地址落卡与
`/api/vpn` 状态；Android 由模拟器逐包验证。

Every field parsed before a packet enters the tunnel eats untrusted bytes and is
fuzzed. The per-packet path is proven on real devices — Linux in two network
namespaces, Android on an emulator, Windows and macOS for device creation and
addressing.
