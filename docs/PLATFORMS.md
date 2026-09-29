# 平台支持与验证

本文件只讲事实：六个发布目标各自被**什么**执行过、证明了**什么**、以及**什么没有被证明**。
"能编译"与"能运行"是两件事，这里分开写。

## 1. 结论表

| 发布目标 | 本机（Linux 工作站） | CI | 功能检查 |
|---|---|---|---|
| linux/amd64 | 原生执行（`-race` 全套与功能检查见 3.9） | `ubuntu-latest`：gofmt、vet、单测、`-race`、真实 tun 设备的三层隧道、构建、示例配置、版本、`scripts/functional-linux.sh`（有浏览器时 281 项） | **70/70**（另有仓库内可复跑的 281 项，见下） |
| linux/arm64 | qemu-aarch64（arm64 指令集）：单测 14 包 + 仓库内 281 项，跨架构两对亦为 281/281（见 3.12） | 交叉编译 + `test-arm64` 作业（`ubuntu-24.04-arm`，真机 arm64）：vet、单测、构建、示例配置、版本、`scripts/functional-linux.sh`（有浏览器时 281 项） | **70/70**（本机 qemu；仓库内那 281 项在 qemu 上同样 281/281） |
| windows/amd64 | Wine 10.0 执行真实 PE（**71/71**，见 3.20；装法见第 3 节） | `windows-latest`：vet、单测、构建、示例配置、版本、`scripts/smoke-test.ps1`（101 项） | `scripts/wine-check.sh` **71/71**（本轮，九对组合各一遍）；更早的 66/66 来自一个不在仓库里的脚本 |
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

3.20 之后 `windows/amd64` 的混合组合又能跑了，但用的是**仓库里另一套检查**（`scripts/wine-check.sh`，
71 项）。上面这张表里的**所有九对组合**都用那套重跑过一遍，各 **71/71**——包括 `windows/amd64`
参与的四对，以及本机 qemu 下的 arm64 那几对。**两套数字不要互换**：上面这张表的 25/6 项是那份不在
仓库里的脚本跑出来的，3.20 的 71 项是另一套，项数与检查内容都不同；这张表保留原样是因为它记的是
当时的运行，而 3.20 是同一批组合在新的、可复跑的一套检查下的结果。

## 3. 模拟执行是怎么搭起来的

两者都不需要 root：把 Ubuntu 的 `.deb` 用 `dpkg-deb -x` 解到私有目录即可。

- **qemu-aarch64**：软件包 `qemu-user`（Ubuntu 26.04 起 `qemu-user-static` 已并入它）。
  Go 的产物是静态链接的，因此 `qemu-aarch64 ./linux-arm64-client` 直接可跑，不需要 sysroot。
  仓库里的 `scripts/emulate-linux-arm64.sh` 把这条路径固定下来：它交叉编译三个 arm64 产物、
  为每个产物写一个 `exec qemu-aarch64 ... "$@"` 的包装脚本，然后先跑单测
  （`go test -exec <包装脚本>`），再把这三个包装脚本交给 `scripts/functional-linux.sh`，
  于是套件本身并不知道自己在跑 arm64。用法是
  `bash scripts/emulate-linux-arm64.sh /usr/bin/qemu-aarch64-static [sysroot]`（sysroot 可省略，
  静态产物不需要它）。`go test -exec` 的包装脚本必须是**透传**的：Go 会把自己构造的测试
  二进制路径追加到 `-exec` 命令之后，若那里写的是一个具体产物的启动器，那个产物就会把测试
  二进制当成参数收下（实测表现为"解析配置时遇到控制字符 0x7f"，即 ELF 的魔数）。
- **Wine 10.0**：软件包 `wine64`、`libwine`、`wine-common`、`wine64-preloader`、
  `libz-mingw-w64`（`libwine` 依赖它提供 `zlib1.dll`，缺了它 user32 加载失败）。
  `wineserver` 会从编译期路径 `/usr/share/wine/nls` 读区域数据，因此启动时在私有挂载命名
  空间里（`unshare -rm`）把解出来的 `share/wine` 绑到该位置。Windows 版读配置文件时用
  `Z:\...` 路径，且 TOML 里要用**字面量单引号**（双引号字符串里的 `\h` 是非法转义）。
- **`go test -race`**：本机没有 C 编译器，以前只能靠 CI。现在 `scripts/race-toolchain.sh` 把需要的
  包用 `apt-get download` 收下来、`dpkg-deb -x` 解进一个缓存目录（**不需要 root**，也不往系统里
  装任何东西），写一个包装脚本指向解出来的 `cc1`/`as`/`ld`/头文件，先自己编译一个测试程序确认能用，
  再把包装的路径打在 stdout 上：

  ```bash
  CC="$(bash scripts/race-toolchain.sh)" CGO_ENABLED=1 go test -race ./...
  ```

  `make test-race` 在没有给 `CC` 时会自己调用它。注意**只解 `gcc-14` 是不够的**：那个包里只有
  驱动程序，真正的编译器 `cc1` 在 `gcc-14-x86-64-linux-gnu` 里，而驱动会去 `/usr/libexec` 找它，
  所以包装脚本按解出来的位置传 `-B`，并给头文件与启动对象传 `-isystem`/`-L`；动态链接器仍指向系统
  那一份，跑出来的测试二进制照常执行。首次解包约 43 MB、约 6 秒，之后复用缓存。
  本轮的运行记录见 3.16。
- **容器不能替代这两条路**：`registry-1.docker.io` 不可达，镜像拉不下来；arm64 的容器要在
  amd64 主机上跑还得先注册 `binfmt_misc`，而那需要 root。3.14 记录了后来发生的事：Docker
  本身已经装上（apt），daemon 也起来了，挡路的只剩镜像仓库。

### 3.1 一次没能跑起来的 Wine

2026-09-23 这一轮里，Wine 没能启动：`unshare -rm` 在建好用户命名空间后写
`/proc/self/uid_map` 被拒（`不允许的操作`），Python 里手工走到同一步也是同样的错误，因此
`wine-inner.sh` 那个绑定挂载做不了，`wineserver` 随即报 `failed to load l_intl.nls` 并退出。
这是**这台机器当次会话的限制**，不是 Wine 或本项目的缺陷：同一个封装在本文件此前记录的轮次里
跑通过（66/66）。缺这一次的代价写在结论表里——`windows/amd64` 一栏仍是上一轮的数字，本轮新增的
两项检查没有在 Windows 上执行过。它们读的是客户端进程写的日志，内容与平台无关，但"与平台无关"
不是"跑过"。

（3.20 之后这一条不再阻塞：拿到 root 后，同一个封装可以在 root 建的挂载命名空间里运行，或者把
`/usr/share/wine` 指向解包目录；`scripts/wine-check.sh` 就是在这之上把 Windows 那一栏重新跑起来的，
包括上面那两个"读客户端日志"的检查。）

## 3.9 补跑出来的证据（当时套件为 38 项，见 3.10 的后续）

上面第 3 节的两条本地执行路径这一轮都真的跑了一遍，命令与结果如下（同一台 amd64 Linux
工作站，没有 arm64 硬件）：

| 跑的东西 | 命令要点 | 结果 |
|---|---|---|
| 整套单测 + `-race` | `CC=<解出的 gcc-14> CGO_ENABLED=1 go test ./... -race` | 14 个包全过（`pkg/server` 30.9s） |
| arm64 单测 | `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -exec <透传包装脚本> ./...` | 14 个包全过 |
| arm64 功能检查 | `bash scripts/emulate-linux-arm64.sh <qemu-aarch64> [sysroot]` | **38/38**，两处都过（带 sysroot 与不带；当时套件是 38 项，见 3.10） |
| 跨架构对 1 | `functional-linux.sh <amd64 服务端> <arm64 客户端包装>` | **38/38**（同上，当时 38 项） |
| 跨架构对 2 | `functional-linux.sh <arm64 服务端包装> <amd64 客户端>` | **38/38**（同上） |

跨架构这两组正是第 2.1 节那张表里 `linux/amd64 ↔ linux/arm64` 两行，这次用仓库内的套件重跑
了一遍（第 2.1 节的数字来自作者那套不在仓库里的脚本，项数也不同：25 项）。

Windows 仍然是空的，原因与 3.1 节记录的是同一个：这一轮 `unshare -rm` 依旧失败，而失败的具体
原因这次定位到了——`kernel.apparmor_restrict_unprivileged_userns = 1`（Ubuntu 24.04 起的默认
值）让非特权用户命名空间不可用，尽管 `kernel.unprivileged_userns_clone = 1`、
`user.max_user_namespaces = 124095` 且 `/etc/subuid`、`/etc/subgid` 都有 `hp:100000:65536`。
Wine 的封装要在私有挂载命名空间里做绑定挂载，因此起不来；同一原因也排除了 rootless 容器。
所以 `scripts/smoke-test.ps1` 这一轮仍然没有被本机执行过，Windows 的数字仍是上一轮的。

## 3.10 代理池与负载均衡也进了仓库内的套件

