# 更新日志 · Changelog

本文件记录每个版本改了什么。

> **关于 v3.1.0 之前的条目**：旧版本宣称 WebRTC、DHT、区块链、抗量子加密、AI 路由等
> 20 项功能，并声称在 14 个平台上产出 28 个二进制。对全部 Go 源码检索这些关键词，
> 出现次数为 0，构建脚本指向的是不存在的目录。v3.1.0 之前的详细功能清单已删除，
> 只保留版本号与日期。v0.1.x 到 v3.0.0 期间完成的是配置、协议与面板的骨架，
> 核心链路在本版本之前未跑通。

---

## [1.0.0] — 2026-09-29

### 新增

- **客户端成为库，移动端绑定随之落地**。客户端实现整体移入 `pkg/clientlib`：`Run(ctx, cfg,
  logger)` 一个入口承担加密协商、身份装载、DHT 解析等待与重连循环；`client/` 只剩旗标解析、
  配置装载与信号处理，行为与原先一致。`pkg/mobile` 用 `Run(configTOML)` / `Stop()` 两个函数
  把同一个客户端交给 gomobile 这样的绑定工具，配置可以直接以 TOML 字符串内嵌
  （`config.LoadString` 与 `Load` 共享同一条解析与校验路径）。CI 新增 mobile 作业：
  android/arm64 编出**完整客户端可执行文件**（pion 依赖的接口枚举助手用 go:linkname，按其
  官方解法加 `-checklinkname=0`），iOS/arm64 编到**库级**（iOS 的可执行文件按平台规则必须用
  Apple 的 cgo 工具链，属于 app 工程的步骤）。客户端的版本打桩随实现移到 `clientlib.Version`，
  `scripts/build-release.sh` 同步，`--version` 输出不变。设计边界里“移动端 App”一项据此
  收窄为“商店应用与设备级 VPN”。
- **Android 产物落地：绑定 AAR 与最小 App 的 debug APK**。CI 的 mobile 作业在运行器上用
  gomobile 把 `pkg/mobile` 绑成 `aethertunnel-mobile-android.aar`——在自己的 Android
  工程里调用 `Mobile.run(configTOML)` / `Mobile.stop()` 就是驱动完整客户端；随后构建
  `mobile/android` 里的最小 App（Kotlin、无 androidx：TOML 配置框、Start/Stop、日志区）出
  debug APK。绑定从一次性包装模块运行：`gomobile bind` 要求被绑定包的模块图里有
  golang.org/x/mobile，而其最新版会把宿主模块的 go 指令顶到 1.26——包装模块以 replace 指向
  本仓库、钉住与此工具链兼容的 x/mobile 版本，主模块 go.mod 一行未动；NDK 27 的最低 API 由
  `-androidapi 21` 满足。两个产物在打版本标签时挂上 Release 页，`mobile/README.md` 写明
  本地复现步骤与 iOS 的对应命令。
- **设备级 VPN 的代码落地**。`pkg/vpn` 新增 `NewFromFD`：包装平台壳建好的 tun 描述符；
  `pkg/clientlib` 新增 `RunWithShell`：在会话已建立、服务器已分配地址的那一刻向壳要接口
  （`openDevice` 回调收到 MTU/地址/前缀/子网——这是 Android 的 VpnService 唯一能正确配置
  接口的时刻），并为所有到服务器的套接字挂 protect 钩子（`net.Dialer.Control`），保证隧道
  自己的流量不被自己喂的接口捕获；`pkg/mobile` 新增 `RunVPN(configTOML, shell)` 与
  `PlatformVPN` 绑定接口；`mobile/android` 的 `TunnelVpnService` 实现壳这一半
  （`VpnService.Builder` 建接口、子网或全设备路由、protect、全模式解析器），App 的界面加
  VPN 按钮、全隧道开关与系统的授权流程。绑定与 APK 改为 arm64 + x86_64 双 ABI（模拟器与 x86 的 Android 设备、Chromebook 可装），
  App 启动时预热绑定、强制 libgojni 加载；CI 新增 e2e 作业：KVM 加速的无头模拟器装 APK、
  启动界面、核实进程存活且 logcat 无崩溃；并在运行器上跑真实服务端、经 appops 预授权
  VPN、启动隧道服务，从设备 ping 服务端的隧道地址——3 发 3 中、0% 丢包，VpnService→
  fd→隧道→内核的逐包路径在 Android 运行时上得到验证（首次 establish 被系统拒绝后由
  重连循环自动恢复，也在真实序列之内）。Go 侧单测覆盖 fd 设备、protect 钩子、`RunVPN`
  约束与会话结束时关闭壳描述符的契约；真机逐包路径未验证，设计边界文档如实记录。
- **对标 frp 的转发治理能力**。`[[proxies]]` 新增 `bandwidth`（客户端侧令牌桶限速，
  十进制字节每秒，双向合计）、`proxy_protocol = "v1"`（服务器把带访客真实地址的
  PROXY protocol 头插到本地服务收到的流最前面，ProxySpec 随注册携带，旧服务端安全
  忽略）、`remote_ports`（端口段展开成多个代理，名后缀为端口号）、`plugin`（
  `static_file` 把目录以 HTTP 发布、支持 basic auth；`unix_domain_socket` 拨本地
  套接字）与 `health_check`（tcp/http 探活，连续失败后拒绝拨号、恢复后自动放行；
  协议没有注销消息，端点保持注册，这是新旧混跑的取舍）；服务端新增
  `[server].allow_ports`（注册阶段拒绝范围外的 remote_port 并记入审计）。协议 JSON
  新增字段对旧端向后兼容。功能套件从 285 项扩到 **292 项**（无浏览器 149 项）：端口段
  两端可通、插件目录穿过隧道取回文件、后端原样收到 PROXY 头、限速把 40 KB 压到五秒
  以上、allow_ports 拒绝时两端日志与审计一致。新增 `docs/VS-FRP.md` 与 frp 的逐项对照。
- **对标 frp 第二轮：子域名托管、经代理连服务器、健康检查升级为真摘除**。`[[proxies]]`
  新增 `subdomain`：客户端只声明一个 DNS 标签（服务端做大小写归一与格式校验），服务端
  把它拼成 `<标签>.[server].subdomain_host` 并按该域名路由，声明之后代理名不再参与
  域名拼接；服务端没有 `subdomain_host` 时注册被拒绝并说明缺哪个设置。`[client]` 新增
  `dial_via`：到服务器的控制/数据连接经 socks5/socks5h/http/https 中转代理建立，
  `http` 用 CONNECT 方法（含 basic auth），`https` 对中转本身再做一层 TLS；隧道自身的
  加密与认证全部加在中转之上，中转只看到不透明的 TLS 形状流量。协议新增
  `ProxyWithdraw`/`ProxyWithdrawAck`（类型 24/25）：健康检查连续失败后，客户端把该代理
  从服务端**注销**，公开端点随之关闭，探活恢复后用原注册参数重新发布；会话重建时会把
  注册时仍处于失败状态的代理再次注销（注销确认与读循环的死锁在这里修掉——注销改在
  自己的 goroutine 上等确认）。旧服务端对注销保持沉默，客户端据此回退为拒绝拨号、
  代理保持注册，混合部署行为不变；注销一次性学习，之后不再重复尝试。单测覆盖注销
  确认/沉默回退/重注册/仅摘除不健康代理、CONNECT 与 SOCKS5 握手的线上字节、标签与
  dial_via 的校验、服务端的子域名合成与未知代理注销的拒绝应答；功能套件新增一节
  （一台服务器、两个客户端、一个可查日志的 CONNECT 中转、一个可杀可复活的后端），
  总数到 **301 项**（无浏览器 158 项）。`docs/VS-FRP.md` 同步：健康检查与
  `transport.dialServerProxy`、`subdomain_host` 从"未做"移入"都有"。
- **对标 frp 第三轮：HTTP 基本认证、tcpmux、SIGHUP 热重载**。这次先把 frp 源码克隆下来
  逐处研读（`server/proxy/tcpmux.go` 的 CONNECT 多路复用、`pkg/util/vhost/http.go` 的
  401 形状、`client/config_manager.go` 的重载路径），再按本项目的结构落地。`[[proxies]]`
  新增 `http_user`/`http_password`（仅 http/https）：服务器在转发之前用常数时间比较
  Authorization 头，不匹配回 401 并带 `WWW-Authenticate: Basic` 挑战，与浏览器的提示
  流程一致。新增 `tcpmux` 代理类型与 `[server].tcpmux_port`：访问者对共享端口发一条
  `CONNECT 主机名:端口`，服务器按主机名选中隧道、回 200，之后的字节就是这条隧道的——
  一个端口发布任意多条 TCP 服务；主机名查不到回 404，非 CONNECT 回 405，不泄漏端口后面
  有哪些名字；访客名单、审计与指标沿用原有路径。客户端新增 SIGHUP 热重载：配置来自文件
  时，信号触发重读与校验，按名差异增删代理（注销/注册走已有的 Withdraw 原语，会话未连上
  时新代理在会话恢复后自动发布）、健康检查与 static_file 插件缓存跟随代理启停、访客
  监听按整组重启（重绑定容忍旧监听器收尾的短暂窗口）；会话级设置（`[transport]`、
  `[encryption]`、`[identity]`、`[dht]`、`[vpn]`）的改动逐项点名"重启后生效"，不静默。
  配置校验补 `tcpmux` 的 multiplexer/端口规则、`http_user` 的适用类型，服务端策略的
  无效键清单同步。单测覆盖 401/放行、CONNECT 路由与 404/405、无主机名注册的拒绝、
  重载的线上收敛（注销+注册帧）、进程内真实 SIGHUP 端到端；功能套件新增一节，总数到
  **309 项**（无浏览器 166 项）。
- **`transport = "webrtc"`：私有代理的访客数据路径可以走 WebRTC DataChannel**。信令就是
  已经完成认证的控制连接（一次 offer 帧、一次 answer 帧，ICE 候选非渐进收集），数据本身是
  DTLS 加密、ICE 选路的 UDP 字节流——直连被墙的网络环境下数据路径的另一种形态。访客配置
  `transport = "webrtc"` 即启用；端到端测试用 pion 在进程内跑完整握手并验证字节穿过隧道。
- **`auth_method = "snark"`：私有代理的访客认证有了真正的 zk-SNARK**。这是设计边界里
  "zk-SNARK"那一项的实现：访客不再用 Schnorr 签名，而是用 Groth16 证明自己知道代理的
  secret——992 个约束的电路（MiMC 承诺 + 挑战绑定），证明只要几十毫秒，不含 secret 的任何
  信息，且绑定本次挑战、无法重放。服务端本就持有 secret，承诺由它自行推导，不需要新的注册
  配置。证明与验证密钥（合计约 170KB）由 `tools/snarksetup` 生成后嵌入二进制，可信设置的
  惯例（谁跑的 setup 谁掌握 trapdoor，可用同一工具重新生成并重建两端）写在 `pkg/snarkauth`
  的文档注释里。单元测试四项（正确密钥通过、错误密钥拒绝、重放拒绝、应答被篡改拒绝），
  变异检查（拆掉电路里的挑战绑定约束、重新生成密钥后测试立刻失败）确认电路是真的；端到端
  两个测试走完整握手。gnark 使 Go 的最低版本升到 1.25，CI 矩阵、Dockerfile 与文档同步。
- **三层隧道在 Windows 与 macOS 上打开真实设备**。Windows 加载官方 `wintun.dll`（本程序
  不安装驱动；DLL 缺失时拒绝并说明 remedy，创建适配器与配置地址需要管理员权限，报错都按
  事实命名）；macOS 打开 utun 控制套接字（打开无需权限，配置地址需要 root）。三个平台的
  转发代码完全共用，设备只是各自的接口缝。验证分层写明：Linux 在真实 tun 设备上**端到端**
  跑通（两端分处不同网络命名空间）；Windows 与 macOS 在 CI 的真实运行器上验证设备创建、
  服务端下发的地址落到网卡上、`/api/vpn` 上报会话、`vpn.require` 拒绝不在隧道上的客户端
  （新增 `scripts/vpn-windows-test.ps1` 与 `scripts/vpn-macos-test.sh`）——同一台机器上
  两块网卡之间的包会被本机路由表抄近路，所以逐包路径由 Linux 那套证明，文档里如实分开。
  `device_other.go` 的拒绝分支收窄到其余平台。
- **`disguise = "tls-session"`：把每条连接放进一个真实的 TLS 会话**。`tls-record` 只有记录头
  没有握手——这正是设计边界里"TLS 会话模拟"那一项存在的原因；这一版把它实现了：每条连接做
  一次真正的 TLS 1.2+ 握手（ClientHello、密钥交换、该有的告警一样不少），之后全部流量都是
  真实的 TLS 加密。服务端每条连接现签一张自签证书（两张证书之间没有可关联的公共位），建模
  TLS 会话的探测器看到的就是一个会话，而服务器保持匿名——证明"这台服务器是谁"仍是身份层
  的职责。`[obfuscation].disguise` 新增该取值，两端配置必须一致。单元测试断言握手真的发生、
  ALPN 是 `aethertunnel/4`、证书按连接更换、TLS 探针能完成握手、非 TLS 的对端被拒绝；变异
  检查（改掉证书 CN）确认测试抓得住伪造。功能套件新增一节（传输、探针、垃圾拒绝、日志报名
  共 4 项），总数到 **285**（无浏览器 142）。`obfs.Wrap` 现在接受一个端点参数（拨号端/监听
  端），因为握手有两个角色。

### 文档

- README 的能力表与设计边界随实现更新：TLS 会话模拟与"Windows/macOS 的 tun 设备"从设计
  边界移入能力表（边界从六项变为四项）；CONFIGURATION.md 写明 `tls-session` 的语义与它没有
  半关闭的取舍，以及三个平台各自的设备来源、权限要求与 wintun.dll 的放置方式。
- **README 按完成态重写**：「这一版有意不做的能力」整节与翻旧账段落改为一句中性的设计边界
  说明；逐平台验证行只保留实测结果；三层隧道标注为 ✅（Linux）；`docs/NOT-IN-THIS-VERSION.md`
  重构为「设计边界 · Scope and Non-Goals」。

- **`scripts/wine-check.sh`：把 Windows 那一栏搬进仓库，并在它上面跑出 71/71**。Windows 的数字
  一直来自一个不在仓库里的封装脚本（`docs/PLATFORMS.md` 第 4 节批评过这件事），而且从 3.1 那一轮
  起就卡住了：`kernel.apparmor_restrict_unprivileged_userns = 1` 让非特权用户命名空间不可用，
  Wine 的封装没法把解出来的 `share/wine` 绑到 `/usr/share/wine`，`wineserver` 读不到区域数据就
  退出。这一轮拿到 root 后两条路都通了：root 建的挂载命名空间不受那条限制，或者直接把
  `/usr/share/wine` 指向解包目录（这次用的后者，一条符号链接，用完可删）。新脚本要两个 PE 与一个
  wine 可执行文件，`WINEPREFIX`/`WINEDLLPATH`/`LD_LIBRARY_PATH` 由调用者给；脚本头部写明 Windows
  侧路径的写法（Wine 把 `/` 映射成 `Z:`，配置里要用 TOML **字面量**字符串，否则 `\` 会被当成
  转义）。它跑 **71 项**：命令行与示例配置（5）、控制端口与面板与指标（6，含面板拒绝计数 +1）、共享 http 端口与虚拟主机（2）、目录（5）、已占用的控制端口（3，见下面的缺陷）、
  已发布代理与双向字节（7）、半关闭之后仍收到应答（2）、私有代理与访问者（4）、socks5 出口与名单
  （4）、四层安全一起（9，含身份密钥在 Windows 上生成、四层各自在自己日志里报名、以及同一份身份
  密钥再次启动报出同一个公钥）、封禁窗口
  （6，含"被拒的客户端日志里没有会话、窗口过后才有"这两条**从未在 Windows 上执行过**的检查）、
  带宽账本（13，含经面板断开客户端后条目出现、错误公钥必须校验失败、`--ledger-proof` 的单条前缀
  单独可校验）、审计轮转（3，`max_bytes` 到量滚出第一代后当前文件继续在写）、`--reject-unknown-keys`
  （2，默认只是警告、加开关变成错误并点名未知键）。**71/71**。同一轮里它又扩成**按参数决定每一侧怎么跑**
  （`.exe` 走 Wine，否则直接执行，配置里的路径按各自那一侧写成 `Z:\...` 或 Unix 形式），于是
  2.1 节里一直标着"上一轮 22 项那套"的混合组合也能重跑了，并且扩到本机能执行的三种目标（windows/amd64、
  linux/amd64、qemu 下的 linux/arm64）的**全部九对组合，各 71/71**——包括 Windows 参与的四对混合
  组合，以及两对本机 ELF 的自检（用来把平台差异与脚本自身的问题分开）。arm64 那一侧是 `qemu-arm64:`
  前缀，用 `qemu-aarch64` 执行、不给 `-L`（二进制静态），路径仍是 Unix 形式。没跑到的也写明：
  `scripts/smoke-test.ps1` 要 PowerShell（本机没有 `pwsh`）；2.1 节那些**旧数字**没有替换（那是
  另一套、项数不同的检查）。
- **`scripts/race-toolchain.sh`：在没有 C 编译器、也没有 root 的机器上把 `go test -race` 变成一条
  可复跑的路径**。竞态检测需要 cgo，cgo 需要 C 编译器，而 `docs/PLATFORMS.md` 一直只以散文写着
  "可以按需解出 `gcc-14` 与 `libc6-dev`（同样不需要 root）"——照着做会卡住：**`gcc-14` 包里只有驱动
  程序**，真正的编译器 `cc1` 在 `gcc-14-x86-64-linux-gnu` 里，而驱动按编译期路径去 `/usr/libexec`
  找它。现在这段做法固定成脚本：`apt-get download` 收下 8 个包（`gcc-14`、
  `gcc-14-x86-64-linux-gnu`、`cpp-14-x86-64-linux-gnu`、`gcc-14-base`、`libgcc-14-dev`、`binutils`、
  `libc6-dev`、`linux-libc-dev`），`dpkg-deb -x` 解进一个缓存目录，写一个包装脚本按解出来的位置传
  `-B`（cc1/collect2/as/ld）与 `-isystem`/`-L`（libc 头文件与启动对象），**不用 `--sysroot`**，所以
  动态链接器仍指向系统那一份、跑出来的测试二进制照常执行；脚本在把路径交出去之前先编译并运行一个
  测试程序自检，路径走 stdout、进度走 stderr，缓存命中就直接返回。空缓存实测 6.4 秒（下载 42.8 MB
  + 解包 + 自检），`make test-race` 在没有给 `CC` 时自己调用它（`CC="$${CC:-$$(bash
  scripts/race-toolchain.sh)}"`），系统上本来就有编译器时脚本直接打印 `cc`。本轮证据：`go test
  -race ./...` 14 个包全过、**0 处数据竞争**；对并发最重的 `pkg/server`、`pkg/net`、`pkg/reliable`、
  `pkg/dht`、`pkg/discovery`、`client` 与根包再跑 `-count=3`（`pkg/server` 三次共 108.5 秒），同样
  0 处；根包与客户端在 `-race` 下也是全过。`docs/PLATFORMS.md` 第 3 节那条说明改成指向脚本（并写明
  `gcc-14` 不够这件事），新增 3.16 记录本轮的运行数字；README 两处 `make test-race` 的说明也补上。
- **`aethertunnel_data_connections_unmatched_total`：数据面配不上的连接现在有计数**。`DataOpen` 帧
  在任何认证之前就被分发，所以谁都能发一个"格式正确但指向不存在的东西"的帧：会话未知或已过期
  （`sessions.Get` 失败），或者会话是真的但没有访客在等这条流（`TakePending` 失败）。服务端每次都
  写入一条 `DataOpenAck{OK:false}` 作为拒绝，但此前两个计数器都没动、审计也没有——第二种情况里的
  答案只有发帧的人看得到，第一种除了服务端自己那行日志什么都没留下。控制端口的同类拒绝早已逐条
  记账（`refuseFirstFrame` 的注释就写着"服务端自己写下的答案，此前没有记下来"），数据面是最后一处
  只留日志的地方。现在这条计数覆盖这两类；**"客户端连不上自己本地服务"不算在内**：那种情况客户端
  在帧里自带 `Error`、等这条流的访客会立刻收到错误，属于正常结果而不是配不上的连接，把它混进来会
  让这条计数在本地服务挂掉时飙升、看起来像有人在扫数据面。`aethertunnel_data_connections_total` 的
  说明也补上它一直包含"后面没能配对"的那些。回归测试
  `TestADataConnectionTheServerCannotPairIsCounted`：手工发一个未知会话的 `DataOpen`（+1）、再用真实
  会话发一个没人等的 `StreamID`（再 +1）、最后让一个没装拨号器的 socks5 客户端报告"服务不了这条流"
  （计数必须**不动**，同时 `data_connections_total` 三种都 +1、`control_rejected` 一个都不动）；去掉
  任意一处计数会让对应断言失败（`the unmatched counter is 0 after a data-open with an unknown
  session, want 1` / `... with no waiting stream, want 2`），把第三类也计进去会失败（`the unmatched
  counter is 3 after a client reported it could not serve the stream, want it to stay at 2`）。
  `docs/CONFIGURATION.md` 的指标表补上这一行。
- **`--reject-unknown-keys`：把未知配置键从警告变成错误**。`config.ValidateOptions` 一直有
  `RejectUnknownKeys` 这个开关，`docs/CONFIGURATION.md` 的英文摘要还写着"`--check` fails when
  `RejectUnknownKeys` is set"，但两个可执行程序从来没有把它接到命令行上——这个开关此前只出现在
  `pkg/config` 自己的测试里，读者按文档去找也找不到"在哪里设置"。现在服务端与客户端都有
  `--reject-unknown-keys`，用法是 `--check --reject-unknown-keys`。`make check` 与 CI 的两个
  Linux 作业（linux/amd64 与矩阵里的 windows/amd64）都改成用它校验仓库里的两个示例配置，于是
  "示例配置有效"这一步现在也能抓住"在示例里多打一个键"：改之前 `--check` 对未知键只打一行警告并
  退出 0，往示例里加一个拼错的键不会让任何一步失败。实测：`server.toml.example` 与
  `client.toml.example` 在严格模式下退出码都是 0（两个文件本来就没有未知键，所以这一步可以放心
  收紧）；一个含 `bind_adr` 与 `pad_too` 的文件退出码为 1，并打印 `config … contains 2 key(s)
  this version does not understand: server.bind_adr, obfuscation.pad_too`，改回不带开关时它仍然
  只是"valid + 一行警告"。`docs/CONFIGURATION.md` 的用法段与 README 的"配置校验"一行都补上了这个
  开关。验收方式：不往 `scripts/functional-linux.sh` 里加检查——那份件数被 README 与
  `docs/PLATFORMS.md` 里多处逐次运行记录引用（281/138），而 CI 每一步都跑真实二进制、已经把这个
  开关端到端跑了一遍，开关背后的库行为由 `pkg/config` 的
  `TestUnknownKeysAreReportedNotIgnored` 与 `TestWarningsSurviveAValidationFailure` 覆盖。

### 修复

- **socks5 UDP 中继的"最久未用"驱逐在 Windows 上会选错对象**。`lastUsed` 原来用 `time.Now()`
  打点，而 Windows 的时钟粒度粗：同一毫秒内的"新建"与"再使用"拿到同一时间戳，驱逐的比较并列，
  实际驱逐谁退化为 map 的随机顺序。现在改为互斥锁下的单调使用计数器——驱逐是全序，与平台时钟
  无关（windows-latest 的真实 CI 先红后绿）。
- **Kubernetes 清单契约测试在 Windows 上恒失败**。容器内的路径用 `filepath.Join` 拼接，Windows
  上得到 `\etc\aethertunnel\server.toml`，与镜像真实的 `/etc/...` 永远不等；容器路径是 POSIX
  形状、与跑测试的平台无关，改用 `path.Join`。

- **Windows 上，控制端口绑定失败时报的是一个要自己去查的错误码**。同一个失败在两个平台上的读法
  完全不同：Linux 写 `bind: address already in use`（诊断就是结论），Windows 写
  `bind: winapi error #10048`——Go 的 net 包把 socket 错误原样带出来，而 Windows 的这些代码没有
  文本。原因在 Go 的 `syscall` 包本身：Windows 侧的 `syscall.EADDRINUSE` 是一个合成的
  `APPLICATION_ERROR` 值，与网络栈真正报的 `WSAEADDRINUSE`（10048）**永不相等**，所以按 errno
  分类在那一侧永远不会命中——这也是为什么这个缺口一直没被"到处都是现成的 errno 文案"掩盖住。
  现在 `pkg/net` 新增 `ListenError`/`ListenCause`：两类最常见的拒绝（地址已被占用、权限不足）
  在两个平台上都说出人话，第三种（地址本机没有）也一并认出，识别不了的错误保留 net 自己的文本并
  带上所请求的地址；服务端的控制端口、共享 http 端口、面板端口、代理的两类公开端口（tcp 与 socks5）、xtcp 的
  会合端口与客户端的两种访问者监听（tcp 与 udp）共八处绑定点全部走它。两个平台的现在读起来一样：
  `server stopped: listen on 127.0.0.1:34807: the address is already in use (another process is
  listening on it)`。回归测试 `pkg/net/listen_test.go` 直接喂 syscall 错误（不走网络栈，这样
  Linux 上也能测 Windows 分支的映射逻辑）：三种可识别的失败各有其话、不认识的错误保留原文并带上
  地址；两个变异（去掉地址占用的分支、识别出原因后仍保留 net 文本）都会让测试失败。Windows 侧的
  端到端证据是 `scripts/wine-check.sh` 新增的"已占用的控制端口"一节：修复前那条检查失败（输出里
  是 #10048），修复后通过。
- **一条被填上的空洞会让乱序缓冲里的一段数据永远留在那里**（`pkg/reliable`）。接收侧把乱序段按
  序号放在 `ooo` 里，等连续的一端走到它那里再取出来；取出的条件是**序号正好等于**空洞。重传并不
  按第一次发送时的分段边界到达——这是重传的常态——所以一段已经缓冲的数据可能跨越空洞：它起在空洞
  之前、伸到空洞之后。这时其中已经交付给读者的那一段必须先去掉，剩下的部分应该挂在空洞上，
  而不是继续挂在它原来的序号上。原来的代码只做"序号完全相等"的匹配，于是这种条目**永远匹配不上
  空洞**：它带着自己的字节留到连接关闭，`oooBytes` 一直计着它们，而对外通告的窗口
  （`recvWindow - 缓冲中的连续数据 - oooBytes`）就按这个量长期变小，`oooHigh` 也一直非零、让对端
  以为还有空洞而反复重传。极端情形是一条连接永久停摆。现在 `appendInOrderLocked` 在推进连续端之后
  先把这类条目收拾干净：已经被交付的部分丢掉，剩下的重挂到空洞上并在同一趟里被排空；如果重挂时
  空洞上已经有一个条目，留更长的那一个（两个条目描述的是同一起点之后的同一段流，短的那个是长的
  前缀）。回归测试 `pkg/reliable/stale_ooo_test.go` 三个用例：跨空洞的重传（缓冲 1100..1500、
  重传覆盖 1000..1300，要求 500 字节全部可读且 `ooo` 为空）、被完整覆盖的缓冲段（1100..1200 落在
  1000..1300 里，条目必须消失）、以及两个缓冲段在空洞处相遇（1100..1500 与 1300..1600，必须留下
  更长的那个）。三个变异都会让测试失败：去掉整段收拾、只删不重挂、重挂时不比较长短（第三个只有
  第三个用例会失败）。回归之后 `gofmt -l .` 无输出、`go vet ./...` 无输出、`go test ./... -count=1`
  14 个包全过、`scripts/functional-linux.sh` 281 项全过。
- **面板与 `/metrics` 上的令牌拒绝不留任何痕迹**。控制端口那边的每一种拒绝都进计数与审计，唯独
  面板监听器对"没带令牌 / 令牌不对"的请求只回一个 401：`auth_failures_total` 不动（它的 help 写着
  "a wrong token"，而这正是它）、`control_rejected_total` 不动、审计文件**一个字节都不长**、服务端
  **连一行日志都不写**。实测：对一个真在跑的服务端连发 5 个错令牌请求，五条都是 401，而
  `/metrics` 里两条计数都是 0、审计文件 0 字节。这个监听器**自己没有任何限流**（令牌桶、封禁与
  `max_connections` 都只作用于控制端口），而 `service.yaml` 把 7500 一并发布在 LoadBalancer 上——
  也就是说，一个扫到这个端口的人、或者一个令牌被轮换掉的 scraper，无论在指标、审计还是日志里都
  看不见。现在 `pkg/server/dashboard.go` 的 `withAuth` 与 `withMetricsAuth` 都走同一个
  `refuseUnauthorized`，把请求计入新增的 `aethertunnel_dashboard_unauthorized_total`；
  **只计数、不写审计**，理由写在代码与指标说明里：面板本身每隔几秒轮询若干端点，一次请求一行会
  让一个拿着过期令牌的客户端把痕迹埋掉（控制端口每条一次成立，是因为一条连接只对应一次拒绝）。
  `docs/CONFIGURATION.md` 的指标表与 `docs/SECURITY.md` 第 8 节同步（后者顺带写明 7500 被发布在
  LoadBalancer 上意味着令牌明文过网，需要部署方自己加 TLS 与来源限制）。回归测试
  `TestAnUnauthorizedDashboardRequestIsCounted`：五条覆盖 `/api/*` 与 `/metrics` 的拒绝各自计数、
  带对令牌的请求一条都不计数、`/healthz` 这类公开端点同样不计数，且 `auth_failures` /
  `control_rejected` / `control_connections` 三条控制端口的计数**都不动**（HTTP 请求不是控制连接）。
  三个变异都会让它失败（`/api` 路径不计数、`/metrics` 路径不计数、公开端点也计数）。容器内实测
  （20.10.24 与 29.2.1）：401 前后这条计数从 N 变到 N+1。
- **首帧读不出来时，那次拒绝只写进了日志：指标里是 0、审计里什么都没有**。这一条是容器的功能
  验证挖出来的：把服务端按 `deploy/kubernetes/configmap.yaml` 那份 `server.toml` 跑起来
  （`[obfuscation] disguise = "tls-record"`），再让一个**没启用伪装**的客户端连上去，服务端日志写着
  `handshake from 127.0.0.1:60300 failed: obfs: the stream is not carrying record-framed data: the first
  byte is 0x01, want 0x17 for an application-data record`，而 `/metrics` 的
  `aethertunnel_control_rejected_total` 还是 0、审计文件是空的。同一类缺口在这个仓库里已经补过两次
  （访客入口的拒绝、垃圾请求帧），`refuseFirstFrame` 的注释甚至写着"以前只有一行日志"——但"连首帧
  都读不出来"这一条漏在网外。它覆盖的正是最需要被看见的几种情况：伪装、加密或 TLS 两端不一致
  （TLS 握手是懒的，失败也落在同一次读上），以及一个连上什么都不发就离开的对端。现在
  `pkg/server/server.go` 新增 `refuseHandshake`：计入汇总 `aethertunnel_control_rejected_total`
  （`admit()` 的那几种拒绝同样是不回答直接关闭，也计入这里），计入新增的
  `aethertunnel_handshake_failures_total`，并写下 `handshake_failed` 审计记录，`detail` 就是日志里
  那句话（含 `handshakeFailureHint` 给出的设置提示）。两处 help 文本跟着改准确，
  `docs/CONFIGURATION.md` 的指标表与 `docs/SECURITY.md` 的事件列表同步。回归测试
  `TestAFirstFrameThatCannotBeReadIsCountedAndAudited`：伪装开着的服务端、一个不带伪装的客户端、再加
  一个连上不发的对端；三个变异都会让它失败（回到只写日志、只计数不审计、不移动汇总）。容器内实测
  （20.10.24 / 24.0.9 / 26.1.4 / 27.5.1 / 28.4.0 / 29.2.1 六个版本）：`handshake_failed` 记录里带着
  `record-framed`，两条计数都不是 0。这一轮的版本矩阵与十个场景记在 `docs/PLATFORMS.md` 的 3.18。
