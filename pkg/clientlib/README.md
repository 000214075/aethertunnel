# pkg/clientlib

客户端实现的库形态。`Run(ctx, cfg, logger)` 一个入口承担加密协商、身份装载、DHT 解析
等待与会话重连循环；`RunWithShell` 让移动平台在会话建立、服务器分配好地址的那一刻
交出自己的 tun 设备（`PlatformVPN`/fd 路径）。命令行 `client/` 只是它的一层壳，所以
同一个 `Run` 在三种形态里跑的是同一份代码。

The client as a library. `Run` is the one entry point — cipher negotiation, identity
loading, DHT resolution waiting, session reconnects; `RunWithShell` lets a mobile
platform hand over its own tun device at the moment the session is up and the address
is known. The command line in `client/` is only a shell around it, so all three forms
run the same code.
健康检查的注销/重注册（`withdraw.go`）、经中转代理连服务器（`dialvia.go`）与
SIGHUP 热重载（`reload.go`）也在这层。

Health checks that withdraw and re-publish a proxy (`withdraw.go`), the
dial-through-a-proxy path to the server (`dialvia.go`) and the SIGHUP hot
reload (`reload.go`) live here too.
