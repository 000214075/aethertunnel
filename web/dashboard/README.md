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
| `DELETE /api/clients/{id}` | Disconnect button, then re-poll / 断开按钮，随后重新轮询 |

`GET /api/health` is public but unused here: `/healthz` and `/readyz` are the probes an
orchestrator polls. `GET /api/ledger` and `GET /api/dht` exist on the same listener and
need the same token, but this page does not render them; read them with `curl` or a
dashboard client. Failures raise a dismissible error banner; a failed `/api/status` also
raises "disconnected — retrying". Empty lists say "no clients connected" / "no proxies
registered"; before the first successful load, the tables show `—`. 请求失败会显示可关闭
的错误横幅；`/api/status` 失败还会显示「连接断开 — 正在重试」；列表为空时明确显示
「当前没有已连接的客户端」「暂无已注册的代理」。

## Proxy pools / 代理池

`/api/proxies` returns one row per proxy, with the pool's aggregate counters and a
`members` array (`client_id`, `local_addr`, `latency_ms`, `failures`, `healthy`). The
proxies table shows the member count and how many of them are healthy in the *Members*
column, next to the first member's client and local address; `/api/proxies` carries the
per-member detail for anything the table does not show. `/api/proxies` 每个代理返回一行，
含池的合计计数与 `members` 数组（`client_id`、`local_addr`、`latency_ms`、`failures`、
`healthy`）；表格的「成员」列显示成员数与其中可用（healthy）的个数，客户端与本地地址仍是
第一个成员的；其余成员明细需要直接读取 `/api/proxies`。

Byte and connection counters advance when a stream or a request finishes, and the page polls
every two seconds, so a stream that is still open shows the totals of the ones that finished
before it. An `http`/`https` proxy counts each served request; when several clients share a
pool, a request or a datagram session is credited to every member in proportion, because the
server does not attribute it to one of them. A member that disconnects takes its counters out
of the pool aggregate, because the aggregate is the sum of the members that are present.
字节与连接计数在一条流或一个请求结束时累加，页面每 2 秒轮询一次，所以仍在进行中的流只显示
此前已结束流的合计；`http`/`https` 代理按每个完成的请求计数；池中有多个客户端时，一个请求
或一个数据报会话按比例计入每个成员，因为服务端并不把它归给某一个成员；成员断开后，它的计数
不再计入池的合计（合计等于当前成员之和）。

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
宽度小于 860 px 时侧边栏变为抽屉，由汉堡按钮打开，可用遮罩、`Esc` 或选择栏目关闭；
小于 700 px 时表格每行重排为带标签的卡片，360 px 屏幕上不会出现被截断的内容；
标签页隐藏时暂停轮询，回到前台后立即刷新一次。
