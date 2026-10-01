# web/

内嵌面板的源码：[`dashboard/`](dashboard/) 里单文件的 `index.html` 由根目录的
`embed.go` 以 `go:embed` 打进服务端二进制——面板无需单独部署，服务端起来就有。
面板的 143 项浏览器检查由 `scripts/panel-checks.py` 驱动。

The dashboard sources: the single-file `index.html` in [`dashboard/`](dashboard/) is
embedded into the server binary by `embed.go` — no separate deployment. The panel's
143 browser checks are driven by `scripts/panel-checks.py`.
