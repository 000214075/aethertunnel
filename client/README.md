# client/

`aethertunnel-client` 的命令入口，只有一层壳：旗标解析、配置装载、信号处理，然后把配置
交给 [`pkg/clientlib`](../pkg/clientlib) 的 `Run`。客户端的一切行为——加密协商、身份装载、
DHT 解析、会话重连、数据路径、访客、插件、热重载、管理 API——都在库里，所以同一个客户端
既能是命令行程序，也能是一个 Android App（[`pkg/mobile`](../pkg/mobile)）。壳薄到这个
程度是有意的：命令行与 App 之间没有第二份实现可以走偏。

The command entry of `aethertunnel-client`: a shell around flag parsing, configuration
loading, signal handling and one call into `Run`. Everything the client does lives in
[`pkg/clientlib`](../pkg/clientlib), which is what lets the same implementation also be
an Android app. Keeping the shell this thin is deliberate — there is no second
implementation for the command line and the app to drift apart.

| 旗标 | 作用 |
| --- | --- |
| `--config <路径>` | 配置文件；缺省 `client.toml` |
| `--check` | 只校验配置并退出；与 `--reject-unknown-keys` 一起用时未知键直接失败（`make check` 就是这样校验示例文件的） |
| `--reject-unknown-keys` | 把"不认识这个键"从警告变成错误 |
| `--version` | 打印版本、提交与构建时间；版本打桩在 `clientlib.Version` |
| `--identity` | 打印本客户端的 Ed25519 公钥，用来填服务端的 `identity.allowed_keys` |
| `--discover <名字>` | 以客户端角色经 DHT 解析一个代理名并退出 |

信号：`SIGHUP` 重读配置文件并按名增删代理与访客（改不动的会话级小节会明确提示
"重启后生效"），`SIGINT`/`SIGTERM` 关闭并退出。管理 API 也可以触发同一套重载
（[`pkg/clientlib`](../pkg/clientlib) 的 `POST /api/reload`），容器里只剩信号可用时
`SIGHUP` 就是那条路。

`SIGHUP` re-reads the configuration and adds or removes proxies and visitors by name;
`SIGINT`/`SIGTERM` shut down. The admin API can trigger the same reload, so a
container that only has signals available still has a way in.

测试覆盖旗标组合、退出码与 `--check` 的两种结局；客户端的端到端路径由
`scripts/functional-linux.sh` 与 `scripts/smoke-test.ps1` 用真实二进制驱动。
