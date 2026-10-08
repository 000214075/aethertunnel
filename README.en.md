# AetherTunnel

**Publish a service behind NAT — and embed the client in your own program.**

[中文](README.md) · **English**

[![Release](https://img.shields.io/github/v/tag/000214075/aethertunnel?label=release)](https://github.com/000214075/aethertunnel/releases)
[![License](https://img.shields.io/github/license/000214075/aethertunnel?label=license)](LICENSE)

AetherTunnel is a reverse proxy for NAT traversal: it speaks nine proxy types — TCP, UDP, HTTP,
HTTPS, SOCKS5 and more — and it supports P2P. A service on a machine without a public address
(your NAS, a work box, a Raspberry Pi) is published through a server that has one, and the client
is also a library you can embed in your own program.

```
visitor ──► server public port ──► [tunnel] ──► client ──► local service 127.0.0.1:22
```

**Coming from frp?** The configuration vocabulary is deliberately similar (`[[proxies]]`,
visitors, `remote_port`, `stcp`/`xtcp`, per-proxy rate limits); the comparison table, the
differences and what was deliberately not copied are in [`docs/VS-FRP.md`](docs/VS-FRP.md).
Paste frp's key names (`bindPort`, `localIP`, `customDomains`, `[common]`) and the loader tells
you what each one becomes here.

## Why use AetherTunnel?

- **Nine proxy types** — `tcp` `udp` `http` `https` `stcp` `sudp` `xtcp` `socks5` `tcpmux`:
  from whole-port forwarding and host-routed web services to private tunnels and forward proxies.
- **Pools and balancing** — one name can be served by several clients: `round-robin` `random`
  `latency` `failover` `adaptive`, plus `bandit`, which learns each member's answering speed
  online. Members can come and go without dropping traffic.
- **P2P hole punching** — a successful `xtcp` punch connects the visitor straight to the client,
  with no traffic through the server; when the punch fails it falls back to the relay.
- **Security** — optional end-to-end encryption (XChaCha20-Poly1305 / AES-256-GCM) with
  post-quantum hybrid agreement (X25519 + ML-KEM-768); TLS transport, Ed25519 host identity,
  source allow/deny lists, a token-bucket rate limit, automatic bans after failed logins and a
  JSONL audit log; `stcp`/`xtcp` visitors can authenticate with a zk-SNARK proof that never
  sends the key itself.
- **Decentralized directory** — a built-in Kademlia DHT resolves services by name against a
  public key alone; announcements are signed, carry a TTL and can be withdrawn.
- **A bandwidth ledger that verifies offline** — Ed25519-signed and hash-chained usage records,
  **verifiable with only the public key**, with a prefix export up to entry n for an auditor.
- **Layer-3 tunnel** — real devices on Linux (TUN), Windows (Wintun) and macOS (utun); on Android
  the app's VpnService provides the interface. Where no device can be opened it refuses to start
  rather than degrading silently.
- **Native client plugins** — static file publishing, an HTTP reverse proxy, TLS termination and
  protocol conversion (`https2http`, `tls2raw` and more), forward proxies; useful as standalone
  little tools even without a local service.
- **A web panel on both ends** — bilingual, phone-friendly; Prometheus metrics, `/healthz` and
  `/readyz` probes, graceful shutdown, and a configuration check that **reports** unknown keys
  instead of ignoring them.
- **One implementation, three forms** — the command line, the embeddable `pkg/clientlib` and the
  Android app (gomobile binding + minimal APK); `Dockerfile` and `deploy/kubernetes/` ship ready
  to use.

Every capability has executable checks that ship with the repository; the details live in each
directory's README and under [`docs/`](docs/README.md).

## Quick start

```bash
go build -o aethertunnel-server .          # server
go build -o aethertunnel-client ./client   # client
```

Server (`server.toml`) — bind address, port, a shared `auth_token`, and the dashboard:

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7001
auth_token = "replace-with-at-least-16-random-characters"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"   # set a token before exposing it
port = 7500
```

Client (`client.toml`) — where the server is and which local ports to publish:

```toml
[client]
server_addr = "your-server:7001"
auth_token = "same value as the server"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 6022        # opened on the server
```

```bash
./aethertunnel-server --config server.toml
./aethertunnel-client --config client.toml
ssh -p 6022 user@your-server
```

The dashboard is at `http://127.0.0.1:7500/` on the server itself and shows live clients, tunnels and traffic; to reach it from another machine, set `dashboard.bind_addr = "0.0.0.0"` and a `token`.

## Common recipes

### Publish a web service under a domain

`http`/`https` proxies ride the server's shared listener and are selected by the request's Host
header — the server needs no port per service.

```toml
# server
[server]
bind_port = 7001
http_port = 7080                       # the shared listener for http/https proxies
subdomain_host = "tunnel.example.com"  # suffix used when a client declares only a label
```

```toml
# client
[[proxies]]
name = "web"
type = "http"
local_port = 8000
domains = ["app.example.com"]   # or just subdomain = "app", which the server joins with subdomain_host
```

### Give a phone a SOCKS5 exit

Let a phone (or anything that speaks SOCKS5) reach the network the client sits in:

```toml
[[proxies]]
name = "phone-exit"
type = "socks5"
remote_port = 6100
allow_targets = ["192.168.1.0/24"]   # required: the only addresses this exit will dial
```

Point the phone's SOCKS5 proxy at `your-server:6100` and it is done.

### A private tunnel: only those with the secret

```toml
# the publisher: this tunnel occupies no public port
[[proxies]]
name = "db"
type = "stcp"
local_port = 5432
secret_key = "a random string only the visitor knows"
```

```toml
# the visitor: opens a local port and forwards it into the private tunnel
[[visitors]]
name = "db-visitor"
type = "stcp"
server_name = "db"
bind_addr = "127.0.0.1"
bind_port = 15432
secret_key = "identical to the publisher's"
```

```bash
psql -h 127.0.0.1 -p 15432            # the wrong secret gets nothing at all
```

### Embed the client in your own program

```go
cfg, err := config.LoadClient("client.toml")
if err != nil {
    return err
}
// logger is a *log.Logger; Run keeps going until ctx is cancelled
return clientlib.Run(ctx, cfg, logger)
```

## Repository layout

Every directory carries its own README, and each one says what the code in it
actually does.

| Directory | Contents |
| --- | --- |
| [`client/`](client/) | the `aethertunnel-client` command (flags, configuration, signals) |
| [`pkg/`](pkg/) | every implementation package: clientlib, server, vpn, obfs, snarkauth, webrtcvisitor — each with its own README |
| [`mobile/`](mobile/) | the gomobile binding and the minimal Android app, VpnService shell included |
| [`web/`](web/) | the embedded dashboard |
| [`deploy/`](deploy/) | Kubernetes manifests |
| [`scripts/`](scripts/) | the builders, the functional suites and the per-platform test scripts |
| [`tools/`](tools/) | SNARK circuit and key generation |
| [`docs/`](docs/) | architecture, configuration reference, platform records, security model, scope decisions |

## Scope

Two items remain **deliberate design decisions** rather than pending work: blockchains and
tokens, and the store-distributed mobile app with its on-device VPN. The reasoning behind each,
and what this program offers instead for the same purpose, is in
[`docs/NOT-IN-THIS-VERSION.md`](docs/NOT-IN-THIS-VERSION.md). The client is an embeddable
library with a gomobile binding and a minimal Android app, so nothing about the mobile story
is missing except the store listing and a physical-device run.

## Encryption

Both ends must agree, or the handshake fails with a clear `encryption mismatch`:

```toml
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"   # or "aes-256-gcm"
passphrase = ""                     # empty = derive from auth_token
salt = "aethertunnel"               # must match on both ends
post_quantum = true                 # agree per-connection keys with X25519 + ML-KEM-768
```

The key is `HKDF-SHA256(passphrase, salt)`, not the token itself. Every control frame and
every stream record carries a fresh random nonce, and a tampered record is rejected by the
AEAD and drops the connection. With `post_quantum` on, the session key is derived from both
an X25519 and an ML-KEM-768 secret, and every data connection derives its own key with
`HKDF(session_key, stream_id)`.

## Dashboard

Assets are embedded with `go:embed`, so a released binary serves its own UI with nothing next
to it. It listens on loopback by default; set `[dashboard].token` to expose it, after which
`/api/*` needs `Authorization: Bearer <token>` (`/api/health`, `/healthz` and `/readyz` stay
public and carry no secrets). Every number on screen comes from live server state; empty lists
say they are empty.

- `/api/ledger` returns the signing public key, the chain head, the most recent entries and the
  per-client totals. The entries are already signed, so a reader can check them independently.
  The dashboard's Ledger view renders this response (`?limit=` caps the entries it asks for).
- `/api/dht` returns the DHT node identity, its bound address, the number of known peers, the
  host it advertises and the proxy names it currently announces. The dashboard's Directory
  view renders this response.

## Operations

```bash
# validate a configuration without starting
./aethertunnel-server --config server.toml --check

# resolve a proxy record by name (the server's own configuration works: the query node binds
# an ephemeral port)
./aethertunnel-server --config server.toml --dht-lookup ssh

# the same from a client-role configuration
./aethertunnel-client --config client.toml --discover ssh

# print this client's public identity key, to put into the server's identity.allowed_keys
./aethertunnel-client --config client.toml --identity

# check a bandwidth ledger offline; only the public key is needed
./aethertunnel-server --verify-ledger ledger.jsonl --ledger-key <64 hex characters>

# write the ledger prefix up to entry 41: hand an auditor that period only
./aethertunnel-server --ledger-proof ledger.jsonl --proof-index 41 > proof.jsonl
```

Every capability has a check you can run yourself from this repository (the functional suites,
the layer-3 tunnel on real tun devices, the manifests on a real k3s, the dashboard in a real
browser); the commands are under "Build and test" below, and what each script does is in
[`scripts/README.md`](scripts/README.md).

To verify graceful shutdown by hand: start a server, keep one stream open, then `kill -TERM <PID>`
on Linux and macOS (on Windows, `taskkill /PID <PID>` without `/F` — `Stop-Process` is a hard kill
that skips the whole drain). The log should show `shutting down:` and one `graceful shutdown: ...`
line.

## Build and test

```bash
make build     # both binaries into bin/
make test      # unit + end-to-end tests
make test-race # the same under the race detector (needs CGO and a C compiler; scripts/race-toolchain.sh unpacks one without root)
make lint      # files that are not gofmt-ed, then go vet
make vet
make cross     # 12 artifacts + dist/SHA256SUMS
make check     # validate the example configs (an unknown key fails)
```

Run the checks (Linux and arm64 bind loopback ports only; no root needed):

```bash
scripts/functional-linux.sh bin/aethertunnel-server bin/aethertunnel-client
# last line: ALL LINUX FUNCTIONAL CHECKS PASSED
```

The layer-3 tunnel and the Kubernetes deployment have harnesses of their own; they need root and
extra components, and when one is missing they say which and exit 0 rather than misreporting an
environment problem (script notes in [`scripts/README.md`](scripts/README.md)):

```bash
sudo bash scripts/vpn-linux-test.sh bin/aethertunnel-server bin/aethertunnel-client
# last line: ALL LAYER-3 CHECKS PASSED
sudo bash scripts/kubernetes-linux.sh bin/aethertunnel-server bin/aethertunnel-client
# last line: ALL KUBERNETES CHECKS PASSED
```

With no arm64 machine to hand, `scripts/emulate-linux-arm64.sh <qemu-aarch64> [sysroot]`
cross-compiles the arm64 artifacts, runs the whole unit suite under qemu, and hands wrapper
scripts to `scripts/functional-linux.sh` for the full functional battery. The static binaries need
no sysroot.

## Security model, stated plainly

- Authentication is a shared secret (`auth_token`) or an Ed25519 identity, and neither is a
  certificate. The token comparison is constant-time and a failed attempt never writes the
  offered token to the log.
- `[encryption]` protects the payload, `[transport] enable_tls` provides transport encryption
  and server authentication, and `[obfuscation] disguise` changes only the appearance: it
  provides no confidentiality.
- The dashboard has a single Bearer token: no user accounts, no sessions.
- A private tunnel's `secret_key` is not sent on the wire when `auth_method = "nizk"`, but the
  proxy's metadata (name, type, address) is published to the DHT unless `[dht]` is disabled.
  With `[dht] signing_key_file` those records carry a signature, which a reader uses to refuse
  a rewritten one.
- The automatic ban counts by **source address**: behind a load balancer, a reverse proxy or a
  monitoring host, those addresses belong in `ban_ignore_cidrs`, or they share the source
  address of an attacker and are refused along with it.

See [`docs/SECURITY.md`](docs/SECURITY.md) for the full list, including what an attacker on
the path can still learn.

---

## Contributing

- **Report a problem**: bugs and feature requests each have a form (see
  [`.github/ISSUE_TEMPLATE/`](.github/ISSUE_TEMPLATE/)); **do not open security issues in public** —
  report them privately as [`docs/SECURITY.md`](docs/SECURITY.md) describes.
- **Change code**: [`CONTRIBUTING.md`](CONTRIBUTING.md) covers local development, the check list and
  "one PR does one thing"; the code of conduct is [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
- **Every capability has an executable check**: a new capability ships with tests and a section of
  the functional suite — that is this repository's bar, and the reason every claim in the
  capability list can be trusted.

## License

MIT — see [LICENSE](LICENSE).