3.9 之后又补了一组：**两个客户端发布同名同 `group` 的代理**（第二名成员刻意请求另一个
`remote_port`），7 项检查——池报出两名成员、池的端点仍是第一名成员的端口、后到成员请求的那个
端口**没有被监听**、六次连接后**两名成员都真的服务过**、杀掉一名成员后池报出只剩一名、
剩下的成员继续服务、移除进了审计日志。套件因此从 38 项变成 45 项（后来陆续加上 3.11、3.12 记录的
几组，现在有浏览器时是 281 项、没有时 138 项）；同一轮还用变异测试
确认这些检查有鉴别力：把第二名的 `group` 去掉，恰好失败 3 项（池只剩一名成员、只有一名成员
服务过、审计里没有 `proxy_removed`）；把 `pickExcluding` 的 round-robin 分支改成永远返回
`candidates[0]`，恰好失败 1 项（"两名成员都服务过"变成 1）。

同一轮还加了一处**可诊断性**的改动：套件在启动服务端之前会逐个确认自己要用的端口还能绑定，
不能就打印"某个进程或上一次被中断的运行仍占着它"并以退出码 2 结束。起因是一次真实的踩坑——
把套件输出接到 `| head` 时管道提前关闭，脚本没走到自己的清理，残留进程占着端口，下一次运行
于是级联失败 29 项，看起来像产品坏了。现在这种情况只报一行。

## 3.11 一个池里的成员服务下线时，各策略浪费多少次尝试、它恢复后又被用了几次

按 3.10 的方式搭起"一名成员可用、另一名的本地服务无人监听"的池，每种策略跑 **30 次访问**（池的
端点从面板读回，不假定是哪名成员请求的端口），读的是失败成员自己的 `consecutive_failures`——
一次失败的尝试就加一；随后在那个端口上起一个会应答的服务，再跑 **30 次**，看两次分别由哪名成员
应答。同一台机器、同一份二进制：

| 策略 | 浪费的尝试（成员服务下线时） | 服务恢复后该成员的应答数 |
|---|---|---|
| `round-robin` | 15 | 15 / 30 |
| `bandit` | 4 | 12–18 / 30 |
| `latency` | 2 | 1 / 30（那次探测） |
| `adaptive` | 2 | 1 / 30（那次探测） |
| `failover` | 0 或约 30 | 0 或 30，取决于哪名成员先注册 |

`round-robin` 的 15 次正是第 1 节记的"30 次访问里浪费 15 次"，`bandit` 的 4 次也与那里一致。
`failover` 只认定第一个成员（"first healthy only"），所以它的数字完全取决于注册顺序：第一个是
可用成员就一次都不浪费，第一个是死成员就每次都浪费一次；`round-robin`/`random` 按轮转把一半
尝试分给死成员，`bandit` 学得快。

问题出在 `latency` 与 `adaptive`：`dialLatency` 只在成功开流时写入，本地服务下线的成员永远是 0，
而这两个策略都把"未测量"当成"最便宜"，于是永远挑那个永远不会应答的成员——实测浪费 **31 / 30**
与 **30 / 30**，比 `round-robin` 还差。修复分两步，两步都有实测与回归测试：

1. **已经失败且从未应答的成员排在所有被测量过的成员之后**（`neverAnswered`）。浪费从 31/30 降到
   **2 / 30**（首次访问时谁都没有测量值，加上每 20 次选一次的探测）。
2. **但它不能被永久埋掉**：只做第 1 步时，一个服务恢复过来的成员在 30 次访问里被用了 **0** 次，
   这与 `cost()` 上那句"惩罚有上限，否则一个长时间故障的成员永远回不来"的注释相矛盾。因此这两个
   策略每 20 次选择用一次探测（`neverAnsweredProbeEvery`，用独立的计数器，免得与策略自己的轮转
   计数器互相干扰）去问那个成员一次，于是它恢复后能被重新纳入（表里那 1 / 30）。

第 2 步的探测有独立的单元测试（40 次选择里那个成员恰好 2 次），套件里则有一组端到端：把探测关掉
会让它报 "it served none of 25 visits"。

**这一节要在机器空着的时候跑。** 它给第二名成员 30 秒完成注册（`member_count` 轮询到 2 为止）。
把它与一个 8 线程的 qemu 模拟放在同一台机器上同时跑时，实测 30 秒内只等到 1 名成员，于是这一节
的两项一起失败（`the latency pool reports both members -> want '2' got '1'` 与
`the member whose service came back is used again -> it served none of 25 visits`——后者是前者的
后果：第二名成员根本不在池里）。同一套件单独跑时 138/138 与 281/281 都过，连着三次都一样。
CI 上每个作业独占一台运行器，不会撞到这件事；本机上同时跑跨架构与 arm64 两遍时就会。

## 3.12 套件长到 281 项之后，arm64 与跨架构这两条在 qemu 上重跑了一遍

3.9 那张表里的 38/38 是套件只有 38 项时测的。套件此后长到 281 项（有浏览器时；没有浏览器时
138 项，其中最近几轮新增的是账本一节 23 项、去中心化目录与它的通报生命周期 14 项，以及面板检查
里的账本、目录、字典与窄屏几组共 66 项），所以同一台 amd64 工作站上把 3.9 的三条路径按现在的
项数重跑了一次，全部走仓库内的脚本，没有一处是靠"与平台无关"推断的：

| 跑的东西 | 命令 | 结果 |
|---|---|---|
| arm64 单测 | `bash scripts/emulate-linux-arm64.sh <qemu-aarch64> <sysroot>` 里的 `go test -exec` 一段 | 14 个包全过（`pkg/server` 33.4s） |
| arm64 功能检查 | 同一个脚本的后一段 | **281/281**（含真实浏览器那一节的 143 项），`ARM_EXIT=0` |
| 跨架构对 1 | `functional-linux.sh <amd64 服务端> <arm64 客户端包装> <arm64 助手包装>` | **281/281**，0 失败 |
| 跨架构对 2 | `functional-linux.sh <arm64 服务端包装> <amd64 客户端> <amd64 助手>` | **281/281**，0 失败 |
| amd64 原生 | `gofmt -l .`、`go vet ./...`、`go test ./... -count=1`、`functional-linux.sh <amd64 三个产物>` | gofmt 与 vet 无输出；14 个包全过；**281/281** |

值得单独记一句的是 arm64 那一遍里套件的最后一节：浏览器是宿主机的 Chrome，服务端是被测的
qemu 进程，74 项检查全过——也就是说"这一页在真实浏览器里能跑出正确数字"这件事，在服务端跑着
模拟指令集时同样成立（页面本身跑在宿主机的浏览器里，跨架构的是它读的 API）。

## 3.13 三层隧道与它的面板状态（本机第一次，需要 root）

`[vpn]` 的隧道此前只在 CI 的 `ubuntu-latest` 里对着真实 tun 设备跑过。这一轮用一次
`pkexec` 拿到 root 后，本机也跑了一遍，并把**面板上那个一直只由 CI 覆盖的状态**在真实浏览器
里核对了一次。前提本机早已具备：`/dev/net/tun` 存在、`ip` 与 `ping` 在位。

| 跑的东西 | 命令 | 结果 |
|---|---|---|
| 三层隧道套件 | `pkexec bash <wrapper>` → `scripts/vpn-linux-test.sh <server> <client>` | **23/23 PASS，0 FAIL** |
| 隧道面板（浏览器） | 自建一条活隧道后 `panel-checks.py --url http://127.0.0.1:<dashboard>` | **11 项全过**（面板总计 94→143 项的那一段） |

套件那 23 项里的关键行：两个命名空间由 veth 相连；服务端在真实 tun 设备上开出 `at0`
（192.168.99.0/24，MTU 1400）；客户端在第二个命名空间里配好 `at1` 并被分到 192.168.99.2；
**客户端 ping 服务端 3/3、服务端 ping 客户端 3/3**，两侧接口的收发包计数都动了
（`at0 rx 6 tx 14`、`at1 rx 6 tx 12`）；`GET /api/vpn`、`GET /api/config` 的 `[vpn]` 段与审计
日志里的地址分配一致；客户端退出后地址归还池子；`vpn.require = true` 时一个不带隧道请求的
会话被拒并留下记录。

隧道面板那 11 项：自建部署（两个命名空间 + veth + 真实 tun 设备 `atp0`/`atp1`，ping 5/5）后，
用 Chrome 打开控制台**配置**页，核对接口名、服务端隧道地址、子网、MTU、地址池已用/总数、
对端数、从接口读到的包与送入接口的包（**各自都落在两次 API 读数之间的窗口里**，因为流量还在
走）、丢失包等于不可路由加丢弃、`/api/vpn` 与 `/api/config` 描述同一条隧道，并记录当时真的
搬过包（`28 from the interface, 5 to it, 1 peer`）。

**这一轮在这条路径上发现并修掉了一个缺陷**：配置页的**数值**不跟随语言切换。标签是
`data-i18n`，切换时会被重画，但「加密已启用 / 需要令牌 / 隧道状态」这几个是渲染时用 `t()`
取过一次的 `yes`/`no`，切到中文后仍显示英文。用活隧道那次运行留下的证据最直接：修好前
`the configuration values follow the language switch -> want ['否','是','是'] got ['no','yes','yes']`，
重新编译同一个面板后同一项通过。修法与前几轮一致：把最后一份配置响应留在状态里，切换语言时
连同其它视图一起重画。

