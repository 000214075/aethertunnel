# pkg/mobile

gomobile 绑定层。`Run(configTOML)` / `Stop()` 驱动与命令行同款的客户端，配置直接
以 TOML 字符串内嵌；`RunVPN` + `PlatformVPN` 接口让 Android 的 VpnService（或 iOS
的 NetworkExtension）在会话建立、服务器分配好地址的那一刻交出 tun 描述符。

The gomobile binding. `Run`/`Stop` drive the same client the command runs, with
the configuration embedded as a TOML string; `RunVPN` + `PlatformVPN` let
Android's VpnService (or iOS's NetworkExtension) hand over a tun descriptor at
the moment the session is up.
