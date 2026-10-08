# pkg/mobile

gomobile 绑定层，本身几乎没有逻辑：职责是让一个 Android（或 iOS）应用用两个函数驱动与
命令行**同一个**客户端实现，配置直接以 TOML 字符串内嵌。

The gomobile binding, which holds almost no logic of its own: its job is to let an
Android or iOS app drive the *same* client implementation the command line runs, with the
configuration embedded as a TOML string.

| 函数 | 作用 |
| --- | --- |
| `Run(configTOML)` / `Stop()` | 起一个客户端；配置直接以 TOML 字符串内嵌，走 `config.LoadString` 与文件同一条解析与校验路径。`Run` 在调用线程上阻塞，一个进程同时只跑一个客户端，第二次 `Run` 返回错误 |
| `RunVPN(configTOML, shell)` | 起一个三层隧道客户端；配置必须启用 `[vpn]`，壳的 `OpenTun` 每会话调用一次 |
| `PlatformVPN`（壳实现） | `OpenTun(mtu, address, prefix, subnet)` 建接口并返回 tun 描述符，`ProtectSocket` 为每条到服务器的套接字做保护，使全设备路由不会把隧道自己的流量捕获 |

`RunVPN` 的时序是关键：平台接口在**创建时**就要地址，而隧道地址由服务端在会话建立
之后才分配，所以壳是在"会话已认证、服务端应答里带着地址"的那一刻被调用的。
`logging_android.go` 把日志转到 logcat，所以 App 的日志区与 `adb logcat` 看到的是同一批行。

gomobile 可以把这里绑成 `aethertunnel-mobile-android.aar`；VpnService → fd → 隧道 → 内核
的逐包路径在模拟器上验证过（从设备 `ping` 服务端的隧道地址）。

The timing in `RunVPN` is the whole point: a platform interface is configured at creation
time while the tunnel's address arrives after the session is up, so the shell is called at
the moment both facts are known. The package binds into an AAR with gomobile, and the
packet path is verified on an emulator.