（3.20 同一轮用**当前代码**把这套套件重跑了一遍：`scripts/vpn-linux-test.sh` 在两个命名空间里
**23/23、0 失败**——本节写的每一条都在现在的二进制上重新成立：两端的 ping、两侧接口的收发包
计数、`/api/vpn` 与 `/api/config` 与审计的一致、客户端离开后地址归还池子、以及 `vpn.require`
拒绝一个不请求隧道的会话。）

同一轮还修掉了检查器自己的两处脆弱：在**没有任何已注册代理**的部署上，卡片版式与桌面版式
那几项会因为拿不到行而失败（现在打印 `SKIP`，只跳过与行有关的断言），桌面那一段也不再对
`null` 调用 `getComputedStyle`。

## 3.14 容器：装上了 Docker，但 Docker Hub 拉不下来

这一轮按项目的说明装了 Docker（`apt-get install docker.io`，29.1.3；daemon 正常，`docker info`
可用），随后立刻撞上这台机器真正的限制：

```
Step 1/20 : FROM golang:1.24 AS builder
failed to resolve reference "docker.io/library/golang:1.24": ... dial tcp 43.226.16.8:443: i/o timeout
```

`registry-1.docker.io`、`auth.docker.io`、`production.cloudflare.docker.com` 三者的 443 都是超时，
而 `archive.ubuntu.com` 与 `github.com` 可达（所以 apt 与代码下载没问题，被挡住的只有镜像仓库）。
**结论：本机装得上 Docker，但 `Dockerfile` 的两个基础镜像拉不下来**，因此那个镜像本身在这里
仍然只能由 CI 验证——`.github/workflows/ci.yml` 会 `docker build` 并把它跑起来，检查
`/healthz`（第 189 与 207 行）。

不过这次 root 没有白拿，容器路径上真正需要验证的两件事都补上了：

- **ConfigMap 加环境变量的凭证契约**进了套件（8 项，不需要 root 也不需要 Docker）。它把
  `deploy/kubernetes/configmap.yaml` 里的 `server.toml` 原样取出来：这份文件**单独校验不通过**
  （`server.auth_token is required`），而带上 `AETHERTUNNEL_AUTH_TOKEN` 与
  `AETHERTUNNEL_DASHBOARD_TOKEN` 后校验通过、服务端起来、面板对不带令牌的请求回 401、带令牌
  回 200 且 `auth_required` 为真、一个**启用了同样伪装**的客户端能通过它搬运字节、审计日志写在
  ConfigMap 指的那条路径上，而一个**没启用伪装**的客户端会被拒（服务端日志写着
  `not carrying record-framed data`）——也就是说 ConfigMap 打开的 `[obfuscation] disguise =
  "tls-record"` 不是装饰。这一节第一次跑起来时就纠正了我自己两处写错的断言（`--check` 必须带上
  环境变量才有意义；测试客户端必须与服务端用同一种伪装）。
- **容器运行时**用一个本地 `FROM scratch` 镜像验证（不需要仓库）：同一个静态二进制、同样的
  `USER 65532:65532`、同一份 ConfigMap 配置，端口与状态卷按清单发布与挂载。实测：
  `docker inspect` 报 `User=65532:65532`；`/healthz` 200；`/api/status` 不带令牌 401、带环境变量
  里的令牌 200；`/metrics` 也要求令牌；一个客户端在 18007 端口发布的隧道**从宿主机穿过容器**
  搬运了 21 字节；容器内以 uid 65532 写出的 `audit.jsonl`、`ledger.jsonl`、`ledger.key`、
  `dht.key` 都出现在挂载的状态卷里（密钥文件是 0600）。**这不是那个 Dockerfile 本身的验证**
  ——它的基础镜像是 distroless，本地拉不到——而是它所依赖的运行时契约的验证。
- 顺带加了两条**单元测试**，把镜像与清单之间那两处只能靠约定维持的一致性钉住：`Dockerfile` 的
  `CMD ["--config", …]` 路径必须等于 Deployment 的配置挂载点加上 ConfigMap 的键（端口也要在
  `Dockerfile`、Deployment、Service 三处一致），以及 ConfigMap 里每一条 `path`/
  `signing_key_file` 都必须落在 Deployment 的 state 挂载点之下。

**Wine 与 PowerShell 仍然没有装。** 这台机器上两者都不存在，而网络现在确实可达（`github.com`
与 `archive.ubuntu.com` 都通），所以它们**可以**装——但那是一次比"装上项目说明里点名的 Docker"
大得多的系统改动，作者那套 Wine 封装也不在仓库里，因此 Windows 那 101 项仍然只由 CI 的
`windows-latest` 覆盖。这一点写在这里是为了让下一个人知道：挡路的不是网络，而是一个决定。

## 3.15 Kubernetes：清单第一次真的被 apply 到一个集群上

在此之前，`deploy/kubernetes/` 只在 CI 里被 `kubectl kustomize` 渲染过（`.github/workflows/ci.yml`
第 180 行），镜像也只在 CI 里被 `docker build` 过。渲染出来的 YAML 不会告诉你 Service 有没有把
pod 的端口发布出去、探针会不会通过、状态卷能不能写。这一轮起了一个**单节点 k3s**
（v1.37.0+k3s1，Kubernetes v1.37.0），把清单原样 apply 上去，再从集群外面驱动它。

集群是空转的：k3s 二进制从 Rancher 的镜像站取（`rancher-mirror.rancher.cn`，sha256 与官方
`sha256sum-amd64.txt` 里的 `39eed8f5…c7a2` 逐字节一致），数据目录放在脚本自己的工作目录里，
启动时 `--disable coredns,local-storage,metrics-server,servicelb,traefik`，所以除 pause 之外不需要
任何镜像。Docker Hub 仍然拉不动（见 3.14），而清单里的镜像是
`ghcr.io/aethertunnel/aethertunnel:v3.7.4`——于是把仓库里的两个二进制按 `Dockerfile` 的运行时契约
（同一路径的静态二进制、uid 65532、同样的工作目录、入口与命令）装进一个 `FROM scratch` 镜像，
pause 也用一个只做阻塞的小程序当替身。**这不是那个 Dockerfile 的验证**（它的基础镜像是
distroless，见 3.14），而是**清单的验证**：apply 上去的 Kubernetes 对象就是仓库里的那几个。

`scripts/kubernetes-linux.sh`（14 项，本机一次 72–75 秒）跑到的：

- `deploy/kubernetes` apply 成功，Deployment 自己声明的那对探针真的通过——`/readyz` 是 kubelet 在打；
- 从集群外（`kubectl port-forward` 进 pod）看：`/healthz`、`/readyz` 都是 200；`/api/status`
  不带令牌 401、带 **Secret 里的**面板令牌 200 且 `auth_required` 为真（也就是说
  `AETHERTUNNEL_AUTH_TOKEN` 真的进了程序）；`/metrics` 不带令牌 401；
- 一个客户端用 **Secret 里的**共享令牌连上控制端口、把本地回环服务发布到 18080，
  访问者的字节从集群外穿过隧道到那个本地服务再原路回来（走 `kubectl port-forward` 进 pod
  的 18080：Service 只转发 service.yaml 里列出的端口，而代理的端口是客户端自己挑的——这一条现在
  写在 `service.yaml` 与 ConfigMap 的注释里）；
- 面板把这条代理列出来（名字与端口都对）；
- 会话结束后**带宽账本**里出现条目——容器自己的根文件系统是 `readOnlyRootFilesystem: true`，
  这个条目读得回来，说明服务端确实写进了 Deployment 挂的那个 state 卷；
- **目录（DHT）**：集群内一个 pod 用 **Service 的 ClusterIP** 去 bootstrap 并解析出一个名字，
  集群外一个进程用 **Service 的 NodePort** 做同样的事。

**这一轮抓到两个只有真跑才会出现的问题：**

1. **`--dht-lookup` 把查询节点绑在回环地址上，跨主机查询永远收不到回答。** `LookupProxy`
   写死 `settings.ListenAddr = "127.0.0.1:0"`（那里的注释只说"别占用配置里那个端口"，本意是端口，
   主机被顺带写成了回环）。Kademlia 的对端把回答发给**请求的源地址**，源地址是 `127.0.0.1` 时
   回答被送回对端自己的回环地址：同主机查询照常通过（套件里全在 127.0.0.1 上，所以一直没暴露），
   跨主机一定超时。实测：部署在集群里的服务端，另一个 pod 里的容器 bootstrap
   `10.42.0.23:7003` 得到 `dht: request timed out`；把这一行改成 `0.0.0.0:0` 后同一个查询解析出
   `kubernetes-probe -> aethertunnel.aethertunnel.svc.cluster.local:18080`。单测
   `TestLookupProxyQueriesFromAnAddressTheAnswerCanReach` 用一个只记录源地址、从不回答的裸
   socket 把它钉住：改回回环绑定会失败并打印
   `the request came from 127.0.0.1:47794, an address only a node on this machine answers`。
