# Kubernetes deployment

单节点集群上的一套完整清单，`kubectl apply -k` 即可。它演示两件在本仓库别处也成立的事：
配置里不写凭据（令牌来自 Secret 的环境变量），以及运维面真的接上（探针、状态卷、审计路径）。

The whole set for a single-node cluster, applied with `kubectl apply -k`. It shows two
things that hold elsewhere in this repository too: no credential in the configuration
(the tokens come from the Secret through environment variables), and an operations side
that is really wired up (probes, a state volume, the audit path).

## What is created

| Object | Purpose |
| --- | --- |
| `Namespace` `aethertunnel` | Holds everything below. |
| `ConfigMap` `aethertunnel-server` | `server.toml` without any credential. |
| `Secret` `aethertunnel-secrets` | `AETHERTUNNEL_AUTH_TOKEN` and `AETHERTUNNEL_DASHBOARD_TOKEN`. |
| `Deployment` `aethertunnel-server` | One replica running the server image. |
| `Service` `aethertunnel` | Control port 7001/tcp, rendezvous 7002/udp, DHT 7003/udp, dashboard 7500/tcp. |

## Ports

| Port | Protocol | Purpose |
| --- | --- | --- |
| 7001 | TCP | Control connections (and `[server] bind_port`). A proxy's public port is the `remote_port` its client chose, so it has to be added to the Service as well. |
| 7003 | UDP | The DHT node when `[dht] enabled` is true. |
| 7002 | UDP | XTCP hole punching; `[server] p2p_port`. |
| 7500 | TCP | Dashboard, `/healthz`, `/readyz` and `/metrics`. |

## Apply

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml

kubectl -n aethertunnel create secret generic aethertunnel-secrets \
  --from-literal=auth-token="$(head -c 32 /dev/urandom | base64)" \
  --from-literal=dashboard-token="$(head -c 32 /dev/urandom | base64)"

kubectl apply -k deploy/kubernetes
```

With a locally built image instead of a registry:

```sh
docker build -t aethertunnel:dev .
kubectl -n aethertunnel set image deployment/aethertunnel-server \
  server=aethertunnel:dev
```

## Check

```sh
kubectl -n aethertunnel get pods
kubectl -n aethertunnel logs deploy/aethertunnel-server
kubectl -n aethertunnel port-forward svc/aethertunnel 7500:7500
curl -s localhost:7500/healthz
curl -s localhost:7500/readyz
```

## Credentials

The `ConfigMap` holds no credential. The `Secret` carries two keys, `auth-token` and
`dashboard-token`, and the `Deployment` maps them onto the environment variables the
server reads:

| Secret key | Environment variable | Overrides |
| --- | --- | --- |
| `auth-token` | `AETHERTUNNEL_AUTH_TOKEN` | `[server] auth_token` |
| `dashboard-token` | `AETHERTUNNEL_DASHBOARD_TOKEN` | `[dashboard] token` |

The mapping is written out (`env` + `valueFrom.secretKeyRef`) because `envFrom: secretRef`
would turn each key into a variable of the same name — `auth-token`, not
`AETHERTUNNEL_AUTH_TOKEN` — which the server does not read; it then refuses to start with
`server.auth_token is required`. `AETHERTUNNEL_ENCRYPTION_PASSPHRASE` is not set here
because the `ConfigMap` does not enable `[encryption]`; add it the same way if you do.

## Clients

The `ConfigMap` enables `[obfuscation]` with `disguise = "tls-record"`. **Every client
must set the same disguise**, or it is closed at the handshake and sees only
`connection reset by peer` while it reconnects:

```toml
[client]
server_addr = "<host>:7001"
auth_token = "<the Secret's auth-token>"

[obfuscation]
enabled = true
disguise = "tls-record"
```

`pad_to` and `jitter_millis` do **not** have to match: the frame itself carries whether
it was padded, so each end picks its own. Only the disguise has to agree.

## Tun device

The `[vpn]` section needs a tun device and `NET_ADMIN`. Set `vpn.enabled = true` and
`vpn.address = "10.7.0.0/24"` in the ConfigMap, then make the four edits the comment
above `volumeMounts` in `deployment.yaml` lists: add `NET_ADMIN` to the container's
`securityContext.capabilities`, add the tun `volumeMount`, add the `hostPath` volume,
and drop the pod's `runAsNonRoot` and `runAsUser` — a process whose uid is not 0 never
receives the capability it asks for, so `TUNSETIFF` is refused with `EPERM`. They are
edits to keys that are already there rather than a block to uncomment: a second
`securityContext` or `volumeMounts` is a duplicate key, and the copy that wins silently
drops `readOnlyRootFilesystem` and the state mount. Without the device the server
refuses to start, and says so in its log.

## State

The audit log, the bandwidth ledger and the signing keys — the ledger's and the DHT
announcement key from `[dht] signing_key_file` — are written to
`/var/lib/aethertunnel`, which is an `emptyDir` here and therefore lost when the pod
is replaced. Replace it with a `PersistentVolumeClaim` before using the ledger as a
record of anything, and before handing clients the announcement key: a node restarted
with a fresh key file signs with a key those clients do not trust.
