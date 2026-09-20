# 架构 · Architecture

**English summary.** AetherTunnel has one listening socket per server and two roles on it:
clients keep a long-lived *control connection* (auth, heartbeat, tunnel registration), and
open a short *data connection* per visiting connection, which then carries raw bytes. The
server binds one public port per tunnel and matches each visitor with the data connection
the client opens for it, so a client behind NAT never needs an inbound port. Sessions and
tunnels are registered in managers that remove them on disconnect, and all state the
dashboard shows is read from those managers — there is no second copy to drift.

---

## 1. 角色与连接

```
                        ┌──────────────────── 服务器 ────────────────────┐
内网机器                  │                                               │
┌──────────────┐  控制连接 │  :7001  ┌──────────────────────────────┐      │
│ aethertunnel │ ────────►│ ┌─────► │ SessionManager / TunnelManager│      │
│   -client    │          │ │       └──────────────────────────────┘      │
│              │  数据连接 │ │                                             │
│  ┌────────┐  │ ────────►│ │  tunnel "ssh" :6022 ┌─────────────────┐     │
│  │ 本地服务│◄─┼──────────┼─┘◄────────────────────│ 访问者           │     │
│  │ :22    │  │          │                        └─────────────────┘     │
│  └────────┘  │          │                                              │
└──────────────┘          └──────────────────────────────────────────────┘
```

- **控制连接**：客户端启动后建立，发送 `AuthRequest`，之后保持长连接用于心跳、注册隧道、
  接收 `DataRequest`。一个客户端只有一条。
- **数据连接**：每当有访问者连到某条隧道的公网端口，服务器通过控制连接发一个
  `DataRequest{proxy, stream_id}`，客户端**新拨一条** TCP 连接回来，发
  `DataOpen{session, proxy, stream_id}`；服务器把这条连接与正在等待的访问者配对，
  此后双方只走原始字节（加密开启时走记录层）。
- 两条角色复用同一个端口，防火墙只需放行一个端口；服务器不区分"控制端口"和"数据端口"。

## 2. 为什么是"客户端回拨"

客户端通常在 NAT 后面，服务器无法主动连它。因此数据面必须由客户端发起：服务器只负责
"告诉客户端有人来了"和"把两条连接对接起来"。

## 3. 帧格式

```
+--------+--------+------------------+-------------------+
| type:1 | flags:1| length:4 (BE)    | payload:length    |
+--------+--------+------------------+-------------------+
```

- `length` 允许为 0（心跳），读取前先与上限（默认 1 MiB）比较，**不会**按对端声明的长度
  直接分配内存。
- `flags` 目前只用了 bit0 = 已加密。两端加密设置不一致时，读取端会给出
  `encryption mismatch` 而不是让请求不断失败。
- 写入用单次 `Write` 提交整帧，并由互斥锁串行化，因此心跳线程与数据请求线程不会交叉写坏。

## 4. 隧道数据面

```
访问者连接 ──► Tunnel.acceptLoop ──► 分配 streamID
                                    │
              控制连接 ─────────────┘  DataRequest
                                    │
客户端新拨数据连接 ──► handleData ──► 按 streamID 找到等待中的访问者
                                    │
                                    └─► flynet.Pipe(访问者, 数据连接)
```

`flynet.Pipe` 的要点：

- 两个方向各自独立，用 `CloseWrite` 半关闭，所以 HTTP/1.0、SSH 这类"先关一个方向"的
  协议不会被截断（旧实现任一方向结束就关掉两条 socket）。
- 双向复制各带空闲超时（读超时），防止对端静默时永久占用 goroutine 与 fd。
- 32 KiB 缓冲来自 `sync.Pool`。

## 5. 状态与生命周期

| 对象 | 谁持有 | 何时释放 |
|---|---|---|
| `Session` | `SessionManager`（按 id） | 控制连接结束、心跳超时、面板断开 |
| `Tunnel` | `TunnelManager`（按名字）+ 所属 `Session` | 会话结束时（自动）、或注册被替换 |
| 等待中的访问者连接 | `Session.pending`（按 streamID） | 被数据连接认领、超时、或会话关闭时全部失败关闭 |

释放路径只有一条：`Session.Close`（幂等）→ 关闭 pending → 关闭隧道监听器 → 关 socket；
随后会话的属主把隧道从 `TunnelManager` 注销。旧版本的问题正是这条路径不完整：会话表
只增不减，隧道表在关闭时被提前清空。

## 6. 目录结构

```
main.go                 服务端入口（flag 解析、加载配置、面板、信号处理）
client/main.go          客户端入口（会话循环、重连、每条流的转发）
pkg/config              配置加载、默认值、校验（未知键会被报告）
pkg/protocol            帧格式、消息类型、JSON 负载结构
pkg/crypto              HKDF 派生、AEAD 封装、记录层、常数时间比较
pkg/net                 双向复制（半关闭、超时、缓冲池）
pkg/server              Server / SessionManager / TunnelManager / Dashboard
web/embed.go            go:embed 的入口
web/dashboard           面板单页（内嵌进二进制）
scripts/build-release.* 跨平台构建与校验和
```

## 7. 扩展点

- **新的代理类型**（UDP、HTTP 等）：在 `TunnelManager.Register` 的类型检查处放开，
  并在数据面按类型选择搬运方式；目前只有 `tcp` 被接受，其他类型会明确报错。
- **新的数据面传输**：`handleData` 收到 `DataOpen` 后把连接交给配对逻辑，替换这一步即可
  接入 WebSocket/QUIC 之类的承载。
- **指标输出**：所有计数都在 `Tunnel` 与 `Session` 的原子字段里，`pkg/server/dashboard.go`
  是唯一的读取方，接入 Prometheus 只需再写一个读取方。