2. **Service 没有发布 DHT 的 UDP 端口，而 ConfigMap 指向的正是它。** Deployment 声明了
   `containerPort: 7001/UDP`（DHT），ConfigMap 让 DHT 听在 7001，`advertise_host` 写的是 Service
   的集群内域名——而 Service 只发布了 7001/TCP、7002/UDP、7500/TCP。结果是集群内另一个 pod 用
   ClusterIP 去 bootstrap 会失败（实测 `dht: key not found`），集群外的客户端更无从加入。
   **补这个端口时又撞上更糟的一件事**：Service 的端口列表按 `port`（数字）合并，而控制端口已经是
   7001——同一个 Service 里两个 7001 会让 `kubectl apply` 的三方合并把它们当成同一个条目。实测：
   先 apply 旧清单（7001/TCP、7002/UDP、7500/TCP），再 apply 带 `dht 7001/UDP` 的新清单，
   `kubectl apply` 报 `service/aethertunnel configured`，**控制端口却从活对象里消失了**（端口列表
   只剩 rendezvous 与 dashboard），再 apply 一次也修不回来——`kubectl apply` 不会重新添加它认为
   "两边都没变"的条目。因此清单改成给 DHT 一个自己的号（**7003/udp**，ConfigMap、Deployment、
   Service 与 `Dockerfile` 的 `EXPOSE` 四处一致），并加了单测
   `TestTheServiceGivesEveryPortItsOwnNumber`：同一个 Service 里任何两个端口的数字重复、
   或 ConfigMap 绑了一个 Service 没有发布的端口，都直接失败。修好之后的升级路径也实测过：
   先 apply 旧清单再 apply 新清单，四个端口都在，控制端口不再丢。
- **顺带把自己弄坏的一次**：把 DHT 挪到 7003 之后，套件里 ConfigMap 那一节的端口改写规则仍然
  只匹配 `0.0.0.0:7001`，于是被抽查的服务端会直接去绑清单里写的 7003。单独跑一次看不出来
  （7003 恰好空着），两套套件同时跑就撞成 `listen udp 0.0.0.0:7003: bind: address already in
  use`——重跑跨架构那两对时，arm64 服务端那一对因此 5 项失败。规则改成按 ConfigMap 里实际的
  端口号匹配之后，两对同时跑各 **281/281**。
- 顺带改掉一处写错的注释：ConfigMap 里 `ban_ignore_cidrs = ["127.0.0.1/32"]` 旁边原先写着
  "never ban the kubelet's probe address"，但封禁列表只由**控制端口**的握手失败喂饱
  （`recordAuthFailure` 只在控制端口的处理函数里被调用），kubelet 的探针打在面板的 HTTP 端口上、
  走的是面板自己的令牌比对，根本不进这个列表——那句话把读者引向一个不存在的风险，而真正被它
  保护的是"pod 内部连 `127.0.0.1:7001` 的客户端"。注释改成实测到的事实。

**这一轮没证明的：** 集群没开 CoreDNS（少一个镜像），所以 ConfigMap 里 `advertise_host` 用的那个
集群内域名没有被解析验证过——DHT 记录里那个地址字段就是它，它的含义与限制现在写在 ConfigMap 的
注释里；`type: LoadBalancer` 的 Service 在没有负载均衡实现时（这里把 servicelb 关了）
`EXTERNAL-IP` 会一直停在 `<pending>`，Service 仍然照常分配 NodePort 并被 kube-proxy 转发（上面
集群外那一项走的就是它）；集群里没有真的 tun 设备，`[vpn]` 那部分仍以 3.13 的 namespace 验证为准。
`scripts/kubernetes-linux.sh` 需要 root、可用的 docker daemon 与一个 k3s 二进制，三者任一缺失时它
打印缺什么并以 0 退出。

**跑完留下了什么，是量出来的。** 一开始只做到"杀掉 k3s 进程组、删掉工作目录"，看上去干净；把
`containerd-shim`、`ip netns`、veth、`/run/k3s`、`/var/lib/rancher/k3s` 一项项列出来之后才看到：
每跑一次会留下两个 pod 的网络命名空间、两个 veth、两个 shim 进程和 containerd 的运行时目录
（`/run/k3s` 里的 overlay 挂载还让目录删不掉，报 `设备或资源忙`）——**k3s 被杀掉之后 pod 的
sandbox 会活下来**。现在的做法是先让集群自己删掉 pod（containerd 顺手把 sandbox、命名空间与挂载
一起收走），再按"这次跑之前有哪些"的差集清掉残留（sandbox 进程、shim、命名空间、veth），
最后卸载 `/run/k3s` 下的挂载再删目录。**第一版这么做之后仍有一个进程活下来**：容器自己的进程
（`/usr/local/bin/aethertunnel-server`）不是 sandbox，也不叫 `pause`，把 shim 杀掉之后它连着自己的
网络命名空间一起留着。它所在的位置是判据——`/proc/<pid>/cgroup` 写着
`/kubepods/burstable/pod<uid>/<容器 id>`，而机器上只可能有这一个集群（脚本在已经有 k3s 在跑时会
直接退出），所以在杀掉集群之后把所有落在 `kubepods` 里的进程一并收掉。实测跑完之后：容器进程 0、
sandbox 0、shim 0、命名空间 0、veth 0、`/run/k3s` 与 `/var/lib/rancher/k3s` 都不在、docker daemon
照常。k3s 自己解包出来的运行时（默认在 `/var/lib/rancher/k3s`）只在这次跑之前不存在时才删——
机器上本来就装了 k3s 的话，那是它的缓存，下次还要用。

（3.20 同一轮用**当前代码**把这套 14 项重跑了一遍：**14/14、0 失败**——探针、两种令牌、
环境变量里的凭据、客户端穿过隧道、面板列出代理、DHT 从集群内外各解析一次、带宽账本落在状态卷
里，全部照旧。收尾也照旧：跑完之后系统 docker 的容器数与镜像数都是 0，k3s 已退出。镜像仍然不是
`Dockerfile` 那个（基础镜像拉不到，见 3.14），替换的依据写在脚本的注记里。）

## 3.16 `-race` 现在是一条可以复跑的本机路径（脚本化之后的第一轮）

3 节那条 `-race` 的做法此前只以散文形式写着（"解出 `gcc-14`、`libc6-dev` 等包"），照着做会卡在
`cc1` 上——`gcc-14` 包里只有驱动程序，`cc1` 在 `gcc-14-x86-64-linux-gnu` 里，而驱动按编译期路径去
`/usr/libexec` 找它。现在这段做法固定在 `scripts/race-toolchain.sh` 里（`.deb` 列表、包装脚本、
自检、缓存都在里面），`make test-race` 也会在没给 `CC` 时调用它。本轮用它跑的记录：

| 跑的东西 | 命令要点 | 结果 |
|---|---|---|
| 整套单测 + `-race` | `CC="$(bash scripts/race-toolchain.sh)" CGO_ENABLED=1 go test -race ./...` | 14 个包全过，**0 处数据竞争**（`pkg/server` 36.9s、`pkg/reliable` 14.9s） |
| 并发最重的几个包各跑三次 | 同上，`-count=3 ./pkg/server/ ./pkg/net/ ./pkg/reliable/ ./pkg/dht/ ./pkg/discovery/ ./client/ .` | 全过，**0 处数据竞争**（`pkg/server` 三次共 108.5s） |
| 解包耗时（空缓存） | `time bash scripts/race-toolchain.sh /tmp/racetest` | 6.4 秒（下载 42.8 MB + 解包 + 自检），之后复用缓存 |
| 交叉编译矩阵 | `bash scripts/build-release.sh test-version` | 退出码 0，`dist/` 里 13 个文件 = 12 个产物 + `SHA256SUMS`，与 README 的说法一致 |

这一轮**没有**跑的是 Windows 与 arm64：`unshare -rm` 仍然失败（`写失败：/proc/self/uid_map:
不允许的操作`，与 3.1 记录的是同一件事），而且这台机器上没有 `wine`、没有 `pwsh`、也没有
`qemu-aarch64-static`。它们的数字仍是 3.12 与 3.13 记录的上一轮结果。

3.20 那一轮之后又复跑了一次，用来盖住中间落进树里的改动（`pkg/net` 的绑定诊断与它接进的八处
绑定、配置的 CRLF/BOM 测试）：同一条命令，14 个包全过、**0 处数据竞争**（`pkg/server` 37.7s、
`pkg/reliable` 14.1s、新的 `pkg/net` 2.4s）。这次缓存是空的，所以它顺带把"空缓存首次解包"也
重新证明了一遍：下载 42.8 MB 用时 11 秒，解包加自检后直接可用。

## 3.17 三个 Docker 版本并排跑同一个容器场景（20.10.24 / 28.4.0 / 29.2.1）

