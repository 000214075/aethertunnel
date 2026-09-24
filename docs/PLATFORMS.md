# 平台支持与验证

本文件只讲事实：六个发布目标各自被**什么**执行过、证明了**什么**、以及**什么没有被证明**。
"能编译"与"能运行"是两件事，这里分开写。

## 1. 结论表

| 发布目标 | 本机（Linux 工作站） | CI | 功能检查 |
|---|---|---|---|
| linux/amd64 | 原生执行 | `ubuntu-latest`：gofmt、vet、单测、`-race`、真实 tun 设备的三层隧道、构建、示例配置、版本 | **70/70** |
| linux/arm64 | qemu-aarch64（arm64 指令集） | 只做交叉编译（CI 里还没有 arm64 的执行作业） | **70/70** |
| windows/amd64 | Wine 10.0 执行真实 PE（**本轮未跑，见 3.1**） | `windows-latest`：vet、单测、构建、示例配置、版本、`scripts/smoke-test.ps1`（101 项） | 上一轮 67 项时为 **66/66** |
| darwin/arm64 | **无法执行** | `macos-latest`（arm64）：vet、单测、构建、示例配置、版本 | 仅 CI |
| darwin/amd64 | **无法执行** | 仅交叉编译（`macos-latest` 是 arm64 运行器） | 仅静态核对 |
| windows/arm64 | **无法执行** | 仅交叉编译 | 仅静态核对 |

功能检查是同一套 70 项，分六组，都在每个平台上真跑：

**隧道（25 项）**：八种代理类型（`tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5`）、
共享 http/https 监听（含 TLS 与未知主机拒绝）、socks5 越界目标被拒、三个访问者
（stcp/sudp/xtcp）、面板四个接口、指标与 `/api/status` 数字一致、审计里同时出现
`proxy_registered` 与 `visitor_accepted`、**真实客户端进程的日志里两条到达路径各自的行**
（一条流来自访问者、另一条来自公网端口，各一项），以及**半关闭之后仍能收到应答**（一个
"读到 EOF 才回答"的服务经 socks5 隧道访问：请求发出后半关闭，应答仍要回来）。

**命令行（7 项）**：服务端与客户端的 `--version`（含协议版本）、两个二进制分别对随版本发布的
`server.toml.example` 与 `client.toml.example` 做 `--check`、以及客户端 `--identity`
（生成密钥文件并打印公钥，且不打印任何其它内容）。

**池策略（3 项）**：两个客户端组成一个池，一个转发到可用的回显服务、另一个指向无人监听的端口。
访问者不会被丢（服务端发现成员不应答会改问另一个），差别在于**浪费掉多少尝试**：30 次访问里
`round-robin` 浪费 15 次、`bandit` 只浪费 4 次（三个平台实测一致），且 `bandit` 仍会采样那个成员
（恢复后能被重新测量）。

**命名与目标（5 项）**：`domains` 的 `*.通配` 对单级与多级子域都生效、对裸后缀不生效；
`allow_targets` 里放 CIDR 时，目标写成**域名**会被先解析再匹配（`localhost` 被 `127.0.0.1/32`
允许），解析到范围外的名字仍被拒。

**发现与私有认证（8 项）**：DHT 名称解析（`--dht-lookup` 把 `tcp` 名解析到它的公网端口、
`--discover` 把私有名解析到控制端口）、一个 `server_addr` 留空的客户端靠 `[dht] discover` 解析出
服务器并真实转发一次数据、把公网类型的名字当作服务器地址会被报告为错误、nizk 访问者（知道密钥
的能过、密钥错误的被拒）、以及没有 `domains` 的 http 代理以 `<名字>.<subdomain_host>` 可达。

**四层安全栈（两遍，各 8-9 项）**：加密、TLS 控制口、`disguise = "tls-record"` 伪装、Ed25519
身份认证同时打开，真实转发一次数据，并逐条断言每一层自己的日志行——某一层被静默关掉就会失败。
文档里的**两种算法各跑一遍**：`xchacha20-poly1305` 一遍；`aes-256-gcm` 那一遍同时改用
`ca_file`（**真实证书校验**，而不是 `insecure_skip_verify`）并要求服务端 `require_identity = true`。

