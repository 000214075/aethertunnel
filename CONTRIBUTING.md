# 参与贡献 · Contributing

欢迎 issue 与 PR。为了让每一次改动都能被后来的读者核实，本仓库有两条与能力同样认真的
约定：**能力必须有可执行的检查**（不是宣传词）、**每个配置键必须有文档**（守卫测试强制）。

Issues and pull requests are welcome. Two conventions here are held as seriously as the
features: every capability has an executable check (never marketing copy), and every
configuration key is documented (enforced by a guard test).

## 报告问题 · Reporting a problem

- 功能不工作 / 与文档不符：用 [bug 报告模板](.github/ISSUE_TEMPLATE/bug_report.yml)，带上
  `--version` 的输出、两端（尽量脱敏后的）配置与日志。日志里的令牌永远不要贴。
- 想要新能力：先用 [feature 模板](.github/ISSUE_TEMPLATE/feature_request.yml) 说明场景。
  与 frp 的对照（哪些已复现、哪些有意不做）见 [`docs/VS-FRP.md`](docs/VS-FRP.md)，
  有意不做的清单见 [`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md)。
- **安全问题不要开公开 issue**：按 [`docs/SECURITY.md`](docs/SECURITY.md) 的说明私下报告。

- Something does not work or does not match the docs: use the
  [bug template](.github/ISSUE_TEMPLATE/bug_report.yml) with the `--version` output and
  both ends' (redacted) configuration and logs. Never paste a token into an issue.
- A capability you miss: start from the
  [feature template](.github/ISSUE_TEMPLATE/feature_request.yml) and describe the
  scenario. The honest frp comparison lives in [`docs/VS-FRP.md`](docs/VS-FRP.md) and the
  deliberate non-goals in [`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md).
- **Do not open a public issue for a security problem**: report it privately as
  [`docs/SECURITY.md`](docs/SECURITY.md) describes.

## 本地开发 · Local development

```bash
make build      # bin/aethertunnel-server 与 bin/aethertunnel-client（纯 Go，无 CGO）
make test       # go test ./...（gofmt 与 vet 在 make lint / make all 里）
make test-race  # 竞态检测（需要 C 编译器；无 root 时 scripts/race-toolchain.sh 会解包一份）
make cross      # 12 个发布产物 + dist/SHA256SUMS
```

功能套件 `bash scripts/functional-linux.sh bin/aethertunnel-server bin/aethertunnel-client`
驱动真实二进制把每项能力过一遍（面板部分需要本机有 Chrome 类浏览器，没有时自动跳过）；
Windows 用 `scripts/smoke-test.ps1`。改动协议、面板或平台路径时，请把对应脚本一起跑。

The functional suite, `bash scripts/functional-linux.sh bin/aethertunnel-server
bin/aethertunnel-client`, drives the real binaries through every capability (the dashboard part
needs a Chrome-like browser and is skipped without one); on Windows, `scripts/smoke-test.ps1`.
If you touch the protocol, the panel or a platform path, run the matching script as well.

## 改动的形状 · What a change looks like

- **新能力**：实现、测试（单元 + 功能套件的一节）、[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)
  的键、[`docs/VS-FRP.md`](docs/VS-FRP.md) 的对照行，
  一个都不少——守卫测试会替你检查前两项。
- **改行为**：先改测试再改实现；拒绝与告警的文案是契约，改动要在 PR 里说明谁会看见不同。
- **改文档**：文档错了也算 bug；`docs/` 下每一页都对应仓库里可核实的事实。

- **A capability**: implementation, tests (unit plus a functional-suite section), the key
  in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) and the row in
  [`docs/VS-FRP.md`](docs/VS-FRP.md) — the
  guard tests check the first two for you.
- **A behaviour change**: the test first, then the implementation. Refusal and warning
  wording is a contract; say in the pull request who will see a different message.
- **A documentation fix**: wrong docs are bugs; every page under `docs/` corresponds to
  verifiable facts in this repository.

## 提交 · Pull requests

一个 PR 只做一件事；描述里写清动机、验证方式（跑了哪些脚本、结果数字）与对配置兼容性的
影响。CI 对每个 PR 跑完整测试、竞态检测、fuzz、三层隧道与功能套件，全绿是合并的前提。

One pull request does one thing. Describe the motivation, how you verified it (which
scripts, which numbers) and what it means for configuration compatibility. CI runs the
full tests, the race detector, the fuzz targets, the layer-3 tunnel and the functional
suite on every pull request; green is the bar for merging.

参与即同意 [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md)。

By participating you agree to [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