- **非 root 进程打不开 tun 设备时，报错只有一句 errno；清单里的 `[vpn]` 用法注释照着做会让 Pod
  起不来**。这两处都来自同一轮对容器的验证：把三个 docker 版本（20.10.24、28.4.0、29.2.1）并排
  起在同一台机器上，各跑一遍同一个 21 项场景（构建、`USER`、面板端口、指标、审计、ledger、
  隧道真的搬字节，再加 `--device /dev/net/tun --cap-add NET_ADMIN` 那一节），**63/63 全过**，
  版本之间没有差异——差异出现在 uid 上。以 uid 0 跑时 `/api/vpn` 报 `enabled: true`、接口真的
  建出来；以 **uid 65532** 跑时，同一个命令在三个版本上都只得到
  `vpn: TUNSETIFF for "aetvpn0": operation not permitted`。原因是内核在进程变成非 root 用户时
  清掉 permitted/effective 权限集合，而 docker 与 Kubernetes 都没有把它放进 ambient 集合，
  所以"给容器加 `NET_ADMIN`"对非 root 进程不生效。运维看到的只有一个 errno，而 Deployment 里
  那段注释写的恰好是这个办法（加 `NET_ADMIN`、挂 `/dev/net/tun`），照着做是一个起不来的 Pod。
  程序侧现在把这句话换成能照着做的说明：`pkg/vpn/device_linux.go` 新增 `tunOpenError`、
  `tunIoctlError` 与 `tunCapabilityHint`，`/dev/net/tun` 打不开时按 errno 分别指出"容器要拿到
  设备"（`--device /dev/net/tun`、hostPath CharDevice）或"需要 CAP_NET_ADMIN"，并**在 uid
  不是 0 时把它自己看到的 uid 与"要以 uid 0 跑隧道"一并写出来**；与设备、权限都无关的 errno
  不加提示，免得把人指向错误的设置。打开路径多了一个参数化的 `openTunDevice`，让"设备不存在"
  这一条能在有 `/dev/net/tun` 的机器上被测到。实测（容器内，20.10.24 与 29.2.1 各一次）：
  `cannot start server: vpn: TUNSETIFF for "aetvpn0": operation not permitted; opening a tun device
  needs CAP_NET_ADMIN (--cap-add NET_ADMIN in docker, securityContext.capabilities in Kubernetes),
  and a process whose uid is 65532 rather than 0 does not receive a capability the container adds,
  so run the tunnel as uid 0`。回归测试 5 条：两条纯函数检查（uid 0 与非 0 的提示必须不同、
  EINVAL/EMFILE 这类与权限无关的失败**不得**带上权限提示）、一条让真实 `Open` 在不提权的环境里
  走到 EPERM 并检查提示、两条驱动 `openTunDevice` 走 ENOENT 与 EACCES。变异验证：把任一处调用点
  换回不带提示的 `fmt.Errorf`、或让 `tunCapabilityHint` 忽略 uid、或给所有 errno 都加提示，
  对应断言都会失败。三个版本的完整结果记在 `docs/PLATFORMS.md` 的 3.17。
- **`deploy/kubernetes/deployment.yaml` 里那段 `[vpn]` 用法注释整块取消注释会与本容器已有的键
  重复**。它把 `securityContext:`、`volumeMounts:`、`volumes:` 都写在了同一个缩进上，而这三者
  在本容器里已经有两个（`volumes` 在容器里根本不是合法字段，它属于 Pod）。解析器遇到重复键取
  后一个，于是取消注释之后**容器会丢掉 `readOnlyRootFilesystem: true`、
  `allowPrivilegeEscalation: false` 和 state 挂载**——最后一条正好是
  `TestTheConfigOnlyWritesStateWhereTheDeploymentMountsIt` 防的那种"审计与 ledger 写进容器自己的
  文件系统、Pod 一重建就没了"。而 `runAsNonRoot: true`/`runAsUser: 65532` 不在那个块里，所以照着
  做即使键不重复也还是会得到 `TUNSETIFF … operation not permitted`（见上一条）。现在那段注释改成
  四条"对已有键的修改"（在 `securityContext.capabilities` 加 `NET_ADMIN`、在 `volumeMounts` 加
  一条、在 Pod 的 `volumes` 加一条、把 Pod 的 `runAsNonRoot`/`runAsUser` 去掉），并写明服务端会
  用哪一句话拒绝启动。新增 `TestTheVPNRecipeIsEditsRatherThanABlockThatDuplicatesKeys`：
  清单里不得出现可整块粘贴的 `# securityContext:`/`# volumeMounts:`/`# volumes:` 注释块，
  且注释里必须点到 `runAsUser` 与 `uid`（**只在注释行里找**：Pod 自己的 `runAsUser: 65532` 会
  让整文件检索白白通过，这一条在第一版里正是这样假过的）。变异验证：把注释改回可粘贴的块、
  或删掉那段 uid 说明，用例都会失败。

- **文档里"直接执行脚本"的写法在丢了可执行位的检出上会失败**。`Makefile` 的 `cross` 目标写的是
  `./scripts/build-release.sh`，README 与 `docs/MIGRATION.md` 写的是
  `sudo scripts/vpn-linux-test.sh …` / `sudo scripts/kubernetes-linux.sh …`；这些文件需要一个可执行
  位，而**这个检出里所有脚本都被记成 100644**（本仓库的 git 配置里 `core.fileMode = false`，
  `git ls-files -s scripts/` 逐条都是 100644），实测 `./scripts/build-release.sh` 直接报
  `权限不够`。发布流程其实早就撞上过这件事——`.github/workflows/release.yml` 在跑它之前先
  `chmod +x scripts/build-release.sh`——但那个绕行只救了 CI，`make cross` 与照着 README 敲命令的人
  仍然踩坑；仓库里 Linux 侧的其它调用（`functional-linux.sh`、`vpn-linux-test.sh`、
  `verify-release-linux.sh`）本来就写成 `bash scripts/…`，说明这已经是本仓库的习惯。现在这些调用统一
  走 `bash`：`Makefile` 的 `cross`（附一条说明为什么）、README 的两条 `sudo` 命令、
  `docs/MIGRATION.md` 的一条、`docs/PLATFORMS.md` 里三处 `emulate-linux-arm64.sh` 的用法行，并把
  `release.yml` 里那行 `chmod +x` 去掉（它绕的问题已经不存在了）。这样无论检出的模式位是什么都能跑。
  实测：`bash scripts/build-release.sh test-version` 退出码 0，`dist/` 里 13 个文件 = 12 个产物 +
  `SHA256SUMS`，与 README 的"12 个产物 + `dist/SHA256SUMS`"一致。
- **数值范围检查有的取决于所在段是否启用，而且同一个块里前后不一致**。`--check` 承诺"负数的时长
  与计数"会被拒绝，`docs/MIGRATION.md` 也照着这个说法写，但实际上有的范围检查与 `enabled` 无关
  （`audit.max_bytes`、`obfuscation.jitter_millis`、以及 `pad_to` 的**上限**），有的却被它挡掉：
  `obfuscation.pad_to = -1` 会被接受，而同一个键的上限检查就在 8 行之外、并不看 `enabled`；
  `[vpn] mtu = -1` 会被接受；`[dht] announce_ttl_seconds = 1`（迁移文档专门交代"升级前先把它改成
  2 或更大"的那个值）、`ttl_seconds = -1`、`republish_seconds = -1` 也都会被接受。后果是
  `--check` 对一份写着错值的文件报 "valid"，直到你把那个段打开才失败——而迁移文档给的恰恰就是
  "升级前跑一次 `--check`"这条路径，实测过：`[obfuscation] enabled = false` 配 `pad_to = -1`、
  `[vpn] enabled = false` 配 `mtu = -1`、`[dht] enabled = false` 配 `announce_ttl_seconds = 1`，
  三种都打印 "is valid" 并退出 0。现在规则统一为：**单个数值的范围检查与段的 `enabled` 无关**
  （`pad_to` 的负数、`vpn.mtu` 的 576–9000、三个 DHT 数值的负数与 `announce_ttl_seconds >= 2`），
  **需要该段真的在用的检查仍然只在启用时执行**（要能解析的地址与密钥，以及两个值之间的关系：
  未启用时 `republish_seconds = 0` 表示"按 TTL 推导"，无从与 `announce_ttl_seconds` 比较——这一条
  也是这条边界的技术依据）。这条边界写进了 `docs/CONFIGURATION.md` 顶部，并补在 `docs/MIGRATION.md`
  关于负数与 `announce_ttl_seconds` 的两条上。回归测试 `TestRangeChecksApplyToDisabledSections`
  （四个"段未启用但值写错"的用例必须被拒，外加一个"段未启用但值正常"的用例必须通过，防止收得太
  紧）；把任意一条范围检查重新挂回 `Enabled &&` 会让对应子用例失败（`expected an error containing
  "obfuscation.pad_to cannot be negative", got nil`）。仓库自己的配置——两个示例、Kubernetes
  ConfigMap、`scripts/vpn-linux-test.sh` 与功能脚本生成的配置——用的都是正常值，逐个看过，
  行为不变。
- **配置校验失败时，未知配置项与其他加载警告一起被丢掉**。`config.Load` 把"这个版本不认识的键"和
  "哪些凭据来自环境变量"收进 `cfg.Warnings`，但校验失败时返回的是 `nil, err`，调用方
  （`main.go`、`client/main.go`）先 `if err != nil { Fatalf }`，那些警告再也拿不到。README 承诺
  "未知配置项会**报出来**而不是静默忽略"，实际只在文件**其余部分都合法**时成立：一个文件里同时
  有拼错的键和缺的必填项时，`--check` 只报后者，使用者改完再跑一次才看到前者——而这两处通常就是
  同一个手误。现在 `Load` 在校验失败时连同它构建出的配置一起返回（只有这一条路返回非 nil 的配置，
  文档注释写明该配置不可使用、只用来取警告），两个入口都先打印 `cfg.Warnings` 再处理错误。实测
  （对服务端 `--check` 喂一个同时含 `[serverr]`、`bind_adr`、`BindPort`、`max_connection`、
  `pad_too`、`[nested]` 六个错处的配置）：改之前只输出三行校验失败；改之后先输出
  `contains 10 key(s) this version does not understand: …` 再输出那三行，退出码仍是 1。回归测试
  `TestWarningsSurviveAValidationFailure`（校验失败时返回的配置非 nil，警告里同时出现 `webrtc` 与
  `bind_adr`；同时保持 `RejectUnknownKeys` 那条路径仍以错误形式报告）；把返回值改回 `nil, err`
  会失败（`the rejected configuration is nil, so its warnings cannot be reported`）。
- **`Makefile` 声明了两个并不存在的目标**。`.PHONY` 里列着 `lint` 与 `release`，文件里却没有这两条
  规则，`make lint` / `make release` 只会得到 "No rule to make target"；README 也从没提过它们。现在
  补上 `lint`：先报出没有 gofmt 过的文件（`make fmt` 会写文件，不能拿来当检查），再跑
  `go vet ./...`——正是 CI 的 Linux 作业在测试前跑的那一对。gofmt 的路径由 `$(GO) env GOROOT` 推出，
  不假设它在 PATH 上：否则 gofmt 不在 PATH 时该命令会失败、输出为空，这条检查就**静默通过**了
  （本机在没有 PATH 的情况下实测到这一点，所以加了这一层）。`release` 从 `.PHONY` 去掉，它的等价物
  是 `cross`。两个 README 的 `make` 命令清单都补上 `make lint`，与 `Makefile` 保持一致。
- **访客连接与数据打开的"首帧不可用"没记在这条序列上**。控制端口上有三种帧类型可以起一个连接：
  认证请求（`TypeAuthRequest`）、访客连接（`TypeVisitorConnect`）、数据打开（`TypeDataOpen`）；三条都在
  任何认证之前被分发，所以载荷解析不了的帧就是一枚打向这个端口的垃圾帧，无论它自称是哪种。
  `aethertunnel_unusable_request_frames_total` 只在认证请求那一条计数：用访客连接或数据打开发垃圾
  的扫描在这条序列上显示为零，而它正是操作者用来把"垃圾帧扫描"和"策略拒绝"分开看的序列，
  `docs/CONFIGURATION.md` 的说明也只列了认证请求一种。现在访客连接的坏载荷在（上一轮已加上的）
  汇总计数与审计记录之外也记进这条序列；数据打开的坏载荷记进这条序列并补一条日志——它此前连
  日志都没有，连接被静默关闭，四类拒绝里只有这一种什么都不留。数据打开**不**进
  `aethertunnel_control_rejected_total`：那条计数说的是"控制端口上被拒绝的连接"，而数据连接从未
  成为会话，把它算进去会让"已接受 + 已拒绝"超过服务端作答过的连接数。回归测试
  `TestAMalformedVisitorOrDataFrameIsCountedAsUnusable`：访客坏载荷让不可用计数 +1、汇总 +1、审计里
  留下 `visitor_rejected` 记录；数据坏载荷让不可用计数再 +1，而汇总计数、已接受计数与数据连接计数
  都不动。删掉访客那处 `unusableFrames.Add(1)` 会失败（`the unusable-first-frame counter is 0 after
  a malformed visitor-connect, want 1`），删掉数据那处会失败（`... after a malformed data-open too,
  want 2`）。`docs/CONFIGURATION.md` 的指标说明与 `metrics.go` 的 help 文字相应改成列出三种载荷，
  并写明数据打开那一帧不计入汇总。
- **访客的拒绝几乎不进 `aethertunnel_control_rejected_total`，有几处也不进审计日志**。控制端口上
  两条入口共用一套规则：普通客户端的拒绝会同时进聚合拒绝计数、进审计日志，`refuseControl` 的注释
  把这条写成了不变式（"每一次已作答的拒绝都要动它"，因为这是原因无关时告警看的那条序列）。访客
  入口只在**令牌错误**那一条这么做了，其余十处——身份断言失败、代理不存在、代理不是私有的、传输
  类型不匹配、请求里没带抗量子密钥、抗量子协商失败、协商出的密钥不可用、NIZK 证明失败、
  `secret_key` 不通过、首帧不是一个可用的 `VisitorConnect`——都不计数；其中代理不存在、代理不是
  私有的、首帧不可用，以及抗量子相关的那三处（缺密钥、协商失败、密钥不可用），一共六处连审计
  记录都没有，全部不留痕迹。后果是：从访客入口扫代理名、扫 `secret_key`
  或用不匹配的传输类型试探，在这两条观察面上几乎不可见，聚合计数也不再等于"服务端作答过的连接"
  减"已接受的连接"。现在新增 `refuseVisitor`（计数 + 审计 + 在线上拒绝三件事一起做），十处接受
  之前的拒绝都走它，审计事件与拒给访客的理由文字保持不变；**接受之后**才发生的失败（中继建不
  起来：没有成员在发布该代理、应答写不回去等）仍只走 `rejectVisitor`，因为那条连接已经计入已
  接受，再记一次会让"已接受 + 已拒绝"超过服务端作答过的连接数。回归测试
  `TestAVisitorRefusalBeforeAcceptanceMovesTheRejectionCounter`（令牌错/代理不存在/`secret_key`
  错/传输不匹配四种拒绝各断言聚合计数恰好 +1、且不计为已接受）、
  `TestOnlyThePreAcceptanceRefusalIsBooked`（用 `net.Pipe` 直接区分两个 helper：接受之后的
  `rejectVisitor` 不动计数，接受之前的 `refuseVisitor` 恰好 +1）与
  `TestAnAcceptedVisitorIsNotCountedAsRejected`（握手成功的访客让已接受 +1、拒绝不动）；把
  `refuseVisitor` 里的计数删掉会让四条子用例全部失败（`the rejection counter is 0 after one
  visitor refusal, want 1`），把计数折进 `rejectVisitor`（看上去像顺手的整理）会让
  `TestOnlyThePreAcceptanceRefusalIsBooked` 失败（`the rejection counter moved to 1 for a refusal
  after acceptance, was 0`）。`docs/SECURITY.md` 第 7 节与 `docs/CONFIGURATION.md` 的指标表因此
  把这条计数与 `aethertunnel_control_rejected_total` 的 help 文字都改成"控制端口上被作答为拒绝的
  连接，含凭据不通过与访客入口的拒绝"。
- **`aethertunnel_auth_failures_total` 的说明比它实际计的东西窄**。这一条计数的是所有凭据类失败：
  错误的令牌、无效的身份断言、失败的抗量子密钥协商，以及访客在私有代理上 `secret_key` / NIZK
  证明不通过；但暴露在 `/metrics` 里的 help 文字一直写的是"Authentication attempts with an invalid
  token"，只提令牌一种。抓指标配告警或写规则时，这句话会让人以为身份断言与协商失败不会体现在这条
  序列上。现在 help 文字改成"a wrong token, a failed identity assertion, a failed key agreement, or
  a visitor's failed proof for a proxy's secret key"，与 `docs/CONFIGURATION.md` 里"凭据错误的认证
  尝试"那句一致。改的只是 help 文字，序列名、类型与取值都不变，`functional-linux.sh` 与
  `smoke-test.ps1` 都按序列名取值，不受影响。
- **换一种首帧就能绕过 `ban_after_failures`：访客入口不记认证失败**。控制端口上有两条入口都带着
  同一份凭据：`AuthRequest`（普通客户端）与 `VisitorConnect`（访客）。两条都检查同一个
  `auth_token`、同一份身份断言、同一套抗量子密钥协商，但只有前者在失败时调
  `recordAuthFailure`——而那是唯一施加封禁的地方，它自己的注释就写着"每一种认证失败都要算"。
  于是攻击者只要把猜测包成 `VisitorConnect` 帧，就可以无限次猜令牌、身份或协商密钥，永远不会被
  封；文档 §7 承诺的"认证失败达到次数后在握手前拒绝该来源"被这一条绕过。现在访客入口的五处
  凭据失败（令牌、身份断言、抗量子协商、NIZK 证明、`secret_key`）与成功后的清零都走
  `recordAuthFailure` / `recordAuthSuccess`，与控制连接一致。`ban_after_failures` 默认为 0
  （关闭），所以没有开启封禁的部署行为不变。回归测试
  `TestAVisitorThatFailsACredentialCheckIsBanned`（三种凭据失败各一条子用例，断言该来源真的进了
  封禁名单、计数也记了）与 `TestAVisitorThatAuthenticatesClearsItsFailureCount`（先失败两次、
  成功一次、再失败一次，阈值 3 时不应被封）；去掉 `recordAuthFailure` 会失败
  （`the list reports 0 banned sources after a visitor credential failure, want 1`），只去掉
  `recordAuthSuccess` 会失败（`a source that authenticated once and then failed once was banned`）。
  `docs/SECURITY.md` §7 的括号里补上访客的 `secret_key` / NIZK 证明，并写明用访客入口猜令牌不会
  绕过封禁。
- **文档把账本说成会覆盖 `xtcp` 直连的流量，实际上那部分根本到不了服务端**。`docs/SECURITY.md`
  第 9 节原来写"`xtcp` 按直连或中继的流"记账，读起来是直连那条流也会被计；但打洞成功后两端直接
  对话，服务端看不到任何字节，也**没有**任何机制会把它们补上——客户端只有上线、注册、心跳、数据与
  打洞这几类帧，没有上报用量的帧。所以那条直连流在账本里是 0 字节，指标的两个双向字节与按隧道的
  字节同样不含它；服务端能记下的只有这次尝试与它报告的结果。靠账本计费时这一点是实质性的：直连
  省掉的正是账本看不到的那部分。现在 `docs/SECURITY.md` 第 9 节把记账范围逐项写清（`tcp`/`stcp`
  按流、`udp`/`sudp` 按数据报会话、`http`/`https` 按请求、`xtcp` 按下**经服务端中继**的流）并单独
  说明直连流量为什么不在其中；`docs/CONFIGURATION.md` 的指标一节与 README 的账本一行也补上同一句；
  面板「账本」页那条说明（`ledger.note`，中英两处加静态回退文案）同样写明"账只记服务端搬过的
  字节"。行为本身没有改：现有测试已经钉住了"访客回报直连时服务端不中继"（`p2p_relayed` 为 0），
  这次修的是文档与面板的说法。
- **`sudp` 代理转发的数据报不计入任何计数器**。`aethertunnel_udp_datagrams_total` 的说明是
  "转发的 UDP 数据报"，`docs/CONFIGURATION.md` 的指标表也这么写，但实际上只有 `udp` 代理的数据报泵
  在加它（`startUDP` 里 `OnDatagram` 那个回调）；`sudp` 访客走的是 `relayDatagrams`，那条路上一个
  计数都没有，于是只搬数据报的 `sudp` 代理在 Prometheus 里恒为 0，而字节计数在涨——看这个序列的人
  会读成"隧道没在干活"。现在 `relayDatagrams` 收一个 `onDatagram` 回调，`pipeDatagrams` 用它把每个
  转发的数据报计进 `udpDatagrams`，与 `udp` 代理的语义一致（双向各计一次）。指标说明与
  `docs/CONFIGURATION.md` 一并写清：这一条覆盖 `udp` 与 `sudp`，socks5 出口的数据报有它自己那条
  `aethertunnel_socks5_udp_datagrams_total`；`aethertunnel_udp_sessions_active` 仍然只反映 `udp`
  代理按来源地址维护的会话（`sudp` 的访客是一条独立的流，不进这个数）。回归测试在
  `TestSUDPVisitorRelaysDatagrams` 里断言一次往返之后计数至少为 2；去掉那个回调会失败
  （`aethertunnel_udp_datagrams_total is 0 after one datagram round trip, want at least 2`）。与
  同类的 `streams_total` 那一条一样，这一处只加在集成测试里：功能套件已经用真实二进制覆盖了
  `sudp` 访客搬数据报这一段，而计数器本身在进程内就能断言，不必再多一项检查。
- **访客连接与三层路由的协程没有 panic 防护，一个坏输入能带走整个进程**。`docs/SECURITY.md` 的资源
  表写着"每个连接的处理器带 `recover`，panic 只关掉那条连接"，而实际上只有控制连接那条路径
  （`Server.handleConn`）有这道防护：`acceptLoop` 为每个访客 `go g.serveVisit(conn)`，那条协程上
  没有任何 recover，三层路由的读循环（`go router.Run(ctx)`）与每个对端的收/发方向（`Peer.serve`
  里各起一条协程）同样没有。上一版修掉的那个空 IP 包 panic 正是因为这个原因才是**进程级**的：包从
  客户端的控制连接进来，但 `Validate` 跑在路由的对端协程上，`handleConn` 的 recover 够不到它。
  现在这三处都加了防护，语义与既有的那道一致：`serveVisit` 记一行日志并关掉这条访客连接；
  `Router.Run` 记数并返回一个说明 panic 的错误（此前调用方会永远等下去，因为 goroutine 带着
  panic 死掉、`done` 上什么也不会来）；对端的收发方向各自记数并结束，`serve` 随即摘掉那个对端、
  释放它的地址，其余客户端的隧道不受影响。回归测试
  `TestVisitorHandlerSurvivesAPanic`、`TestStreamVisitorHandlerSurvivesAPanic`（把 `Read` 会
  panic 的连接交给 `serveVisit`，它必须正常返回）、`TestRouterRunSurvivesADevicePanic` 与
  `TestPeerLoopPanicDetachesOnlyThatPeer`（后一个还断言同路由上另一个对端仍在、被终结的地址可以
  被下一个会话重新占用）；去掉任意一道防护，对应的测试都会 panic 并带走测试进程。
- **空 IP 包让 `vpn.Validate` 越界 panic，整进程退出**。`Validate` 用 `Version(packet)` 分发，而
  `Version` 对空切片返回 0（长度不够，读不出首字节的版本 nibble），落到 `default` 分支后那句
  `fmt.Errorf(..., packet[0])` 去读一个不存在的字节——`index out of range [0] with length 0`。
  这条路径是**对端可达**的：`[vpn]` 启用时，控制连接上的 `TypeVPNPacket` 帧由
  `transport.Deliver(msg.Payload)` 直接送进隧道，而 `ReadFrame` 对长度为 0 的帧给出的 payload 是
  nil，于是 `Peer.receiveLoop`/`transportToDevice` 调 `Validate` 时进程崩掉。服务端 879 行那处没有
  任何长度检查，所以一个拿到隧道地址的客户端发一个空 payload 的帧就能带走整个服务端（连同上面所有
  其它隧道）；反过来服务端也能用一个空帧让客户端崩。现在 `Validate` 对空包返回
  `ErrMalformedPacket`（"the packet is empty"），与其它畸形包一样只记一次丢弃并继续。回归测试：
  `TestValidateAcceptsIPv6AndRejectsNonsense` 增加 `Validate(nil, …)` 与 `Validate([]byte{}, …)`
  两例；`TestTunnelDropsPacketsThatAreNotValidIP` 从**对端方向**投递一个 nil 包，断言它被丢弃、进程
  存活、计数加一。去掉这个判断会 panic（`index out of range [0] with length 0`）。
- **socks5 出口会把一个含方括号的域名当作目标发出去**。`ReadRequest` 与 `ParseUDPDatagram` 把
  域名地址（ATYP=3）的字节和端口拼成 `host:port` 用的是 `net.JoinHostPort`，而它只给**含冒号**的
  主机加方括号：域名 `a[` 会拼成 `a[:80`，这个串再用 `net.SplitHostPort` 是拆不开的
  （`unexpected '[' in address`）。目标串在本项目里处处用 `SplitHostPort` 读——服务端拿它比对代理的
  `allow_targets`、隧道另一头的客户端按它解析并拨号、应答再原样带回——所以一个访客只要发一个域名
  里带 `[` 的 CONNECT，服务端就会先把请求送进隧道、开出一条数据连接，客户端再回一句"目标不是
  host:port"：白开一条流，错误还指向客户端自己的解析。现在两处拼接都走 `joinHostPort`，含 `[` 或
  `]` 的主机在此被拒（`ErrBadAddress`，CONNECT 回 `ReplyAddressNotSupported`）。这两个字节本来
  就不可能是主机名的一部分，所以拒它不会拒掉访客真正想要的目标；会被加方括号、拆回来仍然是原样的
  名字（如 `a:b`）照常通过。回归测试
  `TestReadRequestRefusesADomainThatCannotBeHostPort`（两种命令 × 四个名字）、
  `TestParseUDPDatagramRefusesADomainThatCannotBeHostPort` 与
  `TestReadRequestResolvesTheAmbiguousDomainForms`（`a:b`、`example.com`、`xn--…` 都照常解析且
  拆得开）；去掉这个判断，下面的 fuzz 语料会失败。
- **网络入口的解析器此前没有任何模糊测试**。新增 14 个 fuzz 目标，覆盖本项目里所有**直接吃不可信
  字节**的解析器：`pkg/protocol` 的 `FuzzReadFrame`（控制端口，握手之前就能到）、
  `FuzzDecodePunchRequest` 与 `FuzzDecodePunchResponse`（会合端口，服务端唯一没有握手挡在前面的
  端口）、`pkg/socks` 的 `FuzzReadRequest` 与 `FuzzParseUDPDatagram`（公开的 socks5 端口，CONNECT
  与 UDP ASSOCIATE 各一条）、`pkg/dht` 的 `FuzzDecode`（UDP 端口）、`pkg/vpn` 的 `FuzzValidate` 与
  `FuzzSetIPv4Checksum`（对端写入的每个 IP 包）、`pkg/crypto` 的 `FuzzSchnorrVerify`（访客的 NIZK
  证明，从帧里直接读一个 P-256 点和一个标量）、`FuzzHybridServerFinish`（认证之前就解析的 X25519
  与 ML-KEM 公钥）、`FuzzCipherOpen`（开着加密时每个帧都要过的解密）与 `FuzzVerifyIdentity`（身份
  断言），以及 `pkg/obfs` 的 `FuzzRecordRead`（伪装流的记录读取）与 `pkg/reliable` 的
  `FuzzParseSegment`（打洞传输的数据报解析）。这些目标都带断言：解析成功的地址必须是可还原的
  `host:port`、解码出的 token 长度必须正确、`Validate` 接受的包必须版本可读且地址可解析、写好的
50→  IPv4 头校验和必须验证通过、验证通过的 Schnorr 证明换个上下文必须失败、协商成功的会话密钥长度
  必须是 32、解开的密文不会比输入长、解析出的数据报负载必须正好是去掉帧头与 MAC 之后的尾部。每个
  目标另有测试期用的内存 `net.Conn`（`fuzzConn`），因为用 `net.Pipe` 时解析器提前返回会让写端永久
  阻塞，一次一泄漏的 goroutine 会在找到问题之前先耗尽内存。
  上面第一条修复是 `FuzzValidate` 的种子语料（一个空包）在跑基线覆盖时就直接 panic 出来的；两条
  socks5 修复是 `FuzzReadRequest` 与 `FuzzParseUDPDatagram` 跑出来的，那两条失败输入已作为语料留在
  `pkg/socks/testdata/fuzz/`（`go test ./...` 会把语料当普通用例重放，所以它们不会再溜回来）。
  `pkg/crypto`、`pkg/obfs`、`pkg/reliable` 这几个新增目标各跑 40 秒、其余各跑 45 秒后全部通过，
  没有再找到问题。CI 的 Linux 作业新增一步，对每个目标跑 10 秒的有界模糊测试。
- **数据报泵在关闭与建链竞态时会漏掉会话记账、漏掉数据连接**。`pkg/net` 的 `DatagramPump` 有两处：
  一是 `deliver` 把新会话插进 `sessions` 并解锁之后才调 `OnSession(1)`，若此时泵正在关闭，关闭会先
  把该会话算进快照、调用 `OnSession(-1)` 并把它移出，随后那句 `+1` 就永久留在
  `aethertunnel_udp_sessions_active` 上（面板与指标从此多一个并不存在的会话）；现在 `+1` 与插入
  会话在同一把锁里完成，关闭要么看不到这个会话、要么在 `+1` 之后再减一。二是建链在别的 goroutine
  里开数据连接，若会话在连接开好之前就已结束（关闭、空闲回收、首次写失败），`closeFramers` 早已跑
  过且当时没有路径可关，`establish` 却仍把后到的路径装进会话——那条数据连接、以及读它的 goroutine
  就再也没人关；现在 `setFramers` 在会话的锁下检查会话是否已结束并如实返回，`establish` 对返回
  假的情况当场关闭刚开的路径。顺带把 `sessions` 映射的创建从 `Run` 的 goroutine 挪进 `Start`：它
  原来在锁外赋值，而 `Sessions()`、`Shutdown()`、`deliver()` 都在锁内读同一个字段，启动后立刻读
  就是一个数据竞争。回归测试 `TestDatagramPumpIgnoresDatagramsAfterShutdown`（关闭后再投递一个
  数据报，会话数、会话计数与已开连接都必须是 0）与
  `TestDatagramPumpClosesThePathsOfASessionThatEndedWhileTheyOpened`（让 `Open` 阻塞，先关闭再放行，
  断言那条路径被释放、会话计数回到 0）；去掉关闭保护会失败（`the session gauge is 1 after a datagram
  arrived post-shutdown, want 0`），让 `setFramers` 无条件接受会失败
  （`the path opened for a session that had already ended was never released`）。
