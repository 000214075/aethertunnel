# AetherTunnel dashboard / 控制台

`index.html` is the whole dashboard: one self-contained file, inline CSS and JS, no CDN,
no build step, no framework. It renders only what the API returns — no sample data, no
random numbers, no placeholder rows. `index.html` 就是整个控制台：单文件、内联 CSS 与
JS，无 CDN、无构建、无框架；页面只渲染 API 返回的数据，没有示例数据或占位行。

## Endpoints / 调用的接口

| Request | When / 时机 |
| --- | --- |
| `GET /api/status` | every 2 s / 每 2 秒 |
| `GET /api/clients` | every 2 s / 每 2 秒 |
| `GET /api/proxies` | every 2 s / 每 2 秒 |
| `GET /api/config` | Configuration view + Refresh button / 打开配置页与刷新按钮 |
| `GET /api/ledger` | Ledger view + its Refresh button / 打开账本页与它的刷新按钮 |
| `GET /api/dht` | Directory view + its Refresh button / 打开目录页与它的刷新按钮 |
| `DELETE /api/clients/{id}` | Disconnect button, then re-poll / 断开按钮，随后重新轮询 |

`GET /api/health` is public but unused here: `/healthz` and `/readyz` are the probes an
orchestrator polls. Failures
raise a dismissible error banner; a failed `/api/status` also raises "disconnected —
retrying". Empty lists say "no clients connected" / "no proxies registered"; before the
first successful load, the tables show `—`. 请求失败会显示可关闭
的错误横幅；`/api/status` 失败还会显示「连接断开 — 正在重试」；列表为空时明确显示
「当前没有已连接的客户端」「暂无已注册的代理」。`GET /api/health` 是公开的但这一页用不到：
编排器探的是 `/healthz` 与 `/readyz`。

## Connections / 连接数

The overview counts four things and shows them as four tiles: *Active connections*
(`connections.active`, the sessions that are connected now), *Total connections*
(`connections.total`, every TCP socket the listener has accepted, including the ones
refused before the handshake), *Authenticated connections* (`connections.authenticated`,
the ones that completed the handshake and got a session) and *Max connections*
(`connections.max`). The middle two are different numbers whenever a source connects and
says nothing: only the second of the two matches `aethertunnel_control_connections_total`
on `/metrics`. 概览页的四格分别是当前连接数、监听器接受过的连接总数（含握手前就被拒的）、
通过握手的连接数（与 `/metrics` 的 `aethertunnel_control_connections_total` 同源）与上限；
有人连上却不发握手时，前两个数字就不相等。

## Audit log / 审计日志

The `audit` section of `/api/status` is the only place a stopped audit log is visible, so
the status panel carries an *Audit log* row with four states: `recording`; `N write(s)
recovered after reopening` when a record only landed on the second attempt;
`cannot write — N record(s) lost` while the path cannot be opened, with a banner that
stays up until the server reports it writable again; and `recording again — N record(s)
lost` after a recovery, which keeps the warning because those records are gone. The
counters reset when the server restarts. The row is `no` when `[audit]` is off.
`/api/status` 的 `audit` 段是"审计已经停了"唯一能被看到的地方，因此状态面板有一栏
「审计日志」，四种状态：正常写入；重开后补写 N 次（该条记录第二次才写下）；
无法写入 — 已丢 N 条（路径打不开，此时另有一条横幅，直到服务器报告可写才消失）；
已恢复写入 — 期间丢 N 条（恢复后保留警告，因为那几条确实没了）。计数器在服务端重启时归零；
`[audit]` 关闭时显示「否」。

## Proxy pools / 代理池

`/api/proxies` returns one row per proxy, with the pool's aggregate counters and a
`members` array (`client_id`, `local_addr`, `latency_ms`, `failures`, `healthy`). The
proxies table shows the member count and how many of them are healthy in the *Members*
column, next to the first member's client and local address, and the strategy the pool is
being served with in the *Load balancing* column (`load_balance`). The client rows carry a
*Client version* column, which is what that client reported when it connected. The
configuration view shows *Proxy policies*, the number of `[[proxies]]` entries in the
server's own configuration — not the number of proxies connected clients have registered,
which is the *registered* count on the overview. `/api/proxies` each proxy returns one row
and carries the per-member detail for anything the table does not show. `/api/proxies`
每个代理返回一行，含池的合计计数与 `members` 数组（`client_id`、`local_addr`、
`latency_ms`、`failures`、`healthy`）；表格的「成员」列显示成员数与其中可用（healthy）的
个数，客户端与本地地址仍是第一个成员的，「负载均衡」列显示这个池当前使用的策略
（`load_balance`）。客户端一行的「客户端版本」列是它连接时报告的版本；配置页的
「代理策略」是服务端自己配置里的 `[[proxies]]` 条数，与概览页「已注册」那个数字不同——
后者属于已连接客户端。其余成员明细需要直接读取 `/api/proxies`。