## 2. 三个无法在 Linux 上执行的目标

- **darwin/amd64、darwin/arm64**：macOS 二进制需要 Darwin 内核。QEMU 的用户态模拟不实现
  Darwin 的系统调用，整机模拟又需要 macOS 系统镜像（且其许可限于 Apple 硬件）。本机没有
  任何合法可行的执行方式，因此这两个目标只做了静态核对：Mach-O 格式与架构、Go 版本
  （`go1.24.13`）、内嵌的可执行名字符串与协议/后量子相关符号。darwin/arm64 的运行由 CI 的
  `macos-latest` 作业覆盖；**darwin/amd64 目前没有任何地方执行过**。
- **windows/arm64**：x86-64 上的 Wine 不能执行 ARM64 的 PE。本文件作者尝试过"qemu-aarch64 跑
  ARM64 用户态 + ARM64 版 Wine"这条路，结论是**在用户命名空间内做不到**：Wine 加载 PE 时会
  再次 `exec` 自己的 loader，而在 qemu 用户态下 `/proc/self/exe` 指向 qemu 本身；让这个 exec
  透明化的唯一办法是注册 binfmt_misc 解释器，可在非特权用户命名空间里向 binfmt_misc 注册会被
  内核拒绝（`register` 写入返回 EIO，即使该命名空间已挂载 binfmt_misc）。因此这个目标**只被
  交叉编译过**，要执行它需要一台 ARM64 Windows 机器，或用完整的系统级虚拟化装上 Windows on
  ARM。CI 也没有 Windows on ARM 的运行器。

## 2.1 跨系统连接

上面的检查里两端是**同一个平台**。除此之外还有一组跨系统矩阵：服务端与客户端来自**不同平台**，
每一对都跑同一套功能检查（八种代理类型、共享监听、访问者、面板、指标、审计）。

| 服务端 | 客户端 | 结果 |
|---|---|---|
| linux/amd64 | linux/arm64 | **25/25** |
| linux/arm64 | linux/amd64 | **25/25** |
| linux/amd64 | windows/amd64 | **22/22**（上一轮，22 项那套） |
| linux/arm64 | windows/amd64 | **22/22**（上一轮，22 项那套） |
| windows/amd64 | linux/amd64 | **22/22**（上一轮，22 项那套） |
| windows/amd64 | linux/arm64 | **22/22**（上一轮，22 项那套） |

四层安全（`aes-256-gcm`、后量子 X25519+ML-KEM-768、TLS **带真实证书校验**、服务端
`require_identity = true`）也按同样的方式跨系统验证过，每一对 6 项全过：

| 服务端 | 客户端 | 结果 |
|---|---|---|
| linux/amd64 | linux/arm64 | **6/6** |
| linux/arm64 | linux/amd64 | **6/6** |
| linux/amd64 | windows/amd64 | **6/6** |
| windows/amd64 | linux/amd64 | **6/6** |
| windows/amd64 | linux/arm64 | **6/6** |

这是同平台运行看不到的东西：证书校验走各平台自己的 TLS 实现、后量子与身份签名走各自的密码学
实现、伪装走各自的套接字写入、加密走各自的字节序与对齐。

隧道检查这一套从 22 项变成 25 项（读客户端日志的两项，加一项半关闭后仍要收到应答），表中涉及 `windows/amd64` 的行是
**上一轮**跑出来的，本轮本机起不来 Wine（见 3.1），只重跑了上面两对 Linux 组合。

## 3. 模拟执行是怎么搭起来的

两者都不需要 root：把 Ubuntu 的 `.deb` 用 `dpkg-deb -x` 解到私有目录即可。

- **qemu-aarch64**：软件包 `qemu-user`（Ubuntu 26.04 起 `qemu-user-static` 已并入它）。
  Go 的产物是静态链接的，因此 `qemu-aarch64 ./linux-arm64-client` 直接可跑，不需要 sysroot。