- **udp 代理的数据报会话被当成"流"计数**。`ProxyGroup.recordSession` 在会话结束时调
  `metrics.recordStream`，于是 `udp` 代理每释放一个数据报会话，`aethertunnel_streams_total` 与
  `aethertunnel_tunnel_streams_total{tunnel}` 就各加一——一个只搬数据报、从没建过流的代理，
  面板与告警里的"累计流数"却在涨。同一项目里另外两条数据报路径（`sudp` 走 `pipeDatagrams`、
  socks5 UDP ASSOCIATE 走 `pipeSocksUDP`）都调 `recordDatagramSession`（只计双向字节、不碰流数），
  文档（`docs/MIGRATION.md`、`docs/CONFIGURATION.md`）也写明"数据报不计为流、`streams_total` 不受
  影响"，只有 `udp` 这一条不一致。现在 `recordSession` 改成 `recordDatagramSession`：字节照记，
  流数不动。回归测试 `TestUDPTunnelSessionCountsBytesWithoutCountingStreams` 起一个 `udp` 代理、
  搬一个数据报往返（把 `read_timeout_seconds` 调成 1 秒让空闲会话尽快释放并记账），断言字节计数
  涨了而 `streams_total` 与按隧道的流数都是 0；改回 `recordStream` 会失败
  （`aethertunnel_streams_total is 1 after a datagram session, want 0`）。
- **socks5 UDP 中继不限制并发目标套接字**。客户端按每个目标缓存一个 UDP 套接字，直到空闲超时
  才关；socks5 端口是公开的，一个访客可以发 UDP ASSOCIATE 后朝 `allow_targets` 里的成百上千个
  不同地址各发一个数据报，把客户端的文件描述符耗尽（连带的其它隧道也会跟着打不开新流）。现在中继
  把套接字缓存封顶在 256（`maxSocksUDPTargets`），超了就逐出**最久没用**的那个目标；逐出发生在新
  目标加入、且只在新加入时才检查，所以活跃目标不会被误逐。回归测试
  `TestSocksUDPRelayEvictsTheLeastRecentlyUsedTarget` 用一个 cap=2 的中继验证：加第三个目标后
  缓存仍是 2，被逐出的是最久没碰的那个、刚碰过的和新加的都在；把逐出改成空操作会失败
  （`holds 3 sockets after adding a third, want 2`）。
- **IPv6 地址在拼端口时没有加方括号**。`config.go` 里七个拼地址的函数（`ProxyConfig.LocalAddr`、
  `VisitorConfig.ListenAddr`、`Config.ListenAddr`/`HTTPAddr`/`HTTPSAddr`/`P2PAddr`/`DashboardAddr`）
  用 `fmt.Sprintf("%s:%d", …)` 把主机和端口接起来：IPv4 没问题，IPv6 的 `::1` 会变成 `::1:7001`
  ——一个多义的串，`net.Listen`/`net.Dial` 直接报"too many colons"或拨错地址。实测 `bind_addr = "::1"`
  的服务端能起，但客户端拨 `[::1]:…` 之前，服务端自己已经拼出 `:::7001` 这样绑不上的地址。现在这七个
  函数都改用 `net.JoinHostPort`（`::1` → `[::1]:7001`）。同一处还有一个自定义 `splitHostPort` 用
  `strings.LastIndex` 找冒号，把 `[::1]:7001` 的 host 拆成带括号的 `[::1]`，作为 TLS `ServerName`
  就是错的（x509 按 IP SAN 匹配用的是裸 `::1`）；它也换成 `net.SplitHostPort`。另外 `[dht]` 的
  `advertise_host` 校验用 `strings.Contains(host, ":")` 判端口，会把裸 IPv6 地址误判成带端口；
  现在改成 `net.SplitHostPort` 成功才判为带端口，裸 IPv6 地址能过校验（`serverAddrFor` 本来就能
  给 IPv6 加方括号）。单测覆盖七种拼地址、TLS ServerName 去括号、裸 IPv6 advertise_host 三个点；
  另用真实二进制在 IPv6 回环上走了一遍"服务端 `::1` 起、客户端 `[::1]:…` 连、访客 `::1` 搬字节"。
- **http/https 反向代理转不动 WebSocket 之类的协议升级，转动的字节也不进账**。`serveHTTP` 把
  访客的 `http.ResponseWriter` 包进 `countingResponseWriter`（用来数应答字节），但这个包装只实现
  `Write` 与 `Flush`，没有暴露底层的 `Hijacker`；`httputil.ReverseProxy` 处理 `101 Switching
  Protocols` 时要用 `http.ResponseController.Hijack()` 把访客连接拿过去做双向字节拷贝，拿不到就
  走错误处理器返回 `502 Bad Gateway`（实测一个 `Upgrade: test` 的请求得到 502）。现在包装自己实现
  `Hijack()`：把底层 writer 的 `Hijacker` 暴露出来、把拿到的 `net.Conn` 再包一层 `hijackedConn`
  数双向字节，升级路径就能拿到连接并原样转发，而且升级后的字节照常计入 `recordHTTPTraffic` 的
  双向计数（指标、账本、面板三者读的是同一个数）。回归测试
  `TestHTTPProxyCarriesAProtocolSwitch` 起一个会答 `101` 的后端，经共享 HTTP 端口走一遍
  `Upgrade: test` 再搬字节并断言这些字节进了计数：去掉 `Hijack` 会失败
  （`got "HTTP/1.1 502 Bad Gateway"`），去掉劫持后的字节计数会失败
  （`the upgraded bytes were not counted: in=0 out=0`）。
- **http 代理对 chunked 请求体记零**。`serveHTTP` 用 `r.ContentLength` 数访客发来的请求体，而
  chunked（无 `Content-Length`）请求的 `ContentLength` 是 -1，于是整个上传在账本、指标、面板里都是
  0 字节（实测一个 4096 字节的 chunked POST 被记成 `out=0`）。现在把 `r.Body` 包一层
  `countingReadCloser`，按**实际读走的字节**记账，不再看 `ContentLength`；`read` 用原子计数，因为
  HTTP 传输在它自己的 goroutine 里读请求体。回归测试 `TestHTTPProxyCountsAChunkedRequestBody`
  起一个会读请求体的后端、经共享 HTTP 端口发一个 chunked POST 并断言后端收到全部 4096 字节、
  组计数器也记了 4096；改回按 `ContentLength` 记账会失败（`out=0, want 4096`）。
- **侧边栏的可访问名是唯一一句写死在 HTML 里的话，切换语言后只有它还是英文**。其余每一处
  文案都在两份字典里（`data-i18n`/`data-i18n-label`/`t()`），`applyI18n()` 会在切换时把
  `aria-label` 一并改写；而导航那一行写的是 `aria-label="Sections"`，没有走字典。实测
  （真实 Chrome，读**无障碍树**而不是 DOM 属性）：英文模式下导航的名字是 "Sections"，切到中文后
  其余控件都变成「概览」「代理」「客户端」「配置」「语言」「退出登录」「跳到主要内容」，
  **导航仍叫 "Sections"**——用屏幕阅读器的人会在一屏中文里听到一个英文词，而它正是这一组链接的
  组名。现在改成 `data-i18n-label="nav.sections"`，两份字典各加一条（`Sections` / 「栏目」）。
  回归检查在无障碍树里比对两种语言下导航的名字，把那一行改回写死的版本会让它失败
  （`the navigation's accessible name follows the language switch -> want true got False`）。
- **切换语言后，屏幕上的两条横幅仍停在切换前的语言**。控制台把每一句文案都放在两份字典里
  （`data-i18n` 属性与 `t()`），切换语言时重画状态、表格、计数与连接徽标——但错误横幅与
  「连接断开 — 正在重试」横幅不在其中：前者由 `showError` 直接写入**已经格式化好的句子**，后者
  在 `setConnected` 里写。实测（真实 Chrome，DevTools 协议；先让浏览器把 `/api/config` 判为
  失败，再切换语言）：错误横幅仍是 `Failed to load /api/config: boom`，而同一屏的连接状态已经是
  「已连接」、客户端计数已是「已连接 1 个」。轮询路径上的两条横幅（`err.status`/`err.clients`）
  最多错 2 秒就自我修正，**配置页的那一条不会**——配置加载不在轮询路径上，除非用户重新点一次
  刷新，它会一直停在旧语言。现在横幅以**生成它的键与参数**保存在状态里（`state.lastError`），
  连接横幅抽成 `renderConnBanner()`（这样重画不会像一次成功的请求那样顺手改写 `lastSuccessAt`），
  切换语言时两条都按新语言重画、保留服务端给的原因原文。回归证据：把语言切换里那两行重画删掉，
  浏览器检查里对应两项失败（`the banner follows the language switch -> want true got False`、
  `the disconnected banner follows the switch -> want true got False`）。
- **首帧不是可用请求时，服务端会回答、却什么都不记**。`handleConn` 只看首帧类型：认证请求的
  载荷解不开、或首帧类型不能开局（例如心跳）时，服务端都会**回答一句再关闭**——前者回
  `"malformed auth request"`，后者回 `"first frame must be ... got ..."`——但这两次拒绝不增加任何
  计数器、不写审计，而前者连一行日志都没有（后者至少有日志）。实测（无加密、开着审计与指标，
  向控制端口先后发三个解不开的认证请求）：`aethertunnel_control_rejected_total` 仍是 **0**、审计
  文件 **0** 行、服务端日志 **0** 行，客户端却每次都拿到明确的拒绝。于是一台服务器完全无法得知
  有人在拿垃圾帧扫它的控制端口，而同一个计数器在别处的注释是"不关心原因时看的那条"，
  `guard_test.go` 里也写着"每条在握手前拒绝连接的路径都必须移动它"。现在这两条路径都走
  `refuseControl`：计入汇总的 `aethertunnel_control_rejected_total`、写一条审计
  `control_rejected`（`detail` 说明是"载荷解不开"还是"这个类型不能开局"）、留一行日志；另外新增
  `aethertunnel_unusable_request_frames_total` 单独计这两种帧——与上一轮为 socks5 出口加
  `aethertunnel_socks5_malformed_requests_total` 同一个理由：协议垃圾与策略拒绝应当分得开。同一轮
  还补上另外三条**同样有回答、却什么都没记**的握手拒绝路径：`post_quantum` 要求密钥交换而客户端
  没给（此前没有计数、没有审计、也没有日志）、抗量子协商失败（此前没有审计记录）、会话密钥不可用
  （此前只有日志）。回归测试 `TestAnUnusableFirstFrameIsRefusedAndCounted`（两条路径的计数、审计，
  以及"被拒绝的不算已接受"）与扩写后的
  `TestPostQuantumServerRefusesAClientWithoutAKeyExchange`（拒绝也要进汇总与审计）；套件里新增一组
  6 项，把 `refuseFirstFrame` 改回空操作会让其中 5 项失败
  （`both unusable first frames are counted on their own -> want '2' got '0'`、
  `an unusable first frame also moves the shared rejection counter -> want '6' got '4'`）。
  指标表里 `aethertunnel_control_rejected_total` 的含义与新增序列一并更新。
- **`aethertunnel_visitors_denied_by_proxy_total` 把三种东西算成一个数，而它的说明只写了其中一种**。
  这个计数器实际统计：被**服务端**为某个名字定的策略拒绝的访客、被**代理自己的**名单拒绝的访客，
  以及**在 socks5 出口上发了一句不是 SOCKS5 请求的内容**的连接。它的帮助文本只写"refused by a
  proxy's own allow/deny lists"（漏了服务端策略那一半），而 `CONFIGURATION.md` 明确写着服务端策略
  的拒绝要计入这个计数器——两边说法不一致；至于"不是 SOCKS5 请求"那一种，两份文档都没提。实测
  （一个名字带服务端 `deny_cidrs`、一个代理带自己的 `deny_cidrs`、再向 socks5 出口发一句垃圾）：
  打开的正常代理 200、计数器不动；两次策略拒绝都是 403 且各 +1；那句垃圾也让计数器 +1，而它并
  不是任何名单拒绝的访客。现在把协议错误拆成自己的序列
  `aethertunnel_socks5_malformed_requests_total`（审计与日志里本来就有那行拒绝记录），并把两个
  策略来源都写进帮助文本与指标表——按"被策略拒绝"与"有人在往端口里灌垃圾"分别告警，是运维真正
  需要区分的事。套件里新增一组（见下）覆盖这两侧加这一种连接，把这次改动改回去会让它报
  `a connection that is not a SOCKS5 request is counted on its own -> want '1' got '0'`。
- **`latency` 与 `adaptive` 会把每一次访问都押在"从未应答过"的成员上**。`dialLatency` 只在成功
  开流时写入，因此本地服务已下线的成员永远是 0；而 `latency` 比较的是最小值、`adaptive` 的
  `cost()` 把"未测量"记为 0（原意是"先试它一次，好让新成员有机会被测量"），于是一个永远失败的
  成员每一次都比会应答的成员更"便宜"。实测（30 次访问、两名成员、其中一名的本地服务无人监听、
  池的端点从面板读回而不是假定）：`latency` 浪费 **31** 次、`adaptive` **30** 次，而
  `round-robin` 是 15 次、`random` 13–16 次、`bandit` 4 次、`failover` 0 次——两个"按延迟选"
  的策略反而最差，与它们文档里的说法相反。现在**已经失败且从未应答**的成员排在所有被测量过的
  成员之后（`neverAnswered`），而"未测量且尚未失败"仍然优先——后者是新成员被测量到的方式，也是
  `strategy_test.go` 里既有的断言（`TestLatencyStrategyKeepsChoosingTheFasterMember` 明确要求
  未测量的成员必须被试用）。修复后同一实验里两者各浪费 **2** 次（首次访问时谁都没有测量值，加上
  下面那次探测）。回归测试 `TestAMemberThatFailedWithoutAnsweringWaitsBehindTheOthers` 在修复前对
  两个策略都失败、修复后通过；套件里对应的端到端一组把规则换回旧版，会报
  "9 wasted attempt(s) in 10 visits"（探测那一步的证据见下一条）。
- **只把"从未应答过"的成员排到最后，会把恢复过来的成员永久埋掉**。接上一条：排序本身解决不了
  恢复问题——那个成员没有测量值可供惩罚，只要还有成员应答，它就永远排在后面。实测：一名成员的
  本地服务下线、随后又起来，30 次访问里它被用到 **0** 次，而这与 `cost()` 上那句"惩罚有上限，
  否则一个长时间故障的成员永远回不来"的注释相矛盾（上限只对**测量过**的成员生效）。因此
  `latency` 与 `adaptive` 现在每 20 次选择用一次探测去问那个成员（`neverAnsweredProbeEvery`；
  探测用**独立的计数器**，否则 `adaptive` 自己的轮转计数器会让探测节奏漂移，这一点是写单元测试
  时才发现的）。恢复后它会被重新纳入（30 次访问里 1 次），代价是那一次探测可能失败：同一实验里
  这两个策略 30 次访问浪费 2 次，仍是 `round-robin`（15 次）的七分之一。单元测试断言 40 次选择
  里那个成员恰好被选 2 次；套件里的端到端一组把探测关掉就会报 "it served none of 25 visits"。
- **Windows 构建脚本写出的 `SHA256SUMS` 无法被校验**。两个发布脚本写出的校验和文件形状不同：
  `scripts/build-release.sh`（发布流水线在 Linux 上用的那个）按 `sha256sum ./*` 的写法把条目
  写成 `./<文件名>`、行尾是 LF；而 `scripts/build-release.ps1`（给没有 make/POSIX shell 的
  Windows 机器用的）写成裸文件名、行尾是 CRLF。`scripts/verify-release-linux.sh` 认不出后者：
  它按 `(^|/)<名字>$` 查条目，裸文件名前面没有 `/` 所以不匹配，行尾的 CR 又让 `$` 锚点失效，
  两种情况它都报成 "want ''"——看起来像哈希不符，实际是根本没查到条目。现在 `.ps1` 写出与
  `.sh` 相同形状的文件（`./` 前缀、LF 行尾），`verify-release-linux.sh` 也接受两种写法并容忍
  CRLF，查不到条目时给出明确信息而不是空的期望值。这条差异此前没被发现，是因为流水线只用
  `.sh` 构建、从不执行 `.ps1`。
- **`go.mod` 把直接依赖标成了间接依赖**：`golang.org/x/sys` 标着 `// indirect`，但
  `pkg/vpn/device_linux.go`（`//go:build linux`）直接导入 `golang.org/x/sys/unix`。这个标记
  是此前在 Windows 上跑 `go mod tidy` 留下的——那里只有 `device_other.go` 参与编译，`unix`
  不算直接依赖。在 Linux 上 `go mod tidy` 会去掉它，于是同一个文件在两个平台上互相打架：
  谁跑一次 `make tidy`，谁就留下一个看起来"多余"的改动。这里按 Linux 的结果保留（发布流水线
  与 CI 都在 Linux 上构建），并记下原因，免得下次在 Windows 上又被改回去。
- **`docs/PLATFORMS.md` 说 CI 里没有 arm64 的执行作业，但作业早就在**：`.github/workflows/ci.yml`
  的 `test-arm64`（`runs-on: ubuntu-24.04-arm`）会跑 `go vet`、`go test ./...`、构建两个二进制、
  用随发布的示例配置做 `--check`、打印版本，并在真机 arm64 上真的传数据。该文件 2026-09-24
  更新时仍写着"CI 里还没有 arm64 的执行作业"，把已经做到的证据说小了；结论表与第 4 节已按实际
  作业改写，并写明 CI 那一栏跑到的是哪几项、和 70 项的那组数字不是同一套。

- **配置页的数值不跟随语言切换**。标签是 `data-i18n`，切换语言时会被重画，但「加密已启用 /
  需要令牌 / 隧道状态」这几格是渲染时用 `t()` 取过一次的 `yes`/`no`，切到中文后仍停在英文，
  直到用户按一次刷新或重新打开配置页。前几轮修过的横幅、账本、目录都是同一个毛病，只有配置页
  漏了。这一轮在一条**活的**隧道上用真实浏览器抓到的：`the configuration values follow the
  language switch -> want ['否','是','是'] got ['no','yes','yes']`；把最后一份配置响应留在状态里、
  切换语言时重画之后，同一项通过（同一台部署、同一个检查器，只有面板重新编译过）。
- **面板检查器在没有已注册代理的部署上会误报**。卡片版式那两项要求「一行里的每个单元格都是卡片
  块」，桌面那两项要读一个单元格的计算样式；一个只跑着 `[vpn]`、没有任何客户端注册代理的部署
  没有这样的行，前两项于是失败、后一项直接对 `null` 调用 `getComputedStyle` 抛异常。现在与行有关
  的断言在没有行时打印 `SKIP`，桌面那段也不再读一个不存在的单元格。这个是拿检查器去核对隧道
  面板时发现的——套件自己的部署永远有代理，所以此前看不见。

- **`--dht-lookup` 把查询节点绑在回环地址上，换一台机器解析就永远收不到回答**。
  `LookupProxy` 写死了 `settings.ListenAddr = "127.0.0.1:0"`，那里的注释只说"别占用配置里那个
  端口"——端口是对的意图，主机被顺带写成了回环。Kademlia 的对端把回答发给**请求的源地址**，
  源地址是 `127.0.0.1` 时回答被送回对端自己的回环地址：同一台机器上照常通过（套件里所有 DHT
  检查都在 127.0.0.1 上，所以一直没暴露），换一台机器一定超时。这是把 `deploy/kubernetes/`
  真的 apply 到一个 k3s 集群后发现的：集群里另一个 pod bootstrap `10.42.0.23:7003` 只得到
  `dht: request timed out`。现在绑 `0.0.0.0:0`，源地址交给路由决定，同一个集群里的同一个查询
  随后解析出 `kubernetes-probe -> aethertunnel.aethertunnel.svc.cluster.local:18080`。单测
  `TestLookupProxyQueriesFromAnAddressTheAnswerCanReach` 用一条只记录源地址、从不回答的裸
  socket 把这条钉住：改回回环绑定会失败并打印
  `the request came from 127.0.0.1:47794, an address only a node on this machine answers`。
- **Service 没有发布 DHT 的 UDP 端口，而 ConfigMap 指向的正是它；补上时又发现端口号不能重复**。
  Deployment 声明了 `containerPort: 7001/UDP`（DHT），ConfigMap 让 DHT 听在 7001，
  `dht.advertise_host` 写的是 Service 的集群内域名，而 Service 只发布了 7001/TCP、7002/UDP、
  7500/TCP——集群里另一个 pod 用 ClusterIP 去 bootstrap DHT 只会拿到 `dht: key not found`，
  集群外的客户端更是无从加入。补这个端口时撞上第二件事：Service 的端口列表按 `port`（数字）合并，
  控制端口已经是 7001，于是 `kubectl apply` 把两条 7001 当成同一个条目——**实测先 apply 旧清单、
  再 apply 带 `dht 7001/UDP` 的新清单，命令报 `configured`，控制端口却从活对象里消失了**，再
  apply 一次也修不回来（`kubectl apply` 不会重新添加它认为两边都没变的条目）。因此 DHT 挪到
  **7003/udp**（ConfigMap、Deployment、Service 与 `Dockerfile` 的 `EXPOSE` 四处一致），并加单测
  `TestTheServiceGivesEveryPortItsOwnNumber`：同一个 Service 里任何两个端口的数字重复、或
  ConfigMap 绑了一个 Service 没发布的端口，都直接失败。修好之后的升级路径也实测过：先 apply
  旧清单再 apply 新清单，四个端口都在，控制端口不再丢。
- **ConfigMap 里 `ban_ignore_cidrs` 的注释说错了它保护的东西**。原文写着"never ban the kubelet's
  probe address"，但封禁列表只由**控制端口**的握手失败喂饱（`recordAuthFailure` 只在控制端口的
  处理函数里被调用），kubelet 的探针打在面板的 HTTP 端口上、走面板自己的令牌比对，根本不进这个
  列表。那句话把读者引向一个不存在的风险；真正被 `127.0.0.1/32` 保护的是 pod 内部连
  `127.0.0.1:7001` 的客户端。注释改成实测到的事实。

- **套件里的 ConfigMap 一节把 DHT 端口写死成 7001，改了清单之后它会去绑清单里那个号**。把
  DHT 挪到 7003 之后，那一节的改写规则（把 `listen_addr = "0.0.0.0:7001"` 换成一个空闲端口）
  不再匹配，被抽查的服务端于是直接去绑清单里写的 7003：单跑一次看不出来（7003 恰好空着），
  两套套件同时跑就撞成 `listen udp 0.0.0.0:7003: bind: address already in use`——实测 arm64
  服务端那一对因此 5 项失败。现在规则按"ConfigMap 给 DHT 的那个号"匹配（`0\.0\.0\.0:\d+`），
  与清单里写几号无关。

- **`scripts/kubernetes-linux.sh` 跑完留下的东西比它说得多**。第一版只做到"杀掉 k3s 进程组、
  删掉工作目录"，看上去干净；把 `containerd-shim`、`ip netns`、veth、`/run/k3s` 与
  `/var/lib/rancher/k3s` 一项项列出来之后才看到：每跑一次留下两个 pod 的网络命名空间、两个 veth、
  两个 shim 进程和 containerd 的运行时目录——k3s 被杀掉之后 pod 的 sandbox 会活下来，而
  `/run/k3s` 里它留下的 overlay 挂载让目录也删不掉（`设备或资源忙`）。现在先让集群自己删掉 pod
  （containerd 顺手把 sandbox、命名空间与挂载收走），再按"这次跑之前有哪些"的差集清掉残留，
  最后卸载 `/run/k3s` 下的挂载再删目录。**第一版这么做之后仍有一个进程活下来**：容器自己的进程
  （`/usr/local/bin/aethertunnel-server`）不是 sandbox、也不叫 `pause`，杀掉 shim 之后它连着自己
  的网络命名空间一起留着；它所在的 `/proc/<pid>/cgroup` 写着 `/kubepods/burstable/pod<uid>/…`，
  而机器上只可能有这一个集群（脚本在已经有 k3s 在跑时直接退出），于是把落在 `kubepods` 里的进程
  一并收掉。k3s 自己解包的运行时只在这次跑之前不存在时才删。实测跑完：容器进程 0、sandbox 0、
  shim 0、命名空间 0、veth 0。

### 新增
- **socks5 出口补上 UDP `ASSOCIATE`**：此前 `pkg/socks` 只实现 `CONNECT`，`UDP ASSOCIATE` 与
  `BIND` 一起被拒（`only the CONNECT command is supported`）。现在访客可以走标准 SOCKS5 UDP：
  服务端在 socks5 TCP 端点上接受 `UDP ASSOCIATE`，开一个 UDP 中继、把它的真实地址写回应答
  （`WriteReplyBound`），再把每个按 RFC 1928 §7 包好的数据报原样经隧道转发给客户端；客户端用
  `ParseUDPDatagram` 取出每个数据报自己的目标，按 `allow_targets` 拨号并发送数据，用
  `WrapUDPDatagram` 把应答包回去。中继只收**建立该关联的那个来源地址**的数据报（除非访客在
  请求里写了具体地址），所以别的来源不能往一个访客的关联里注包。`BIND` 仍被拒。两个新序列
  `aethertunnel_socks5_udp_associations_total` 与 `aethertunnel_socks5_udp_datagrams_total`
  分别计关联数与双向转发的数据报数。单测覆盖 `socks` 包的封装/解析/边界地址回写，集成测试用一个
  真 UDP 回显服务走完整条 `ASSOCIATE → 发数据报 → 收应答` 的路径，另有一条断言来自另一来源的
  数据报被丢弃；客户端的中继逻辑也在 `client` 包里有单独测试。`scripts/functional-linux.sh` 也补了
  三项（中继搬一次数据报、关联计数、双向数据报计数），套件因此长到 281 项（无浏览器时 138 项）。
  协议只给 `DataRequest` 加了一个可选字段（`socks_udp`），帧布局与消息集合不变，`ProtocolVersion` 仍为 4。
- **`deploy/kubernetes/` 第一次被 apply 到一个真集群上**（`scripts/kubernetes-linux.sh`，
  **14 项全过**，本机一次 72 秒）。此前清单只在 CI 里被 `kubectl kustomize` 渲染过，而渲染出来的
  YAML 不会告诉你 Service 有没有把 pod 的端口发布出去、探针会不会通过、状态卷能不能写。脚本起一个
  自己的单节点 k3s（数据目录落在工作目录里，`--disable coredns,local-storage,metrics-server,
  servicelb,traefik`，不碰机器上已有的任何东西），用仓库里的两个二进制搭出清单点名的那个镜像，
  apply 之后从集群外驱动它：Deployment 自己声明的探针通过；`/healthz`、`/readyz` 200；
  `/api/status` 不带令牌 401、带 Secret 里的令牌 200 且 `auth_required` 为真；`/metrics` 不带
  令牌 401；一个客户端用 Secret 里的令牌把本地服务发布到 18080，访问者的字节穿过
  Service → pod → 隧道再回来；面板列出这条代理；会话结束后带宽账本里出现条目（容器根文件系统是
  只读的，读得回来才说明它写进了 Deployment 挂的状态卷）；DHT 既从集群内的 ClusterIP 解析出名字，
  也从集群外的 NodePort 解析出名字。清单里那个镜像的基础镜像在这台机器上拉不到，所以用的是按
  `Dockerfile` 的运行时契约（同一路径的静态二进制、uid 65532、同样的工作目录、入口与命令）搭的
  替身，脚本会把这一点打印出来——**这不是那个 Dockerfile 的验证**，它由 CI 负责。脚本需要 root、
  可用的 docker daemon 与一个 k3s 二进制，缺任一即打印缺什么并以 0 退出；跑完把集群、镜像与工作
  目录都清掉（`--keep` 可留下）。

- **三层隧道与本机第一次跑它**（`scripts/vpn-linux-test.sh` **23/23**）。这个套件此前只在 CI 的
  `ubuntu-latest` 上对着真实 tun 设备跑过——本机缺的不是设备（`/dev/net/tun`、`ip`、`ping` 都在），
  而是 root。这一轮用一次密码弹窗（`pkexec`）拿到 root 后完整跑了一遍：两个命名空间由 veth 相连，
  服务端开出真实 tun 设备 `at0`（192.168.99.0/24，MTU 1400），客户端在第二个命名空间里拿到
  192.168.99.2，**两个方向各 ping 3/3**，两侧接口的收发包计数都动了（`at0 rx 6 tx 14`、
  `at1 rx 6 tx 12`），`GET /api/vpn` 与 `GET /api/config` 的 `[vpn]` 段和审计里的地址分配一致，
  客户端退出后地址归还，`vpn.require = true` 时一个不带隧道请求的会话被拒并留痕。
- **面板的隧道状态进了真实浏览器**。`[vpn]` 打开时的那一栏此前是唯一"只由 CI 覆盖"的面板状态。
  自建一条活隧道（两个命名空间 + veth + 真实 tun 设备，ping 5/5）后，用 Chrome 打开配置页逐项核对：
  接口、服务端隧道地址、子网、MTU、地址池已用/总数、对端数、从接口读到与送入接口的包（各自要求
  落在**前后两次 API 读数之间**的窗口里，因为流量随时可能再走一轮）、丢失包等于不可路由加丢弃、
  `/api/vpn` 与 `/api/config` 描述同一条隧道，并记录当时真的搬过包（`28 from the interface,
  5 to it, 1 peer`）。面板检查因此从 140 项长到 143 项。
- **Kubernetes ConfigMap 的凭证契约进了套件**（8 项，不需要 root 也不需要 Docker）。清单里那份
  `server.toml` 一直只有单测"能加载"，没有人真的用它启动过服务端。现在套件把它原样取出来：
  **单独校验不通过**（`server.auth_token is required`）、带上 `AETHERTUNNEL_AUTH_TOKEN` 与
  `AETHERTUNNEL_DASHBOARD_TOKEN` 后校验通过、服务端起来、面板对不带令牌的请求回 401、带令牌回
  200 且 `auth_required` 为真、一个启用了**同样伪装**的客户端能穿过它搬运字节、审计日志落在
  ConfigMap 指的路径上，而一个没启用伪装的客户端被拒（服务端日志 `not carrying record-framed
  data`）。写这一节时我自己的断言错了两处（`--check` 不带环境变量自然通不过；测试客户端必须与
  服务端用同一种伪装），两次都是被测配置纠正了我。
- **镜像与清单之间的一致性有了单元测试**。`Dockerfile` 的 `CMD ["--config", …]` 指向
  `/etc/aethertunnel/server.toml`，而这个文件在容器里只有一个来源：Deployment 把 ConfigMap 挂在
  `/etc/aethertunnel`。两者分家的话，Pod 会在监听之前退出，而 `docker run` 会去找一个镜像里根本
  没有的路径。新增的测试要求 `CMD` 的路径等于「Deployment 的挂载点 + ConfigMap 的键」，端口在
  `Dockerfile`、Deployment、Service 三处一致，并且 ConfigMap 里每条 `path`/`signing_key_file`
  都落在 Deployment 的 state 挂载点之下。