Byte and connection counters advance when a stream or a request finishes, and the page polls
every two seconds, so a stream that is still open shows the totals of the ones that finished
before it. An `http`/`https` proxy counts each served request; when several clients share a
pool, a request or a datagram session is credited to every member in proportion, because the
server does not attribute it to one of them. A member that disconnects takes its counters out
of the pool aggregate, because the aggregate is the sum of the members that are present.

The two stream columns a client row shows — active and total — come from the session's own
counters: the total advances when a stream opens, and the active count comes back down when it
ends. Both used to stay at zero, which is why a row could show traffic and no streams. Some
streams stay open by design: a `udp`/`sudp` session lives per visitor address, and the reverse
proxy keeps its tunneled connection for the next request, so "active" is not expected to reach
zero while such a proxy is in use. 客户端一行的「活动流 / 累计流」两列取自会话自己的计数：
打开流时累计加一，流结束时活动减一；这两个数字此前一直是 0。`udp`/`sudp` 的会话按访客地址
常驻，反向代理也会保留它的隧道连接给下一个请求，因此这类代理在使用期间「活动流」不为 0
是正常的。
字节与连接计数在一条流或一个请求结束时累加，页面每 2 秒轮询一次，所以仍在进行中的流只显示
此前已结束流的合计；`http`/`https` 代理按每个完成的请求计数；池中有多个客户端时，一个请求
或一个数据报会话按比例计入每个成员，因为服务端并不把它归给某一个成员；成员断开后，它的计数
不再计入池的合计（合计等于当前成员之和）。

## Layer-3 tunnel / 三层隧道

The Configuration view has a second panel for `[vpn]`: whether the tunnel is on, the
interface, the server's own address, the subnet, the MTU, how many addresses have been
handed out of how many, the peers, and the packets read from the interface, delivered to
it, and dropped or unroutable. Every number comes from the same section of `GET /api/config`
that `GET /api/vpn` returns, and both are read from the tunnel inside the server process:
the interface lives there, so this is the only way to see whether it is up and carrying
traffic. With the section disabled the panel reports "no" and leaves the rows as `—`.
配置页的第二个面板显示 `[vpn]`：是否启用、接口、服务端地址、子网、MTU、地址池已用/总数、
对端数，以及从接口读到的包、送入接口的包和丢弃或无法路由的包。这些数字与 `GET /api/vpn`
同源，都读自服务端进程内的隧道；未启用时该面板显示「否」，其余各行为 `—`。

## Ledger / 带宽账本

The Ledger view reads `/api/ledger` when it is opened and when Refresh is pressed, rather
than on the two-second poll: the ledger is a file, and its numbers move when a session
ends. It shows whether `[ledger]` is on, the file, how many entries the chain holds, the
chain head (abbreviated in the cell, whole hash in its `title`), the bytes the sessions
that are still connected have used but which are not in the chain yet, and the
verification key in full. Two tables follow: the per-client totals (entries, bytes in,
bytes out) and the most recent entries, each with its index, time, client, proxy, both
byte counts and the hash the server signed. With `[ledger]` off the API answers
`{"enabled": false}`, which this view reports as "no" and explains — a row of zeroes would
read as "nothing was used", which is a different statement. The key in the panel is what
`--verify-ledger <file> --ledger-key <key>` needs, so the numbers on screen can be checked
against the file without the server. Only the bytes this server carried are billed, which
the page says in its own note: an `xtcp` stream that punched a direct path never comes back
through the server, so it is in no entry — reading a zero there as "this client used
nothing" is the mistake that note exists to prevent.
账本页在打开时与点击刷新时读取 `/api/ledger`（不参与 2 秒轮询）：账本是一个文件，
它的数字在会话结束时才变化。页面显示 `[ledger]` 是否启用、文件路径、链上的条目数、
链头（单元格里是缩写，完整哈希在 `title` 里）、仍连接的会话已产生但尚未写入链的字节，
以及完整公钥；下面两张表分别是按客户端汇总（条目数、入站、出站）与最近的条目（序号、
时间、客户端、代理、双向字节与服务器签名的哈希）。`[ledger]` 关闭时接口返回
`{"enabled": false}`，页面照实显示「否」并说明原因——画一排 0 会被读成"没有任何用量"，
而那是另一件事。页面上这个公钥就是 `--verify-ledger <文件> --ledger-key <公钥>` 需要的
那把，所以屏幕上这些数字可以脱离服务器与文件对账。账只记服务端搬过的字节，这一点页面
自己也写着：`xtcp` 打洞后走直连的那条流不再经过服务端，因此不在任何一条记录里——把那里的
0 读成"这个客户端没有用量"正是那条说明要挡住的误读。