- **Wine 10.0**：软件包 `wine64`、`libwine`、`wine-common`、`wine64-preloader`、
  `libz-mingw-w64`（`libwine` 依赖它提供 `zlib1.dll`，缺了它 user32 加载失败）。
  `wineserver` 会从编译期路径 `/usr/share/wine/nls` 读区域数据，因此启动时在私有挂载命名
  空间里（`unshare -rm`）把解出来的 `share/wine` 绑到该位置。Windows 版读配置文件时用
  `Z:\...` 路径，且 TOML 里要用**字面量单引号**（双引号字符串里的 `\h` 是非法转义）。
- **`go test -race`**：本机没有 C 编译器时只能靠 CI。现在可以按需解出 `gcc-14` 与
  `libc6-dev`（同样不需要 root），设置 `CC` 与 `CGO_ENABLED=1` 后本地即可跑 `-race`。

### 3.1 一次没能跑起来的 Wine

2026-09-23 这一轮里，Wine 没能启动：`unshare -rm` 在建好用户命名空间后写
`/proc/self/uid_map` 被拒（`不允许的操作`），Python 里手工走到同一步也是同样的错误，因此
`wine-inner.sh` 那个绑定挂载做不了，`wineserver` 随即报 `failed to load l_intl.nls` 并退出。
这是**这台机器当次会话的限制**，不是 Wine 或本项目的缺陷：同一个封装在本文件此前记录的轮次里
跑通过（66/66）。缺这一次的代价写在结论表里——`windows/amd64` 一栏仍是上一轮的数字，本轮新增的
两项检查没有在 Windows 上执行过。它们读的是客户端进程写的日志，内容与平台无关，但"与平台无关"
不是"跑过"。

## 4. 这套环境证明了什么、没证明什么

- 证明了：这三个目标上，八种代理类型与访问者路径**真的能传数据**，四层安全（加密含后量子、
  TLS、伪装、身份认证）能同时生效，面板与指标报的是真实计数，审计真的写下了对应事件。
- 没证明：模拟层自身的行为差异。qemu 与 Wine 都可能掩盖或引入只在真机上出现的问题（例如
  Wine 的套接字实现、Windows 的防火墙与命名管道语义、macOS 的沙箱与公证）。因此
  windows/amd64 仍以 CI 的 `windows-latest` 作业为准（它在真实 Windows 上跑 101 项），
  arm64 目前只有本文件记录的模拟执行（CI 里的 arm64 执行作业尚未落地）。
- 三层隧道（`[vpn]`）**只在 Linux 上存在**，只在 CI 的 `ubuntu-latest` 作业里对着真实 tun
  设备验证；其他平台启动 `vpn.enabled = true` 会明确报错退出，这一点由单测覆盖。

## 5. 这套测试发现过什么

按平台跑同一套检查，能发现只在某个平台上成立的东西——这是单一平台测试做不到的：

- **Windows 上私钥文件没有 0600**：客户端 `--identity` 生成的密钥文件在 Linux 上是 `0600`，
  在 Wine 下是 `0664`（组与其他用户可读）。Windows 没有 POSIX 权限位，Go 的 `0600` 不会设置
  ACL，实际保护来自目录继承的 ACL；在 Wine 这类把 Windows 路径映射到 Unix 文件系统的环境里，
  这个差异就变成真实的暴露。文档原先把"权限 0600"写成无条件的，现在写明平台差异
  （`docs/SECURITY.md` 第 9.1 节）。
- **Windows 上 TOML 路径必须用字面量字符串**：配置文件里 `key_file = "Z:\...\k.key"` 会被
  TOML 解析器以 `invalid escape in string` 拒绝，必须写成 `key_file = 'Z:\...\k.key'`。
  这是配置写法问题，不是程序缺陷，但只有真在 Windows 上跑才会遇到。
- **Windows 作业脚本里 `envFrom` 式的环境变量注入**：见仓库的 Kubernetes 清单检查。