- **Docker 装上了（`docker.io` 29.1.3），容器运行时在这里第一次被验证**。`Dockerfile` 的基础镜像
  来自 Docker Hub，而这台机器到 `registry-1.docker.io`/`auth.docker.io` 的 443 全部超时（Ubuntu
  源与 GitHub 正常），所以**那个镜像本身仍然只能由 CI 验证**（`ci.yml` 会 build 并跑起来检查
  `/healthz`）。能验证的部分改用一个本地 `FROM scratch` 镜像补齐：同一个静态二进制、同样的
  `USER 65532:65532`、同一份 ConfigMap 配置、端口与状态卷按清单发布与挂载——`docker inspect`
  报 `User=65532:65532`，`/healthz` 200，`/api/status` 不带令牌 401、带环境变量里的令牌 200，
  一个客户端发布的隧道**从宿主机穿过容器**搬运了 21 字节，容器以 uid 65532 写出的 `audit.jsonl`、
  `ledger.jsonl`、`ledger.key`、`dht.key` 都落在挂载的状态卷里（密钥 0600）。


- **arm64 与竞态检测这两条"只能靠 CI"的路，现在本机也能跑，而且这一轮跑了**。此前
  `docs/PLATFORMS.md` 把 linux/arm64 记成"只有交叉编译 + 作者本机 qemu 的结果"，`-race` 记成
  "本机没有 C 编译器时只能靠 CI"；两者都只是缺一套可复跑的做法，不是缺机器。新增
  `scripts/emulate-linux-arm64.sh <qemu-aarch64> [sysroot]`：交叉编译三个 arm64 产物、给每个
  写一个 `exec qemu-aarch64 … "$@"` 的包装脚本，先跑整套单测（`go test -exec <透传包装>`），
  再把包装脚本交给 `scripts/functional-linux.sh`，套件本身不知道自己在跑 arm64。**结果**：arm64
  上单测 14 个包全过、那 38 项功能检查 **38/38**（带 sysroot 与不带 sysroot 各一遍）；跨架构
  两组（amd64 服务端 + arm64 客户端、arm64 服务端 + amd64 客户端）同样各 **38/38**；另外用解出
  来的 `gcc-14` 加 `CGO_ENABLED=1` 把 `go test ./... -race` 完整跑了一遍，14 个包全过。套件长到
  281 项之后这三条在同一台机器上又跑了一遍：arm64 单测 14 个包全过、arm64 功能检查 **281/281**
  （含真实浏览器那一节的 143 项）、跨架构两组各 **281/281**，逐项结果记在 `PLATFORMS.md` 的 3.12。
  写这个脚本时踩到一处细节并写进文档：`go test -exec` 会把自己构造的测试二进制路径追加到命令之后，
  所以那里的包装脚本必须透传，若写成某个具体产物的启动器，那个产物会把测试二进制当参数收下
  （表现为"解析配置时遇到控制字符 0x7f"）。文档里 linux/arm64 与 linux/amd64 两栏、以及
  `PLATFORMS.md` 新增的 3.9 节（含每条的实测结果）都按这次的结果改写。
- **面板受保护端点的端到端覆盖**：此前 `/healthz`、`/readyz`、`/metrics`、`/api/status`
  有测试，但 `/api/clients`、`/api/proxies`、`/api/config`、`/api/ledger`、`/api/dht`、
  `/api/vpn` 与 `DELETE /api/clients/{id}` 没有。新增 `pkg/server/dashboard_api_test.go`：
  逐个端点核对"无令牌 401、错令牌 401、正确令牌放行"，核对 `/api/vpn` 关掉时带出原因、
  `/api/ledger` 的 `?limit=` 拒绝非负整数以外的输入，以及 `/api/config` 不把服务端令牌、
  面板令牌、指标令牌或加密口令写进响应。用例经过变异测试验证：把任意一个端点的鉴权摘掉、
  让 `/api/config` 回显口令、去掉 `?limit=` 校验，对应用例都会失败。
- **Linux 与 arm64 的功能检查进了仓库**：`docs/PLATFORMS.md` 里的 70 项检查一直是作者在自己
  机器上（qemu / Wine）跑的，脚本不在仓库里，所以那组数字没人能复跑，Linux 上的功能路径
  在 CI 里也没有覆盖——CI 只有 Windows 的 101 项与 tun 设备那一套。新增
  `scripts/functional-linux.sh`（有浏览器时 281 项、没有时 138 项）：起本地服务（TCP/UDP/HTTP 回显，外加一个只在对端
  半关闭之后才回答的服务）与证书，拉起服务端、一个发布方客户端与三个访问者客户端，验证
  `tcp`/`udp`/`http` 与共享 `https` 监听（两者都按 Host 选隧道，未配置的名字都被拒）、
  `socks5`（`allow_targets` 内可达、范围外被拒）、`stcp`/`sudp`/`xtcp` 三种访问者（密钥错误
  必须拿不到数据；`xtcp` 那一项还会核对服务端报出的打洞路径与三个 `aethertunnel_p2p_*` 计数
  彼此对得上，一条从未被上报结果的打洞不算成功）、半关闭之后仍能收到应答（公网端口与 socks5
  两条路径各一项）、只凭一个名字找到服务端（`--dht-lookup` 把 tcp 名解析到公网端口、`--discover`
  把私有名解析到控制端口、未知名字报错、以及一个 `server_addr` 留空的客户端真的连上去并传了一次
  数据）、**两个客户端组成的代理池**（7 项：池报出两名成员、端点仍是第一名成员的端口、后到
  成员请求的端口没有被监听、六次连接后两名成员都服务过、杀掉一名成员后池只剩一名、剩下的成员
  继续服务、移除进了审计）、**成员服务已下线时策略该选谁**（另起一台 `load_balance = "latency"`
  的服务端，一名成员的本地服务无人监听：10 次访问全部被服务，只把 1 次尝试浪费在不会应答的成员上；
  随后在那个端口上起一个会应答的服务，再看 25 次访问里它是否被重新用到——这一组是上面那条修复
  与其后续探测的端到端证据）、**访客名单的两侧**（6 项：服务端为某个名字定的策略拒绝一次、代理
  自己的名单拒绝一次，访客都拿到 403，而计数器各涨 1 且审计 `detail` 分别指出是哪一边；再向
  socks5 出口发一句不是 SOCKS5 请求的内容，验证它只计进自己那个序列）、**服务端侧的拒绝路径**
  （33 项，见下一条）、**带宽账本的端到端**（23 项，见更下面一条）、**真实浏览器里的控制台**（143 项，见更下面一条）、指标与 `/api/status` 的
  计数、审计里的
  `proxy_registered` 与 `visitor_accepted`，最后把加密、后量子、TLS、身份认证与伪装同时打开再传一次
  数据并逐层核对日志行。CI 的 `ubuntu-latest` 与 `ubuntu-24.04-arm`（真机 arm64）两个作业都跑它，
  发布流程也用已发布的 Linux 二进制再跑一遍。只绑回环端口、不需要 root。用例同样做过变异测试：
  去掉 `allow_targets`、关掉审计、把伪装改成 `none`、把 `require_identity` 改成 `false`、把 tcp
  代理指向没人监听的端口、不启动共享 `https` 监听、在 `pkg/net/pipe.go` 里把半关闭改成整条关闭、
  在 `pkg/server/visitor.go` 里不再记录打洞结果、让服务端不再发布 DHT 公告、把第二名成员的
  `group` 去掉、把 `pickExcluding` 的 round-robin 分支改成永远返回 `candidates[0]`、把
  `latencyScore` 换回"直接返回测量值"的旧版、让 `probeNeverAnswered` 永不触发、去掉服务端为某个
  名字定的策略、去掉代理自己的那份名单、以及把 socks5 的协议错误计回"被名单拒绝"那个计数器，
  对应检查都会失败（分别失败 12、2、1、1、2、2、2、1、3、3、1、1、1、3、3、2 项）。
- **服务端侧的拒绝路径补上端到端覆盖**（`scripts/functional-linux.sh` 从 56 项到 89 项）。
  `deny_cidrs`、令牌桶、`ban_after_failures`、`max_connections` 与不可用首帧的拒绝此前只有单元
  测试（或只有 Windows 那套脚本）覆盖，仓库里能在 Linux 与 arm64 上复跑的那一套几乎一条都没有
  ——而它们正是运维最依赖、也最容易在改动中被绕过的边界。一共起四台专用服务端（在被测的那台上
  改这些设置，会把后面所有检查一起拒掉）：

  - **握手前就被拒的连接**（10 项）。一台 `deny_cidrs = ["127.0.0.2/32"]`（本机的第二个回环
    地址）外加 `rate_limit_per_second = 0.01`、`rate_limit_burst = 2` 的服务端：三条来自被拒
    来源的连接在发出任何一个字节之前就被关闭（客户端的读有 1 秒上限，否则一个仅仅被挂起的
    连接也会被算成"被拒"）、`aethertunnel_connections_denied_by_acl_total` 恰好 +3 并同样计入
    共享的 `aethertunnel_control_rejected_total`、被拒来源不算已接受的连接、日志与审计分别
    记下这次决定并指出来源；随后两次连接落在突发额度内（计数不动），再三次全部被令牌桶拒绝
    （计数 +3、审计三行 `rate_limited`、日志三行 `denied by rate limit`）。读取一个没有被测
    服务端的面板，用的是新加的 `metric_at`/`wait_metric`（拒绝是在关闭连接的那个 goroutine
    里计数的，单次读取可能落在自增之前）。
  - **自动封禁与它的窗口**（9 项）。一台 `ban_after_failures = 3`、`ban_seconds = 10` 的服务端
    上，一个只有令牌写错的客户端每秒重试：三次失败后来源被封（`aethertunnel_sources_banned_total`
    = 1、三次失败都进了审计且 `outcome` 为 denied 并带来源、封禁记录写着 "banned after 3 failed
    attempt(s)"）；此时换一个**令牌正确**的客户端仍然进不去（没有任何会话、拒绝计数在涨、客户端
    日志里没有 session 行），等 `ban_seconds` 过去后它连上了——一拒一放才说明前面拒的是这个来源，
    而不是一个本就连不通的客户端配置。
  - **`ban_ignore_cidrs`**（3 项）。一台 `ban_after_failures = 2` 且忽略回环地址的服务端上，同一个
    错令牌客户端反复失败（实测 5–6 次，阈值是 2）也不会被封禁，也不会被当成被封禁的来源拒绝。
    这个键此前只有单元测试（`ban_test.go`、`guard_test.go`），它配错的方向是"运维的负载均衡
    地址被误封"。
  - **连接上限与不可用首帧**（11 项）。`max_connections = 1` 的服务端上，第一个客户端占住唯一的
    会话，第二个令牌同样正确的客户端被拒：`aethertunnel_control_connections_total` 仍然是 1、
    `aethertunnel_control_rejected_total` 在涨、审计里是 "server is at its connection limit"、
    客户端日志里也写着这句（客户端被告知了原因，而不只是超时）；同一台服务端上（它既没有令牌桶
    也没有封禁，所以来自回环的连接能走到帧读取）再发两个首帧——一个解不开的认证请求、一个不能
    开局的帧类型——每个都被回答，并分别计进 `aethertunnel_unusable_request_frames_total`、
    汇总计数与审计，日志里也各留一行。

  这 33 项同样做过变异测试：让 `AccessControl.CheckAddr` 不再做判断（失败 8 项）、让令牌桶永远
  放行（3 项）、让封禁名单永不建立（5 项）、让会话上限不再生效（4 项）、把 `ban_ignore_cidrs`
  从配置里去掉（3 项）、让两条不可用首帧的拒绝不再记入指标与审计（5 项）。
- **控制台页面第一次有了真正的检查：在真实浏览器里跑**。此前仓库里没有任何东西运行过
  `web/dashboard/index.html`——Go 测试覆盖它读的 API，功能套件覆盖隧道，而这一页本身
  （单文件、内联 CSS 与 JS、会切换语言、有抽屉与确认框）从没被执行过。新增
  `scripts/panel-checks.py`（起初 53 项，这一轮先长到 74 项、又补上账本与字典几组到 143 项，见下面两条）：它用远程调试端口启动
  Chrome，自己实现最小的 WebSocket 与
  DevTools 协议客户端（标准库没有 WebSocket，而这里的帧足够简单：客户端帧永远带掩码、没有
  分片），然后对**渲染后的页面**断言——令牌提示（未带令牌、错误令牌、正确令牌三条路径）、每个
  数字与表格是否与同一进程自己抓到的 `/api/*` 回答一致、配置页各字段、两个表格与代理池的成员列、
  审计那一栏的四种状态、在没有轮询路径上时用 `Fetch` 域让浏览器把 `/api/config` 与 `/api/status`
  判为失败以升起两条横幅、**横幅在屏幕上时切换语言**（上面那条修复的回归检查）、380 px 视口下的
  汉堡与遮罩（`matchMedia` 与 CSS 类一起验证）、断开按钮（先让 `window.confirm` 返回 false 确认
  不发请求，再返回 true，行从表格消失、服务端不再列出该客户端、审计里出现 `dashboard_action`）。
  `scripts/functional-linux.sh` 把它作为最后一节运行（还为它起了两个客户端，让面板的成员列不是
  空跑，其中一个发布一个 58 字符、没有任何空格的代理名——那正是「360 px 下不会被截断」这句话
  针对的情况），把它的 PASS/FAIL 计入套件总数：**有浏览器时 281 项、没有时 138 项**（整段跳过并
  打印 `SKIP`，所以没有 Chrome 的机器上套件照常通过）。顺带补上 `.gitignore` 里缺的一条
  Python 规则（`__pycache__/`、`*.py[cod]`）：用脚本读一次 `panel-checks.py` 就会在 `scripts/`
  下留一个字节码目录，而 `.gitignore` 里此前没有任何一条与 Python 有关。
  变异测试：把语言切换里那两行重画删掉，
  这一节失败 2 项，其余仍过——它测的正是它声称测的东西。
- **面板检查补上「版式」与「无障碍」两组**（53 → 74 项）。`web/dashboard/README.md` 写着
  「700 px 以下表格每行重排为带标签的卡片，360 px 屏幕上不会出现被截断的内容」，这句话此前只有
  肉眼看着对；现在在浏览器里断言：380 px 下每个单元格的计算样式是卡片块、`::before` 的内容
  就是同列的表头文字、整个页面没有元素越界（把每一个元素的 `getBoundingClientRect().right`
  与视口比较）且文档不横向滚动；那个 58 字符的名字必须在卡片内换行而不是把页面撑宽
  （单元格 `scrollWidth ≤ clientWidth` 且高度超过一行）；回到桌面视口后单元格是 `table-cell`、
  表头回来、每格标签消失、汉堡隐藏、侧边栏回到布局里，而宽于窗口的表格是在**自己的容器**里
  滚动（`overflow-x: auto` 且文档不越界）。无障碍一组读的是 Chrome 的**无障碍树**
  （`Accessibility.getFullAXTree`），不是 DOM 属性：每个控件都有非空名字、抽屉按钮的
  `aria-expanded` 在开合时分别报 true/false、`aria-current="page"` 只落在正在显示的那一栏、
  键盘最先到达的是跳转链接且指向主区域，以及上面那条修复的回归检查。变异测试：把
  `@media (max-width: 700px)` 整块删掉（回到「表格永远是表格」），版式一组失败 3 项
  （`every cell of a row is a card block`、`every card carries its column name`、
  `the page does not scroll sideways at 380 px`）；把导航那一行改回写死的 `aria-label`，
  无障碍一组失败 1 项。
- **面板有了「账本」页：带宽账本此前只有接口，没有界面**。`README.md` 把「带宽账本」列为一等
  功能（Ed25519 签名、哈希链、`--verify-ledger`、`--ledger-proof`），`GET /api/ledger` 也一直
  返回公钥、链头、最近条目与按客户端汇总，但 `web/dashboard/README.md` 里明写这一页不渲染它，
  要看只能自己 `curl` ——这是整个面板里唯一"有数据、没有地方显示"的功能。现在新增第五个栏目
  「账本 / Ledger」：是否启用、文件、链上的条目数、链头（单元格里是缩写，完整哈希在 `title`
  里）、尚未入账的活动用量与完整公钥，下面两张表是按客户端汇总（条目数、入站、出站）与最近
  条目（序号、时间、客户端、代理、双向字节、服务器签名的哈希）。`[ledger]` 关闭时接口返回
  `{"enabled": false}`，页面照实显示「否」并说明原因——画一排 0 会被读成"没有任何用量"，那是
  另一件事。两张表与其它表格走同一条重画路径，切换语言时连列标签一起重画；页面上那把公钥就是
  `--verify-ledger <文件> --ledger-key <公钥>` 需要的那把，所以屏幕上的数字可以脱离服务器与
  文件对账。变异测试（真实浏览器）：把语言切换里的那行重画删掉，账本一组失败 2 项
  （`the ledger state row follows it too -> want '是' got 'yes'`、
  `and the rows are redrawn with their new column names -> want '时间' got 'Time'`）；让条目表
  永不填行，失败 3 项（行数与 API 不符、没有第一行、列标签拿不到）。
- **面板补上「目录」页：`/api/dht` 此前也是"有接口、没界面"**。与账本同一条思路——DHT 是
  客户端按名字找服务器的机制，运维要能看见本节点在不在通告、通告了哪些名字、对外给的是哪把
  公钥。现在新增第六个栏目「目录 / Directory」：是否启用、节点标识（行内缩写、完整值在 `title`
  里）、绑定地址、键的命名空间、路由表里的节点数、对外通告的主机名、通告签名公钥（未配置时
  显示「未签名」——那是对这个节点的陈述，而不是缺一个值），以及正在通告的名字表格；`[dht]`
  关闭时接口返回 `{"enabled": false}`，页面照实显示「否」并说明。切换语言时两张表与各行一起
  重画，请求失败时横幅点名端点，故障排除后恢复。变异测试：删掉语言切换里的重画，目录一组失败
  2 项（`the directory state row follows the language switch -> want '是' got 'yes'`、
  `and the rows are redrawn with their new column name -> want '名字' got 'Name'`）。
- **去中心化目录的通报生命周期第一次有了检查**（`scripts/functional-linux.sh` 再增 14 项）。
  `/api/dht` 的字段第一次被逐项核对：节点标识是 160 位十六进制、绑定地址是 `[dht]` 里那个、
  命名空间是默认的 `aethertunnel`、对外主机名来自配置，而 `-dht-key` 打印的公钥必须与 API
  发布的 `signing_key` 相同——那把公钥正是运维要交给 `trusted_keys` 的，两边不一致的后果是
  "看起来签过名、实际全被拒"。更重要的是通报本身：**名字跟着组走，不跟着成员走**，第二个客户端
  加入同名池时不得把名字撤下，只有最后一名成员离开才撤下。此前没有任何检查走过这条路径（撤回
  只发生在拆会话时，而过去的查询从没让会话结束）。变异测试：把 `Unregister` 里"池空了才撤回"
  改成"任一成员离开就撤回"，套件失败 3 项
  （`and the name stays announced while that member is there -> want 'True' got 'False'`、
  `one member leaving leaves the other serving the name -> want '1' got 'error: list index out of range'`，
  以及池那一节原有的 `the pool drops the member that left -> want '1' got '0'`）。
- **面板检查补上「字典」一组，并长到 143 项**（74 → 118）。页面在启动时会比对两份字典，把不一致
  `console.warn` 出来——而控制台里没有人看：一个键只存在于一种语言时，那句话会一直用另一种语言
  显示，一个谁都没提到的键则白占空间（这个文件被编译进二进制）。这一组直接读页面自己的标记：
  两种语言定义的键数相同、元素或脚本提到的每个键在两份字典里都存在、没有键被定义两次、没有键是
  多余的，并且在浏览器里核对没有任何元素把键当成句子显示出来（缺条目时 `t()` 会回退成键本身，
  那正是用户会看到的东西）。写这一组时自己的第一版有个漏洞：它把"键的定义本身"也算成了一次引用，
  而每个键当然出现在自己的定义里，于是「没有多余的键」这一项在任何文件上都会通过。规则改成先把
  两份字典从文件里整段去掉、再在其余部分里找引用之后，它当场找出了本轮新加的 `err.ledger`
  从未被使用（账本加载失败时只写了那段说明文字），于是把这个键接到错误横幅上，与配置页失败时的
  表现一致。变异测试：从中文字典里删掉 `ledger.entriesTitle`，这一组失败 3 项
  （`both languages define the same number of keys -> want (159, 159) got (159, 158)`、
  `every element's key exists in both languages -> want [] got ['ledger.entriesTitle']`，以及
  中文页面上那行标题仍是 `Recent entries`）。同一轮还新增了账本页一组（与 `/api/ledger` 逐项
  比对、两张表的内容、切换语言重画、请求被浏览器判为失败时横幅点名端点且两张表不再装作有数据、
  失败排除后能恢复），在**关闭了 `[dht]`** 的部署上把「目录未启用」画出来，以及在另一台**同时关闭了 `[ledger]` 与 `[dht]`** 的部署（套件里那台
  `max_connections = 1` 的服务端）上把「未启用」这个状态画出来——同一个文件有两种状态，只在一种
  上验证不算验证。窄屏那一组也从代理表扩到账本页：条目同样是带栏目名的卡片，64 个字符、
  连不成一个词的公钥必须在行内换行而不是把页面撑宽（实测 390 px 视口下 `scrollWidth` 仍是 390）。写这一组时被变异测试抓到自己的一个错误：挑"小于一千字节"的汇总行来逐字比对
  字节单元格时只看了 `bytes_in`，而变异用的部署恰好有一行入站 32 字节、出站 478 KB，检查于是报
  `a client's billed bytes-out match /api/ledger -> want '489440 B' got '478.0 KB'`，看起来像产品
  算错了。现在两个方向都必须小于一千字节才选它，否则打印 `SKIP`；条目表的第一行也改成防御式
  读取，表格没画出行时是一个 FAIL，而不是一段 traceback。
- **带宽账本第一次有了端到端覆盖**（`scripts/functional-linux.sh` 163 → 281 项，其中账本这一节 23 项）。账本是唯一
  "离开服务器"的用量记录：审计与指标都只活在这台机器上，只有它会交给别人核对，而 `[ledger]`
  在此前所有可复跑的检查里**一次都没有打开过**——`--verify-ledger` 与 `--ledger-proof` 这两个
  子命令、以及"改一个字节就会失败"这句承诺，都只有单元测试。现在套件的主部署开着账本，并新增
  一节（23 项）：起一个只发布一个代理的客户端，把 40 字节搬过隧道再结束这个会话（条目是在
  拆会话时写的），然后核对条目里的客户端与双向字节数正是隧道实际搬运的那些、链头等于文件最后
  一条的哈希、服务器为每条记录写了一行日志；随后只凭 `/api/ledger` 给出的公钥离线校验整个
  文件（`--verify-ledger`）、换一把同长度的公钥必须失败、把第一条的 `bytes_in` 改 1 个字节必须
  失败、`--ledger-proof --proof-index 0` 写出的前缀能单独校验且链头正是它停下的那条、越界的
  索引被拒。**把 `[ledger] enabled` 改回 `false`，这一节失败 14 项**（"on"、公钥长度、文件路径、
  条目、字节、日志、离线校验、前缀……每一项都指向同一个根因），而面板那一节会正确地切换到
  「未启用」状态的检查而不是级联报错。
- **套件里有一节把共用的读数助手悄悄指向了另一台服务端**。`api_field`/`metric_value` 读的是
  `DASHBOARD_PORT`/`DASHBOARD_TOKEN` 两个全局变量，而 `latency` 池那一节为了读自己的面板直接
  `DASHBOARD_PORT=$LAT_DASHBOARD`，此后再调用这两个助手的检查都会读到那台 `latency` 服务端——
  上面的面板一节就是这么被带偏的（它对着 latency 服务器跑完了 53 项，并因此报出两条本不该有的
  失败）。现在助手改成 `api_field_at <port> <token> <path> <expr>`，`api_field` 只是用被测部署
  的端口调用它，`latency` 那一节用 `LAT_FIELD` 明确指向自己的面板，`DASHBOARD_PORT` 只在一处
  赋值。
- **套件现在会先确认端口可用再启动，并把失败说清楚**。上一轮把套件的输出接到 `| head` 时管道
  提前关闭，脚本没走到自己的清理，残留进程占着端口；下一次运行于是级联失败 29 项，看起来像
  产品坏了，实际只是端口被占。现在 `scripts/functional-linux.sh` 在启动服务端之前逐个确认要用
  的端口还能绑定，被占就打印"某个进程或上一次被中断的运行仍占着它"并以退出码 2 结束（实测：
  硬编码一个被占的端口，退出码 2、只输出一行）。它**不是**那 70 项，差别写在该文件里。

### 文档

- **README 按完成态重写**：「这一版有意不做的能力」整节与翻旧账段落改为一句中性的设计边界
  说明；逐平台验证行只保留实测结果；三层隧道标注为 ✅（Linux）；`docs/NOT-IN-THIS-VERSION.md`
  重构为「设计边界 · Scope and Non-Goals」。
- **三条"文档断言"用当前代码重新跑实，并把其中一条接进配置参考**。三层隧道那套 23 项
  （`scripts/vpn-linux-test.sh`，要 root 与真实 tun 设备）在 3.13 之后一直没有用新代码重跑过——
  这一轮在 root 下重跑，**23/23、0 失败**，3.13 写的每一条（两端互 ping、两侧接口的收发包计数、
  `/api/vpn` 与 `/api/config` 与审计的一致、地址归还、`vpn.require` 拒绝）都在现在的二进制上重新
  成立，3.13 里补了一句。另一条是给 Windows 用户的：**CRLF 行尾与 UTF-8 BOM 的配置文件可以直接
  用**——这个此前没人验证过，也没有测试锁住，而 Windows 编辑器写出的文件默认就长这样（记事本
  还会加 BOM）。实测通过后加了 `TestAConfigWrittenByAWindowsEditorLoads` 锁住它：带 BOM 与 CRLF
  的配置加载无警告、令牌与两处端口分毫不差；这条是行为锁定测试，被锁的行为在 `BurntSushi/toml`
  里，没有可变异的实现分支。`docs/CONFIGURATION.md` 的英文摘要下补了一句给 Windows 用户的说明。第三条是 3.15 的 **Kubernetes 清单套件**（`scripts/kubernetes-linux.sh`，要 root、docker 与
  k3s）：14 项在当前二进制上 **14/14、0 失败**，收尾后系统 docker 的容器数与镜像数都是 0。
- **`-race` 那一节也用当前代码复跑了一遍**。3.16 的记录停在这轮的改动之前（`pkg/net` 的绑定
  诊断与它接进的八处绑定、配置的 CRLF/BOM 测试都是之后落进树的），于是按 3.16 的命令把整套单测
  在竞态检测器下重跑：14 个包全过、**0 处数据竞争**（`pkg/server` 37.7s、`pkg/reliable` 14.1s、
  新的 `pkg/net` 2.4s）；缓存恰好是空的，"空缓存首次解包"也顺带重证了一遍（42.8 MB、11 秒）。
  3.16 末尾补了这段记录。
- **Windows 那一栏从"上一轮的数字"换成实测，并说明它此前为什么跑不起来**。`docs/PLATFORMS.md`
  新增 3.20：非特权用户命名空间那条限制、root 之下两条绕开它的做法（root 的挂载命名空间，或一条
  `/usr/share/wine` 符号链接）、新脚本的分节与项数、四对平台组合各 55/55 的那张表、以及没跑到的
  两件事（`scripts/smoke-test.ps1` 要 PowerShell；`windows/amd64 ↔ linux/arm64` 两对因本机没有
  arm64 硬件没跑）。2.1 节下面补一段说明：那份表里 `windows/amd64` 的四行仍是旧脚本的数字，
  3.20 的 55 项是**第三套**检查，两套数字不要互换。结论表里 `windows/amd64` 一行的
  `**本轮未跑**` 换成 **55/55**；3.1 节保留原来的记录并补一句它已不再阻塞；README 的
  「逐平台功能验证」一行把"其后新增的两项/三项尚未在 Windows 上执行"换成指向 3.20 的
  `scripts/wine-check.sh` **55/55**（README 与 3.1 原来一个说两项、一个说三项，本轮不再引用那个
  数字）。
- **把 `docs/SECURITY.md` 第 9 节那句"直连流量的账本条目是 0 字节"从断言变成实测，并把这条语义
  接到 `[ledger]` 的配置说明上**。第 9 节一直写着账本只覆盖服务端真正搬过的字节、`xtcp` 打洞后的
  流量不在其中，但仓库里没有任何一次运行量过它。这一轮用三个容器（服务端、发布方、访问方）跑一条
  `xtcp`，从访问方的监听端口搬一次数据，并让发布方**优雅停止**——条目只在会话结束时追加——然后同时
  读三处：搬字节成功（`True`）；服务端 `bytes_from_clients_total` 与 `bytes_to_clients_total` 都是
  **0**，而 `p2p_punches_total=1`、`p2p_direct_total=1`、`p2p_relayed_total=0`；账本是
  `idx=0 proxy='p2p' bytes_in=0 bytes_out=0`；审计里是
  `visitor_accepted, p2p_direct, proxy_removed, client_disconnected`。三处结论一致：直连的字节不进
  账本、不进双向字节指标，"这条路是直连"写在审计与 p2p 计数里。同一次运行还确认了写入时机：会话
  结束时追加，所以**被 `docker rm -f` 强杀的服务端不留条目**——3.18 的 `private` 那一节 state 目录里
  账本是空的，而 `core` 会 `docker stop` 客户端再等文件出现，两者差别在此，不是程序行为不一致。
  `docs/CONFIGURATION.md` 的 `[ledger]` 一节补上这句限定与指向第 9 节的引用（原来只写"双向字节"，
  读者可能以为它统计全部用量），`docs/PLATFORMS.md` 3.19 记下这次的读数。
- **矩阵的第十一个场景：把程序放进 musl 用户态跑一遍（Alpine 的 minirootfs），并在同一轮里修掉一个
  会伪造失败的端口分配问题**。3.18 的十个场景全跑在 `FROM scratch` 上——那个镜像里没有 libc、没有
  shell、没有 `/etc`，所以它证明的是"这个静态二进制什么都不需要"，并不能说明这个程序在一个真实
  发行版里跑得起来。这一轮补上这一维：Docker Hub 依旧不可达（没有 `alpine` 镜像可拉），但 Alpine
  把 minirootfs 作为 tarball 发布、而 `docker import` 能把 tarball 变成镜像，于是同一个静态二进制
  可以在 musl + BusyBox 之下跑同样的功能路径。新场景 **13 项**：镜像导入、用户态确实是 musl
  （存在 `/lib/ld-musl-x86_64.so.1`）、BusyBox 可执行、二进制在 musl 下启动、**musl 自己的 `ldd`
  回答 `Not a valid dynamic program`**（这一条把"程序在 musl 用户态里跑"与"这个二进制依赖 musl
  才能跑"分开，后者会让这一维看起来通过而什么都没测）、`/healthz`、面板令牌 401/200、无 panic、
  两个 musl 容器之间搬字节、审计写在该容器里且属主是 uid 65532、mode 0600、客户端无 panic。
  七个版本全过：**六个版本各 95 项、570/570，七个版本合计 573/588**（19.03.15 仍是 cgroup v2
  那条界线）。同轮的另一个发现在 harness 里：场景的端口是"绑定 0 端口、记下号码、立刻关闭"分配的，
  内核可能把同一个号码再分给下一个刚关闭的 socket——**实测 1000 组、每组 10 个端口里有 4 组重复**
  （更早一次抽样 5000 组里 16 组）。同一场景里两个用途拿到同一个号码时，服务端会因
  `address already in use` 退出，而外面只看到"面板连接被拒"与"客户端首字节是 `0x65`"（`e` 来自
  echo 的 `echo:` 应答）；28.4.0 上出现过一次这样的 8 条失败，连跑四遍不再出现，按分配器一测即复现。
  分配器现在跳过已发出的号码，同样 1000 组实测 0 组重复，此后整套连跑三遍每遍都是 573/588、
  19.03.15 之外 0 条失败。这一轮同样**没有改程序代码**：改的是临时 harness 与
  `docs/PLATFORMS.md`（新增 3.19，并更新第 4 节的数字）。