## Directory (DHT) / 去中心化目录

The Directory view reads `/api/dht` when it is opened and when Refresh is pressed. It
shows whether `[dht]` is on, the node identifier (abbreviated in the row, whole value in
its `title`), the address the node bound, the key namespace, how many peers are in its
routing table, the host it advertises, and the announcement signing key — or the word
"unsigned" when none is configured, which is a fact about the node rather than a missing
value. Under it is the list of names this node announces: those are the names a client
can resolve instead of being told which server serves them, so an operator can see
whether a proxy is findable by name. With `[dht]` off the API answers `{"enabled":
false}`, which the view reports as "no" and explains. The key on screen is the one an
operator hands out for `trusted_keys`, and `aethertunnel-server -dht-key -config …`
prints the same value.
目录页在打开时与点击刷新时读取 `/api/dht`：`[dht]` 是否启用、节点标识（行内缩写、完整值在
`title` 里）、节点绑定的地址、键的命名空间、路由表里的节点数、对外通告的主机名，以及通告
签名公钥——没配置时显示「未签名」，那是对这个节点的陈述而不是缺一个值。下面是本节点正在
通告的名字：客户端可以用这些名字找到服务器，所以运维在这里就能看出某个代理是否还能被按名
找到。`[dht]` 关闭时接口返回 `{"enabled": false}`，页面照实显示「否」并说明。屏幕上这把
公钥就是交给 `trusted_keys` 的那把，`aethertunnel-server -dht-key -config …` 打印的是同一个值。

