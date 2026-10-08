# 平台支持与验证

六个发布目标：linux/amd64、linux/arm64、windows/amd64、windows/arm64、darwin/amd64、
darwin/arm64。本文件只讲事实：每个目标被**什么**执行过、证明了**什么**、以及**什么没有被
证明**。"能编译"与"能运行"是两件事，这里分开写。

## 1. 各目标的支持程度

| 发布目标 | 执行程度 | 说明 |
|---|---|---|
| linux/amd64 | 完整 | 原生执行：全套单测（含竞态检测）、真实 tun 设备上的三层隧道、端到端功能套件 |
| linux/arm64 | 完整 | 在真机 arm64 上跑过测试、构建、示例配置校验与端到端功能套件 |
| windows/amd64 | 完整 | 在真机 Windows 上跑过测试、构建、示例配置校验与 Windows 端到端套件 |
| darwin/arm64 | 部分 | 在真机 macOS（arm64）上跑过测试、构建与三层隧道的设备/状态检查，逐包路径见第 3 节 |
| darwin/amd64 | 交叉编译 | 只做过交叉编译与静态核对，未在任何机器上执行过 |
| windows/arm64 | 交叉编译 | 只做过交叉编译与静态核对，未在任何机器上执行过 |

功能检查覆盖：八种代理类型与三种访问者、共享 http/https 监听、代理池与六种负载均衡策略、
访客名单与服务端侧拒绝（`deny_cidrs`、令牌桶、封禁、`max_connections`）、带宽账本端到端、
目录（DHT）端到端、Kubernetes ConfigMap 的凭据契约、面板与指标的计数、审计事件、半关闭后
仍能收到应答、加密+后量子+TLS+身份+伪装四层同时打开时的一次真实传输。

想在本机复跑这些检查：`scripts/` 下的脚本按用途分组，见 [`../scripts/README.md`](../scripts/README.md)。

## 2. 平台差异（已写进对应文档）

- **Windows 上私钥文件没有 0600**：客户端 `--identity` 生成的密钥文件在 Linux 上是 `0600`，
  在 Windows 下没有 POSIX 权限位，实际保护来自目录继承的 ACL；`docs/SECURITY.md` 第 9.1 节
  按平台写明。
- **Windows 上 TOML 路径必须用字面量字符串**：`key_file = "Z:\...\k.key"` 会被 TOML 解析器以
  `invalid escape in string` 拒绝，必须写成 `key_file = 'Z:\...\k.key'`；`docs/CONFIGURATION.md`
  与本节都写明。
- **Kubernetes 清单的环境变量注入**：采用 `envFrom` 式注入的写法，见
  [`../deploy/kubernetes/README.md`](../deploy/kubernetes/README.md)。

## 3. 三层隧道与移动端

- 三层隧道（`[vpn]`）**只在 Linux 上逐包证明过**：真实 tun 设备、两端在两个网络命名空间里
  互相 ping（两个命名空间是必须的——否则内核会把隧道地址当成自己的地址，绕开隧道直接应答）。
  Windows（Wintun）与 macOS（utun）验证到设备创建、地址落卡与 `/api/vpn` 状态；同一台机器上
  两块网卡之间的包会被本机路由表抄近路，所以逐包路径以 Linux 为准，转发代码三个平台共用。
  其他平台启动 `vpn.enabled = true` 会明确报错退出。
- **Android**：绑定 AAR 与最小 App 的 debug APK 随发布产物提供；VpnService → fd → 隧道 → 内核
  的逐包路径在 Android 模拟器上验证过（从设备 ping 服务端的隧道地址，0% 丢包）。物理设备未
  核实，理由见 [`NOT-IN-THIS-VERSION.md`](NOT-IN-THIS-VERSION.md)。
- **iOS**：编到库级；可执行文件需要 Apple 的 cgo 工具链（gomobile/Xcode），在 app 工程里完成。