3.14 那一轮只有一个运行时（系统装的 `docker.io` 29.1.3），"在容器里能跑"因此只在一个版本上被
证明过。这一轮把三个版本并排放到同一台机器上：从 `download.docker.com` 的 `plucky`、`oracular`、
`focal` 三个发行版池各取一套 `docker-ce` + `docker-ce-cli` + `containerd.io` + `runc`
（`containerd.io` 用自己的号段，2.2.1 配 29.2.1、1.7.27 配 28.4.0 与 20.10.24），`dpkg-deb -x`
解进三个目录（各 232 / 250 / 352 MB），再用**一次**提权把三个 daemon 起起来。每个 daemon 都有
自己的 `--data-root`、`--exec-root`、`--pidfile`、socket，`--iptables=false --ip6tables=false
--bridge=none`，并挂一个到点自杀的 `timeout`；脚本在前后各打印一次系统 `docker version` 与容器/
镜像计数（都是 0），所以系统那个 docker 与 3.15 记录的 k3s 状态都没有被碰过。

镜像仍然只能用 `FROM scratch`（Docker Hub 依旧不可达，见 3.14）：同一个静态二进制、同一个
`USER 65532:65532`。三个版本上跑的是同一个 21 项场景——镜像构建、`docker inspect` 报出的
`User`、`/healthz`、`/api/status` 的 401 与 200、`/metrics` 的序列、ledger 签名密钥、客户端连接、
**隧道真的搬了字节**、审计与 ledger 由 uid 65532 写出、无 panic，再加设备那一节：

| 版本 | containerd.io / runc | 场景 | 结果 |
|---|---|---|---|
| 20.10.24~3 | 1.7.27 / 1.2.5 | 控制面 + 数据面 + 审计 + ledger + 设备 | 21/21 |
| 28.4.0 | 1.7.27 / 1.2.5 | 同上 | 21/21 |
| 29.2.1 | 2.2.1 / 1.3.4 | 同上 | 21/21 |

**63/63 全过**：这套程序的容器路径在 20.10 到 29.2 之间没有版本相关的差异。设备那一节加了
`--device /dev/net/tun --cap-add NET_ADMIN`，并且把 uid 显出来——以 **uid 0** 跑时 `/api/vpn` 报
`enabled: true`、接口 `aetvpn0` 真的建出来、地址池给出 `10.77.0.1`；以 **uid 65532** 跑时同一个
命令只得到

```
vpn: TUNSETIFF for "aetvpn0": operation not permitted
```

三个版本都是这一行。这不是本机的偶然：内核在进程变成非 root 用户时清掉 permitted/effective
集合，而 docker 与 Kubernetes 都没有把加上的权限放进 ambient 集合，所以**"给容器加
`NET_ADMIN`"对非 root 进程不生效**。Deployment 里那段注释恰好就是这么写的（加 `NET_ADMIN`、
挂 `/dev/net/tun`），照着做会得到一个起不来的 Pod。这一条变成了本轮两个改动：程序把这句 errno
换成能照着做的说明，清单里那段注释改写成对已有键做四处修改（原来那个块整块取消注释会与本容器
已有的 `securityContext`、`volumeMounts` 重复，而"后者覆盖"会顺带丢掉 `readOnlyRootFilesystem`、
`allowPrivilegeEscalation: false` 和 state 挂载）。两条都写进了更新日志。

**没有跑的是 `Dockerfile` 本身**：它的基础镜像是 `golang:1.24` 与 distroless，都来自 Docker Hub，
这里仍然拉不到。三个版本验证的是它依赖的运行时契约（`USER`、命名空间、设备与权限、挂载卷的
归属），不是那个镜像。

**这一轮的三段脚本（解包三个版本、起三个 daemon、跑那 21 项）是临时的，没有进仓库**，所以要复跑
这一节得重写它们——第 4 节批评作者那套"脚本不在仓库里"的问题，这一节也一样。它需要
`download.docker.com` 可达（约 834 MB）与**一次提权**，而这台机器上第二个条件只有本轮才有；
写进仓库会得到一个默认跑不起来、并且要求 root 的脚本，所以这一节只记结果，不假装它可以一条
命令复跑。两个由它驱动的改动是仓库里的代码与测试（见更新日志）。

## 3.18 七个 Docker 版本、十个场景：功能路径跑进容器（第二轮）

3.17 只跑了三个版本，而且每个版本只验证控制端口一条路。这一轮把它扩到**七个版本**，并把场景从
21 项扩到 82 项——覆盖的是这套程序自己的功能，而不是"容器能不能起来"。

版本按发行版池各取一套，**每个版本配它自己打包里那个 containerd**（这正是版本矩阵要看的东西）：

| 版本 | 池 | containerd.io | runc |
|---|---|---|---|
| 19.03.15~3 | focal | 1.4.13 | 1.0.3 |
| 20.10.24~3 | focal | 1.7.27 | 1.2.5 |
| 24.0.9 | jammy | 1.6.33 | 1.1.12 |
| 26.1.4 | noble | 1.7.29 | 1.3.3 |
| 27.5.1 | oracular | 1.7.27 | 1.2.5 |
| 28.4.0 | oracular | 1.7.27 | 1.2.5 |
| 29.2.1 | plucky | 2.2.1 | 1.3.4 |

七个版本里 `dockerd`、`docker`、`containerd`、`runc` 四个二进制在这台机器上都能执行。**19.03.15 的
daemon 起初起不来**：它不认 `--ip6tables`，收到就退出（`unknown flag: --ip6tables`），而那条 flag
是 3.17 的启动脚本固定写上的。脚本改成先问 `dockerd --help` 再决定加不加，**这一处当时没有重跑**
（要一次提权），所以那一轮的 19.03 只记到"装上、二进制可用、daemon 待重跑"。后来拿到提权重跑，
daemon 正常起来并回答 `client=19.03.15 server=19.03.15`，但它**在这台机器上一个容器也起不来**：
每次 `docker run` 都以退出码 125 失败，device 那一节把原因报了出来——

```
docker: Error response from daemon: cgroups: cgroup mountpoint does not exist: unknown.
```

这台机器是纯 cgroup v2（`/sys/fs/cgroup` 是 cgroup2fs）。这句话起初归因于 runc，**后来用两次替换
把这个归因定位到了 daemon 自己**：把 20.10 那份 runc 1.2.5 放到 19.03 的 containerd 前面
（containerd 1.4 走 runtime v1，它按 PATH 找 `runc`），错误一模一样；再把 19.03 的 dockerd 嫁给
24.0.9 的 containerd 1.6.33 与同一个 runc 1.2.5，仍然一模一样。而 dockerd 自己的日志把这句话写在
它处理请求的地方：

```
level=error msg="Handler for POST /v1.40/containers/…/start returned error: cgroups: cgroup mountpoint does not exist: unknown"
level=warning msg="Your kernel does not support cgroup cpu shares"        （cpu/cfs/rt 等控制器各一行）
level=warning msg="Unable to find blkio cgroup in mounts"
```

发出拒绝的是 **dockerd 19.03 的 daemon 侧**：它在启动容器之前按 cgroup v1 的挂载点去找控制器，而
这个内核只提供统一层级；同一时刻 containerd 的日志里连一次 task 创建都没有。docker **从 20.10 起
才支持 cgroup v2**——这正是本矩阵里 20.10.24 能跑满 82 项、19.03.15 一项也跑不了的那条界线。所以
这一栏不是关于程序的结论，是**"在老 daemon 上跑容器"这件事本身的边界**：19.03 的控制面（API、
镜像构建、`docker server` 版本号）可用，容器运行不可用。

六个跑得起来容器的版本上，每个版本跑十个场景、**82 项**：

| 场景 | 项数 | 内容 |
|---|---|---|
| core | 16 | 镜像构建、`USER`、`/healthz`、`/api/status` 的 401/200（并核对这条 401 让面板那条计数 +1）、`/metrics`、ledger 密钥、客户端连接、隧道搬字节、审计与 ledger 的 uid 与 0600 权限、无 panic |
| device | 5 | `--device /dev/net/tun --cap-add NET_ADMIN`，`/api/vpn` 报出接口与地址池 |
| hardening | 10 | **清单里那套姿态**：`--read-only` 根文件系统（`docker inspect` 核对真的生效）、`--cap-drop ALL`、`--no-new-privileges`、`--memory 256m`、`--pids-limit 128`，两端都这样还能搬字节，审计与 ledger 落在挂载的卷里，日志里没有一句 `read-only file system` |
| private | 10 | 三个容器：服务端 + 发布方 + 访问方，**stcp/sudp/xtcp 各搬一次数据**（含 UDP 数据报），审计里三条 `visitor_accepted`，`/metrics` 三条 p2p 序列 |
| socks5 | 6 | 出口按 `allow_targets` 放行一个目标、拒绝另一个并回 0x02 |
| http | 7 | 共享监听按 Host 选隧道：配好的名字到达本地服务、未配置的名字 404 且到不了 |
| shipped | 12 | **按 `deploy/kubernetes/configmap.yaml` 那份 server.toml 跑**：DHT（含签名密钥、公告、`--dht-lookup` 那条发现路径）、`disguise = "tls-record"`、`load_balance = "adaptive"`、封禁规则；一个**空 `server_addr` + `discover`** 的客户端从 DHT 解析出地址并作为访客连上，一个**没有伪装**的客户端被拒 |
| tls | 5 | `[transport] enable_tls`：证书由 `openssl` 现场生成并只读挂进容器，客户端用 `ca_file` 信任它、搬字节；一个**不带 TLS** 的客户端被拒，并被记进 `handshake_failures`（上一轮那条修复的另一条路径） |
| post_quantum | 5 | `[encryption] enabled` + `post_quantum = true`（X25519 + ML-KEM-768）：两端口令一致时搬字节；口令不同的客户端被拒并同样记入 `handshake_failures` |
| vpn_client | 6 | 三层隧道的**另一半**：客户端容器自己开 tun 接口（`--user 0:0 --cap-add NET_ADMIN --device /dev/net/tun`），从服务端的地址池取到 `10.88.0.x` 并装到接口上，服务端 `/api/vpn` 报出 1 个 peer、池里 1 个地址被占用 |

