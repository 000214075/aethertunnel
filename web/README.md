# web/

内嵌面板的源码。面板是单个 HTML 文件，由 `embed.go` 用 `go:embed` 编译进服务端二进制，
所以发布出去的二进制自带界面，旁边不需要放任何目录，也不需要 CDN 或构建步骤。

The dashboard sources. The panel is one HTML file, compiled into the server binary with
`go:embed`, so a released binary serves its own UI with nothing beside it — no CDN, no
build step.

| 文件 | 作用 |
| --- | --- |
| [`dashboard/index.html`](dashboard/) | 面板本体：单页、自带样式与脚本、中英双语、手机可用；在线客户端、已注册隧道（含代理池逐成员）、流量、账本、去中心化目录、三层隧道与审计各一栏 |
| [`embed.go`](embed.go) | 把上面的文件挂到服务端的路由上；`/` 返回页面，`/api/*` 由 `pkg/server` 提供，令牌与鉴权在那边 |
| [`dashboard/README.md`](dashboard/README.md) | 面板每一栏的数据来自哪个 API、两种语言怎么组织、版式与无障碍的约束 |

面板的检查在**真实浏览器**里跑（`scripts/panel-checks.py`）：令牌提示与错误令牌、
每个数字是否与 API 一致、连接断开时的横幅、切换语言时是否重画、700 px 与 360 px 下的版式、
无障碍树里每个控件是否有名字。

The panel's checks run in a real browser (`scripts/panel-checks.py`).
