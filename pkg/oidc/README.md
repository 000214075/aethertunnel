# pkg/oidc

OIDC 的两半：服务端经提供者的发现文档取 JWKS 并验签，客户端用 client-credentials 换一个
访问令牌。两边都不依赖第三方 SDK——JWS 的验签是自己实现的。

Both halves of OIDC: the server fetches the provider's key set through its discovery
document and verifies signatures, and the client exchanges client credentials for an
access token. Neither half pulls in a third-party SDK; the JWS verification is written
here.

| 方向 | 做什么 |
| --- | --- |
| 服务端 | 读发现文档（issuer → jwks_uri）、缓存 JWKS、按 `kid` 取键、验签（RS/PS/ES 系列）、检查 `iss`/`aud`/`exp`；`skip_expiry_check` 与 `skip_issuer_check` 为声明对不上的提供者留出口 |
| 客户端 | 以 client-credentials 向 token 端点换取 access token，按过期时间提前刷新 |

差异写进了对照表：本项目的 `[oidc]` 是**加一把**凭据——配置了 `[oidc]` 的服务端上，
静态 `auth_token` 仍然有效，所以引入 OIDC 不会把已有的客户端关在门外。

The difference from frp is deliberate: `[oidc]` here adds a credential rather than
replacing one, so a static `auth_token` still works on a server that has OIDC
configured.