**494/504**：六个版本各 **82 项，492/492 全过**；19.03.15 上十节里只有两项成立（`docker server`
的版本号与 `FROM scratch` 的镜像构建），其余十节全部因上面那条 cgroup 原因失败——**504 与 494
的差、以及那 10 项失败，来源只有一个**。六条值得单独写下来的结论：

- **清单要求的姿态是真的可用**。`--read-only` + `--cap-drop ALL` + 内存与 pid 上限下，服务端与
  客户端都正常搬数据，审计与 ledger 只写在挂载的卷里——这正是 Deployment 的
  `readOnlyRootFilesystem: true` / `capabilities.drop: ["ALL"]` 所要求的，之前只在"能起来"这一
  层被验证过。
- **DHT 那条发现路径（空 `server_addr` + `discover`）在容器里是通的**，而且它要求解析的名字是
  **私有代理**的名字：记录里写的端口是"服务这个代理的那个端口"，tcp 代理写的是它自己的公网口，
  私有代理写的才是控制端口。我第一次用 tcp 的名字去 discover，访问方于是连到隧道自己的数据口，
  读回来的 `echo:` 被当成首帧（`first byte is 0x65`）。这不是程序的问题，是断言写错了——`serverAddrFor`
  的注释早就写明这件事。
- **首帧读不出来时，拒绝此前不进指标也不进审计**，这是一处真缺口；两个计数在六个版本上都验证过。
- **面板与 `/metrics` 的令牌拒绝此前不留痕迹**：401 之外什么都没有——`auth_failures_total` 不动、
  审计不增长、服务端连一行日志都不写（实测：连发 5 个错令牌请求，两条计数都是 0、审计文件 0
  字节）。这个监听器没有任何限流，而 `service.yaml` 把 7500 发布在 LoadBalancer 上。这是第二条
  真缺口，改法与验证在更新日志里；`core` 那一项现在会核对这条计数确实 +1。
- **四层里的另外两层在容器里也不靠"能起来"证明**：TLS（`ca_file` 指向现场生成的证书）与
  后量子加密（口令一致时搬运数据、口令不同时被拒）。两条失败路径都落在 `handshake_failures`
  这条计数上——也就是说上一轮那处修复在伪装之外还覆盖了 TLS 与密钥协商两种不一致。
- **三层隧道的客户端那一半此前没有在容器里跑过**（3.13 那 23 项是本机实验，不是容器）。现在
  客户端容器自己开 tun、装地址、被服务端记为一个 peer。设备与权限的要求与 3.17 那条一致：
  非 root uid 拿不到 `NET_ADMIN`，所以两端都以 uid 0 跑。

**这一轮修掉的两处"基础设施"问题**（都不改程序，改的是这台机器上的做法），**两处都在拿到提权之后
重跑了**：

- 前几个版本一直**共用系统那个 containerd**：私有 daemon 没有传 `--containerd`，默认就是
  `/run/containerd/containerd.sock`。证据是系统 dockerd（pid 4308）的日志里对每个私有容器都写着
  `failed to process event … could not find container`（`module=libcontainerd namespace=moby`），
  而且这台机器上系统 docker 是 `containerd-snapshotter=true` 起的（`journalctl -u docker` 里那一行），
  它的镜像就是 containerd 里的镜像——所以系统 `docker images` 会**列出为这些测试构建的镜像**
  （3.17 那次脚本"前后各打印一次镜像数"之所以没看出来，是因为那一次的前后两次都发生在第一次
  `docker build` 之前）。启动脚本现在为每个版本起一个自己的 containerd（`--address`/`--root`/`--state`
  各自独立，版本与它自己打包的那份配对），并让 dockerd 用 `--containerd` 指过去。重跑后的证据是
  三条：七个私有 dockerd 的命令行里 `--containerd` 各指自己的 socket；进程表里除系统那个 containerd
  外多了七个（8 个进程、7 个各自的 `--address`）；而**系统 dockerd 的日志在重跑前 20 分钟里有 1155 行
  `could not find container`，重跑之后是 0 行**（那 1155 行的窗口是 17:37–17:57:37，重跑后的窗口从
  17:57:37 起，同一台机器的同一份日志）。系统 docker 的容器数也回到 0、镜像数回到 0。
- 19.03 的 `--ip6tables` 见上：重跑后 daemon 起来了，挡住它的是宿主的 cgroup v2，不是这条 flag。
- 矩阵的每一项都用**那个版本自己的 `docker` 客户端**去驱动它自己的 daemon。用系统那份 29.1.3 的
  客户端去探旧 daemon 会直接被拒（`Error response from daemon: client version 1.52 is too new.
  Maximum supported API version is 1.40`），拿它的 `--format` 读 `server` 只会得到空字符串——
  这不是 daemon 没起来，是客户端拒绝按老 API 说话。

**这一轮仍然没有跑的是 `Dockerfile` 本身**（基础镜像来自 Docker Hub，这里拉不到），这一节验证的
是它依赖的运行时契约与程序的功能路径，不是那个镜像。三段临时脚本照旧没有进仓库（3.17 已经说明
理由）。容器里那 82 项（连同 3.19 的 13 项）之外，**没有**跑 Windows/macOS 与真机 arm64，那些
数字仍是 3.12/3.13 的。

## 3.19 十一个场景：把程序放进 musl 用户态（第三轮）

3.18 的十个场景全部跑在 `FROM scratch` 的镜像上。那个镜像里没有 libc、没有 shell、没有 `/etc`，
所以它证明的是"这个静态二进制什么都不需要"——它并不能说明这个程序在一个真实发行版里跑得起来。
这一轮补上这一维：同一个二进制放进 **Alpine 的 minirootfs**（musl + BusyBox）里再跑一遍。

Docker Hub 依旧不可达，所以也没有 `alpine` 镜像可拉。Alpine 把 minirootfs 作为 tarball 发布，
`docker import` 能把 tarball 变成镜像——这一维就是这么来的（`rootfs/alpine-3.24.2.tar.gz`，
3.7 MB，从 `dl-cdn.alpinelinux.org` 取，实测 273 秒）。镜像里的二进制是同一个（静态、
`CGO_ENABLED=0`），容器以 uid 65532 运行，其余配置与 `core` 那一节相同。

新的 `musl` 场景 **13 项**：

| 断言 | 说明 |
|---|---|
| minirootfs 导入成镜像 | `docker import --change 'ENTRYPOINT [...]'` |
| 用户态确实是 musl | 容器里存在 `/lib/ld-musl-x86_64.so.1` |
| BusyBox 能跑 | `/bin/busybox uname -m` 回 `x86_64` |
| 静态二进制在 musl 下启动 | `--version` 打印 `protocol 4` |
| **musl 自己的 `ldd` 说它不是动态程序** | `Not a valid dynamic program` |
| 服务端在 musl 容器里起得来 | `/healthz` 回 200 |
| 面板令牌两侧 | `/api/status` 401 / 200 |
| 服务端无 panic | 日志里没有 `panic` |
| 两个 musl 容器之间搬字节 | 隧道真的传了数据 |
| 审计写在 musl 容器里 | 文件非空 |
| 审计的属主与权限 | uid 65532、mode 0600 |
| 客户端无 panic | 同上 |

七个版本 × 13 项全过（**六个版本各 95 项，573/588**；19.03.15 仍是 3.18 那条 cgroup 界线）。
第五项是这一维的要点：musl 的 `ldd` 回答 `Not a valid dynamic program`，它把"程序在 musl 用户态里
跑"与"这个二进制依赖 musl 才能跑"分开——后者会让这一维看起来通过了而实际什么都没测。结论：
这个静态二进制在 musl 用户态里的表现与在 `scratch` 里一致，面板、令牌、隧道、审计的属主与权限
都一样。

同一轮里还**验证了 `docs/SECURITY.md` 第 9 节那句断言**（此前只是写在那里）：一句 xtcp 会话如果
走的是打洞直连，账本条目就是 0 字节。做法是三个容器——服务端、发布方、访问方——跑一条 `xtcp`，
从访问方的监听端口搬一次数据，让发布方**优雅停止**（条目只在会话结束时追加），然后同时读三处：

```
从访问方端口搬字节:  True
服务端指标:          aethertunnel_bytes_from_clients_total 0
                     aethertunnel_bytes_to_clients_total 0
                     aethertunnel_p2p_punches_total 1
                     aethertunnel_p2p_direct_total 1
                     aethertunnel_p2p_relayed_total 0
账本:                idx=0 proxy='p2p' bytes_in=0 bytes_out=0
审计:                … visitor_accepted, p2p_direct, proxy_removed, client_disconnected
```

