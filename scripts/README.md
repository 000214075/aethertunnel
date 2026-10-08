# scripts/

构建、验证与运维脚本。每个脚本自己起服务、自己收尾，跑完打印通过项数，失败时点名是哪一项。

### 构建与发布 · Build and release

| 脚本 | 作用 |
| --- | --- |
| `build-release.sh` / `build-release.ps1` | 出 12 个产物（linux/darwin/windows × amd64/arm64）与 `dist/SHA256SUMS`；`.ps1` 供没有 make 的 Windows 用 |
| `race-toolchain.sh` | 没有 C 编译器时把一份工具链解到缓存目录，`make test-race` 用它，不需要 root |
| `emulate-linux-arm64.sh` | 交叉编译 arm64、在 qemu 上跑整套单测，再把包装脚本交给功能套件——没有 arm64 机器时验证 arm64 |
| `verify-release-linux.sh` | 在真实内核上核对已发布的 Linux 二进制 |

### 功能与平台验证 · Functional and per-platform checks

| 脚本 | 作用 |
| --- | --- |
| `functional-linux.sh` | 主力套件：起本地服务与自签证书，拉起服务端、发布方客户端与三个访问者客户端，逐项验证八种代理类型、共享监听、代理池、访客、DHT、加密/TLS/身份/伪装、面板 API 与审计。不需要 root |
| `smoke-test.ps1` | Windows 上的对应套件：同一批能力，另加真实停止信号驱动的优雅关闭、代理池与多路径、审计轮转、`--reject-unknown-keys` |
| `wine-check.sh` | 让同一套检查在 Linux 上执行真实 PE（Windows 二进制）：按参数决定每一侧走 Wine、qemu 还是直接执行 |
| `panel-checks.py` | 在**真实浏览器**里驱动面板：令牌、每个数字与 API 是否一致、两种语言的词典、700 px 与 360 px 版式、无障碍树 |
| `vpn-linux-test.sh` | 真实 tun 设备上的三层隧道：两端放进不同网络命名空间互相 ping，核对接口计数、`/api/vpn`、`/api/config` 与审计。需要 root 与 `/dev/net/tun`，缺了就打印原因并以 0 退出 |
| `vpn-windows-test.ps1` / `vpn-macos-test.sh` | Windows 与 macOS 上验证设备创建、地址落卡与 `/api/vpn` 状态 |
| `kubernetes-linux.sh` | 在只属于它的单节点 k3s 上 apply `deploy/kubernetes/`，再从集群外驱动一遍。需要 root、docker 与一个 k3s 二进制，缺什么就说什么 |
| `smoketest/` | 功能套件共用的辅助程序，现场编译后随真实二进制一起运行 |

每个脚本在缺前提条件时打印**缺什么**并以 0 退出，不会把环境问题误报成失败；
功能套件只绑回环端口，所以在本机可以直接跑。

Every script names the prerequisite it is missing and exits 0 rather than misreporting an
environment problem. The functional suites bind loopback ports only, so they run on a
laptop as they do anywhere else.
