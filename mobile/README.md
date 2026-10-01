# 移动端 · Mobile

本目录是移动端的接缝：Go 侧的绑定在 `pkg/mobile`，Android 工程在这里。

## 结构

| 路径 | 内容 |
| --- | --- |
| `../pkg/clientlib` | 客户端实现的库形态：`Run(ctx, cfg, logger)` 一个入口 |
| `../pkg/mobile` | gomobile 绑定：`Run(configTOML string) error` / `Stop()`，配置直接以 TOML 字符串内嵌 |
| `android/` | 最小 Android 工程（Kotlin，无 androidx 依赖）：一个配置输入框、Start/Stop 两个按钮、一块日志区 |

App 本身刻意保持最小：它只做 UI 与线程，隧道的一切都在 `pkg/clientlib` 里——同一个客户端
命令行在跑的东西，App 里跑的就是它。Go 的日志输出被 gomobile 路由到 logcat，屏幕上的日志区
显示启动、停止与错误。

## 构建

CI（`.github/workflows/mobile-app.yml`）在 ubuntu-latest 上完成全部两步，产物挂在 Release 页：

```bash
# 0) 安装与钉住的 x/mobile 同版本的 gomobile/gobind
#    （gomobile bind 要求被绑定包的模块图里有 golang.org/x/mobile，而其最新版会强制
#    宿主模块的 go 指令升到 1.26；因此绑定从一个一次性包装模块运行，主模块 go.mod 不动）
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260821151724-5c0595f4cdbb

# 1) 包装模块：replace 指向本仓库，一个只有空导入的桩文件固定模块图
mkdir -p /tmp/binder && cd /tmp/binder
cat > go.mod <<'EOF'
module aethertunnel/binder

go 1.25.0

require (
	github.com/aethertunnel/aethertunnel v0.0.0
	golang.org/x/mobile v0.0.0-20260821151724-5c0595f4cdbb
)

replace github.com/aethertunnel/aethertunnel => /path/to/aethertunnel
EOF
cat > binder.go <<'EOF'
package binder

import (
	_ "github.com/aethertunnel/aethertunnel/pkg/mobile"
	_ "golang.org/x/mobile/bind"
)
EOF
go mod tidy

# 2) 绑定 AAR（需要 ANDROID_NDK_HOME；-checklinkname=0 是 pion 的接口枚举助手
#    用 go:linkname 带来的官方解法，见 pkg/mobile 的文档注释；NDK 27 的最低
#    API 由 -androidapi 21 满足）
gomobile bind -target=android/arm64 -androidapi 21 -javapkg=io.github.aethertunnel \
  -ldflags='-checklinkname=0' \
  -o /path/to/aethertunnel/mobile/android/app/libs/aethertunnel.aar \
  github.com/aethertunnel/aethertunnel/pkg/mobile

# 3) App 的 debug APK
gradle -p /path/to/aethertunnel/mobile/android assembleDebug
```

产物：

- `aethertunnel-mobile-android-arm64.aar` — 把客户端嵌进你自己的 Android 工程用的绑定包；
  调用 `Mobile.run(configTOML)`（在工作者线程上）与 `Mobile.stop()`。
- `aethertunnel-app-android-arm64-debug.apk` — 本目录的 App 骨架装出来的 debug 包
  （arm64 设备；debug 签名，不可用于分发）。

## iOS

`pkg/mobile` 与 `pkg/clientlib` 在 iOS/arm64 的库级编译由 CI 逐次验证（`go build ./pkg/...`
在 GOOS=ios 下）。一个 iOS 可执行文件必须用 Apple 的 cgo 工具链链接——那是 Xcode/gomobile
app 工程自己的步骤，本仓库的 Linux CI 做不了，也装不出来。在 macOS 上：

```bash
gomobile bind -target=ios/arm64 -o AetherTunnel.xcframework ./pkg/mobile
```

## 有意不做的

- **上架商店的第一方应用**：签名、审核与开发者账号不是代码能替的。想要自己的入口，用
  AAR 包一个壳即可。
- **设备级 VPN**：Android 的 VpnService 与 iOS 的 NetworkExtension 各要一套 JNI/Swift 壳与
  真机验证；`[vpn]` 在 android/ios 上的运行时行为是 `device_other.go` 的明确报错
  （“此构建没有 tun 实现”）。手机上不用装任何东西也能用本程序：服务端发布一个 `socks5`
  出口，任意 SOCKS5 客户端指向它即可。