- **七个 Docker 版本的矩阵补齐：十个场景、494/504，19.03.15 的失败定位到它自己的 daemon**。
  3.18 那一轮结束时有两件事只做到一半，两处都写明了"要一次提权才能重跑"：19.03.15 的 daemon 因为
  启动脚本固定传 `--ip6tables` 而起不来（`unknown flag: --ip6tables`），以及所有私有 daemon 还在
  共用系统那个 containerd。本轮拿到一次授权之后把两件都重跑了，并把场景从七个扩到十个（新增
  `tls`、`post_quantum`、`vpn_client`），每个版本 **82 项**。结果：**六个版本各 82 项、492/492
  全过；19.03.15 上十节里只有两项成立（`docker server` 版本号与 `FROM scratch` 的镜像构建），
  合计 494/504**。19.03 那 10 项失败来源只有一个：daemon 起来并回答 `client=19.03.15
  server=19.03.15`（说明那次 `dockerd --help` 探测是对的），但每次 `docker run` 都以退出码 125
  失败，device 那一节报出
  `docker: Error response from daemon: cgroups: cgroup mountpoint does not exist: unknown.`——
  这台机器的 `/sys/fs/cgroup` 是 cgroup2fs。这句话起初归因于 runc，两次替换把它定位到了 daemon
  自己：把 20.10 那份 runc 1.2.5 放到 19.03 的 containerd 前面（containerd 1.4 走 runtime v1，
  按 PATH 找 `runc`），错误不变；再把 19.03 的 dockerd 嫁给 24.0.9 的 containerd 1.6.33 与同一个
  runc 1.2.5，错误仍然不变——而 dockerd 的日志把它记在自己处理请求的那一行
  （`Handler for POST /v1.40/containers/…/start returned error: cgroups: cgroup mountpoint does not
  exist: unknown`，前面还有 cpu/cfs/rt 各一行 `Your kernel does not support cgroup …` 与一行
  `Unable to find blkio cgroup in mounts`），同一时刻 containerd 的日志里一次 task 创建都没有。
  也就是说拒绝来自 **dockerd 19.03 的 daemon 侧**，而 docker 从 20.10 起才支持 cgroup v2——这正是
  20.10.24 能跑满 82 项、19.03.15 一项也跑不了的那条界线。所以这一栏是"在老 daemon 上跑容器"
  这件事本身的边界，不是程序的结论：19.03 的控制面（API、镜像构建、版本号）可用，容器运行不可用。
  另外，矩阵的每一项都用**那个版本自己的 `docker` 客户端**驱动它自己的 daemon：系统那份 29.1.3
  的客户端对旧 daemon 直接拒绝（`Error response from daemon: client version 1.52 is too new.
  Maximum supported API version is 1.40`），拿它的 `--format` 读 `server` 只会得到空字符串，那不是
  daemon 没起来。两处基础设施改动重跑后的证据：
  七个私有 dockerd 的命令行里 `--containerd` 各指自己的 socket，进程表里除系统那个 containerd
  之外多了七个（8 个进程、7 个各自的 `--address`）；系统 dockerd 的日志里 `could not find
  container` 从重跑前 20 分钟的 **1155** 行降到重跑后的 **0** 行，系统 docker 的容器数与镜像数
  都回到 0。本轮**没有改程序代码**，改的是这台机器上的做法，以及 `docs/PLATFORMS.md` 3.18 与
  第 4 节里的数字（"七个场景 / 65 项 / 390/390" 全部换成 "十个场景 / 82 项 / 六个版本 492/492、
  七个版本 494/504"）。验收按同一套流程走了一遍：`gofmt -l .` 无输出、`go vet ./...` 无输出、
  `go test ./... -count=1` 14 个包全过、`scripts/functional-linux.sh` 281 项全过（含真实浏览器
  那 143 项），与上一轮一致。
- **`docs/MIGRATION.md` 两处把未知键说成会让 `--check` 失败**。第 2.3 节的 `[vpn]` 旧键一行写着
  "**未知键**：启动时列出、`--check` 失败"，第 4.1 节的 `[obfuscation] default_type` 一行写着
  "现在会被当作未知键，`--check` 会失败并指出该行"；按默认行为 `--check` 只会打一行警告并退出 0
  （实测：把 `[obfuscation] default_type = "tls-record"` 放进配置，输出 `c.toml is valid`、退出码
  0）。迁移文档恰恰是让人"升级前跑一次 `--check`"的地方，照着它读会以为这一步能把残留的旧键挡住。
  两处都改成写明默认只是警告、并指出 `--check --reject-unknown-keys` 才是失败；第 5 节检查清单的
  第 2 步也改用这个组合，让残留的旧键在升级检查里直接失败。同一次核对里用真实二进制验了迁移文档
  另外几条 `--check` 断言，都对：同一客户端里两个同协议代理抢同一个 `remote_port`（拒绝并点名两个
  代理）、`http_port` 与 `bind_port` 撞同一地址（拒绝并点名两个键）、`dial_timeout_seconds = -1`
  （拒绝）、`pad_to` 超过帧上限（拒绝并给出上限）、`max_reconnect_seconds < reconnect_seconds`
  （拒绝）、以及服务端 `[[proxies]]` 里写客户端专属键时逐条给出警告。
- **`docs/ARCHITECTURE.md` 的调度策略表少了 `bandit`**。表格列了 `round-robin`、`random`、
  `latency`、`failover`、`adaptive` 五种，而实现里是六种：`config.LoadBalanceBandit`（UCB1）在
  `README.md`、`docs/CONFIGURATION.md` 与 `pkg/server/bandit_test.go` 里都在；只有架构文档的
  表格与它下面那段（讲移动平均与连续失败数）还停在五种，读架构文档的人会以为在线学习那一种不在
  实现里。现在补上表格一行（UCB1 索引 = 平均奖励 + `sqrt(2·ln(候选被选次数之和+1) / 本成员被选次数)`）与一段
  说明：奖励按 `1 / (1 + 秒数 / 0.05)` 记，每 20 次选择强制去测观测最少的成员，学习只来自这个池子
  服务过的流，没有离线训练与模型文件。文字与 `pickByUCB`、`banditRewardFor`、
  `banditRecoveryInterval` 的当前实现逐条对照过。这一条只改文档，`gofmt`、`go vet`、全部单元测试与
  `scripts/functional-linux.sh`（281 项）仍全过。

---

## [3.7.4] — 2026-09-22

本版本把面板的代理池一行补全：`/api/proxies` 在每个成员上返回 `latency_ms` 与
`consecutive_failures`（池状态字段与这几种均衡策略都是 v3.2.0 加的），但面板只显示
"几个成员、几个可用"，于是 `latency`、`failover`、`adaptive` 三种策略为什么把流量给了
这个成员、绕开了那个成员，在界面上看不出来。现在成员数大于 1 的池会逐成员列出这两个数。
另外让服务端把池里**实际生效**的端口回给客户端（此前客户端日志写的是它请求的端口，
服务端却在另一个端口上监听），修掉自动封禁里一段**永远不会发生**的"时长翻倍"、让
`[client].heartbeat_seconds` 从装饰变成真正的回退值、修掉一个会让门禁时绿时红的不稳定测试，
并修正三个随版本发布的文件里的过时版本号。配置与线协议没有变化，v3.7.3 的配置与二进制
可以直接升级。

### 新增

- **每个平台都真实跑过一遍功能**，不只是"能编译"。此前六个发布目标的验证程度并不相同：
  linux/amd64 与 windows/amd64 有端到端脚本（CI 的 Windows 作业跑 101 项），macOS 跑单测，
  而 **linux/arm64 与 windows/arm64 只被交叉编译、从未被执行过**。这一版把这几个平台真正
  跑了起来（细节与证据见 `docs/PLATFORMS.md`）：
  - **linux/arm64**：用 qemu-aarch64 在 arm64 指令集上跑完整套件、29 项全过（CI 只交叉编译
    它，从未执行）；
  - **windows/amd64**：用 Wine 跑真实的 PE 二进制、28 项全过（与 CI 的 Windows 作业互为独立证据）；
  - **linux/amd64**：原生 29/29。
  - 每个平台的检查都分两组：隧道 22 项 + 命令行 7 项（两端 `--version`、两个二进制分别对随版本
    发布的 `server.toml.example` / `client.toml.example` 做 `--check`、客户端 `--identity`
    生成密钥并打印公钥）。命令行这组立刻抓到一个只在 Windows 上出现的差异：客户端生成的
    私钥文件在 Linux 上是 0600，在 Windows（Wine 下）是 0664——Windows 没有 POSIX 权限位，
    Go 的 0600 不设置 ACL，实际保护来自目录继承的 ACL。文档原先把"权限 0600"写成无条件的，
    现在写明平台差异（`docs/SECURITY.md` 新增 9.1 节，`docs/CONFIGURATION.md` 的键表同步）。
  - darwin/amd64、darwin/arm64、windows/arm64 在本机无法执行（Darwin 内核无法模拟，x86-64
    上的 Wine 也跑不了 ARM64 的 PE），只做了静态核对：二进制格式、架构、Go 版本与内嵌符号。
    macOS 上的执行由 CI 的 macos-latest 作业覆盖（它是 arm64），darwin/amd64 与 windows/arm64
    目前**没有任何地方执行过**，这一点写在文档里而不是含糊过去。windows/arm64 另有一条路
    （qemu 跑 ARM64 用户态 + ARM64 版 Wine）也被试过并记录为不可行：Wine 加载 PE 时要再 exec
    自己的 loader，而在 qemu 用户态下 `/proc/self/exe` 指向 qemu；让该 exec 透明的唯一办法是
    注册 binfmt_misc，可非特权用户命名空间里的注册会被内核拒绝。

- **`--ledger-proof <文件> --proof-index <n>`：导出到第 n 条为止的账本前缀**。账本里早就有
  `ledger.Proof`（带单元测试），但没有任何接口能产出这样的证明，于是"只需证明某一条的
  包含性"这个能力使用者够不到——审计场景里只想证明某一段用量的人，此前只能把整条链交出去。
  现在可以把这段前缀按账本自己的 JSONL 格式写到标准输出，`--verify-ledger` 可以直接校验它，
  其链头就是整条链在第 n 条的哈希；索引越界、缺失或账本为空都会报错。

### 新增

- **`load_balance = "bandit"`：在线学习的代理池策略**（UCB1 多臂老虎机）。每条流按应答速度记为
  奖励（立即回答记 1，越慢越小；失败记 0），据此估计各成员的平均奖励并加一个探索项来选择成员。
  这补上的是此前 `adaptive` 的一个真实短板：它的代价函数是确定性的，遇到一个坏成员只能靠惩罚项
  的上限慢慢翻身，而**奖励差距大时纯 UCB1 也会长期不再采样那个成员**——所以新策略每第 20 次选择
  会把机会给观测最少、且估计最差的那个成员，让恢复后的成员能被重新测量。判断抽成
  `meanReward`/`ucbScore`/`banditRewardFor`，`adaptive` 与其它策略行为不变，默认仍是
  `round-robin`。
  - **单测 6 项**（`pkg/server/bandit_test.go`）：先均匀试一遍每个成员；600 次选择后把 ≥80% 的
    后半段流量给更快的成员，且**不会**给 100%（探索项失效会被发现）；一直不应答的成员在 300 次里
    被选中不超过 30 次、但也不为 0（能翻身）；估计最差的成员仍会被采样；未观测成员分数为 +∞；
    奖励随延迟单调下降且落在 (0,1]。测试调用的是 `pick()` 这条**真实分发路径**（含策略选择与健康
    过滤），把策略分发改坏后其中三项按预期失败（"the better member served 0 of the last 300
    streams"、"sent 300 of 300 streams to a member that never answered"）。
  - **真实进程对照，并在三个平台上各跑一遍**：两个客户端组成一个池，一个转发到可用的回显服务、
    另一个指向无人监听的端口。30 次访问里 `round-robin` 浪费 **15 次**、`bandit` 只浪费 **4 次**
    （linux/amd64、linux/arm64、windows/amd64 三个平台实测一致）；访问本身在两种策略下都不丢
    （服务端发现成员不应答会改问另一个成员），而"仍在采样"这一条也成立。
- **`docs/NOT-IN-THIS-VERSION.md` 里的"机器学习路由"一项随之删除**：那份清单的意义是"名称按实际
  能力书写"，现在路由里确实有一个在线学习算法，这一项不再成立。新能力按实际做法描述：在线学习、
  没有离线训练与模型文件。

### 变更

- **面板的代理池逐成员显示延迟与连续失败次数**。`load_balance` 为 `latency` 时按延迟选，
  `adaptive` 按"延迟 × 连续失败惩罚"选，`failover` 在成员连续失败后换人——这三个数就是
  它们判断的依据，此前只有 API 返回、界面不讲。成员行的标识是**客户端 ID**而不是本地地址：
  一个池里的成员通常都转发到同一台服务的同一个端口，本地地址区分不开它们；只有当某个成员的
  本地地址与整行显示的不同时，才额外把它跟在客户端 ID 后面。**还没有应答过一次的成员延迟
  显示 `—`**：服务端在成员第一次应答之前把 `latency_ms` 留为 0，显示成"0.0 毫秒"等于报出
  一次并不存在的测量（连续失败次数照常显示，那正是这种成员会累加的数）。只有一个成员的代理
  保持原来的一行显示，行为与 v3.7.3 一致。
- **服务端把池里实际生效的端口回给客户端**。代理池只拥有一个端点，端口取自第一个成员；
  后到的成员即使请求了别的端口也会被并入池中，而它自己那条请求不会被采纳。此前客户端只收到
  "server confirms N tunnel(s)"，日志里留着它**请求**的端口，服务端却在另一个端口上监听——
  照着日志去连会连到一个没人监听的端口。现在 `TypeProxyList` 的 `RemotePort` 报的是这个
  名字实际可达的端口（池成员报池的端口，不在池里的代理报自己的），客户端日志相应写成
  `server confirms 2 tunnel(s): pooled on port 6022`。**线上的字段与取值方式没有变化**：
  只是服务端填这个字段时改用了实际端口。
- **`[client].heartbeat_seconds` 从"写了不生效"变成真正的回退值**。心跳间隔一直由服务端在
  会话建立时下发（`AuthResponse.heartbeat_seconds`），客户端按它发心跳——这是对的，因为
  "连续三次收不到就断开"的是服务端。但客户端此前**完全忽略**自己的配置项，服务端没下发时
  回退到硬编码的 30 秒，于是这个键只是装饰：`docs/CONFIGURATION.md` 把它写作"心跳间隔"，
  改它没有任何效果。现在服务端没下发时用配置值，与下发的值不同时客户端会明确报告
  （`the server asks for a heartbeat every 2s, so client.heartbeat_seconds (1m0s) has no effect
  on this session`）。同时删掉 `Config.ServerHeartbeatInterval`——它全仓库无人调用，
  注释还写着旧名字。
- **`server.toml.example` 与 `client.toml.example` 的首行版本号从 `v3.3.0` 改为当前版本**。
  这两个文件随每个 Release 一起发布，此前四个版本没有更新过首行，读者会以为它们描述的是
  v3.3.0 的配置。`deploy/kubernetes/kustomization.yaml` 的 `newTag` 同样从 `v3.2.0` 更新。

### 修复

- **`[dht] discover` 这个客户端功能此前从未真正工作过，已修**。文档写着"`client.server_addr`
  为空时必须设置 `[dht] discover`，据此按名字找到服务器"，但客户端启动时**没有先向 bootstrap
  邻居引导**就发起解析（命令行 `--discover` 会先引导，客户端不会），于是对着空路由表查询，
  必然得到 `key not found`，接着直接 `Fatalf` 退出——同一份配置用命令行却能查到。实测：客户端
  进程启动即报 `did not resolve`，等 70 秒也不会恢复。现在客户端在首次解析前先引导，并对
  "服务器尚未通告 / 自身节点尚未与邻居通话"这种正常启动顺序做有界重试（30 秒，每 3 秒一次），
  超时后的报错说明重试了多久。三平台实测：一个 `server_addr` 留空、只写 `[dht] discover` 的
  客户端能解析出控制端口、连上并真实转发数据。
  - 顺带修掉一处会把操作者带偏的地方：解析到的记录若属于**公网类型**（`tcp`/`udp`/`http`/
    `https`），它的地址是"访问者从哪里到达该代理"（`tcp`/`udp` 是公网端口，`http`/`https` 是
    共享监听端口），并不是控制端口——只有私有代理（`stcp`/`sudp`/`xtcp`）的记录才写控制端口。
    客户端此前会把这个地址当成控制地址闷头去连，现在会明确警告并指出该用私有代理名。
    判断抽成 `namesAControlPort` 并配单测。


- **修掉一处我自己上一轮引入的误导性提示**：客户端在"服务端接受连接却在握手前关闭"时一律说
  "check that [obfuscation] and [transport] match on both ends"，而这类关闭的原因有好几种——
  `[encryption]` 的算法/口令/salt 不一致、`[transport]` 不一致、伪装不一致、访问名单拒绝、
  封禁。实测七个用例里有四个被指向了错误的设置。现在提示改为列出候选原因并指向真正写下原因的
  服务端日志，服务端一侧也对"首帧认证失败"这一类补上了
  `(check that both ends agree on [encryption] algorithm, passphrase, salt and post_quantum)`。
  该判断抽成纯函数 `handshakeFailureHint` 并配单测（认证失败给出提示、伪装与长度错误不给
  误导性提示）；把提示条件改成永不成立后，单测按预期失败。


- **修掉测试脚手架里的一处数据竞争**（`pkg/server/proxy_test.go`）：`testAgent.serve` 为每条流
  起一个 goroutine，而这些 goroutine 会调用 `t.Logf`；agent 清理时只等控制循环退出，不等它们。
  于是前一个测试结束后，它的流 goroutine 仍可能往已结束的测试里写日志——竞态检测器报
  `Read at ... testing.(*common).Logf ... serveTarget ... Previous write ... tRunner.func1`。
  产品代码没有任何竞争，但 **CI 的 `-race` 作业会随机变红**（本仓库 2026-09-22 的
  `ccb0eb1` 那次就是这样红的，同一提交前后两次都绿）。现在 agent 用 `WaitGroup` 跟踪这些流
  goroutine 并在清理时等待它们。修复前复现过两次（一次在 CI、一次在本机），修复后连续约十轮
  未再出现；由于这是时序相关的竞争，`-race` 作业仍是权威。
- **本机现在可以跑 `-race`**：解出 `gcc-14`、`binutils`、`libc6-dev` 等包（`dpkg-deb -x`，
  不需要 root），设置 `CC` 与 `CGO_ENABLED=1` 即可。此前交接文档注明 `-race` 只能靠 CI。
  本轮 14 个包在 `-race` 下全部通过。


- **自动封禁的"时长翻倍"此前永远不会发生**，`ban_max_seconds` 因此是一段死配置。
  `server.toml.example` 写着"重复违规者每次封禁时长翻倍，直到 `ban_max_seconds`"，
  `ban.go` 里的实现与单元测试也都在，但真实路径上做不到：封禁期间该来源的连接在握手前就被
  拒（`aethertunnel_banned_connections_refused_total` 计的就是这件事），它不可能再累加失败
  次数；而 `banList.obsolete` 把"封禁已过期、失败次数为 0"的条目当作没有价值的信息删掉，
  于是一个来源一出封禁期、下次连接就把自己的历史连同封禁次数一起丢掉，再犯时永远是
  `ban number 1`、永远拿初始时长。实测 `ban_seconds = 2`、`ban_max_seconds = 16` 时，
  连续五轮的时长是 2/2/2/2/2 秒，审计每次都是 `ban number 1`。
  现在被封禁过的来源在封禁结束后**保留一个窗口**（与失败计数同一个 10 分钟）：窗口内再犯
  才按倍数增长，窗口过后条目照样回收，列表仍然只由正在失败的来源决定。修复后同样配置实测
  为 2/4/8/16/16 秒，审计依次是 `ban number 1..5`。
  - 一直没被发现的原因值得记下来：`TestBanListDoublesEachBanAndStopsAtTheMaximum` 存在且
    一直通过，但它直接调用 `fail()`，**从不经过 `blocked()`**，而删条目的正是服务端每接受
    一条连接都会先走的 `blocked()`。测试绕开了真实路径，于是断言了一个服务端做不到的行为。
- **`TestDatagramPumpAccountsForEverySession` 是个不稳定测试，已修**。数据泵在两个方向上
  都是"先把数据交给对端、紧跟着计数"，于是一个已经收到回显的客户端可能赶在计数之前调到
  `Shutdown()`，让会话报出的总字节数少一半。单独跑 60 次不复现，六个并行跑（各 `-count=10`）
  就有一次报 `only 9 bytes were accounted for`——而"门禁全绿才发布"的前提是门禁本身可靠。
  现在测试通过 `OnDatagram`（紧跟在计数之后触发）等到两个方向都到账再关闭；同样八路并行
  跑 80 次全绿。产品侧的语义没有动：计数仍然只在成功转发之后加，把写失败的字节算作已发送
  才是错的。
- **Kubernetes 部署照原样部署起不来，已修**。`deploy/kubernetes/deployment.yaml` 用
  `envFrom: secretRef` 注入凭据，而 `envFrom` 是把 Secret 的**键名**原样变成环境变量名：
  这里的键名是短的 `auth-token` / `dashboard-token`，服务端读的却是
  `AETHERTUNNEL_AUTH_TOKEN` / `AETHERTUNNEL_DASHBOARD_TOKEN`，凭据于是根本传不进程序，
  服务端拒绝启动并报 `server.auth_token is required`（好在是拒绝启动，不是拿空令牌运行）。
  现在 Deployment 用显式的 `env` + `valueFrom.secretKeyRef` 做映射，Secret 的键名与
  `kubectl create secret` 的既有用法都不用改。同时补上部署文档里**漏写的一条**：`ConfigMap`
  打开了 `[obfuscation] disguise = "tls-record"`，因此**每个客户端都必须配同样的伪装**，
  否则会在握手处被关闭（`pad_to` 与 `jitter_millis` 不必一致——帧里自带是否填充）。
- **客户端在"服务端没应答就断开"时给出可操作的提示**：上面的伪装不一致场景里，服务端日志
  写着 `the stream is not carrying record-framed data`，客户端却只说 `read: connection reset
  by peer; reconnecting` 并无限重试——配客户端的人正是看着这份日志的人。现在客户端会在错误里
  点明"检查两端的 `[obfuscation]` 与 `[transport]` 是否一致"。

### 文档

- **README 的结构改了**：原先紧跟能力表的"这一版没有什么"（七项有意不做的能力）移入
  [`docs/NOT-IN-THIS-VERSION.md`](NOT-IN-THIS-VERSION.md)，README 中只留一行指针。**事实一条未删**
  ——每条仍然写明缺的具体是什么、以及同样目的下本程序真正可用且已被检查覆盖的做法；补充了每项
  "要真正实现需要什么"（Wintun 驱动与可在 Windows 上验证的环境、iOS/Android 工具链、或引入
  WebRTC/zk-SNARK 这类重依赖）。这么改是因为 GitHub 首页上那段清单会被读成"这个项目有很多功能
  没做完"，而它本意是"名称按实际能力书写"。
- **README 的能力表新增一行"逐平台功能验证"**：三个可执行平台各 64 项、跨系统矩阵六对组合各 22 项
  与五对组合各 6 项，全部实测通过；同时修掉英文能力表里一处事实漂移（运维脚本写成 89 项，
  实际是 101 项，中文那份是对的）。


- **新增跨系统矩阵：两端来自不同平台，跑同一套功能检查**。此前的平台测试两端同源，看不到
  跨系统才有的差异。现在六对组合（linux/amd64 ↔ linux/arm64 双向、linux ↔ windows 双向、
  arm64 ↔ windows 双向）各跑 22 项隧道检查，**全部 22/22**；四层安全（`aes-256-gcm`、后量子
  X25519+ML-KEM-768、TLS **带真实证书校验**、服务端 `require_identity = true`）也按跨系统
  方式跑了五对，**每对 6/6**。证书校验因此走过了三个平台各自的 TLS 实现，后量子与身份签名走过了
  各自的密码学实现。
- **每个平台的检查那时扩到 67 项**（隧道 22 + 命令行 7 + 池策略 3 + 命名与目标 5 + 发现与私有认证 8 +
  四层安全两遍共 17，另有 1 项在 Windows 上为报告而非断言）。那一轮三平台实测：linux/amd64 **67/67**、
  linux/arm64（qemu）**67/67**、windows/amd64（Wine）**66/66**。
- **每个平台的检查新增一组"命名与目标"（5 项）**：`domains` 的 `*.通配` 对单级与多级子域都
  生效、对裸后缀不生效；`allow_targets` 里放 CIDR 时，**目标写成域名会被先解析再匹配**（`localhost`
  被 `127.0.0.1/32` 允许），解析到范围外的名字仍被拒。
- **"四层安全栈"扩成算法矩阵**：文档里的两种算法各跑一遍；`aes-256-gcm` 那一遍同时改用
  `ca_file`（**真实证书校验**，而不是 `insecure_skip_verify`）并要求服务端
  `require_identity = true`。


- **每个平台的检查从 38 项扩到 46 项，新增"发现与私有认证"一组（8 项）**：DHT 按名字解析
  （`--dht-lookup` 对 `tcp` 名解析到公网端口、`--discover` 对私有名解析到控制端口）、一个
  `server_addr` 留空的客户端靠 `[dht] discover` 解析出服务器并真实转发数据、把公网类型名字
  当作服务器地址会被报告为错误、nizk 访问者（知道密钥的能过、密钥错的被拒）、以及
  `subdomain_host` 约定（没有 `domains` 的 http 代理以 `<名字>.<该值>` 可达）。三平台实测：
  linux/amd64 **46/46**、linux/arm64（qemu）**46/46**、windows/amd64（Wine）**45/45**。
- **每个平台的检查从 29 项扩到 38 项，新增"四层安全栈"**：加密（`xchacha20-poly1305` +
  `post_quantum = true`）、TLS 控制口、`disguise = "tls-record"` 伪装、Ed25519 身份认证
  （服务端 `allowed_keys` 只放客户端的公钥）**同时打开**，然后真实转发一次数据。每一项都断言它
  自己那条日志（`encryption: xchacha20-poly1305`、`post-quantum session key ... agreed`、
  `the control port is wrapped in TLS`、`connection disguise: tls-record`、
  `client identities: 1 allowed key(s)`），因此某一层被静默关掉会失败，而不是靠其它层通过。
  三个可执行平台实测：linux/amd64 **38/38**、linux/arm64（qemu）**38/38**、
  windows/amd64（Wine 下真实 PE）**37/37**（私钥模式一项在 Windows 上报告而非断言）。
  这也回答了一个此前没有验证过的问题：**Windows 版客户端的加密、TLS、伪装与身份认证是否真的
  可用**——之前只在 Windows 上跑过运维脚本，没有单独验证这四层。


- **修掉运维脚本里一项会让 Windows 作业随机变红的检查**：`scripts/smoke-test.ps1` 的
  "a source inside the burst is served and then rate limited" 依赖"检查开始时突发额度是满的"，
  而同一台 guard 服务器在检查开始前被 `Wait-ForPort` 探测过——`Wait-ForPort` 是**用连接**判断
  就绪的，每次探测都消耗该来源的一个令牌，而 `rate_limit_burst` 只有 2。旧配置的速率是
  1 次/秒，令牌能被补回来，于是这项检查多数时候通过、偶尔失败（2026-09-22 的 `c89ae98` 那次
  Windows 作业就是这样：100 项通过、这一项失败）。
  现在：等待就绪改为探测**面板端口**（另一个监听器、不受限流），突发检查本身不再被提前消耗；
  `rate_limit_per_second` 从 1 改为 **0.01**，使检查窗口内不可能补发令牌，结论不再取决于尝试
  发得多快；两项计数改为**按增量**断言，因此累积计数里有什么历史值都不影响结论。断言强度没有
  放宽。
  验证方式：本机用同一组配置（0.01/2）跑等价探测，并**故意把突发外的三次尝试间隔 1.2 秒**
  ——正是旧写法会失败的条件——四项全过（突发内不被限流、突发外每次都被拒、审计写
  `rate_limited`、服务端日志说明原因）。PowerShell 脚本本身无法在本机执行，仍需在 Windows 上
  跑一遍确认。


- **新增 Go 测试 `TestAPooledMemberIsToldThePoolsPort`**：两个成员请求不同的公网端口，
  第二个成员从 `TypeProxyList` 里读到的必须是池的端口而不是它自己请求的那个；不在池里的
  代理仍报自己的端口。把 `sendProxyList` 改回填成员自己的端口，这条测试按预期失败
  （`the member was told port 41527, want the pool's 44663`），据此确认它判断的是行为。
- **面板改动在真实浏览器里核对**（headless Firefox + WebDriver，30 项）：起一台服务器与
  **多个同名同组的客户端**组成代理池（其中一个的本地服务是关闭的端口，因此它从未应答过），
  让页面先跑出真实延迟，再核对
  - 池里每个成员各占一行，行数与 `/api/proxies` 的 `members` 一致；
  - 每行渲染出的延迟与失败次数等于同一时刻 API 返回的 `latency_ms` 与
    `consecutive_failures`；
  - 从未应答的成员显示 `—` 而不是 `0.0 毫秒`，同时仍显示它累计的连续失败次数；
  - 两行靠客户端 ID 区分得开（这是本次改动要修的点：两个成员共用同一个本地地址）；
  - 数值与单位占同一个行盒，且 `毫秒` 不被拆成两行——用 `Range.getClientRects()` 测，
    跨行会返回两个矩形；
  - 手机视口下汇总行与成员行纵向堆叠（表格在窄屏变成卡片，单元格本身是 flex 行）；
  - 中英两种语言、1280×900 与 390×844 两种视口下都成立，且页面不出现横向滚动、
    没有未翻译的键或未替换的占位符。
  - 把 `word-break: keep-all` 与数值单位之间的不换行空格撤掉后重跑，单位那一项按预期失败
    （1280px 下 `0.5 毫秒` 的行盒变成两个）；把"零延迟不显示数字"的判断撤掉后重跑，那一项
    也按预期失败（死掉的成员被显示成 `0.0 ms`）。据此确认这些检查真的在判断渲染结果。