## How this page is checked / 这一页怎么被验证
`scripts/panel-checks.py` drives this page in a real browser: it starts Chrome with the
remote debugging port open, speaks the DevTools protocol over a WebSocket, and asserts on
the rendered page — the token prompt and its rejection, every tile and table against the
API's own answer, the configuration view, the two banners under a request the browser was
told to fail, a language switch while those banners are up, the disconnect button (whose
effect is confirmed in the audit log), **the ledger view** (the state, file, entry count,
chain head and verification key against `GET /api/ledger`, one row per client in the
totals, the most recent entries with the hash the server signed, both tables redrawn when
the language changes, and the off state drawn on a second deployment whose `[ledger]` is
disabled), **the directory view** (state, node id, address, namespace, peers, advertised
host and announcement key against `GET /api/dht`, the announced names, both tables
redrawn when the language changes, and the off state), **the dictionaries** (read out of the page's own markup: both languages define
the same keys, every key an element or a script mentions exists in both, no key is defined
twice, no key is dead weight, and no element ever shows a key instead of a sentence),
**the layout** (that below 700 px each row is a card
carrying its column name, that a proxy name too long for the screen wraps inside its card
instead of widening the page, that nothing sticks out of a 360 px viewport and that a wide
table scrolls inside its own wrapper rather than the page) and **what a screen reader
gets** (every control in the accessibility tree has a name, the drawer button reports
`aria-expanded`, the skip link is the first thing the keyboard reaches and points at the
main region, and the navigation's accessible name follows the language). It is the only
check of this file; the Go tests cover the API it reads. `scripts/functional-linux.sh` runs
it as its last section and folds its checks into the suite's totals, and skips it on a
machine with no Chrome-like binary. 这一页由 `scripts/panel-checks.py` 在真实浏览器里验证：
脚本用远程调试端口启动 Chrome，通过 WebSocket 讲 DevTools 协议，然后对**渲染后的页面**断言——
令牌提示与它对错误令牌的拒绝、每个数字与表格是否与 API 自己的回答一致、配置页、在"请求被浏览器
判为失败"时的两条横幅、横幅在屏幕上时切换语言、断开按钮（它的效果回到审计日志里确认）、
**账本页**（状态、文件、条目数、链头与公钥是否与 `GET /api/ledger` 一致，按客户端汇总一人一行，
最近条目连同服务器签名的哈希，切换语言时两张表都重画，以及在另一台关闭了 `[ledger]` 的部署上
把「未启用」这个状态画出来）、**目录页**（状态、节点标识、地址、命名空间、节点数、对外主机名
与通告公钥是否与 `GET /api/dht` 一致，已通告的名字，切换语言时重画，以及关闭 `[dht]` 时
把「未启用」画出来）、**字典**（直接读页面自己的标记：两种语言定义的键相同、元素或脚本
提到的每个键都存在、没有键被定义两次、没有键是多余的、也没有哪个元素把键当成句子显示出来）、**版式**
（700 px 以下每行是一张带栏目名的卡片、太长而放不下的代理名在卡片内换行而不是把页面撑宽、
360 px 视口下没有任何元素越界、宽表格在自己的容器里横向滚动而不是让整页滚动）以及**屏幕阅读器
拿到的东西**（无障碍树里每个控件都有名字、抽屉按钮报出 `aria-expanded`、跳转链接是键盘最先到达
的元素并指向主区域、导航的可访问名跟随语言）。这是这个文件唯一的检查；它读的 API 由 Go 测试覆盖。
`scripts/functional-linux.sh` 把它作为最后一节运行并把它的项数计入套件总数，机器上没有 Chrome 类
浏览器时跳过。

Every view that draws a value through `t()` is redrawn when the language is switched: the
status tiles, both tables, the ledger, the directory and the configuration view, whose
yes/no cells used to stay in the language they were drawn in until the view was reloaded.
Sentences are kept in state as the key and parameters they were built from, so a banner
raised by a failed request cannot stay in the previous language either. 每一个用 `t()`
画数值的视图在切换语言时都会重画：状态格、两张表、账本、目录与配置页（配置页那几格
`yes`/`no` 以前会一直停在切换前的语言，直到重新打开这一页）。句子以生成它们的键与参数存
在状态里，因此请求失败留下的横幅也不会停在切换前的语言。

Everything that is a sentence on screen is redrawn when the language is switched,
including the error banner and the "disconnected — retrying" banner: they are kept in
state as the key and parameters they were built from, so a banner raised by a failed
configuration load cannot stay in the previous language until the page is reloaded.
屏幕上的每一句文案在切换语言时都会重画，包括错误横幅与「连接断开 — 正在重试」横幅：
它们以**生成自己的键与参数**保存在状态里，因此一次配置加载失败留下的横幅不会一直停在
切换前的语言，直到用户刷新页面。

## Dashboard token / 启用令牌

```toml
[dashboard]
enabled   = true
bind_addr = "127.0.0.1"
port      = 7500
token     = "use-openssl-rand-hex-32"   # protects /api/* / 保护 /api/* 路由
```

Set `[dashboard] token` to a non-empty string: requests without a valid
`Authorization: Bearer <token>` header then get `401 {"error":"unauthorized"}`. The page
prompts for the token, keeps it in `localStorage` (`aethertunnel.dashboard.token`) and
sends it as the Bearer header; "Sign out" clears it. Use HTTPS/TLS — the token travels
in a plain header, and `localStorage` is readable by any script on the origin.
设置 `[dashboard] token` 后，未携带有效 Bearer 头的请求会收到 401；页面会提示输入
令牌，保存在 `localStorage`，并在后续请求中携带；侧边栏「退出登录」可清除。
令牌以明文头传输，请通过 HTTPS/TLS 访问。

## Mobile / 移动端

Below 860 px the sidebar becomes an off-canvas drawer opened by the hamburger button and
closed by the scrim, `Esc`, or picking a section; below 700 px table rows restack as
labelled cards, so nothing is cut off at 360 px. Polling pauses while the tab is hidden.
Above 700 px a table wider than the window scrolls inside its own wrapper, so the page
itself never scrolls sideways. The card layout, the absence of anything sticking out at
360 px (including a proxy name no phone can fit on one line) and the wrapper's own
scrolling are asserted in a browser by `scripts/panel-checks.py`.
宽度小于 860 px 时侧边栏变为抽屉，由汉堡按钮打开，可用遮罩、`Esc` 或选择栏目关闭；
小于 700 px 时表格每行重排为带标签的卡片，360 px 屏幕上不会出现被截断的内容；700 px 以上
宽于窗口的表格在自己的容器里横向滚动，页面本身不会左右滚动。卡片版式、"360 px 下没有任何
元素越界"（含一个任何手机都放不下的代理名）以及容器自身的滚动，都由 `scripts/panel-checks.py`
在浏览器里断言。标签页隐藏时暂停轮询，回到前台后立即刷新一次。
