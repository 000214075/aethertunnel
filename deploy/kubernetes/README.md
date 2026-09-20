# Kubernetes deployment

## What is created

| Object | Purpose |
| --- | --- |
| `Namespace` `aethertunnel` | Holds everything below. |
| `ConfigMap` `aethertunnel-server` | `server.toml` without any credential. |
| `Secret` `aethertunnel-secrets` | `AETHERTUNNEL_AUTH_TOKEN` and `AETHERTUNNEL_DASHBOARD_TOKEN`. |
| `Deployment` `aethertunnel-server` | One replica running the server image. |
| `Service` `aethertunnel` | Control port 7001/tcp, rendezvous 7002/udp, dashboard 7500/tcp. |

## Ports

| Port | Protocol | Purpose |
| --- | --- | --- |
| 7001 | TCP | Control connections and public proxy ports bound by clients. |
| 7001 | UDP | The DHT node when `[dht] enabled` is true. |
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

The server reads three environment variables and lets them override the file, so the
`ConfigMap` never contains a secret:

| Variable | Overrides |
| --- | --- |
| `AETHERTUNNEL_AUTH_TOKEN` | `[server] auth_token` |
| `AETHERTUNNEL_DASHBOARD_TOKEN` | `[dashboard] token` |
| `AETHERTUNNEL_ENCRYPTION_PASSPHRASE` | `[encryption] passphrase` |

## Tun device

The `[vpn]` section needs a tun device and `NET_ADMIN`. The Deployment carries both
as commented blocks; uncomment them and set `vpn.enabled = true` and
`vpn.address = "10.7.0.0/24"` in the ConfigMap. Without the device the server refuses
to start, and says so in its log.

## State

The audit log, the bandwidth ledger and the ledger's signing key are written to
`/var/lib/aethertunnel`, which is an `emptyDir` here and therefore lost when the pod
is replaced. Replace it with a `PersistentVolumeClaim` before using the ledger as a
record of anything.
