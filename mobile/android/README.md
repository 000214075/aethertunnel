# mobile/android/

最小 Android 工程：一个 Activity、一个 VpnService、一份 Gradle 脚本，没有 androidx
依赖。它给 `pkg/mobile` 的绑定一个现场（`app/libs/` 放 gomobile 产出的 `aethertunnel.aar`），
也让「设备级 VPN 真的能跑」有一个可执行的证明——在模拟器上从设备 ping 通服务端的隧道地址。

A minimal Android project: one Activity, one VpnService, one Gradle script, no androidx
dependency. It gives the `pkg/mobile` binding a place to live (`app/libs/` holds the AAR
gomobile produces) and gives the on-device VPN claim something runnable — the emulator
pings the server's tunnel address from the device.

| 文件 | 作用 |
| --- | --- |
| `app/src/main/java/io/github/aethertunnel/app/MainActivity.kt` | 界面：TOML 配置框、Start/Stop/VPN 三个按钮、日志区；启动时预热绑定并强制加载 `libgojni`，让绑定缺失立刻可见而不是等第一次点击 |
| `app/src/main/java/io/github/aethertunnel/app/TunnelVpnService.kt` | VpnService 壳：`OpenTun` 里用 `Builder` 建接口（子网模式或全设备路由 + 解析器）、`establish()` 交出 fd，`ProtectSocket` 保护隧道自己的套接字 |
| `app/build.gradle.kts` | debug 构建；脚本里没有 ABI 配置——ABI 取决于 gomobile 以 `-target=android/arm64,android/amd64` 产出的 `app/libs/aethertunnel.aar`（含 arm64-v8a 与 x86_64 两份 `libgojni.so`） |
| `app/src/main/AndroidManifest.xml` | 声明主 Activity 与 `VpnService`（由系统以 `BIND_VPN_SERVICE` 绑定，不是声明式前台服务） |

构建与产物见上一级 [`../README.md`](../README.md)。

Build steps and artifacts are in [`../README.md`](../README.md).