- **新增 Go 测试 `TestBanListRemembersARepentantSourceThroughBlocked`**：走真实路径，
  封禁 → 经过 `blocked()`（服务端接连接时走的就是它）→ 服满封禁 → 再失败，必须是
  `ban number 2` 且时长翻倍；再验证窗口过后记忆被清掉、重新从 `ban number 1` 开始。
  撤掉 `obsolete` 里那段保留逻辑后，这条测试按预期失败
  （`the second failure imposed=true ban number 1, want ban number 2`）。
- **新增封禁探测脚本**（`ban_ignore_cidrs` 与 `ban_max_seconds` 此前没有任何检查驱动过）：
  用合法 `AuthRequest` 携带错误令牌触发真实的认证失败（发畸形字节不算），核对
  - 达到 `ban_after_failures` 后来源被封、审计写 `source_banned`；
  - 封禁期间的连接在握手前被拒并计入 `aethertunnel_banned_connections_refused_total`，
    审计写 `ban_refused`；
  - 连续五轮的时长按 2/4/8/16/16 秒增长并在 `ban_max_seconds` 封顶；
  - `ban_ignore_cidrs` 里的来源失败次数照常计入 `aethertunnel_auth_failures_total`，
    但**永远不会被封**，审计里没有 `source_banned`。
  撤掉 `obsolete` 的保留逻辑后，其中 8 项按预期失败（每轮都是 `ban number 1`、都是 2 秒）。
- **新增 `client` 包的单元测试（4 项）——这个包此前一个测试都没有**：心跳间隔由服务端定的
  时候采用它并报告配置值不生效；服务端没下发时用配置值（负值同没下发）；两边都没有时落到
  30 秒；两边一致时不输出任何东西。把回退逻辑改回硬编码 30 秒，其中一项按预期失败
  （`interval is 30s, want the configured 45s`）。
- **新增超时与上限探测脚本（18 项）**：`max_connections`、`handshake_timeout_seconds`、
  `heartbeat_seconds`、`dial_timeout_seconds`、`read_timeout_seconds` 这五个键此前没有任何
  测试或脚本设置过，脚本用最小协议客户端（自造帧：6 字节头 + JSON）逐一驱动它们：
  - 超过 `max_connections` 的会话被明确拒绝，已在线的两个不受影响；
  - 连上但不发第一帧的连接在 `handshake_timeout_seconds` 左右被断开，按时发出的被保留；
  - 停止心跳的客户端在**三倍心跳间隔**左右被断开，持续心跳的保持连接；
  - 客户端一直不回拨数据连接时，访客不会挂在那里，在 `dial_timeout_seconds` 左右被断开；
  - 空闲的隧道流在 `read_timeout_seconds` 左右被关闭。
  另外核实客户端会采用服务端下发的间隔：服务端要 2 秒、客户端配 60 秒时，客户端在服务端
  6 秒的期限之后仍能正常转发，并在日志里说明配置值不生效。
- **新增账本篡改探测脚本（21 项）**：签名哈希链的全部意义在于"改过就能查出来"，而此前
  没有任何检查验证这一点。脚本用真实二进制记一段用量，然后六种方式动它：
  - 原样校验通过，且 `[ledger] signing_key_file` 的权限是 0600；
  - 在第一条记录里翻转一个字节 → 拒绝，并指出是第几条；
  - 调换两条记录的顺序 → 拒绝；
  - 追加一条签名全零的伪造记录 → 拒绝；
  - 截掉末尾一条 → **链本身仍自洽**（文档就是这么说的），但链头变了，因此"比对已发布的
    链头"能发现它；
  - 换一把公钥 → 拒绝。
  再加上 `--ledger-proof` 的八项：前缀正是 0..n 条、只凭公钥可独立校验、链头等于整条链在
  第 n 条的哈希、不含更后面的条目、索引越界与缺失都报错、把某条的 `bytes_in` 改大后拒绝、
  把别条的签名挪过来后拒绝。撤掉 `writeLedgerProof` 的前缀裁剪（改成输出整条链）后，
  仓库内那条 Go 测试按预期失败（`the proof has 4 lines, want entries 0 through 2`）。
- **新增 `main_test.go`（3 项）**：证明能被独立校验、越界索引被拒、空账本被拒。
- **新增 Kubernetes 清单检查（3 项，`pkg/config/deploy_test.go`）**：这是"随版本交付的部署
  清单必须与程序接得上"的第一道自检——
  - 清单里出现的每个 `AETHERTUNNEL_*` 名字都必须是本包真正读取的（改名或拼错会被静默忽略）；
  - Deployment 必须设置 `AETHERTUNNEL_AUTH_TOKEN`，且不得使用 `envFrom`（它会把短键名原样
    变成变量名），凭据必须来自 Secret 而不是写在清单里；Secret 的键名必须与 Deployment 读的
    一致；
  - ConfigMap 里的 `server.toml` 必须能被本版本接受（把它抽出来交给 `LoadServer` 校验）。
  结构检查会先剔除注释行，所以清单**不能靠注释**满足检查，也不会因为**解释**这个陷阱而误报。
  把 Deployment 改回 `envFrom` 后，第二条按预期失败（`deployment.yaml does not set
  AETHERTUNNEL_AUTH_TOKEN, so the server would refuse to start`）。
- **新增部署契约探测（9 项）**：把清单里的 `server.toml` 抽出来、按 Deployment 的映射方式
  给出环境变量（值取自"Secret"），核对服务端能启动、客户端带 Secret 里的令牌能连上、
  Secret 里的面板令牌才是生效的那个；再验两个反面：伪装不一致的客户端连不上且日志里有
  提示，短键名当变量名时服务端拒绝启动而不是空凭据监听。
- **Windows 侧的 `.uitest/panel-columns-check.js`（24 项）需要重跑**：它用的两个客户端组成
  代理池，而池的「成员」一格现在多出逐成员的行，凡是把这格文字当成一个整体来比对的断言都要
  相应放宽或改成按成员比对。上面那 30 项是在 Linux 上另跑的一套，不能替代它。

### 修复

- **`[dht]` 的派生重发间隔在短的 `announce_ttl_seconds` 下失效，让一个仍在运行的服务端的名字
  停止解析**。`republish_seconds` 的默认值是 `announce_ttl_seconds / 3`，而整数除法的结果在
  `announce_ttl_seconds` 小于 3 时是 **0**；0 在 discovery 这一层读作"调用者没有指定"，于是换成
  它自己的 30 秒默认值。结果是间隔比 TTL 还长，通告在重写之前就失效了——正是配置文档里警告过的
  那种情况（"否则通告会在被重写之前失效"），而当时的校验放过了它：`republish_seconds >=
  announce_ttl_seconds` 这一条只检查配置文件里写出来的值，不看派生值。
  - 现在派生值有 1 秒下限（`config.DefaultRepublishSeconds`，导出以便测试与文档引用），
    discovery 这一层也改为按 `AnnounceTTL` 派生（库调用者与配置文件走同一条规则，不再用固定
    常量），校验另外拒绝 `announce_ttl_seconds` 小于 2 秒的配置：间隔以整秒计，1 秒的 TTL
    没有办法在失效之前被重写。
  - 单测四处：`pkg/config` 断言 2 秒 TTL 的派生值是 1 秒、且**节点实际拿到的间隔**短于 TTL，
    另一个测试断言 `ttl_seconds`、`announce_ttl_seconds`、`republish_seconds`、
    `lookup_timeout_seconds` 四个时长都真的传到了节点；`pkg/discovery` 断言任意 TTL
    （2 秒到 1 小时）派生出的间隔都落在 TTL 之内且为正，并保留默认那一对的断言。
  - 反向验证：把派生改回 `announce_ttl_seconds / 3`、把 discovery 的派生改回固定 30 秒常量后，
    `DHT.RepublishSeconds = 0, want 1`、`an announce TTL of 2s gets a republish interval of
    30s, which does not fit inside it` 按预期失败。
  - 这一项顺带暴露出一个**断言了错误行为的集成测试**：`pkg/server` 里原来那个"记录会在配置的
    TTL 之后不再解析"的测试（`ttl_seconds = announce_ttl_seconds = 2`）长期通过，靠的正是上面
    这个坏掉的派生值——重发间隔 30 秒比 2 秒的 TTL 还长，所以没有任何东西重写它。修好之后，
    一个仍在运行的服务端每秒重写一次，记录不再失效。该测试改为断言修正后的行为（**短 TTL 下
    名字持续可解析**，这只有在 TTL 之内重写才做得到），并把"发布者消失后记录失效"那一半留给
    它真正可观测的地方：`pkg/dht`（存储层过期）与 `pkg/discovery`（过期通告被拒）。
- **修掉两处被编码破坏的注释**（`client/main.go`、`pkg/config/config_test.go`）：文件曾被以 GBK
  读过再写回，注释里的破折号变成了 `鈥?` 并在原处多出一个字符。两处都改回 `—`。全仓库重新扫过
  这一类破坏（UTF-8 标点被按 GBK 解码后的典型字符），没有第三处。

### 变更

- **`TypeProxyList` 只保留客户端能据以行动的四项**（`name`、`type`、`remote_port`、
  `group_members`）。这条消息是服务端发给**客户端**的，此前还带着 `local_addr`、`domains`、
  `client_id`、`active_connections`、`total_connections`、`bytes_in`、`bytes_out`：`client_id`
  就是这条连接自己的会话号（这条列表里只含本会话发布的代理），其余几个计数在服务端的面板与
  指标里已经有人读，客户端收到也只能复述一遍。释义也一并写清：它是**发给客户端的列表**，
  不是面板的代理表（面板读的是服务端自己的结构）。
  - 字段减少不影响兼容性：JSON 解码忽略多出来的键，缺失的键读作零值，`ProtocolVersion` 不变。
- **`AuthRequest.EncryptionSalt` 删除**。客户端从来没有发过它，服务端从来没有读过它，两端都按
  各自 `[encryption]` 的 `salt` 派生密钥；而承载它的那个帧本身就用这个 salt 派生出的密钥加密，
  真要读取就得先解开一个还没有密钥可用的帧。这是本轮清掉的"定义了没人用"的字段之一，同样不影响
  线协议版本。
- **客户端把 visitor 流与公网端口流分开写进日志**（`DataRequest.Visitor` 此前没有任何读取方）。
  两种流在客户端看到的其余字段完全一样（同一个代理名、同一种流标识），所以这个标记是客户端唯一
  能据以区分的依据：现在结束行与失败行写成 `stream for "x" (from a visitor) finished (...)`
  与 `(from the public port)`，日志阅读者不必再去猜一条流是从哪里来的。判断抽成 `streamOrigin`
  并配单测。
  - 端到端检查两项（逐平台功能套件，读**真实客户端进程**的日志）：两条路径各自的行都必须出现过
    ——只断言一侧的话，"标记永远为真"或"永远为假"的实现照样会通过。把服务端标记访客流的那一处
    改成 `Visitor: false` 后，其中一项按预期失败（23/24），另一项仍过。
  - 服务端一侧同时补了 Go 测试：`pkg/server` 的 STCP 中继测试断言访问者流的请求带 `Visitor`，
    http 代理测试断言走公网端口的流**不带**它——测试代理现在会把每条 `DataRequest` 记下来。
- **客户端确认行带上代理类型与共享成员数**，现在是
  `server confirms 2 tunnel(s): pooled (tcp) on port 6022 shared by 2 clients`。`group_members`
  是"第二个客户端并入了已发布的池、而不是又开了一个端点"的唯一可见证据（端口那一项是 v3.7.4
  修的），`type` 让它不必回头翻自己的配置。只有一个成员时仍然不写共享字样，与之前一致。
  `pkg/server` 的池测试新增两项断言：成员被告知共享这个名字的客户端数是 2、类型是 `tcp`；
  `client` 包的单测覆盖"带类型与端口""共享时写出成员数""独占时不写"三种写法。
  - 反向验证：把 `Type`、`GroupMembers` 从 `sendProxyList` 里去掉后，池测试按预期失败
    （`the member was told the name is served by 0 client(s), want 2`）；把确认行改回"只有名字
    与端口"后，`client` 包的两项按预期失败。

### 文档

- **README 的逐平台功能验证一行从 64/64 更正为 69/69**（第 4 轮把每平台的检查从 64 加到 67 时，
  `docs/PLATFORMS.md` 改了、README 的中英两行都漏了），并写明本轮新增的两项检查**只在
  linux/amd64 与 linux/arm64 上跑过**：本机这次无法创建用户命名空间，Wine 起不来（它需要私有
  挂载命名空间把解出的区域数据绑到 `/usr/share/wine`），Windows/Wine 这一路因此是本轮唯一
  没有重跑的可执行平台，`docs/PLATFORMS.md` 里记下这一点。
- `docs/PLATFORMS.md`：结论表与分组说明同步到 69 项（隧道 22 → 24），并把 Windows 一行标注为
  "本轮未重跑"。
- `[dht]` 的两个键在 `docs/CONFIGURATION.md` 的说明里补上"最少 2 秒"与"至少 1 秒"。

### 修复

- **优雅关闭时审计日志丢掉"会话结束"的两条记录**。`client_disconnected` 与 `proxy_removed` 是
  **连接处理器**在拆除会话时写的，而 `Shutdown` 在 `sessions.CloseAll` 之后立刻关闭了审计日志：
  `Record` 对已关闭的日志直接返回，于是这两条记录既没进文件，也没有留下任何痕迹——审计日志
  停下来是"没有别的痕迹"的那种失败，而这个包存在的理由正是不要这样。
  - 实测（真实进程：一个客户端在线、一条流在传，对服务端发 SIGTERM）：修之前 `audit.jsonl`
    只有 `control_accepted` 与 `proxy_registered` 两行，日志里也没有下线记录；修之后多出
    `proxy_removed`（`detail = server shutting down`）与 `client_disconnected`
    （`detail = connected for 2s`）。
  - 审计日志改为在 `closeStores` 里关闭：那正是"最后一个处理器已经返回"的地方（账本与 DHT
    节点本来就在那里收尾），而 `Shutdown` 的注释现在写明它不等处理器。
  - **关闭之后到达的记录不再静默丢弃**：计入 `records_lost` 并在服务器日志里写出是哪一条事件
    （`audit: the client_disconnected record arrived after the log was closed`）。仍有写记录
    的东西活过关闭时，这条线索就是唯一的提示；面板与 `/api/status` 的 `audit` 段也照常报告。
  - 单测两项：`TestAGracefulShutdownRecordsTheSessionEnd`（关闭一个在线会话，日志里必须同时
    出现这四条事件、且各只有一条）、`TestARecordThatArrivesAfterTheLogIsClosedIsReported`
    （关闭后再写一条：计入丢失、写进日志、文件不被重建）。把 `auditor.Close()` 挪回 `Shutdown`
    后，第一项按预期失败（`has no client_disconnected record; it holds map[control_accepted:1
    proxy_registered:1 proxy_removed:1]`）；把晚到记录改回静默返回后，第二项按预期失败。
- **成员被移除时只有一条路径写审计记录**。服务端自己关闭会话时——**优雅关闭走的就是这条**——
  `Session.Close` 直接把成员从池里摘掉，而写 `proxy_removed` 的代码在 `Unregister` 里，那是
  控制处理器拆除会话时走的另一条路径；此时池里已经没有这个成员，于是记录被漏掉。文档写着
  "代理池里少一个成员也记录"，`Unregister` 的注释写着"审计线索跟着每个成员走"，而事实上
  服务端主动关掉的会话一条都不留。
  - 现在记录写在**真正移除成员的那一处**（`ProxyGroup.remove`），并且带上移除原因：客户端
    自己断开时是 `client disconnected`，服务端关闭会话时是关闭原因（例如 `server shutting
    down`）。两条路径都覆盖，且只写一次。
  - 单测一项：`TestTheAuditTrailRecordsAMemberThatLeavesOnItsOwn`（客户端自己断开：日志里
    恰好一条 `proxy_removed`，`detail` 是那条路径的原因）。把记录挪回 `Unregister` 后，
    `TestAGracefulShutdownRecordsTheSessionEnd` 按预期失败（`has no proxy_removed record`），
    而这一项仍然通过——两个方向各由一项盯住。

### 文档

- `docs/CONFIGURATION.md` 的 `[audit]` 段补两句：`proxy_removed` 写在移除发生的那一处、服务端
  关闭会话时同样会写；关闭之后到达的记录计入 `records_lost` 并写进服务器日志。

### 修复

- **负数的时长与计数不再是"有效配置"**。`--check` 此前对 `reconnect_seconds = -1`、
  `dial_timeout_seconds = -1`、`heartbeat_seconds = -1`、`max_connections = -5` 这类值回答
  `is valid`，而它们并不是"更小的设置"——程序对它们的处理是另一个意思：
  - **客户端 `dial_timeout_seconds` 为负等于永远连不上**：`net.DialTimeout` 拿到负的时限立即
    超时。实测（真实进程，服务端一切正常）客户端每一轮都报
    `session ended: dial 127.0.0.1:17451: dial tcp ...: i/o timeout`，一次也没有连上。
  - **`reconnect_seconds` 为负等于每秒重试一次**：抖动函数对非正值回退到 1 秒，而退避再也不
    增长（负数翻倍仍是负数，永远到不了上限）。实测日志每行都是 `reconnecting in 1s`，一直不停；
    `max_reconnect_seconds` 为负同理。
  - **服务端一侧是"这条限制不再执行"**：心跳窗口（连续三次未收到即断开）、流的空闲上限、
    握手时限、连接数上限都在"值大于 0 才设置"的判断里，负值等于把这条限制悄悄关掉；
    `rate_limit_burst` 更直接，被 `<= 0` 兜成 1。启动横幅还会照原样打印 `heartbeat: -1s`，
    看上去像是设置成功了。
  - 现在这些键取负值直接拒绝，`max_reconnect_seconds` 小于 `reconnect_seconds` 也拒绝——第一个
    等待就会超过它自己的上限。**0 的语义没有变**：仍然是"取文档里的默认值"。
  - 单测两项：`TestNegativeDurationsAndCountsAreRejected`（12 个键 + 上限与起始值的关系，各一
    例）与 `TestZeroDurationsAndCountsStillSelectTheDefaults`（同名的 0 仍然得到默认值，这条
    防止把规则写成"必须为正"）。把新增的校验删掉后，前面那项的每个用例都按预期失败。

### 文档

- `docs/CONFIGURATION.md`：`[server]`、`[client]` 与 `[dht]` 的时长/计数各行补上"负数被拒绝"，
  `max_reconnect_seconds` 注明不得小于 `reconnect_seconds`；`rate_limit_burst` 注明负值此前会
  被静默当作 1（`dht.lookup_timeout_seconds` 的负值此前会被 discovery 静默换成 5 秒默认值）。

### 修复

- **`obfuscation.pad_to` 超过帧上限时，隧道一条帧也发不出去**。补齐发生在写出之前，而发送端自己
  会拒收超过帧上限（1 MiB）的帧，于是 `pad_to = 1500000` 这样的配置把**每一条**帧都挡在门外：
  实测客户端连认证请求都发不出，日志每一轮都是
  `send auth request: protocol: refusing to send 1500000 byte frame (limit 1048576)`，服务端那一侧
  只看到 `handshake failed: EOF`，而 `--check` 当时回答 `is valid`。
  - 现在 `pad_to` 超过 `protocol.DefaultMaxPayload` 直接拒绝，报出上限与实际值。上限本身仍可用：
    比它小的负载会补齐到正好等于上限，而帧检查只拒收"更大"的帧，这一点由测试里的第二个用例
    钉住（`pad_to` 取到上限仍然加载成功）。
  - 单测一项：`TestAPaddingTargetAboveTheFrameLimitIsRejected`。把这条校验改成永假后，它按预期
    失败（`expected a pad_to limit error, got <nil>`）。
  - 顺带说明：`pkg/config` 因此开始依赖 `pkg/protocol`（只为这个常量），依赖方向没有环——
    `pkg/protocol` 只依赖 `pkg/crypto`。

### 文档

- `docs/CONFIGURATION.md` 的 `pad_to` 一行补上上限与原因（两端不必一致，但都受同一个帧上限约束）。

### 修复

- **同一个进程里两个监听器撞在同一个地址上，`--check` 说配置有效，而失败信息指向别处**。实测四种
  组合（改一个数字就能撞上，全是常见手误）：
  - `http_port == bind_port`：服务端先绑好控制端口、打印
    `listening on 127.0.0.1:17500`，紧接着
    `server stopped: listen on 127.0.0.1:17500: bind: address already in use` 退出——信息里说的是
    控制地址被占用，而真正冲突的是它自己的另一个监听器；
  - `dashboard.port == bind_port`：面板先把端口抢走，于是同一条信息变成指控控制端口；
  - `https_port == http_port`：两个共享监听器只能有一个起来；
  - 客户端两个 visitor 用同一个 `bind_port`：`--check` 有效，运行时第一个监听成功、第二个只留一行
    `visitor "two": cannot listen on ...`，客户端继续跑着，其中一个 visitor 永远不工作。
  - 现在这些组合在 `--check` 阶段就被拒绝，信息把两个键都点出来
    （`server.bind_port and server.http_port would both listen on tcp port 7001 (...)`），服务端
    与客户端都不会带着一个注定失败的上线。判定规则是"同一个地址"而不是"同一个数字"：地址相同、
    或者任一方是通配地址（`0.0.0.0`/`::`，即 `ip.IsUnspecified`）才算冲突，所以控制端口在
    `127.0.0.1`、面板在 `192.0.2.1` 用同一个端口号仍然合法（有单测钉住这一点）。DHT 的 UDP
    `listen_addr` 与 `server.p2p_port` 也一起检查。
  - 单测两项：`TestTwoListenersOnOneAddressAreRejected`（7 个组合：控制端口与 http/https/面板、
    http 与 https、通配对具体、打洞 UDP 对 DHT UDP、两个 visitor）与
    `TestTheSamePortOnDifferentAddressesIsAccepted`（同端口不同地址仍然合法）。把这条校验改成永假
    后，前者的 7 个子用例全部按预期失败。

### 文档

- `docs/CONFIGURATION.md`：`server.bind_port`、`server.http_port`、`server.p2p_port`、`dht.listen_addr`
  与 visitor 的 `bind_port` 各行补上冲突判定（注明 `http_port` 绑的是 `server.bind_addr`，因此与
  `bind_port` 同端口必然是冲突）。

### 修复

- **同一客户端里两个代理请求同一个 `remote_port` 时，`--check` 说配置有效，第二个代理永远发布
  不了**。实测（真实进程，一个客户端两个 `tcp` 代理都要 17602）：服务端为第一个绑住端口，第二个
  以 `cannot publish second on 127.0.0.1:17602: listen tcp ...: bind: address already in use`
  被拒——消息里有端口号，却没有说端口在**它自己的另一个代理**手上；客户端那侧只看到一行
  `server reported: ...`，审计里是一条 `proxy_rejected`。两份配置都在同一个文件里，这个冲突在
  启动前就能判定。
  - 现在同一客户端里同协议的两个代理请求同一个端口会被拒绝，信息把两个代理名都点出来
    （`proxies "first" and "second" both ask for tcp port 7002, and only the first to register is published`）。
    判定按协议分开：`tcp` 与 `socks5` 绑 TCP，`udp` 绑 UDP，所以 **`tcp` 与 `udp` 用同一个端口号
    仍然合法**——这一点先起真实进程验证过（同一端口号上 tcp 与 udp 各自回显成功），再写成反例单测。
    `http`/`https` 与私有类型本来就要求 `remote_port = 0`，不参与这条判定。
  - 单测一项：`TestTwoProxiesCannotAskForOnePublicPort`（两个 tcp、tcp 与 socks5、两个 udp 各一例，
    外加"tcp 与 udp 同端口号可用"的反例）。

### 文档

- `docs/CONFIGURATION.md` 的 `remote_port` 一行补上这条规则（含"tcp 与 udp 可以共用一个端口号"）。

### 修复

- **同时设置 `client.server_addr` 与 `[dht] discover` 时，客户端会跟着 DHT 上的记录走——
  与它自己打印的警告、以及文档里的说法完全相反**。实测（真实进程，两台服务器：一台是 DHT 上
  发布 `moved` 这个名字的发布者 `127.0.0.1:17701`，另一台是配置里写明的 `127.0.0.1:17703`）：

  ```
  warning: client.server_addr is set to 127.0.0.1:17703 and dht.discover to "moved":
           the configured address is used and the DHT is not consulted
  dht: "moved" resolves to 127.0.0.1:17701 (type stcp), unsigned
  connected to 127.0.0.1:17701 as session cb7694699c5f4c1e
  ```

  它先声明"用配置的地址、不查 DHT"，然后连到了 DHT 给的地址。这不只是措辞问题：DHT 上的记录
  除 `dht.trusted_keys` 之外没有签名，**任何能应答这个名字的节点都能把已经写死服务器的客户端
  改写到别处**，而客户端会把 `auth_token` 放进第一个帧交给那台服务器（默认不开加密时就是明文）。
  `docs/SECURITY.md` 第 10 节把"显式配置 `client.server_addr`"列为 DHT 投毒的缓解手段，
  `docs/CONFIGURATION.md` 的 `discover` 一行也写着"为空时才解析"——三处说法一致，只有代码不是。
  - 现在客户端在 `server_addr` 非空时**不启动解析**（`usesDiscoveredAddress()` 一个条件决定），
    与文档和警告一致。想要按名字跟随服务端迁移的部署照旧只需留空 `server_addr`。
  - 修好之后同一个脚本的日志：`connected to 127.0.0.1:17703`，并且不再出现 `dht: node ... resolving`。
  - 单测一项：`client` 包的 `TestAConfiguredAddressIsNotOverriddenByDhtDiscover`（四种组合：
    两者都有 / 只有 discover / 只有地址 / 只有 discover 但没有 `[dht] enabled`）。把那个条件改回
    `cfg.DHT.Enabled && cfg.DHT.Discover != ""` 后，第一、第三个用例按预期失败。

### 文档

- `docs/CONFIGURATION.md` 的 `discover` 一行、`docs/SECURITY.md` 第 10 节的缓解手段各补一句：
  两者同时设置时以 `server_addr` 为准，以及为什么（无签名的记录等于让能应答这个名字的人改地址）。

### 变更

- **访问者绑到非回环地址时会警告**。访问者（`[[visitors]]`）的本地监听器自身没有认证：能连上那个
  端口的人就能用这条隧道。服务端的 `[[proxies]] allow_cidrs` / `deny_cidrs` 也帮不上——它们判断的是
  访问者连接**服务端**时的来源地址，也就是运行访问者的这台客户端，而不是它后面的用户。
  - 实测（真实进程，本机地址 `192.168.10.16`）：访问者绑 `0.0.0.0` 时，从该地址连上去**能拿到回显**；
    改绑 `127.0.0.1` 后同样的连接 `Connection refused`。也就是说绑到非回环地址确实是把私有代理交给
    了那个网络，而此前 `--check` 与启动日志都不提这件事（只有配置里那一行默认值 `127.0.0.1` 暗示过）。
  - 现在这类配置会得到一条警告，写明原因；默认值与回环地址（含 `127.0.0.0/8` 的其它地址、`::1`、
    `localhost`）不警告。
  - 顺带把 `isLoopback` 从"等于三个字面量"改成按 IP 判断（`ip.IsLoopback()`），所以
    `127.0.0.2` 这类地址不再被误报；面板那条"非回环地址且未设 token 时警告"用的是同一个判定，
    行为只会更准。
  - 单测一项：`TestAVisitorOnANonLoopbackAddressWarns`（`0.0.0.0`、`192.0.2.1`、`::`、`[::]` 各警告
    一次；`127.0.0.1`、`127.0.0.2`、`::1`、`[::1]`、`localhost` 都不警告）。

### 文档

- `docs/CONFIGURATION.md` 的 visitor `bind_addr` 一行与 `docs/SECURITY.md` 第 7 节的能力表各补上这条
  警告及其原因（该监听器没有认证、服务端名单看不到它后面的用户）。

### 修复

- **会话账本把它离开时还在收尾的那条流算进去**。账本条目在控制连接拆除时写入，读的是隧道的
  字节计数；而流是在它的双向复制都返回之后才把这些字节记到隧道上的。客户端在一条流还在收尾时
  离开（比如应答正在回程上），那条流的字节此前会**整条漏掉**。这个窗口本来很窄，本轮修好半关闭
  之后变宽了——macOS 的 CI 立刻把它照出来：`bytes_in is 0, want 29`。现在拆除时会**有界等待**
  本会话的流结束（上限 2 秒）再写条目，所以"离开时正在回程"的那部分带宽会进入账本。
  - 单测一项：`TestTheLedgerEntryIncludesAStreamThatIsStillFinishing`（一个延迟 300 毫秒才回答
    的本地服务；确认负载已经到达服务之后再拆控制连接，账本条目必须记下 29 字节）。去掉那段等待
    后它按预期失败（`bytes_in is 0, want 29`）。
- **半关闭在每一层包装上都退化成"整条关闭"，于是"读完请求才回答"的协议被截断**。`flynet.Pipe`
  每个方向结束时用 `CloseWrite` 半关闭，连接不支持时才退回整条关闭；而隧道的字节流在每一层都是
  包装过的：服务端与客户端各有一层 `cryptoStreamConn`（加密记录层）、开着伪装时还有一层
  `obfs.recordConn`、xtcp 直连走 `pkg/reliable` 的可靠流。**这些包装都没有实现 `CloseWrite`**，
  于是每一次半关闭都变成整条关闭——README 与 `docs/ARCHITECTURE.md` 里"TCP 保留半关闭"那句
  话，在三种包装上都不成立。
  - 实测（真实进程：一个"读到 EOF 才回答字节数"的本地服务，客户端发布它，访问者发 4 KiB 后
    `shutdown(SHUT_WR)`）：
    - 修之前：公网 tcp 端口与 stcp 访问者**都只收到空回复**（回复确实产生了——客户端日志写着
      `sent 15 bytes to the server, received 4096`——但服务端那侧的数据连接已经被关掉）；
    - 修之后：两者都收到 `read 4096 bytes`，`[transport] enable_tls` 与
      `disguise = "tls-record"` 两种配置下同样正确。
  - 修复是四处 `CloseWrite`：`cryptoStreamConn`（`pkg/server/tunnel.go` 与 `client/main.go`
    各一份）、`obfs.recordConn`（转交给下面那层）、以及 `pkg/reliable` 流的 `CloseWrite`
    （只发 FIN、不释放传输层，所以对端的回复还能回来）。
  - 单测四项，各自盯住一层，且都在把该层的 `CloseWrite` 改回"整条关闭"后按预期失败：
    `pkg/server` 的 `TestAHalfClosedStreamStillCarriesTheReply`（端到端：公网端口半关闭后
    仍要拿到回复）、`client` 的 `TestTheRelayedStreamHalfClosesInsteadOfClosing`、
    `pkg/obfs` 的 `TestRecordConnHalfClosesTheConnectionUnderneath`、`pkg/reliable` 的
    `TestCloseWriteHalfClosesTheStream`。
  - 这一处此前**没有任何检查**：仓库里的脚本、逐平台套件、Go 测试都没有发过一次半关闭。

### 文档

- `docs/ARCHITECTURE.md` 第 4 节把"每一层包装都必须实现 `CloseWrite`"写成规则，并列出三个
  包装与各自的位置，避免下次再漏一层。

### 修复

