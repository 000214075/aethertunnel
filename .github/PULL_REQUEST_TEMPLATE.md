# 一个 PR 只做一件事 · One pull request does one thing

## 动机 · Motivation

<!-- 为什么改：场景、与 frp 的对照（如适用）、它修的 issue -->

## 验证 · How it was verified

<!-- 跑了哪些脚本与结果：go test ./...、scripts/functional-linux.sh 或 scripts/smoke-test.ps1，
     改了 web/ 时再跑面板检查 scripts/panel-checks.py -->

## 兼容性 · Compatibility

<!-- 配置键与线上格式是否变化；拒绝与告警的文案是否变了、谁会看见不同；
     是否需要两端一起升级 -->

## 清单 · Checklist

- [ ] `make test`（或等价的 `gofmt`、`go vet`、`go test ./...`）全绿
- [ ] 新能力有测试与功能套件的一节
- [ ] `docs/CONFIGURATION.md` 的键与 `docs/VS-FRP.md` 的对照行已更新（守卫测试会查键）
