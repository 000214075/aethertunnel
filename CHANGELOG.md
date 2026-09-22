# 更新日志 · Changelog

本文件记录每个版本改了什么。

> **关于 v3.1.0 之前的条目**：旧版本宣称 WebRTC、DHT、区块链、抗量子加密、AI 路由等
> 20 项功能，并声称在 14 个平台上产出 28 个二进制。对全部 Go 源码检索这些关键词，
> 出现次数为 0，构建脚本指向的是不存在的目录。v3.1.0 之前的详细功能清单已删除，
> 只保留版本号与日期。v0.1.x 到 v3.0.0 期间完成的是配置、协议与面板的骨架，
> 核心链路在本版本之前未跑通。

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

### 运维测试

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
- **Windows 侧的 `.uitest/panel-columns-check.js`（24 项）需要重跑**：它用的两个客户端组成
  代理池，而池的「成员」一格现在多出逐成员的行，凡是把这格文字当成一个整体来比对的断言都要
  相应放宽或改成按成员比对。上面那 30 项是在 Linux 上另跑的一套，不能替代它。

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