- **访客的传输形状与代理不一致时，隧道会"什么都不发生"地失败**。服务端按**访客请求的传输**决定怎么
  转发（`sudp` 访客走数据报转发），而客户端按**自己配置里那个代理的类型**决定怎么处理（`stcp` 代理
  走字节流直通）：两端的"帧"看法不同，于是服务端写出的 `TypeUDPPacket` 帧头被客户端原样灌进本地
  服务，本地服务的应答又被服务端当成帧去解析——解析不了，访客什么也收不到，审计里却是一条
  `visitor_accepted`。
  - 实测（真实进程）：`stcp` 代理 + `sudp` 访客，本地服务收到请求后回自己的 `pong`。服务端日志
    是 `datagram session for visitor ... finished (5 bytes out, 0 bytes in)`，客户端日志是
    `stream for "stream-proxy" (from a visitor) finished (sent 4 bytes to the server, received 11)`
    ——那 11 字节正是帧头加负载，被塞给了本地服务；访客那边 6 秒内**没有任何数据报回来**。
    本地服务若是回显，就会把帧字节原样送回，于是这个错配**看起来是通的**（本轮一开始就是这么被骗
    过去的，最初那次探测用了回显服务）。
  - 现在服务端在授权之后检查形状：`stcp` 与 `xtcp` 是字节流访客、`sudp` 是数据报访客，形状必须
    一致（`xtcp` 也是字节流——见下一轮的修正）。不一致时以
    `proxy "x" carries a byte stream, and a "sudp" visitor carries datagrams: use a stcp visitor` 拒绝，并留下 `visitor_rejected` 审计记录与一行服务器日志。
  - 单测一项：`TestAVisitorTransportMustMatchTheProxyShape`（拒绝与接受各若干例，见第 15 轮的
    修正）。把这条检查改成永假后，拒绝用例按预期失败（`ack.OK is true`）。

### 文档

- `docs/CONFIGURATION.md` 的访客 `type` 一行、`docs/ARCHITECTURE.md` 第 5 节前各说明这条形状规则
  与原因（两端对帧的看法不同）。

### 修复

- **上一轮的形状规则把 `xtcp` 当成了"两者皆可"，实际上它是字节流访客**——于是
  `sudp` 代理 + `xtcp` 访客这一对被放行，而它和上一轮修掉的错配一样是坏的：服务端按访客的
  `type` 走 `serveXTCPVisitor`（打洞或退回 `relayStreamVisitor`，都是**字节流**），而拥有者客户端
  按自己代理的类型走 `serveDatagrams`（数据报），两端的帧看法再次不同。
  - 实测（真实进程：`sudp` 代理 + `xtcp` 访客）：访客的本地监听器是 **TCP**（xtcp 本来就是字节流），
    TCP 上连过去之后服务端此前放行、随后整条会话静默失败；现在服务端在握手阶段就以
    `proxy "datagrams" carries datagrams, and a "xtcp" visitor carries a byte stream: use a sudp
    visitor` 拒绝，访客日志里也有同一句话。带 `p2p_port` 与不带 `p2p_port` 两种情形都已确认。
  - 规则现在是**两个形状的严格比较**：`stcp` 与 `xtcp` 是字节流访客，`sudp` 是数据报访客；代理侧
    `stcp`/`xtcp` 是字节流、`sudp`/`udp` 是数据报（`config.IsDatagramProxyType` 是同一个判定）。
    上一轮那句 `(xtcp works for either shape)` 已从错误信息、`docs/CONFIGURATION.md`、
    `docs/ARCHITECTURE.md` 与 `docs/MIGRATION.md` 里删掉。
  - 单测：`TestAVisitorTransportMustMatchTheProxyShape` 增加"`xtcp` 访客对 `sudp` 代理"一例；
    把检查放宽回上一轮的写法后，这一例按预期失败（`ack.OK is true`，即放行了一个注定静默失败的组合）。
- **这一轮的教训写在交接文档里**：上一轮"确认"这条规则时，我用 UDP 往 `xtcp` 访客的端口发数据报
  ——而 `xtcp` 的监听器是 TCP，那次探测什么也没测到，我却把它当成了"这个组合也通"的证据。
  规则没被验证过就不该写进错误信息与文档；这一轮补的是同一件事的另一半。

### 文档

- `docs/CONFIGURATION.md` 的访客 `type` 一行、`docs/ARCHITECTURE.md` 第 5 节前的说明、
  `docs/MIGRATION.md` 的 4.8 条目都改成"`stcp` 与 `xtcp` 是字节流访客（`xtcp` 的本地监听器是 TCP，
  打洞之后也走字节流）、`sudp` 是数据报访客"。

---

## [3.7.3] — 2026-09-21

本版本让服务端配置里的 `[[proxies]]` 真正生效：它从"会被校验、会被计数、但对注册不做任何
比对"变成**按代理名的策略**。另外补上两组运维检查：面板与指标报的是不是同一批数字，以及
客户端在服务端重启后能不能自己恢复。配置与线协议没有破坏性变化，v3.7.2 的配置可以直接用。

### 变更

- **服务端 `[[proxies]]` 现在是按代理名的策略**。任何客户端注册这个名字都按它执行，没有
  条目的名字不受约束（与升级前一致）：
  - `type` / `remote_port`：注册时比对，不一致就拒绝，客户端收到服务端期望的类型或端口，
    审计写 `proxy_rejected`。
  - `allow_cidrs` / `deny_cidrs`：访客来源先过服务端名单，再过客户端为该代理声明的名单，
    两边都通过才建立隧道。被服务端拒绝的访客写 `proxy_visitor_denied`（`detail` 指出是
    服务端策略拒绝的），并计入 `aethertunnel_visitors_denied_by_proxy_total`。客户端无法
    放宽服务端的限制。
  - `type` 留空表示任意类型：服务端条目不再被默认成 `tcp`。
  - 描述客户端自身服务的键（`local_ip`、`local_port`、`group`、`multipath`、`secret_key`、
    `auth_method`、`allow_targets`、`domains`）加载成功但不生效，`--check` 逐条给出警告，
    因此同一份 `[[proxies]]` 列表可以放在两种角色的配置里。服务端条目不再要求 `local_port`
    （此前缺失会以 `local_port must be 1-65535` 报错）。
- **`GET /api/status` 新增 `connections.authenticated`**：完成握手的连接数，与
  `aethertunnel_control_connections_total` 同源。`connections.total` 计的是监听器接受过的
  每个 TCP socket，包括握手前就被拒的那些；两者的差别此前没有任何地方说明。
- **面板**：概览页新增「通过握手的连接」一格；配置页那一行由「发布的代理」改为
  **「代理策略」**（中英双语），显示服务端配置里 `[[proxies]]` 的条数，与注册路径读的是同
  一份列表。`/api/config` 的字段名不变。

### 运维测试

- **修掉运维脚本里又一处"假定次序"的检查**：`the client reconnects and publishes its proxy again
  by itself` 在轮询到 `/api/proxies` 已经列出该代理之后，**只读一次**审计文件就断言里面有
  `proxy_registered`。而服务端是先让注册成功（面板立刻能看到），随后才写这条审计记录——在忙碌的
  CI 运行器上两者之间的窗口足以让断言落空，于是出现"代理已重新发布、下一条'流恢复'也通过，却报
  审计里没有 `proxy_registered`"这种自相矛盾的失败（2026-09-23 的 `9acb8d8e` 那次 Windows 作业
  即如此，同一提交几分钟前全绿）。现在这两条记录也带 15 秒期限轮询等待，断言强度未变。
  说明：这一次只能靠日志推理（本机无法执行 PowerShell），下一次 CI 运行会给出确认。


- `scripts/smoke-test.ps1`（89 → 101 项）新增：
  - **两个端点报同一批数字**：`/api/status` 与 `/metrics` 在流量字节、
    `connections.authenticated` 对 `aethertunnel_control_connections_total`、活动流数、
    审计的丢失/写失败/恢复上必须相等；另外校验 `total >= authenticated >= active`、
    `/api/clients` 的条数等于 `connections.active`、`/api/proxies` 的条数等于
    `proxies.registered`，以及两个端点读出的 uptime 相差不超过 1 秒。
  - **握手前被拒的连接**：三个连上就关的 socket 必须计入 `connections.total`，但不能计入
    `connections.authenticated`——这正是两个计数器存在的理由。
  - **客户端在服务端重启后自己恢复**：起一对独立的服务端与客户端，先跑通一条流，给服务端
    发停止信号，客户端察觉后服务端在原端口重启，客户端自己重连并重新发布代理，流再次可用，
    审计里有 `control_accepted` 与 `proxy_registered`。
  - **服务端按代理名的策略**（五项）：不符的注册被拒且审计里的原因写明服务端期望的端口；
    策略描述的那个注册被发布并能跑通流；被拒注册要的端口没有监听；服务端名单拒绝客户端已
    放行的来源；拒绝被记进审计并计数。这一项为此起了一台专属服务端与两个客户端。
- 面板改动在真实浏览器里核对：24 项检查覆盖中英两种语言、1280×900 与 390×844 两种视口。
  四个概览数字与同一时刻 `/api/status` 返回的一致，且 accepted 与 authenticated 的差值确实
  出现（驱动先开两个只连不说话的 socket，再读面板）；配置页的「代理策略」与 `/api/config`
  一致。把面板改成在那一格显示 `total` 后这组检查失败（4 vs 2），说明它不是照抄常量。
- 移除服务端策略的两处判断后重跑运维脚本，三项检查按预期失败（不符的注册被接受、被策略
  排除的访客拿到了数据、审计里没有这条拒绝），据此确认这几项检查真的在判断行为。

### 修复

- 修掉新检查自身的一处错误：`/metrics` 的行尾是 CRLF，正则的 `$` 锚点因此匹配不上，
  第一次运行报"没有这个序列"。改为 `\r?$`。

### 文档

`docs/CONFIGURATION.md` 新增服务端 `[[proxies]]` 的键表与判定顺序，`docs/SECURITY.md`
把服务端策略单列一行，`docs/ARCHITECTURE.md` 写明两层名单的执行位置与拒绝记录，
`docs/MIGRATION.md` 给出升级检查清单，`server.toml.example` 补上带注释的策略示例。

---

## [3.7.2] — 2026-09-21

本版本补上四条"代码会写、但没有任何检查读过"的审计记录，把三处已经由 API 返回、面板却
没显示的数据补进面板，并把这轮新增的检查放进流水线。配置与线协议没有破坏性变化，
v3.7.1 的配置可以直接用。

### 新增

- **面板的「负载均衡」列**（代理表）：显示这个代理当前使用的池策略（`round-robin`、
  `random`、`latency`、`failover` 或 `adaptive`）。`/api/proxies` 一直在返回这个字段，
  面板此前没有显示它，运维看不出池是按什么在选成员。
- **面板的「客户端版本」列**（客户端表）：显示客户端连接时报告的版本，排查"哪台机器上
  的旧版本客户端"时不用再手写 `/api/clients`。
- **配置页的「发布的代理」一行**：服务端自己配置里的 `[[proxies]]` 条数。它与概览页的
  「已注册」不是一回事——后者属于已连接的客户端。

### 运维测试

- **四条审计记录此前只被写过、从没被读过**：`auth_failed`、`proxy_rejected`、
  `dashboard_action` 与 `vpn_address_rejected`。它们都是运维事后回看时要找的东西（谁在
  爆破、哪次注册被拒、谁从面板断开了谁、哪个会话没拿到隧道地址），而现在各有检查读回来：
  - `scripts/smoke-test.ps1`（86 → 89 项）新增三项：连续认证失败的来源在审计日志里留下
    `auth_failed`（含 `outcome` 与来源）；一个被拒绝的注册留下 `proxy_rejected` 且**没有**
    顶掉已经发布的那个代理；从面板 `DELETE /api/clients/{id}` 断开一个客户端留下
    `dashboard_action`，记录里是那个客户端 ID 且服务器日志也写了这件事。这一项为此起了一对
    专属服务端与两个客户端（一个发布代理，另一个用同名不同类型的代理去撞）。
  - `scripts/vpn-linux-test.sh`（17 → 20 项）新增三项：服务端开 `vpn.require = true` 时，
    一个没有 `[vpn]` 段的客户端被拒绝，审计日志留下 `vpn_address_rejected`、服务器日志与
    记录正文都写明原因。这一项只能在真实 tun 设备上跑（CI 的 Linux 作业与 WSL 里都跑过）。
- 面板改动在真实浏览器里核对过：14 项检查覆盖中英两种语言、1280×900 与 390×844 两种视口，
  三个新值都与同一时刻 `/api/proxies`、`/api/clients`、`/api/config` 返回的一致，
  移动端不横向滚动，控制台没有错误。

### 修复

- 无。这一版没有改动服务端行为。

### 文档

`web/dashboard/README.md` 说明三个新列的来源与含义（尤其「发布的代理」与「已注册」的区别），
`README.md`、`docs/ARCHITECTURE.md` 与 `docs/MIGRATION.md` 同步检查项数与这一版的说明。

---

## [3.7.1] — 2026-09-21

本版本补上审计日志的保留策略，并把端到端运维脚本接进流水线：CI 的 Windows 作业每次推送
都跑它，发布一个版本之前它也必须先通过。配置与线协议没有破坏性变化，v3.7.0 的配置可以
直接用。

### 新增

- `[audit] keep`：轮转时保留多少代，范围 1–100，默认 1（即 v3.7.0 的行为，只留 `<path>.1`）。
  轮转时各代按序号往后挪一位（`.1` → `.2`，…），最老的一代被覆盖丢弃，因此保留的文件数量
  由 `keep` 决定，不随运行时间增长。写 0 与不写一样按默认 1 处理；超出范围 `--check` 报出
  `audit.keep must be 0-100`。
- 发布流程新增一个 Windows 作业，在打标签的源码上跑 `scripts/smoke-test.ps1`，
  `create the release` 以它为前提：脚本不通过就不会创建 Release。CI 的 Windows 作业每次
  推送也跑同一个脚本，所以这套真实进程检查不再只在开发者的机器上跑过。
- 发布流程在 Release 建好之后还有两个作业检查**发布页本身**：一个在 Windows 上逐个文件核对
  `SHA256SUMS`、确认两个 Windows 二进制报出的版本就是标签版本、用随发布一起上传的示例配置
  各做一次 `--check`，再用这两个**已发布**的二进制跑完整套运维脚本；一个在 Linux 上把
  `aethertunnel-server-linux-amd64` 与客户端下载下来，核对校验和与版本，然后跑
  `scripts/verify-release-linux.sh`——服务端起得来、`/healthz` 200、审计日志在运行中被
  `mv` 走之后按配置路径重建、`audit.keep` 留下该留的代数、`SIGTERM` 后退出并写出日志。
  这些是构建作业证明不了的：它们测的是刚构建的产物，不是上传之后的那份；而审计日志被改名
  重建这一条在 Windows 上根本做不出来。`scripts/smoke-test.ps1` 为此新增 `-ServerExe`、
  `-ClientExe`、`-HelperExe`：给了哪个就用哪个，不再重新构建。

### 修复

- **轮转不再先删掉最老的一代。** 改名本身就会覆盖目标文件，先删一次会让被保留的那一代在
  两次调用之间短暂消失——运维脚本读它时正好撞上这个窗口，报出"找不到 `<path>.2`"。
  现在只做移位与改名：`.1` → `.2`，…，`<path>` → `<path>.1`。
- **改名改不动时不再把日志切短，而是推迟这次轮转。** 旧的退路是"改名失败就把当前文件清空
  从零继续写"（Windows 上另一个进程打开着日志时就会走到这里，例如杀毒扫描器、日志转发器
  或正在读它的运维人员），代价是丢掉整个上一代的记录，而且是静默丢掉。现在这次轮转推迟到
  写入下一条记录时再试：文件在那之前继续变大，服务器日志写出一行
  `audit: cannot rotate ...; it keeps growing until it can be rotated`。
  实测（Windows 上持有一个外部句柄再写 18 条记录）：旧行为只剩 5 条可读，新行为 18 条都在。

### 测试

- `pkg/config/config_test.go` 新增 `TestAuditRetentionDefaultsAndValidation`：默认值、显式
  `keep = 5`、显式 `keep = 0` 按默认处理，以及 `101` 与 `-1` 被拒绝并报出键名与范围。
- `pkg/server/audit_test.go` 新增两项：`keep = 2` 时连续轮转三批记录之后，`<path>.1` 是较新的
  那一代、`<path>.2` 是更早的那一代、`<path>.3` 不存在，且每一代都是能逐行解析的完整记录；
  不写 `keep` 的配置只保留一代。把位移那一步去掉，第一项立刻失败并报出读不到 `<path>.2`。
- `pkg/server/audit_test.go` 另新增 `TestAuditLogNeverDropsRecordsWhenItCannotRotate`：
  从外面持有一个句柄把改名挡住，再写 18 条记录，确认这 18 条在被保留的文件里全都能读到
  （Windows 上改名会被挡住，Unix 上不会，所以断言写在"记录有没有丢"上而不是"有没有轮转"；
  这个用例把 `keep` 设成 10，比这些记录能触发的轮转次数大，好让"丢掉最老的一代"这条保留
  规则不参与进来——否则它会在 Unix 上把断言变成假的）。把推迟改回"改名失败就清空当前文件"，
  它立刻失败并报出"18 条里只有 5 条能读到"。

### 运维测试

`scripts/smoke-test.ps1` 从 83 项扩到 86 项：

- **宽限期内到达的访客与两条探针**：服务器正在收尾时，新到的访客连接被拒绝，且
  `aethertunnel_streams_refused_while_draining_total` 计数上涨——这条计数器此前只被检查过
  "存在"，从来没有被观察到上涨，而文档写的是"期间到达的访客被立即拒绝并计入指标"；
  同一时刻 `/readyz` 报 503、`/healthz` 仍是 200，文档里"正在关闭时返回 503"这一半此前
  没有任何检查读过（单元测试只覆盖了监听器就绪之前那半）。
- **`audit.keep`**：把 `keep = 2` 的服务器逼出两次轮转，确认两代都在、没有第三代、
  两个文件都是完整记录，且 `.2` 里装的是最早的那批记录。

### 文档

`docs/CONFIGURATION.md`（`[audit]` 的 `keep` 与轮转规则）、`docs/SECURITY.md`、
`server.toml.example`、`README.md`（审计与测试两行）、`docs/ARCHITECTURE.md`、
`docs/MIGRATION.md`（新增 v3.7.0 → v3.7.1）。

---

## [3.7.0] — 2026-09-21

本版本修掉审计日志的一个静默失败：写不进去时它会停止记录，而服务器看起来一切正常。
同时补上了一些"声明了但没有任何测试或脚本用过"的命令行参数、接口与指标的真实运维检查。
配置键与线协议都没有破坏性变化，两端的旧配置可以直接用。

### 修复

- **审计日志写不进去时会悄悄停止记录。** `Auditor.Record` 丢弃写入错误，代码注释说这个错误
  会"在下一次抓取文件大小时通过面板的错误横幅浮现出来"——而没有任何代码抓取过它。
  两种情况都会命中：外部日志轮转把文件改名并新建（logrotate 的默认模式）之后，服务器手上
  的句柄指向的是已被改名的旧文件，操作者查看的路径从此不再有新记录；句柄失效或路径被替换时，
  重开失败会把 `file` 与 `encoder` 置空，此后每一条记录都被丢掉，重启之前再也写不出来。
  实测：把日志文件换成同名目录，再产生 4 条记录，丢失计数从 0 涨到 4，而 `/healthz` 仍是 200。
  现在写入失败会重新打开 `path` 并重试该条记录一次（瞬时的错误不留下空洞），仍然失败才计入
  丢失；每条记录写入前还会核对配置路径是否仍指向手上这个文件，被轮转走时会重开。**配置的
  路径整个消失也算被替换**：`mv` 到别处而不补新文件、或者为了腾空间 `rm` 掉日志，都会让句柄
  写进一个再也没有名字指向它的 inode，路径本身直到服务器重启都不会再出现；现在这种情况会
  重新创建 `path` 并继续写（Linux 上的实测，见下）。
  写不进去**不会**让服务器停止服务，这是有意的：否则一个只读的日志目录就能让隧道下线。
- **`aethertunnel_control_rejected_total` 漏掉了 ACL 与限流两条路径。** 这个计数器的说明是
  "被拒绝的控制连接（容量、ACL、限流或封禁）"，但 `deny_cidrs` 与令牌桶只增加各自那条更具体的
  计数器（`connections_denied_by_acl_total`、`connections_rate_limited_total`），汇总的那条停在 0。
  实测：三次被 `deny_cidrs` 拒绝的连接之后，汇总计数仍是 0。现在两条路径都计入汇总，
  两个计数器故意重叠：一个回答"握手前一共挡掉多少"，另一个回答"为什么"。
  新增回归测试 `TestEveryPreHandshakeRefusalIsCounted`，去掉任一处计数即失败。
- **`aethertunnel_bytes_from_clients_total` 与按隧道的字节计数不含 UDP。** 数据报会话只在
  `Tunnel` 上记账、只写进账本，没有进指标，而 `aethertunnel_bytes_to_clients_total` 的说明是
  "发往客户端的字节"。现在 UDP 会话的字节也计入全局与按隧道的字节计数（数据报会话不计为"流"，
  所以 `streams_total` 不受影响）。这些字节是在**会话释放时**一次性计入的，也就是该来源地址
  安静 `server.read_timeout_seconds`（默认 120）秒之后，与它从
  `aethertunnel_udp_sessions_active` 消失的时刻相同；按数据报即时增加的是
  `aethertunnel_udp_datagrams_total`。所以数据报在传、而两个字节计数器还没动，是正常现象。
- **`GET /api/status` 的 `traffic` 在客户端全部断开后归零**，而面板上写着"计数器自服务器启动起
  累计"。它原本是"当前注册的这些隧道各自累计了多少"，最后一个发布该代理的客户端断开后就变成 0；
  同一时刻 `/metrics` 的两个字节计数器仍在累计，两个视图互相矛盾。现在 `traffic` 是自启动起的
  累计值，与 `/metrics` 一致；按隧道的数字仍随注册重置，留在 `/api/proxies` 里。

### 新增

- `GET /api/status` 增加 `audit` 段：`enabled`、`writable`、`path`、`max_bytes`、
  `bytes_written`、`write_failures`、`records_lost`、`recovered`、`last_error`。
  配置了审计但当前写不进去时报告为 `enabled: true` 且 `writable: false`，而不是
  `enabled: false`——这两种情况对运维意味着完全不同的东西。
- 指标 `aethertunnel_audit_write_failures_total`、`aethertunnel_audit_records_lost_total`、
  `aethertunnel_audit_records_recovered_total`。只在 `[audit] enabled = true` 时输出：
  恒为 0 的"丢失 0 条"会被读成"审计正常"，而实际上没有任何日志。
- 面板的"服务器状态"栏新增"审计日志"一行，四种状态：正常写入、重开后补写 N 次、
  无法写入 — 已丢 N 条（此时另有一条横幅，直到服务器报告路径可写才消失）、
  已恢复写入 — 期间丢 N 条（恢复后保留警告，因为那几条确实没了）。中英双语。
- 服务器日志在进入故障状态与恢复时各写一行，重复故障不会每个记录刷一行。

### 测试

- `pkg/server/audit_test.go` 新增五项：句柄被关掉之后继续记录（记录不丢，错误被报出并清除）、
  路径无法打开时计入丢失并保持可运行、配置路径被外部轮转走之后重开并写到操作者看的文件里
  （在 Linux 上跑；Windows 不允许改开着的文件的名字，该平台的等价轮转是原地截断，句柄仍然有效）、
  配置路径被整个删掉之后重建出来（同样在 Linux 上跑；Windows 拒绝删除开着的文件）、
  `Close` 之后的记录不会把日志文件重建出来。把重开重试去掉，第一项立刻失败并报出
  "3 条记录只写下 1 条"；把"路径是否被替换"的判断去掉，第三项失败并报出
  "操作者查看的路径里是空的"；把"`os.Stat` 失败也算被替换"这一条去掉，第四项失败并报出
  "配置的路径没有被重建"。
- `pkg/server/ops_test.go` 新增两项：`/api/status` 的 `audit` 段在健康与故障两种状态下的取值，
  以及三条指标与 `/api/status` 报的是同一组数字；审计关闭时三条审计序列**不出现**。
- `pkg/server/guard_test.go` 新增 `TestEveryPreHandshakeRefusalIsCounted`：`deny_cidrs`、
  令牌桶与封禁三条拒绝路径各自移动自己的计数器，同时都移动汇总的
  `control_rejected_total`，且都不计为已接受。把 ACL 与限流两处的汇总计数去掉，它立刻失败并
  同时报出两条路径。

### 运维测试

`scripts/smoke-test.ps1` 从 75 项扩到 83 项：

- **审计日志故障三项**：一台独立服务器，先确认健康状态报的是可写且零丢失；把日志文件移除并在
  同一路径放一个目录，再产生记录，确认 `/metrics` 的丢失计数上涨、`/api/status` 报
  `writable: false` 且带出错误、`/healthz` 仍是 200、日志里出现 `audit: cannot write`；
  最后把路径恢复，确认不需要重启就开始重新记录、`last_error` 被清空、日志里出现
  `is writable again`。
- **`/api/health` 与 `/api/status` 两项**：`/api/health` 不带令牌可读、报出状态与协议版本；
  `/api/status` 逐字段核对面板绘制时读的每一项（连接数、代理数、双向字节、是否需要令牌、
  `audit` 段），并且版本号与 `--version` 一致。这两条接口此前没有任何测试或脚本请求过。
- **`aethertunnel_control_rejected_total`**：服务端级拒绝的服务器上，三次拒绝之后该计数
  至少为 3，且不低于更具体的 ACL 计数。
- **`aethertunnel_visitors_denied_by_proxy_total`**：按代理 ACL 的拒绝被记录进审计之后，
  计数至少为 1。
- **`aethertunnel_udp_sessions_active`**：从一个新的来源端口发一个数据报，该 gauge 必须上涨
  （此前没有任何测试读过它）。
- **`aethertunnel_tunnel_http_requests_total`**：按 `tunnel="web"` 的标签读到至少 1，
  按 `tunnel="tcp-echo",direction="from_client"` 的字节计数读到至少 1。

发布后在**真实 Linux 内核**上又把发布产物本身跑了一遍（发布验证脚本，
`scripts/smoke-test.ps1` 覆盖不到的都在这里）：两个二进制与 `SHA256SUMS` 一致并报出
`v3.7.0`、一条 tcp 字节流与一个 udp 数据报各走一次真实隧道、数据报的字节在会话释放时
进入全局计数、运行中的日志被 `mv` 走后服务器在**配置的路径**上重建并在日志里写出这件事、
`SIGTERM` 之后按日志所述退出。最后一项在 Windows 上做不出来：那里不允许改开着的文件的名字。

### 文档

`docs/CONFIGURATION.md`（`[metrics]` 一节的序列名逐条列出、`[audit]` 一节的失败行为）、
`docs/SECURITY.md`（审计日志失败必须可见）、`README.md`（能力表、运维一节新增客户端
`--identity`）、`web/dashboard/README.md`（审计一栏与横幅）、`docs/MIGRATION.md`
（新增 v3.6.0 → v3.7.0）。

---

## [3.6.0] — 2026-09-21

本版本修好一个"文档里写了、实际用不了"的服务端设置，并把三个从未被任何测试或脚本
驱动过的配置键补上真实验证。配置键与线协议都没有破坏性变化，两端的旧配置可以直接用。

### 修复

- **`server.subdomain_host` 之前完全用不了。** 客户端在加载配置时就要求 `http`/`https`
  代理至少写一个 `domains`，因此不带域名的代理过不了校验，服务端里"把没有域名的代理按
  `<代理名>.<subdomain_host>` 注册"的那段代码不可能被触达，文档描述的访问方式因此是死的。
  这个设置属于服务端，客户端无从知道它，所以现在客户端只给出一条警告，说明该代理会以
  `<代理名>.<server.subdomain_host>` 发布、以及服务端在没有这个设置时会拒绝注册，
  真正的决定留给服务端。实测（修复前）：客户端直接以
  `invalid configuration: - proxy "web": a http tunnel needs at least one entry in domains`
  退出。修复后同一份配置启动成功，`Host: web.tunnel.test` 得到 200。

### 变更

- 配置加载的警告增加了这一条。它出现在客户端启动日志与 `--check` 的输出里，
  与既有的"未知键""弱 auth_token"等警告走同一条路径。

### 测试

- `pkg/server/vhost_subdomain_test.go`：`TestASubdomainHostPublishesAProxyWithoutDomains`
  覆盖 `web.tunnel.example` 命中、大小写不同的形式同样命中、`other.tunnel.example`、
  `web.other.example` 与裸 `tunnel.example` 都是 404；
  `TestAProxyWithoutDomainsIsRefusedWithoutASubdomainHost` 断言拒绝信息里点出
  `subdomain_host`，让人知道该开哪个键。
- `pkg/config/config_test.go`：`TestAnHTTPProxyWithoutDomainsIsAcceptedWithAWarning`。
  把这条校验改回硬错误，它立刻失败。
- `pkg/server/audit_test.go`：`TestAuditLogRotatesAtMaxBytes` 核对上一代文件存在、
  大小落在限制附近（大小是在写入前检查的，所以一代最多比限制多一条记录）、
  每一行都是完整记录、当前文件重新从小尺寸增长、轮转之后还能继续写；
  `TestAuditLogRotationCanBeDisabled` 核对 `max_bytes = 0` 时四十条记录都留在同一个文件里。
  把轮转的判定改成永假，前者失败并报出实际字节数。
- `pkg/server/dht_test.go`：`TestADHTRecordLapsesAfterTheConfiguredTTL`。配置文件以 TOML
  写盘再加载，`ttl_seconds` 与 `announce_ttl_seconds` 都设成 2（校验要求存储时长不短于
  通告时长，这是最短的合规组合）；记录在寿命内能解析，之后必须以 `dht.ErrNotFound` 结束
  ——存储层已经丢掉它——而不是 `ErrStale`（存储层还留着、只是通告过期）。这个差别正是
  该键的作用。让存储层忽略 TTL，测试在二十秒后以 `ErrStale` 失败。

### 运维测试

`scripts/smoke-test.ps1` 从 71 项扩到 75 项：

- **`idle_timeout_seconds` 两项。** 访客客户端的空闲上限设为 2 秒：一次回显之后停止发送，
  连接必须在几秒内自行结束；同一段时间里另一条持续来回的会话必须活着。两项一起把
  "超时针对静默、而不是连接有时长上限"钉住。
- **审计轮转两项。** 一台 `max_bytes = 1024` 的独立服务器，用 30 次被拒连接制造记录，
  核对上一代文件存在、每一行都能被 JSON 解析、当前文件重新从小尺寸增长，并且上一代里
  记着被拒来源。之前这个键从来没有被任何测试或脚本设置过。
- **服务端级访问控制三项**（同一轮早先补上）：一台独立服务器用 `deny_cidrs` 与令牌桶
  限流验证被拒来源在握手前断开、不计入 `aethertunnel_control_connections_total`，
  两种拒绝分别出现在 `/metrics`、审计与日志里。
- **`subdomain_host` 两项**：不带域名的 `http` 代理通过 `<代理名>.<subdomain_host>` 可达，
  没发布过的名字仍然 404。

`[obfuscation] jitter_millis` 之前只在单元测试里出现，现在整轮脚本都开着它：控制连接、
每条数据连接与每个访客连接的每一次写入都被随机延迟，脚本其余部分就是它与协议共存的证据。

### 文档