也就是说这三句话现在都有实测支撑：直连的字节不进账本、不进双向字节指标，"这条路是直连"写在审计
与 p2p 计数里。顺带确认了条目的写入时机：会话结束时追加，所以**被 `docker rm -f` 强杀的服务端不
留条目**——`private` 那一节的 state 目录里账本是空的，而 `core` 那一节会 `docker stop` 客户端再等
文件出现，两者的差别就在这里，不是程序的行为不一致。

**这一轮还修掉了一个会伪造失败的东西**（与程序无关，但它决定"矩阵报出来的失败有多少是真的"）：
矩阵给每个场景分配端口的方式是"绑定 0 端口、记下号码、立刻关闭"，而内核可能把同一个号码再分给
下一个刚关闭的 socket——实测 **1000 组、每组 10 个端口里有 4 组拿到重复号码**（更早一次抽样是
5000 组里 16 组，0.32%）。一个场景里如果有两个用途拿到同一个号码，比如 harness 自己那个 echo 目标
与服务端要绑的控制端口，服务端就会因为 `address already in use` 直接退出；而外面看到的是"面板连接
被拒"、"`/api/dht` 连接被拒"，以及"客户端的首字节是 `0x65`"（`e` 正是 echo 应答 `echo:` 的第一个
字母）——三条现象都不指向根因。28.4.0 上出现过一次这样的 8 条失败，连跑四遍不再出现，按分配器一测
就复现了。现在分配器记住已经发出去的号码并跳过，同样的 1000 组实测 **0 组**重复；此后整套连跑
三遍，每遍都是 573/588，19.03.15 之外 0 条失败。

## 3.20 把 Windows 那一栏跑起来：`scripts/wine-check.sh`

3.1 与 3.9 记下的阻塞是**非特权**用户命名空间：`unshare -rm` 被内核拒绝
（`kernel.apparmor_restrict_unprivileged_userns = 1`），而 Wine 的封装要在私有挂载命名空间里把
解出来的 `share/wine` 绑到 `/usr/share/wine`，`wineserver` 读不到区域数据就退出。这一轮这台机器上
**拿到了 root**，于是两条路都通了：root 建挂载命名空间不受那条限制（在一句话里
`unshare -m` + `mount --bind` + `setpriv` 降回普通用户即可），或者干脆把 `/usr/share/wine` 做成
指向解包目录的符号链接——这一次用的是后者，一条 `ln -s`，用完删掉即可。Wine 本身仍是第 3 节那个
root-free 解包，没有装进系统。

