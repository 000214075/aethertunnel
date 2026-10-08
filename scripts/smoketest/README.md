# scripts/smoketest/

功能套件共用的辅助程序：套件需要的本地服务（TCP 回显、UDP 回显、HTTP、会在半关闭后
再应答的 halfclose，以及证书生成）都在这里，所以跑一遍功能检查不依赖 Python 或任何
其他运行时。static_file 与 SOCKS5 由客户端自己的插件实现，不在这里。
`functional-linux.sh` 与 `smoke-test.ps1` 现场编译它，再让真实的服务端与客户端二进制穿过
隧道访问它——检查的是真的往返过的字节。

The helper the functional suites share: every local service they need (TCP echo, UDP echo,
HTTP, halfclose, which answers only after the request's write side is closed, and
certificate generation) lives here, so a full run depends on no other runtime. The
static_file and SOCKS5 endpoints are the client's own plugins, not this helper's. `functional-linux.sh` and `smoke-test.ps1` compile it on the
fly and let the real server and client binaries reach it through the tunnel, so what is
checked is bytes that really made the round trip.

它绑定回环端口，把地址以一个 JSON 对象打印到标准输出，然后一直服务到被杀掉：

| 字段 | 服务 |
| --- | --- |
| `tcp` | TCP 回显 |
| `udp` | UDP 回显，按来源地址分会话 |
| `http` | 最小 HTTP 服务，把 `host=` 与 `path=` 答回，用来验证按 Host 选隧道与头注入 |
| `halfclose` | 读到对端关闭写方向**之后**才回答，用来钉住"半关闭仍然能收到应答" |

也支持 `-cert-dir <目录> [-cert-hosts a,b]`：生成一张自签证书（`server.crt`/`server.key`）
后退出，供 TLS 传输层、https 虚拟主机与 SNI 透传的检查使用。

It binds loopback, prints its addresses as one JSON object on stdout and serves until
killed; `-cert-dir` makes it write a self-signed certificate and exit instead.