`docs/CONFIGURATION.md`（`domains` 与 `subdomain_host` 的关系、`idle_timeout_seconds`
的适用范围）、README（能力表、运维一节）、`docs/MIGRATION.md`（新增 v3.5.0 → v3.6.0）。

---

## [3.5.0] — 2026-09-21

本版本把三层隧道放到真实 tun 设备上跑，修掉了它和客户端退出路径上的两个缺陷，
并让打洞结果、代理移除与面板操作有了真实来源。配置键与线协议都没有破坏性变化。

### 修复

- **Linux 上的 `[vpn] enabled = true` 起不来。** 服务端给自己那张 tun 设备配地址时，
  把整个 `sockaddr_in` 交给只接受 4 字节地址的 `SetInet4Addr`：它返回 `EINVAL`，
  代码没有检查这个返回值，于是内核收到一个空地址请求并再次以 `EINVAL` 拒绝，
  服务端随即拒绝启动。实测报错：`vpn: assigning 192.168.99.1 to at0: ... invalid argument`。
  只在真实设备上出现，用假设备的单测看不到。现在传入 4 字节地址并检查返回值。
- **客户端收到停止信号后最长要等一个心跳周期才退出。** 会话循环阻塞在读控制帧上，
  取消只在读到下一帧后才会被看到，而自发到达的帧只有心跳应答（默认 30 秒一次）。
  现在取消会直接关闭控制连接，`Ctrl-C` 与 `SIGTERM` 立刻结束会话。实测：修复前
  `kill` 之后 10 秒进程仍在，修复后立刻退出。

### 变更

- **打洞结果有了真实来源。** 走直连的访客不再使用控制连接、也不再发任何帧，服务器只能看到
  连接消失——这和访客中途放弃是同一个现象。现在访客在直连建立后向会合端口回报路径
  （新增数据报格式 `ATP3" | token:32 | path:1`），服务器在控制连接消失后最多再等 2 秒；
  收到 `'D'` 才计入 `aethertunnel_p2p_direct_total` 并写 `p2p_direct` 审计，
  什么都没收到时写新的 `p2p_abandoned`，不把未知当成功。这个数据报是新增的，
  旧服务端会把它当作无效的会合请求丢弃；旧客户端不发它，那些尝试记为"未知"。
- **`proxy_removed` 现在真的会写出来。** 之前 `ProxyGroup.remove` 只在"池子清空"时返回真，
  因此代理池少一个成员不会记录。现在成员被移除就记录，`detail` 带原因；
  端点与 DHT 通告仍然只在整个名字不再由任何人提供时撤回。
- **`dashboard_action` 现在真的会写出来。** 从面板断开客户端会留下一条审计记录，
  之前这个事件名只出现在文档里。
- **新增 `GET /api/vpn`**，并把同一份摘要并入 `GET /api/config` 的 `vpn` 段；
  面板配置页新增"三层隧道"一栏（中英双语），显示接口、地址池与包计数。
  之前 `[vpn]` 的运行时状态在面板上完全看不到。

### 测试

- `pkg/vpn` 新增 Linux 专用测试 `TestTunDeviceTakesAnAddress` 与
  `TestTunDeviceRefusesAddressesItCannotUse`：打开真实 tun 设备、配置地址并从内核回读。
  第一个测试在修复前会以 `invalid argument` 失败。
- `pkg/server/strategy_test.go`：补上此前完全没有测试的两个调度策略——`random`（40 次请求
  必须触达每个成员）与 `latency`（时延移动平均更低的成员持续被选中，未测量的成员仍会被试用）。
- `pkg/server/punch_test.go`：四项，覆盖"访客回报直连 → 计入直连并写 `p2p_direct`"、
  "访客什么都没回报 → 记为未知、不计直连"、"要求中继 → 计入中继"、"未知 token 的回报无影响"。
- `pkg/protocol/rendezvous_test.go`：`ATP3` 的编解码、与 `ATP1` 的区分、畸形输入与非法入参。

### 运维测试

`scripts/smoke-test.ps1` 从 59 项扩到 66 项，新增的一节用两个真实客户端组成代理池：

- 两个客户端发布同一个名字、共用一个端口，成员数为 2；
- 六个请求全部被正确应答，且两个成员都各自服务过请求；
- `multipath = 3` 的 UDP 代理：一次数据报会话确实开了 3 条数据连接（读
  `aethertunnel_data_connections_total` 的增量）；
- 打洞结果与访客自己日志里报的路径一致，审计里恰好只有一种结果事件；
- 停掉其中一个成员后池子继续服务，且移除被写进审计。

新增 `scripts/vpn-linux-test.sh`：在真实 tun 设备上跑三层隧道。两端分别处于两个网络命名空间，
因此隧道地址不是对端的本地地址，ICMP 必须真的穿过设备；脚本核对双向 ping、两侧
`ip -s link` 的收发包计数、`GET /api/vpn`、`GET /api/config` 的 `vpn` 段、
审计里的地址分配，以及客户端退出后地址回到池中。缺 root、`/dev/net/tun` 或网络命名空间时
打印原因并以 0 退出。该脚本已接入 CI 的 ubuntu 任务。

### 文档

README 的能力表与运维一节、`docs/CONFIGURATION.md`（`[vpn]`、指标序列、审计事件）、
`docs/SECURITY.md`（审计事件）、`docs/ARCHITECTURE.md`（新增 5.1 打洞结果上报）、
`docs/MIGRATION.md`（新增 v3.4.0 → v3.5.0）、`web/dashboard/README.md`（三层隧道一栏）。

---

## [3.4.0] — 2026-09-21

本版本处理了两个"写了没用"的配置键——一个补上真实行为、一个删除，并修好了面板上两个
长期不动的数字。

### 修复

- **活动连接数只增不减。** 访问者流的两条服务路径（通用代理与 socks5 出口）在流结束时
  没有释放记账，于是 `aethertunnel_streams_active`、按隧道的活动流与面板的
  `active_connections` 会随流量一直往上加。实测：三条**已完成**的请求之后指标读到 3、
  `/api/proxies` 显示 `active_connections: 3`，而不是 0。现在两条路径都释放，
  且 `release` 只生效一次（`sync.OnceFunc`），失败路径与成功路径重复调用也不会重复扣减。
  新增回归测试 `TestStreamCountersReturnToZeroWhenAStreamIsServed`、
  `TestStreamCountersReturnToZeroForASocks5Stream`、`TestStreamCountersReturnToZeroForAnHTTPRequest`，
  以及确认"流在跑的时候能被看见"的 `TestAnOpenStreamIsCountedWhileItRuns`。
- **每个客户端的 `active_streams` 与 `total_streams` 恒为 0。** 这两个字段在会话上存在、
  面板也在读，但没有任何地方写入过。现在在流打开与结束处记账；
  smoke test 的 `the clients view counts the streams the session carried` 会核对实际次数。

### 变更

- `[server] graceful_shutdown_seconds` **之前完全无效**：服务器收到停止信号就把所有客户端
  断开。现在按它的字面含义工作：先关闭监听、停止接受新访客，给正在传输的流最多这么多秒完成，
  然后才断开；期间到达的访客被立即拒绝并计入
  `aethertunnel_streams_refused_while_draining_total`；没有流在传时立刻退出，不会空等。
  日志会写明是 `every stream finished within` 还是 `stream(s) were still running after`。
  负数现在会被 `--check` 拒绝。
- **删除 `[obfuscation] default_type`。** 它从未被任何代码读取，文档也只写着"保留键"。
  配置里再写它会被当作未知键报出来。

### 运维测试

`scripts/smoke-test.ps1` 从 54 项扩到 59 项：

- 优雅关闭三项：信号之后仍在传输的流继续可用；最后一个流结束后服务器立即退出并在日志里
  写明 drained；超过宽限期的流被断开且日志写明放弃。这三项在一台独立服务器上由真实的停止
  信号驱动（Windows 用不带 `/F` 的 `taskkill`，Linux/macOS 用 `kill -TERM`），
  并校验流在宽限期内确实还能交互、宽限期耗尽后确实被断开。
- 活动流计数归零与客户端流计数两项：读 `/metrics` 与 `/api/clients`。

### 文档

`docs/CONFIGURATION.md` 改写 `graceful_shutdown_seconds` 的含义并删除 `default_type`，
指标一节补上新增序列；`docs/MIGRATION.md` 新增 v3.3.0 → v3.4.0 一节；
`server.toml.example`、`README.md` 同步。

---


## [3.3.0] — 2026-09-21

本版本新增一个代理类型（`socks5`）、三项策略能力（按代理的访客 ACL、认证失败自动封禁、
DHT 通告签名），并把它们接进 `scripts/smoke-test.ps1` 的真实运维检查。

### 新增

**代理类型：`socks5` 出口**

- `[[proxies]] type = "socks5"`：访客用标准 SOCKS5（RFC 1928 CONNECT，无认证）指定目标，
  客户端拨号，字节流经隧道转发。没有本地服务，因此 `local_ip` / `local_port` 会被忽略并
  给出警告，`remote_port` 必填。
- `allow_targets` 是**必填**项，列出客户端允许拨号的 CIDR；缺失时注册被拒绝
  （`a socks5 tunnel needs allow_targets`），不会退化成"什么都能连"。不在名单里的目标用
  SOCKS5 回复码 `0x02`（not allowed）拒绝。
- 服务器在收到 CONNECT 之前先完成协商，因此目标不可达时访客看到的是 SOCKS5 错误码而不是
  连接被直接关闭。新增 `aethertunnel_socks5_requests_total` 指标。

**按代理的访客 ACL**

- `[[proxies]] allow_cidrs` / `deny_cidrs`：服务器整体接受连接之后、建立隧道之前，再按该
  代理自己的名单判断来源地址；deny 优先，名单非空而来源无法解析时按拒绝处理。
- 拒绝会写审计记录 `proxy_visitor_denied`（带代理名）并计入
  `aethertunnel_visitors_denied_by_proxy_total`。http/https 代理对被拒访客返回 403。

**认证失败自动封禁**

- `[server] ban_after_failures` / `ban_seconds` / `ban_max_seconds` / `ban_ignore_cidrs`。
  同一来源在 10 分钟窗口内认证失败达到次数后封禁该来源，封禁期间**任何凭据**都先被拒绝、
  不进入握手；认证成功清零计数；重复被封时长翻倍直到上限。
- 新增指标 `aethertunnel_sources_banned_total`、`aethertunnel_banned_connections_refused_total`，
  审计事件 `source_banned` 与 `ban_refused`。

**DHT 通告签名**

- `[dht] signing_key_file`（服务端）：每条通告用 Ed25519 签名，密钥首次使用时生成到该文件
  （0600），公钥可从启动日志、`GET /api/dht` 的 `signing_key` 或新增的 `--dht-key` 读出。
- `[dht] require_signed` / `trusted_keys`（客户端与查询端）：拒绝无签名记录，或只接受指定
  公钥签发的记录。校验在有效期判断之前进行，伪造的记录报 `signature does not verify`
  而不是被当成过期。
- `--dht-lookup` 与 `--discover` 的输出现在会写明记录是否经过签名校验及其公钥。

### 运维测试

`scripts/smoke-test.ps1` 从 38 项扩到 54 项：新增 socks5 出口（到达指定目标、拒绝名单外目标、
面板记账）、按代理 ACL（名单外拒绝、名单内放行、审计记录）、DHT 签名（公钥发布、查询报告
签名者、可信读取端成功、异钥读取端拒绝），以及一处独立的封禁服务器（失败达次数即封禁、
审计与指标、被封来源上有效凭据同样被拒、到期自动解除）。全部用真实二进制与 curl 跑通。
面板另在 Chrome 里验证：列出 socks5 出口的类型与端口、真实 SOCKS5 请求的计数、
名单外目标被拒、中英切换、360 px 布局与断开按钮。

### 修复（由新测试发现）

- 钉住 socks5 与 ACL 的端到端测试根本不覆盖被测路径：`pkg/server` 的测试代理在没有
  `handlers` 表项时直接丢弃数据请求，socks5 测试因此既不拨号也不回应，最终以访客侧超时
  失败（`reply: read tcp ...: i/o timeout`）。现在带目标的请求不再查表。
- 封禁与按代理 ACL 的测试用 127.0.0.2 当"另一个来源地址"，而 macOS 的 lo0 只分配
  127.0.0.1，绑定同网段的其它地址会失败，CI 的 macOS 任务因此有 7 个测试报错
  （Linux 与 Windows 正常，二者在整个 127.0.0.0/8 上都有响应）。规则本身改用单个地址配合
  是否落在名单里的范围验证，真正需要两个来源地址才成立的两条断言（封禁不影响其它地址、
  名单只放行其中一个地址）改为先探测本机能否绑定 127.0.0.2，不能则跳过并写明原因。
  `scripts/smoke-test.ps1` 的同类检查也做了同样的探测。
- `scripts/smoke-test.ps1` 的 `Read-Log` 在文件为空时返回 `$null`，而
  `if ($null -notmatch '模式')` 恒为假，导致**所有读日志的断言都在不校验任何内容的情况下通过**。
  现在它同时读取标准输出与标准错误两个文件，并始终返回字符串。

---


## [3.2.1] — 2026-09-20

### 修复

- **`http` 与 `https` 代理的流量从未被记账。** 反向代理这条路径只更新了组级计数，
  而面板、会话计数与带宽账本读的是成员计数，因此一个 http 代理无论搬运多少字节，
  面板都显示 `0 B` / `0 次请求`，客户端断开时写进账本的也是 `bytes_in=0 bytes_out=0`
  （实测：请求正常返回 200，面板仍是 `total=0 bytes_in=0 bytes_out=0`，
  账本条目同样为 0）。现在每个完成的请求按成员数平均计入各成员，与数据报会话在多路径上的
  记法一致；那三个从未被任何地方读取的组级计数已删除。
  新增两个回归测试：`TestHTTPProxyTrafficIsRecorded`（面板所用的 `Totals()` 与成员摘要）
  与 `TestLedgerRecordsHTTPProxyTraffic`（账本条目），修复前分别失败于
  「the group counted 0 requests, want 1」与「bytes_in is 0, want 30」。
- 文档同步：`docs/SECURITY.md` 的账本一节改为按代理类型说明记账口径，
  `web/dashboard/README.md` 说明 http 代理按请求计数、池内按比例计入成员。

---

## [3.2.0] — 2026-09-20

本版本实现了 v3.1.0 文档中列为"尚未实现"的全部条目（移动端应用与 Windows/macOS 的 tun
设备除外，见文末），并补齐 Kubernetes 清单与容器镜像。

### 新增

**运维与策略层**

- `[server] allow_cidrs` / `deny_cidrs`：按 CIDR 的访问控制，在握手之前执行；
  先匹配 deny，再匹配 allow（allow 为空表示允许全部来源），无法解析的来源地址在存在规则时按拒绝处理。
- `[server] rate_limit_per_second` / `rate_limit_burst`：按来源地址的令牌桶限流，
  同样在握手之前执行；空闲桶会被回收，不在内存中按历史来源地址无限增长。
- `[audit]`：JSON Lines 审计日志，记录接入/拒绝、认证失败、客户端上下线、代理注册与拒绝、
  ACL 与限流拒绝、访客接受与拒绝、打洞结果与隧道地址分配；按 `max_bytes` 轮转，保留一份历史文件。
- `[metrics]`：`GET /metrics` 输出 Prometheus 文本格式（版本 0.0.4），
  含控制连接、认证失败、ACL 拒绝、限流拒绝、数据连接、流数量与并发、双向字节、UDP 数据报与活动会话，
  以及按隧道标签的流与字节序列。可设置 `[metrics] token`，该 token 与面板 token 任一可用。
- `GET /healthz` 与 `GET /readyz`：前者恒为 200，后者在监听器未就绪或正在关闭时返回 503；
  两者都不需要令牌。

**代理类型与调度**

- `udp`：每个访问者来源地址一个会话，服务器用一个 UDP 套接字承载全部会话。
- `http` / `https`：服务器上一套共享监听，按请求的 Host 头选择隧道，支持精确域名、
  `*.通配` 与 `subdomain_host` 后缀匹配；作为反向代理转发并保留流式响应。
- `stcp` / `sudp` / `xtcp`：私有隧道，只对知道 `secret_key` 的访客开放，不开放公网端口。
- `xtcp` 打洞：自研 UDP 打洞（HMAC-SHA256 同时打开 + 可靠有序字节流 `pkg/reliable`），
  失败时经 `[server] p2p_port` 的会合服务回退到中继，两种情况都记入日志与审计。
- 代理池与 `[server] load_balance`：同名代理可由多个客户端组成池，策略为
  `round-robin`、`random`、`latency`、`failover`、`adaptive`（时延移动平均 × 连续失败惩罚）。
- `multipath`：数据报代理最多用 8 条数据连接承载，单条路径故障不影响整个会话。
- `GET /api/proxies` 增加 `member_count`、`members`、`healthy`、`consecutive_failures` 等池状态字段；
  控制台的代理表格新增「成员」列，显示成员数与其中可用（healthy）的个数。

**加密与身份**

- `[transport] enable_tls`：控制端口与所有数据连接使用 TLS，客户端可用 `ca_file` 校验证书。
- `[identity]`：Ed25519 身份签名，服务端可用 `allowed_keys` 白名单、`require_identity` 强制；
  数据连接与访客连接同样校验，不再只有控制连接校验。
- `[encryption] post_quantum`：X25519 与 ML-KEM-768 混合密钥协商，每个会话派生会话密钥，
  每条数据连接再用 `HKDF(session_key, stream_id)` 派生独立密钥。
- `auth_method = "nizk"`：访客用 P-256 上的 Schnorr 证明自己知道 `secret_key`，不发送该值。

**网络与目录**

- `[ledger]`：Ed25519 签名、哈希链式追加的带宽账本（JSONL）。客户端断开时按代理写入一条，
  `GET /api/ledger` 发布公钥、链头、最近条目与按客户端汇总；`--verify-ledger` 离线校验，
  篡改任何一字节或换一串公钥都会失败。
- `[dht]`：Kademlia DHT（160 位标识、k 桶、迭代查找、值/提供者两个命名空间）。服务端把每个
  已发布的代理写成记录（记录里带地址与有效期），客户端可用 `dht.discover` 按名字解析服务器地址，
  并在每次重连前重新解析；运维可用 `--dht-lookup`（服务端配置）与 `--discover`（客户端配置）。
  通告自带有效期，服务端下掉代理后记录会在一个通告周期内失效。
- `[vpn]`：三层隧道。客户端向服务端申请地址，IP 包作为 `TypeVPNPacket` 帧在控制连接上传输；
  服务端用 `pkg/vpn` 的地址池与路由器在多个客户端之间分发。Linux 上打开或创建 tun 设备
  （`device` 为空则向内核申请名字，MTU 写入网卡）；其它平台启动即报错并说明缺少什么。
  地址池不会分配网络地址、广播地址与服务端自用地址；非本客户端的源地址会被丢弃。

**混淆与交付**

- `[obfuscation] disguise = "tls-record"`：把每个写入包进 TLS 1.2 应用数据记录，
  超过 16384 字节的写入按记录上限拆分，读侧透明重组。它不做握手，只改变外观。
- `Dockerfile`（多阶段构建 → distroless 非 root）与 `deploy/kubernetes/`（Namespace、
  ConfigMap、Secret 示例、Deployment、Service、kustomization）。
- `AETHERTUNNEL_AUTH_TOKEN`、`AETHERTUNNEL_DASHBOARD_TOKEN`、
  `AETHERTUNNEL_ENCRYPTION_PASSPHRASE`：凭据可由环境变量提供，并在校验之前生效，
  因此配置文件里可以完全不放密钥。
- 运维子命令：`--dht-lookup`、`--verify-ledger`（服务端），`--discover`（客户端）。
- `scripts/smoke-test.ps1` 扩到 38 项检查：新增目录（发布、两种查询、未知名）、
  账本（记账、校验、篡改检测、异钥拒绝）、隧道设备缺失时的拒绝，以及全程开启的连接伪装；
  新增 `-ProgressLog` 参数与 `-Keep` 保留现场。
- CI 升到 Go 1.24（`crypto/mlkem` 需要），新增六个目标的交叉编译检查与 Linux 上的 `-race` 任务。

### 修复（由新测试发现）

- `pkg/vpn` 的路由器会把读取缓冲区切片排进发送队列，缓冲区被下一次读取复用后，
  排在队列里的包内容会被改写。现在入队前复制，测试用两个不同长度的包复现过该问题。
- `Session.framer` 在会话生命周期中会被抗量子握手替换，而隧道协程同时读取它，
  存在数据竞争；改为加锁访问的 `Framer()` / `SetFramer()`。
- `Server.listener` 由 `Run` 写入、由面板与测试读取，同样存在数据竞争；改为加锁访问。
- `dht.Table` 缺少删除本地记录的方法，导致服务端下掉代理后仍会继续重新发布；
  新增 `Forget`。
- `-dht-lookup` 使用服务端自身配置时会去绑定服务端已经占用的 DHT 端口而失败；
  查询节点现在改绑临时端口，且在没有配置 `bootstrap` 时向 `listen_addr` 指向的节点查询。
- 客户端 `--discover` 与 `dht.discover` 未等待加入 DHT 就查询，首次必然失败；
  现在先完成 bootstrap 再解析。
- 帧长度填充的抖动与补齐在 `[obfuscation] enabled = false` 时也会生效；
  现在 disguise 未启用时会给出警告。
- `obfs` 的 TLS 记录头校验只检查了主版本号，`0x0301`（TLS 1.0）会被当成合法记录；
  现在要求完整的 `0303`。
- IPv6 包的长度校验把"负载长度"当成"整包长度"，导致所有 IPv6 包被判为畸形。
- 测试端口分配在 Windows 上会取到被系统保留的端口，UDP 绑定随即失败；
  UDP 代理测试改用 UDP 端口探测，服务端测试与运维脚本同时探测 TCP 与 UDP。
- `Server.setListener` 递归调用自己，在非可重入互斥锁上自锁死，服务端一启动就卡住；
  现在直接赋值。

### 兼容性

- **线协议升到 4**（`ProtocolVersion = 4`）：本版本新增了帧填充标志位与 16–18 号消息类型，
  3 版对端不认识它们——它会把填充帧里的长度前缀当成数据而拒绝该帧。因此两端必须一起升级。
  版本不一致时握手仍然成功，但两端都会在日志里报出 `protocol mismatch`，
  这一条比"看起来能连上、部分功能静默失效"更容易排查。
- 配置文件向后兼容 v3.1.0：`[server]`、`[client]`、`[[proxies]]` 的既有键含义未变，
  新增段都是可选的，默认关闭。

### 仍未实现

- **移动端应用**：本仓库只产出服务端与客户端两个可执行程序，没有 iOS/Android 工程。
- **Windows 与 macOS 的 tun 设备**：三层隧道只在 Linux 上打开设备；Windows 需要 Wintun 驱动，
  macOS 需要 utun 控制套接字，本程序都不提供。Linux 路径每次 CI 交叉编译，但未在真实 tun
  设备上运行过。

---

## [3.1.0] — 2026-09-20

本版本修复了使核心链路不可用的缺陷，并新增配置校验、可选加密、面板 API、测试与跨平台构建。

### 修复（都是会导致功能完全不可用的缺陷）

- **服务端无法启动**：`main.go` 对同一个 `bind_addr:bind_port` 调用了两次 `net.Listen`，
  第二次必然失败并 `log.Fatalf`，进程启动即退出。现在只监听一个端口，控制连接与数据连接
  复用它。
- **客户端与服务端协议不兼容**：客户端发送 ASCII 文本 `AUTH:<token>`，服务端读取 8 字节
  二进制帧头，认证永远不可能成功；服务端回包客户端也解析不了。现在两端使用同一套
  带版本号的帧格式（`pkg/protocol`），并在握手时交换协议版本与加密算法。
- **心跳消息无法表示**：`NewHeartbeatMessage` 造出空负载，而读取端把 `payloadLen == 0`
  判为错误，于是每次心跳都会踢掉连接。现在空负载合法。
- **隧道数据面是死的**：`HandleConnection` 只读一条消息就返回，连接随后被关闭；目标地址
  被硬编码为服务器自己的 `127.0.0.1:<remote_port>`；服务器从不监听 `remote_port`。
  现在服务器为每条隧道真正绑定公网端口，访问者到来时通过控制连接请客户端回拨一条数据
  连接，两端用 `io.Copy` 半关闭双向搬运（带 32 KiB 缓冲池与空闲超时）。
- **未认证即可开隧道**：旧代码在第一条消息就是 `Proxy` 时直接转发，不检查认证。现在数据
  连接必须携带有效会话 ID 与未使用的流 ID。
- **连接表永久泄漏**：`RemoveConnection` 关闭 socket 却从不 `delete` 表项，超过 100 次
  连接后服务器永久拒绝所有客户端；隧道也不会随会话释放。现在会话与隧道在断开时都会被
  注销，释放公网端口。
- **隧道名在重连后无法复用**：`Session.Close` 清空了隧道表，导致随后注销时什么都没删掉，
  重连的客户端拿到 "a tunnel with that name is already registered" 而失去隧道。现在
  隧道归属会被正确注销，且管理器会把"所属会话已死"的陈旧条目判为可替换。
- **加密完全不可用**：密钥直接取口令字节，而 XChaCha20-Poly1305 要求恰好 32 字节，任何
  普通长度的 token 都会报 `bad key length`。现在用 HKDF-SHA256 派生，任意长度口令可用，
  并支持选择 XChaCha20-Poly1305 或 AES-256-GCM。
- **混淆模块返回被丢弃的明文**：`ObfuscatePacket` 加密后返回的是另一个变量；接收端又在
  已 base64 的数据上再编码一次，任何数据包都解不开。该模块已删除（未实现的功能不再假装
  存在）。
- **WebSocket / HTTP / SCTP 传输一被调用就 panic**：把 `http.ResponseWriter` 断言成
  `net.Conn`、`Close()` 时关闭仍有发送者的 channel、读写锁被用于阻塞的 socket 读导致
  连接无法关闭。这些传输从没有被 `main.go` 启动过，已删除。
- **VPN 包整体不可调用**：`performance` 接口声明 `Disable() bool` 而实现返回 `void`，
  接口无法被满足，调用必 panic；统计模块在同一把非可重入锁上自锁死。已删除。
- **凭据泄漏进日志**：认证失败时把用户提交的完整 token 打进日志。现在只记录来源地址。
- **面板按相对路径读文件**：文件服务器用 `../../web/dashboard`，页面处理器用
  `web/dashboard/...`，两者不可能同时成立，且发布的压缩包里根本没有这些文件。现在用
  `go:embed` 打进二进制。
- **面板显示假数据**：三个页面没有任何网络请求，连接数/带宽/客户端列表/日志/图表分别是
  字面量与 `Math.random()`；`server.html` 因为 CSS 里没有 `.hidden` 规则而根本无法切换
  页面。已重写为单页真实面板（详见"新增"）。
- **`/api/config` 返回示例配置**，包含一个假的 auth token，且与真实配置无关。现在返回
  脱敏后的真实设置。
- 构建脚本与 CI 全部指向不存在的路径（`./server`、`./main_minimal.go`），Docker 构建把
  Go 1.21 与要求 1.22.2 的模块放在一起，CI 的产物路径与 `download-artifact@v4` 不符，
  两个工作流还会向同一个 release 上传同名附件。已重写。
- 版本号被打成 `-X main.Version` 而变量名是小写 `version`，链接器静默忽略，于是二进制永远
  报硬编码的旧版本号。现在 `--version` 能报出真实构建信息。

### 新增

- **`--config` / `--check` / `--version` 命令行参数**（旧版把第一个参数当配置文件路径，
  `--version` 会被当成文件名）。
- **配置校验**：未知配置键会被列出来，`--check` 只校验不启动；端口、代理名重复、不支持的
  代理类型、弱 token、对外暴露却无 token 的面板都会给出明确提示。
- **客户端重连**：指数退避到 `max_reconnect_seconds`，带 ±20% 抖动；断线后自动重新注册
  全部隧道。
- **可选的负载加密**：控制帧与隧道记录都用 AEAD；两端配置不一致时给出
  `encryption mismatch` 而不是无限认证失败。
- **面板 API 与 Bearer token**：`/api/health`（公开）、`/api/status`、`/api/clients`、
  `/api/proxies`、`/api/config`（脱敏）、`DELETE /api/clients/{id}` 断开指定客户端。
- **新面板**：单页、内嵌资源、中英双语（两个语言包键完全对齐）、有真正的移动端导航抽屉、
  空状态与错误提示按实际数据显示、GET/PUT 数据全部用 `textContent` 插入（无 `innerHTML`）。
- **测试**：`pkg/config`、`pkg/crypto`、`pkg/protocol`、`pkg/server` 四组测试，其中
  `pkg/server` 包含真实的端到端测试——启动服务器、注册隧道、访问者连接、数据穿过隧道
  回到本地回声服务，明文与加密各跑一遍；另有会话释放、名字复用、连接上限、垃圾输入
  不致命等回归测试。
- **跨平台构建**：`scripts/build-release.sh`（POSIX）与 `scripts/build-release.ps1`
  （Windows）产出 6 平台 × 2 个二进制 + `SHA256SUMS`；CI 在 Linux/Windows/macOS 上
  跑 `gofmt`/`vet`/`test`/构建；发布工作流按 tag 触发并校验产物格式。
- **文档**：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)、
  [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)、
  [`docs/SECURITY.md`](docs/SECURITY.md)、
  [`docs/MIGRATION.md`](docs/MIGRATION.md)，以及双语 README。

### 移除

- 所有描述不存在功能的文档（共 27 个 md 文件），包括 `PROJECT_SUMMARY.md`、
  `QUALITY_REPORT.md`、`DELIVERY_CHECKLIST.md`、三个安全审计报告与 `docs/` 下 19 个文件。
  需要对照旧说法请查本文件的顶部说明。
- 不可用且未被调用的模块：`pkg/vpn`、`pkg/obfuscation`、`pkg/protocol/{http,websocket,sctp}.go`、
  `pkg/net/mux.go`、`pkg/interfaces`、`sctp-fake`（一个把 `libp2p/go-sctp` 替换成空壳的
  本地模块）、`release/{linux,darwin,windows}-amd64`（三份重复的客户端源码副本）。
- 与产品无关的 Python 编排脚本（`auto_trigger_system.py` 等 4 个）与 `__pycache__`。
- 依赖 `gorilla/websocket` 与 `libp2p/go-sctp`；现在只有 `BurntSushi/toml` 与
  `golang.org/x/crypto`。

### 兼容性

- 配置文件：`server.bind_addr`、`server.bind_port`、`server.auth_token`、`client.server_addr`、
  `client.auth_token`、`[[proxies]]` 的字段含义不变。`[dashboard]` 新增 `bind_addr`/`token`，
  `[encryption]` 是新增段。旧配置里的 `enable_tls`、`[vpn]`、`[obfuscation]`、`[webrtc]`
  等键不再被使用：前者会作为未知键被报出来，后两者会解析但被忽略并给出警告。
- 协议：与旧版本**不兼容**，两端必须一起升级（`ProtocolVersion = 3`）。

---

## [3.0.0] — 2026-02

标签存在，但核心链路未跑通；详细功能清单已按本文件顶部说明删除。

## [2.0.1] / [2.0.0] — 2026-01

同上。

## [1.0.x] / [0.1.1-alpha] — 2025-12 ~ 2026-01

同上。