跑起来的第一件事是补上一个一直缺的东西：**仓库里没有驱动 Windows 二进制的脚本**。此前那些 66/66
是用一个不在仓库里的封装跑出来的（第 4 节批评过这件事），所以数字无法复现。这一轮把它写成
`scripts/wine-check.sh` 并进了仓库：两个参数就是两端，每一端可以是 PE（走 Wine）、带
`qemu-arm64:` 前缀的 arm64 ELF（走 qemu-aarch64），或本机 ELF（直接执行）；`WINEPREFIX`、
`WINEDLLPATH`、`LD_LIBRARY_PATH` 由调用者给，其余都在脚本里。路径来自 Windows 那一侧的写法也在
脚本头部说明：Wine 把 `/` 映射成 `Z:`，脚本自己创建的文件必须以 `Z:\...` 出现在配置里，而且要用
TOML 的**字面量字符串**，否则 `\` 会被当成转义（这正是 3.6 记下的那条教训）。

它跑 **71 项**，分十四节：

| 节 | 项数 | 内容 |
|---|---|---|
| 命令行 | 5 | 两个二进制报出 `protocol 4`；两份示例配置由两侧各自校验通过；越界值被拒并点名键 |
| 控制端口 / 面板 / 指标 | 6 | 服务端起来、`/healthz` 200、`/api/status` 401 与 200、`/metrics` 的序列、**面板拒绝计数 +1** |
| 已发布代理与字节 | 7 | 客户端连上并写下会话、服务端计数、隧道真的搬字节（双向计数都动）、审计里 `control_accepted` 与 `proxy_registered` |
| 半关闭 | 2 | 代理真的发布；**半关闭之后仍收到应答**——Windows 上没有 close-write，只有对连接的 shutdown，这一条在这一栏最有意义 |
| 私有代理与访问者 | 4 | stcp 发布方注册、访问者绑定端口、字节穿过隧道、审计 `visitor_accepted` |
| socks5 出口 | 4 | 出口起来、名单内目标可达、名单外目标被拒、`socks5_requests_total` 计数 |
| 共享 http 端口与虚拟主机 | 2 | `Host:` 指到客户端 `domains` 里的名字时穿过隧道、没有对策的名字被拒——路由的依据是客户端声明的域名清单 |
| 目录 | 5 | `/api/dht` 的开关、160 位节点标识、默认命名空间、绑定的地址，以及客户端的两个代理都在目录里通告 |
| 已占用的控制端口 | 3 | 退出码与退出时的一句话，以及**报错说出原因而不是 Windows 的错误码**（见下面的缺陷） |
| 四层安全一起 | 9 | **身份密钥由客户端生成并打印公钥**、服务端带全套层起来、客户端穿过 TLS+身份+伪装连上、一次真实传输，以及**四层各自在自己日志里报了名** |
| 封禁窗口 | 6 | 三次令牌失败后来源被封、被封来源的拒绝被计数、**被拒的客户端日志里没有会话**、窗口过后同一来源被放进来、**该客户端日志里出现会话** |
| 带宽账本 | 13 | 面板说账本开着；经面板断开客户端（`DELETE /api/clients/{id}`）后条目与文件出现；条目点名的代理与其中的字节；命令行离线校验，含**换一把同样形状的公钥必须失败**；`--ledger-proof` 导出的单条前缀单独可校验 |
| 审计轮转 | 3 | `max_bytes = 700` 时五个连接把文件推进第一代（`audit.jsonl.1` 出现、当前文件继续写）——轮转是审计"不丢记录"承诺的一半 |
| `--reject-unknown-keys` | 2 | 未知键在默认 `--check` 下是警告且文件有效；加开关后变成错误并点名键 |

结果：**71/71**。

### 九对组合，各 71 项

脚本的每一侧按参数判断种类，配置里的路径也跟着那一侧走（`spath()` 与 `cpath()`），所以同一套 71
项可以在本机能执行的三种目标的所有组合上跑：

| `server` 参数 | `client` 参数 | 服务端 | 客户端 | 结果 |
|---|---|---|---|---|
| `bin/….exe` | `bin/….exe` | windows/amd64 | windows/amd64 | **71/71** |
| `bin/…` | `bin/….exe` | **linux/amd64** | **windows/amd64** | **71/71** |
| `bin/….exe` | `bin/…` | **windows/amd64** | **linux/amd64** | **71/71** |
| `bin/…` | `bin/…` | linux/amd64 | linux/amd64 | **71/71** |
| `qemu-arm64:…` | `bin/….exe` | **linux/arm64** | **windows/amd64** | **71/71** |
| `bin/….exe` | `qemu-arm64:…` | **windows/amd64** | **linux/arm64** | **71/71** |
| `qemu-arm64:…` | `qemu-arm64:…` | linux/arm64 | linux/arm64 | **71/71** |
| `bin/…` | `qemu-arm64:…` | **linux/amd64** | **linux/arm64** | **71/71** |
| `qemu-arm64:…` | `bin/…` | **linux/arm64** | **linux/amd64** | **71/71** |

加粗的七行是跨系统组合，其中四行正是 2.1 节里一直标着"上一轮 22 项那套"的那几对——现在用这一套
71 项检查重跑过一遍：TLS 的证书校验走两边的实现、伪装的分帧在一边写一边读、加密与身份签名各自算、
半关闭在 Windows 侧是一次 shutdown、在 Linux 侧是一次 close-write，两边都搬得动数据、都拒得掉该
拒的。最后两行（两端都是本机 ELF）是这套脚本的自检：同一套检查在两份本机二进制上也全过，说明上面
那些行的差别来自平台而不是脚本本身。

arm64 侧用 `qemu-aarch64` 执行，**没有**给 `-L` 支点：这些二进制是静态的（`CGO_ENABLED=0`），
qemu 不需要去找动态加载器（脚本支持 `QEMU_SYSROOT`，对按加载器编出来的 qemu 需要）。qemu 那一侧
的路径仍是 Unix 形式——它跑的是 Linux ELF，不是 PE。

其中**六项正是此前"从未在 Windows 上执行过"的那几项**：封禁窗口里读客户端日志的两条，以及四层
安全里读服务端与客户端日志的四条。这一轮又加的十项里，"已占用的控制端口"那一节同时也是下面那条
缺陷的回归检查。README 与 3.1 曾把这一处写成"新增的两项"与"新增的三项"，两处
自己就不一致；这一轮之后不再引用那个数字，直接指向本节。

**这一轮修掉的一条真缺陷（跨平台诊断信息）**：在 Windows 上，控制端口绑定失败时运维看到的是

```
server stopped: listen on 127.0.0.1:34807: listen tcp 127.0.0.1:34807: bind: winapi error #10048
```

一个要自己去查的号码；Linux 上同一失败写着 `bind: address already in use`。原因在 Go 的
`syscall` 包：Windows 上的 `syscall.EADDRINUSE` 是一个合成的 `APPLICATION_ERROR` 值，与网络栈
真正报的 `WSAEADDRINUSE`（10048）**永不相等**，所以按 errno 分类在那一侧永远不会命中。现在
`pkg/net` 的 `ListenError`/`ListenCause` 在两边都把最常见的两种拒绝说出人话（地址已被占用、权限
不足），识别不了的错误保留 net 自己的文本；服务端的控制端口、共享 http 端口、面板端口、代理的
公开端口、xtcp 的会合端口与客户端的访问者监听六个绑定点全部走它。两个平台的现在读起来一样：

```
server stopped: listen on 127.0.0.1:34807: the address is already in use (another process is listening on it)
```

回归测试 `pkg/net/listen_test.go` 直接喂 syscall 错误：三种可识别的失败各有其话、不认识的错误
保留原文并带上地址；两个变异（去掉地址占用的分支、识别后仍保留 net 文本）都会让测试失败。Windows
侧的证据就是上面那个"已占用的控制端口"小节的第三条检查——修复前它是失败的（#10048），修复后通过。

**没跑到的，写在明处**：

- `scripts/smoke-test.ps1`（101 项）仍然没有执行过：它要 PowerShell，这台机器上没有 `pwsh`，
  Wine 也不会带来它。本节跑的是仓库里用 bash 重写的那套检查，不是那份 ps1。
- 2.1 节那张表里的数字仍是**当时那几套脚本**跑出来的（25/6 项）。本节能跑混合组合，也把那张表的
  **九对组合**都跑了（上面那张表，各 71/71），但这是**另一套检查**，项数与内容都不同，所以 2.1 节
  的表格没有改写，只在下面加了一句指向本节。arm64 那几对是**在本机 qemu 下**跑的，不是真机
  （本机没有 arm64 硬件）：这一点与 2.1 节里 arm64 那几行的性质相同。
- darwin/amd64 与 darwin/arm64 与前几轮一样无法在本机执行；windows/arm64 同样。

## 4. 这套环境证明了什么、没证明什么

- 证明了：这三个目标上，八种代理类型与访问者路径**真的能传数据**，四层安全（加密含后量子、
  TLS、伪装、身份认证）能同时生效，面板与指标报的是真实计数，审计真的写下了对应事件。
- 没证明：模拟层自身的行为差异。qemu 与 Wine 都可能掩盖或引入只在真机上出现的问题（例如
  Wine 的套接字实现、Windows 的防火墙与命名管道语义、macOS 的沙箱与公证）。因此
  windows/amd64 仍以 CI 的 `windows-latest` 作业为准（它在真实 Windows 上跑 101 项）。
  arm64 上 CI 也已经在真机执行（`test-arm64`，`ubuntu-24.04-arm`）；本机 qemu 那一遍与之
  互相印证，但**指令集是模拟的**，不能替代真机（见 3.9 表的最后两行也是同样的性质）。
- **上面那张表的 70 项，是本文件作者在自己机器上跑的**（qemu 与 Wine 的搭建见第 3 节），
  它的脚本不在仓库里，因此谁也复跑不了它。仓库里能复跑的那一套是
  `scripts/functional-linux.sh`（有浏览器时 281 项、没有时 138 项，见 README）：它覆盖 `tcp`/`udp`/`http` 与共享 `https`
  监听（两者都按 Host 选隧道，未配置的名字都被拒）、`socks5`（`allow_targets` 内可达、范围外
  被拒）、`stcp`/`sudp`/`xtcp` 三种访问者（`xtcp` 那一项还会核对服务端报出的打洞结果与三个
  `aethertunnel_p2p_*` 计数彼此对得上）、密钥错误必须拿不到数据、**半关闭之后仍要收到应答**
  （公网端口与 socks5 两条路径各一项）、**只凭一个名字找到服务端**（`--dht-lookup` 把一个 tcp 名
  解析到它的公网端口、`--discover` 把一个私有名解析到控制端口、未知名字报错、以及一个
  `server_addr` 留空的客户端真的连上去并传了一次数据）、**两个客户端组成的代理池**（见 3.10：
  两名成员都服务过、池只保留第一个成员的端口、后到成员请求的端口不被监听、成员离开后继续服务
  且移除进审计）、访客名单两侧的拒绝路径（服务端为某个名字定的策略、以及代理自己的名单：访客都拿到 403，差别在计数与审计的 `detail` 指出的那一边）、**服务端侧的拒绝**（`deny_cidrs` 拒绝的来源在发出字节之前就被关闭、令牌桶在突发额度之后拒绝、`ban_after_failures` 把反复失败认证的来源封禁且窗口过后放行、`ban_ignore_cidrs` 让被忽略的来源永不被封、`max_connections = 1` 时第二个客户端被告知“at its connection limit”、以及首帧解不开或类型不能开局时拒绝被计数并审计）、指标与面板计数、审计事件，以及**去中心化目录**（`/api/dht` 的节点标识、绑定地址、命名空间与对外主机名，`-dht-key` 打印的公钥与它一致，一个名字从被通告、到池里一名成员离开后仍然保持、再到最后一名成员离开后被撤回），最后在**真实浏览器**（Chrome + DevTools 协议，`scripts/panel-checks.py`，143 项）里把控制台页面跑一遍：令牌提示与错误令牌、每个数字与表格是否与 API 自己的回答一致、配置页、请求被判为失败时的两条横幅与切换语言、断开按钮（效果回审计日志确认），、**账本页**（状态、文件、条目数、链头与公钥是否与 `GET /api/ledger` 一致、两张表、切换语言重画、失败横幅，以及在另一台关闭了 `[ledger]` 与 `[dht]` 的部署上把两个「未启用」都画出来）、**目录页**（`GET /api/dht` 的节点标识、地址、命名空间、节点数、对外主机名与通告签名公钥、正在通告的名字、切换语言重画与失败横幅）、**字典**（两种语言键集相同、元素提到的键都存在、没有重复或多余的键）、**版式**（含账本页：条目在窄屏同样是带栏目名的卡片，连不成一个词的公钥在行内换行）（700 px 以下每行是带栏目名的卡片、太长放不下的代理名在卡片内换行、360 px 下没有任何元素越界、宽表格只在容器里横向滚动）与**无障碍**（无障碍树里每个控件都有名字、抽屉报出 `aria-expanded`、跳转链接排在键盘最前、导航的可访问名跟随语言）——机器上没有 Chrome 类浏览器时这一节整段跳过，所以同一台机器上可能是 281 项、也可能是 138 项，以及加密+
  后量子+TLS+身份+伪装同时打开时的一次真实传输；CI 的 `ubuntu-latest` 与 `ubuntu-24.04-arm`
  都跑它，发布流程也用**已发布的 Linux 二进制**再跑一遍。它**不是**那 70 项：没有 `[vpn]`
  （那一项要 root），也没有 Windows 与 macOS 参与的跨系统组合（linux/amd64 ↔ linux/arm64 两对
  在本机 qemu 下跑过，见 3.9）。两边的数字不要互相代替。
- 三层隧道（`[vpn]`）**只在 Linux 上存在**；其他平台启动 `vpn.enabled = true` 会明确报错退出，
  这一点由单测覆盖。它此前只在 CI 的 `ubuntu-latest` 作业里对着真实 tun 设备验证过，
  **3.13 之后本机也跑过一遍**（23/23，并且面板那个状态也进了真实浏览器）。
- `deploy/kubernetes/` 的清单**已经 apply 到一个真集群上**（3.15，14 项全过）：探针、令牌、
  状态卷、隧道与 DHT 都被从集群外驱动过一遍。此前它只被渲染过。清单里那个镜像是用同一个二进制
  按 `Dockerfile` 的运行时契约搭的替身（基础镜像拉不到，见 3.14）。容器路径本身在 **docker
  20.10.24、28.4.0 与 29.2.1** 三个版本上并排跑过（3.17，各 21 项，63/63），随后又扩到
  **七个版本、每个 95 项**（3.18 的十个场景 82 项，其中六个版本跑的是 492/492；3.19 再加 13 项
  musl 用户态，六个版本 570/570、七个版本 573/588。第七个 19.03.15 的 daemon 起得来，但宿主的
  cgroup v2 让它那一套 runtime 起不了容器）：覆盖 stcp/sudp/xtcp 与访问方、
  socks5 出口的名单、共享 http 监听与虚拟主机、**Alpine 的 musl 用户态**，以及**清单要求的姿态**
  （只读根文件系统、丢掉全部能力、内存与 pid 上限）之下仍然搬得动数据。三轮共暴露三条真缺口——
  "非 root 进程拿不到 `NET_ADMIN`"、"首帧读不出来时不进指标也不进审计"，以及"面板与 `/metrics`
  的令牌拒绝不留痕迹"——都改在程序里；但**那个 `Dockerfile` 本身仍然只有 CI 验证过**。

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
