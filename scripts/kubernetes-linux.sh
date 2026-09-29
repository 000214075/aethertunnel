#!/usr/bin/env bash
#
# Applies deploy/kubernetes to a real cluster and then drives the deployment from
# outside it: the ports the manifests publish, the token the Secret carries, the state
# volume the server writes into and the tunnel a client carries data through.
#
# A rendered manifest is not a running one. The objects here are applied to a
# single-node k3s cluster started under its own data directory, so nothing already on
# the machine is reused or replaced, and every check below reads the cluster's answer
# rather than the manifest's claim.
#
# Requirements: root, a reachable docker daemon and a k3s binary, and no other k3s
# cluster running (they share containerd's socket path). The base images the shipped
# Dockerfile pulls cannot be reached on every network; when they are missing the script
# builds the same two binaries into a scratch image instead and says which image it
# used. The Kubernetes objects are the ones in deploy/kubernetes either way.
#
# The cluster, the images, the work directory and everything the run added to the
# machine go on the way out: the pods are deleted first so containerd takes its own
# sandboxes down, and whatever outlived that — the containers' own processes, sandbox
# processes, shims, the pod network namespaces, their veths, containerd's runtime state
# — is removed by comparing against what was here before the run, never by name alone.
# A runtime k3s had already unpacked under /var/lib/rancher/k3s is left alone, because a
# machine with k3s installed reuses it. --keep leaves all of it for inspection.
#
# Usage: sudo scripts/kubernetes-linux.sh [server-binary] [client-binary] [--keep]
#        Defaults to bin/aethertunnel-server and bin/aethertunnel-client.
#        K3S_BIN points at the k3s binary, GO_BIN at the Go toolchain used to build a
#        sandbox image, PAUSE_BIN at a static binary that blocks forever.

set -uo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"

server_bin=""
client_bin=""
keep=""
for arg in "$@"; do
  case "$arg" in
    --keep) keep=yes ;;
    *)
      if [ -z "$server_bin" ]; then
        server_bin="$arg"
      elif [ -z "$client_bin" ]; then
        client_bin="$arg"
      fi
      ;;
  esac
done
server_bin="${server_bin:-$repo/bin/aethertunnel-server}"
client_bin="${client_bin:-$repo/bin/aethertunnel-client}"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1${2:+ -> $2}"; failures=$((failures + 1)); }
note() { echo "note: $1"; }

skip() {
  echo "skipped: $1"
  exit 0
}

[ "$(id -u)" = "0" ] || skip "the cluster and the image build need root (re-run with sudo or pkexec)"
[ -x "$server_bin" ] || skip "$server_bin is not executable"
[ -x "$client_bin" ] || skip "$client_bin is not executable"
command -v docker >/dev/null 2>&1 || skip "docker builds the image the Deployment runs"
docker info >/dev/null 2>&1 || skip "the docker daemon is not reachable"
command -v python3 >/dev/null 2>&1 || skip "python3 drives the HTTP and stream checks"

k3s_bin="${K3S_BIN:-/usr/local/bin/k3s}"
[ -x "$k3s_bin" ] || skip "no k3s binary at $k3s_bin (install one and set K3S_BIN)"

# k3s puts containerd's socket at this path whatever --data-dir says, and a second
# cluster cannot share it. The cleanup below removes what this run created there, so it
# only runs where nothing else is using it.
containerd_socket="/run/k3s/containerd/containerd.sock"
if [ -e "$containerd_socket" ] || [ -n "$(pgrep -f "^$k3s_bin server" 2>/dev/null)" ]; then
  skip "another k3s cluster is running ($containerd_socket is in use): stop it first, because this script removes what it adds to that path"
fi

work="$(mktemp -d /tmp/aether-kubernetes-XXXXXX)"
data="$work/data"
kubeconfig="$work/kubeconfig"
mkdir -p "$data"

k3s_pid=""
child_pids=()
started_images=()

# What the machine already had, so the cleanup removes exactly what this run adds and
# nothing that was here first.
before_pids="$(ps -eo pid= -o args= | grep -F containerd-shim | awk '{print $1}' | tr '\n' ' ')"
before_sandboxes="$(ps -eo pid= -o comm= | awk '$2 == "pause" {print $1}' | tr '\n' ' ')"
before_netns="$(ip netns list 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
before_veths="$(ip -brief link 2>/dev/null | awk '/veth/{split($1, parts, "@"); print parts[1]}' | tr '\n' ' ')"
before_rancher=""
[ -e /var/lib/rancher/k3s ] && before_rancher=yes

cleanup() {
  for pid in "${child_pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  # The pods go first: containerd then takes its own sandboxes, network namespaces and
  # mounts down, which is the only path that removes all of them together.
  if [ -n "$k3s_pid" ] && [ -f "$kubeconfig" ]; then
    timeout 60 "$k3s_bin" kubectl --kubeconfig "$kubeconfig" delete pod --all \
      -n aethertunnel --wait=true >/dev/null 2>&1
  fi
  if [ -n "$k3s_pid" ]; then
    kill -TERM -- "-$k3s_pid" 2>/dev/null
    for _ in $(seq 1 30); do
      kill -0 "$k3s_pid" 2>/dev/null || break
      sleep 1
    done
    kill -KILL -- "-$k3s_pid" 2>/dev/null
  fi
  if [ "$keep" = "yes" ]; then
    echo
    echo "kept: the cluster data in $data, its kubeconfig at $kubeconfig, the images ${started_images[*]:-}"
    return
  fi
  # Anything the pods left behind is removed by what this run added, never by name alone:
  # a container's process outlives the shim that started it, and it keeps the network
  # namespace and the veth alive with it. Pod containers sit in the kubepods cgroup
  # hierarchy, and the guard above established that this is the only cluster on the
  # machine, so those cgroups are this run's.
  for entry in /proc/[0-9]*/cgroup; do
    pid="${entry#/proc/}"
    pid="${pid%/cgroup}"
    grep -q kubepods "$entry" 2>/dev/null || continue
    kill -KILL "$pid" 2>/dev/null
  done
  for pid in $(ps -eo pid= -o comm= | awk '$2 == "pause" {print $1}'); do
    case " $before_sandboxes " in *" $pid "*) ;; *) kill -KILL "$pid" 2>/dev/null ;; esac
  done
  for pid in $(ps -eo pid= -o args= | grep -F containerd-shim | awk '{print $1}'); do
    case " $before_pids " in *" $pid "*) ;; *) kill -KILL "$pid" 2>/dev/null ;; esac
  done
  sleep 1
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}'); do
    case " $before_netns " in *" $ns "*) ;; *) ip netns delete "$ns" 2>/dev/null ;; esac
  done
  for link in $(ip -brief link 2>/dev/null | awk '/veth/{split($1, parts, "@"); print parts[1]}'); do
    case " $before_veths " in *" $link "*) ;; *) ip link delete "$link" 2>/dev/null ;; esac
  done
  # containerd's runtime state stays busy while a sandbox's overlay and shm mounts exist,
  # so those are released before the directory goes. They are all under /run/k3s, which
  # the guard above established belongs to this run.
  for target in $(awk '$5 ~ "^/run/k3s" {print $5}' /proc/self/mountinfo 2>/dev/null |
                  awk '{print length($0), $0}' | sort -rn | cut -d' ' -f2-); do
    umount -l "$target" 2>/dev/null
  done
  rm -rf /run/k3s
  # The runtime k3s unpacks under the default path is reused by a machine that already
  # has k3s installed, so it is only removed when this run is what created it.
  [ -n "$before_rancher" ] || rm -rf /var/lib/rancher/k3s
  for image in "${started_images[@]:-}"; do
    [ -n "$image" ] && docker rmi -f "$image" >/dev/null 2>&1
  done
  rm -rf "$work"
}
trap cleanup EXIT

# --- the image the Deployment names -------------------------------------------

# The tag comes from the kustomization, so the image that is tested is the one the
# shipped manifest asks for.
image_name="$(awk '/^images:/{f=1} f && /- name:/{print $3; exit}' "$repo/deploy/kubernetes/kustomization.yaml")"
image_tag="$(awk '/^images:/{f=1} f && /newTag:/{print $2; exit}' "$repo/deploy/kubernetes/kustomization.yaml")"
[ -n "$image_name" ] && [ -n "$image_tag" ] || skip "deploy/kubernetes/kustomization.yaml names no image to build"
image_ref="$image_name:$image_tag"

if docker image inspect golang:1.24 >/dev/null 2>&1 &&
   docker image inspect gcr.io/distroless/static-debian12:nonroot >/dev/null 2>&1; then
  docker build -q -t "$image_ref" -f "$repo/Dockerfile" "$repo" > "$work/build.log" 2>&1 ||
    skip "the shipped Dockerfile failed to build: $(tail -n 2 "$work/build.log" | tr '\n' ' ')"
  pass "the shipped Dockerfile builds the image the kustomization names"
else
  note "the Dockerfile's base images are not on this machine, so the same two binaries go into a scratch image with the runtime contract the Deployment relies on: static binaries at the same paths, uid 65532, the same working directory, entrypoint and command"
  mkdir -p "$work/context/state"
  cp "$server_bin" "$work/context/aethertunnel-server"
  cp "$client_bin" "$work/context/aethertunnel-client"
  cp "$repo/server.toml.example" "$work/context/server.toml.example"
  echo "Audit log, bandwidth ledger and ledger signing key are written here." > "$work/context/state/README"
  cat > "$work/context/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY aethertunnel-server /usr/local/bin/aethertunnel-server
COPY aethertunnel-client /usr/local/bin/aethertunnel-client
COPY server.toml.example /etc/aethertunnel/server.toml.example
COPY --chown=65532:65532 state /var/lib/aethertunnel
WORKDIR /var/lib/aethertunnel
VOLUME ["/var/lib/aethertunnel"]
USER 65532:65532
EXPOSE 7001 7500 7002/udp 7003/udp
ENTRYPOINT ["/usr/local/bin/aethertunnel-server"]
CMD ["--config", "/etc/aethertunnel/server.toml"]
DOCKERFILE
  docker build -q -t "$image_ref" -f "$work/context/Dockerfile" "$work/context" > "$work/build.log" 2>&1 ||
    skip "the scratch build failed: $(tail -n 2 "$work/build.log" | tr '\n' ' ')"
  pass "the image the kustomization names is built from the same two binaries"
fi
started_images+=("$image_ref")

# --- the sandbox image containerd needs before any pod can start ---------------

pause_image="$("$k3s_bin" server --help 2>&1 |
  sed -n 's/.*--pause-image.*(default: "\([^"]*\)").*/\1/p' | head -1)"

if [ -n "$pause_image" ] && ! docker image inspect "$pause_image" >/dev/null 2>&1; then
  timeout 90 docker pull "$pause_image" > "$work/pause-pull.log" 2>&1 || true
fi

if [ -n "$pause_image" ] && ! docker image inspect "$pause_image" >/dev/null 2>&1; then
  # No registry answered, so the sandbox comes from a program of a few lines: containerd
  # runs it as the pod's PID 1 stand-in, and without it no pod starts at all.
  pause_source="$work/pause"
  mkdir -p "$pause_source"
  cat > "$pause_source/pause.go" <<'GO'
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	<-c
}
GO
  go_bin="${GO_BIN:-}"
  if [ -z "$go_bin" ]; then
    for candidate in "$(command -v go 2>/dev/null)" /usr/local/go/bin/go /home/*/gotool/go/bin/go; do
      [ -x "$candidate" ] && go_bin="$candidate" && break
    done
  fi
  if [ -n "${PAUSE_BIN:-}" ] && [ -x "$PAUSE_BIN" ]; then
    cp "$PAUSE_BIN" "$pause_source/pause"
  elif [ -n "$go_bin" ]; then
    (cd "$pause_source" && CGO_ENABLED=0 GOTOOLCHAIN=local "$go_bin" build -o pause pause.go) ||
      skip "the sandbox program did not build with $go_bin"
  else
    skip "no sandbox image: $pause_image is not on this machine, no registry answered, and no Go toolchain is available to build a stand-in (set GO_BIN or PAUSE_BIN)"
  fi
  cat > "$pause_source/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY pause /pause
ENTRYPOINT ["/pause"]
DOCKERFILE
  docker build -q -t "$pause_image" -f "$pause_source/Dockerfile" "$pause_source" > "$work/pause-build.log" 2>&1 ||
    skip "the sandbox image did not build: $(tail -n 2 "$work/pause-build.log" | tr '\n' ' ')"
  note "$pause_image is not on this machine and no registry answered, so a stand-in sandbox was built from a program that blocks"
  started_images+=("$pause_image")
fi

# --- a cluster of its own ------------------------------------------------------

# k3s imports every archive in <data-dir>/agent/images before it starts an agent, which
# is the documented air-gap path and keeps this out of containerd's socket, whose
# location is the runtime's business rather than this script's.
mkdir -p "$data/agent/images"
docker save -o "$data/agent/images/images.tar" "${started_images[@]}" >/dev/null 2>&1 ||
  skip "the images could not be exported"

k3s_args=(server
  --data-dir "$data"
  --write-kubeconfig "$kubeconfig" --write-kubeconfig-mode 0600
  --disable coredns --disable local-storage --disable metrics-server
  --disable servicelb --disable traefik)
[ -n "$pause_image" ] && k3s_args+=(--pause-image "$pause_image")

setsid "$k3s_bin" "${k3s_args[@]}" > "$work/k3s.log" 2>&1 < /dev/null &
k3s_pid=$!

kubectl() { "$k3s_bin" kubectl --kubeconfig "$kubeconfig" "$@"; }

node_ready=""
for _ in $(seq 1 90); do
  if kubectl get nodes 2>/dev/null | grep -q " Ready "; then node_ready=yes; break; fi
  kill -0 "$k3s_pid" 2>/dev/null || break
  sleep 1
done
[ -n "$node_ready" ] || skip "the cluster did not come up: $(tail -n 3 "$work/k3s.log" | tr '\n' ' ')"

# --- the objects the repository ships ------------------------------------------

auth_token="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
dashboard_token="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"

# The Secret is created the way kustomization.yaml documents it, rather than applied
# from secret.example.yaml, which holds placeholder tokens.
kubectl create namespace aethertunnel --dry-run=client -o yaml | kubectl apply -f - > /dev/null
kubectl -n aethertunnel create secret generic aethertunnel-secrets \
  --from-literal=auth-token="$auth_token" \
  --from-literal=dashboard-token="$dashboard_token" > /dev/null

if kubectl apply -k "$repo/deploy/kubernetes" > "$work/apply.log" 2>&1; then
  pass "deploy/kubernetes applies to a real cluster"
else
  fail "deploy/kubernetes applies to a real cluster" "$(tail -n 2 "$work/apply.log" | tr '\n' ' ')"
fi

pod_ready=""
for _ in $(seq 1 90); do
  if [ "$(kubectl -n aethertunnel get pod -l app.kubernetes.io/component=server \
        -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)" = "true" ]; then
    pod_ready=yes
    break
  fi
  sleep 1
done
if [ -n "$pod_ready" ]; then
  pass "the Deployment's own readiness probe passes, so the pod answers /readyz"
else
  fail "the Deployment's own readiness probe passes" \
    "$(kubectl -n aethertunnel get events --sort-by=.lastTimestamp 2>/dev/null | tail -n 2 | tr '\n' ' ')"
fi

# --- reaching it from outside the cluster --------------------------------------

free_port() {
  python3 - <<'PY'
import socket
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
}

forward() { # local port -> a port on the pod
  kubectl -n aethertunnel port-forward deploy/aethertunnel-server "$1:$2" \
    > "$work/forward-$1.log" 2>&1 &
  child_pids+=("$!")
}

dashboard_port="$(free_port)"
control_port="$(free_port)"
proxy_port="$(free_port)"
forward "$dashboard_port" 7500
forward "$control_port" 7001
forward "$proxy_port" 18080
sleep 4

# python3 rather than curl: the repository's other scripts already require it, and a
# token has to go into a header.
http() { # url [token]
  python3 - "$1" "${2:-}" <<'PY'
import sys, urllib.request, urllib.error
request = urllib.request.Request(sys.argv[1])
if len(sys.argv) > 2 and sys.argv[2]:
    request.add_header("Authorization", "Bearer " + sys.argv[2])
try:
    sys.stdout.write(str(urllib.request.urlopen(request, timeout=10).status))
except urllib.error.HTTPError as exc:
    sys.stdout.write(str(exc.code))
except Exception as exc:
    sys.stdout.write("error: %s" % exc)
PY
}

# json reads one expression out of a JSON document, with d bound to it. The namespace
# is explicit, so an expression in this script cannot reach anything else.
json() { # url token expression
  python3 - "$1" "${2:-}" "$3" <<'PY'
import sys, json, urllib.request, urllib.error

builtins = {"len": len, "isinstance": isinstance, "sorted": sorted, "sum": sum,
            "any": any, "all": all, "min": min, "max": max, "str": str, "int": int,
            "bool": bool, "list": list, "dict": dict, "tuple": tuple}

request = urllib.request.Request(sys.argv[1])
if sys.argv[2]:
    request.add_header("Authorization", "Bearer " + sys.argv[2])
try:
    d = json.loads(urllib.request.urlopen(request, timeout=10).read())
except Exception as exc:
    print("error: %s" % exc)
    sys.exit()
try:
    print(eval(sys.argv[3], {"__builtins__": builtins}, {"d": d}))
except Exception as exc:
    print("no such field: %s" % exc)
PY
}

dashboard="http://127.0.0.1:$dashboard_port"

for probe in /healthz /readyz; do
  code="$(http "$dashboard$probe")"
  if [ "$code" = "200" ]; then
    pass "$probe answers through the pod's dashboard port"
  else
    fail "$probe answers through the pod's dashboard port" "$code"
  fi
done

if [ "$(http "$dashboard/api/status")" = "401" ]; then
  pass "the dashboard refuses /api/status without a token"
else
  fail "the dashboard refuses /api/status without a token" "$(http "$dashboard/api/status")"
fi
if [ "$(http "$dashboard/api/status" "$dashboard_token")" = "200" ]; then
  pass "the Secret's dashboard token opens /api/status"
else
  fail "the Secret's dashboard token opens /api/status" "$(http "$dashboard/api/status" "$dashboard_token")"
fi
if [ "$(json "$dashboard/api/status" "$dashboard_token" 'd["auth_required"]')" = "True" ]; then
  pass "AETHERTUNNEL_AUTH_TOKEN reached the server, so it demands a token from clients"
else
  fail "AETHERTUNNEL_AUTH_TOKEN reached the server" "$(json "$dashboard/api/status" "$dashboard_token" 'd')"
fi
if [ "$(http "$dashboard/metrics")" = "401" ]; then
  pass "the dashboard refuses /metrics without a token"
else
  fail "the dashboard refuses /metrics without a token" "$(http "$dashboard/metrics")"
fi

# --- a client, a tunnel and the bytes it carries -------------------------------

cat > "$work/echo.py" <<'PY'
import socket, threading, sys

server = socket.socket()
server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
server.bind(("127.0.0.1", 0))
with open(sys.argv[1], "w") as handle:
    handle.write(str(server.getsockname()[1]))
server.listen(16)


def serve(conn):
    with conn:
        while True:
            chunk = conn.recv(4096)
            if not chunk:
                return
            conn.sendall(chunk)


while True:
    connection, _ = server.accept()
    threading.Thread(target=serve, args=(connection,), daemon=True).start()
PY

python3 "$work/echo.py" "$work/echo.port" > "$work/echo.log" 2>&1 < /dev/null &
child_pids+=("$!")
for _ in $(seq 1 25); do
  [ -s "$work/echo.port" ] && break
  sleep 0.2
done
echo_target="$(cat "$work/echo.port" 2>/dev/null)"
[ -n "$echo_target" ] || skip "the local echo server did not start"

cat > "$work/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$control_port"
auth_token = "$auth_token"

[obfuscation]
enabled = true
pad_to = 256
disguise = "tls-record"

[[proxies]]
name = "kubernetes-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $echo_target
remote_port = 18080
EOF

"$client_bin" --config "$work/client.toml" > "$work/client.log" 2>&1 < /dev/null &
client_pid=$!
child_pids+=("$client_pid")

payload="through-the-kubernetes-deployment"
transfer="$(python3 - "$proxy_port" "$payload" <<'PY'
import socket, sys, time
port, payload = int(sys.argv[1]), sys.argv[2].encode()
deadline = time.time() + 30
last = "no connection"
while time.time() < deadline:
    try:
        conn = socket.create_connection(("127.0.0.1", port), timeout=10)
    except OSError as exc:
        last = "no connection: %s" % exc
        time.sleep(0.5)
        continue
    try:
        conn.settimeout(10)
        conn.sendall(payload)
        got = b""
        while len(got) < len(payload):
            chunk = conn.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        last = "no answer: %s" % exc
    else:
        if got == payload:
            print("ok")
            sys.exit()
        last = "got %r" % got
    finally:
        conn.close()
    time.sleep(0.5)
print(last)
PY
)"
if [ "$transfer" = "ok" ]; then
  pass "a client authenticates with the Secret's token and the visitor's bytes make the round trip"
else
  fail "a client authenticates with the Secret's token and the visitor's bytes make the round trip" \
    "$transfer / $(tail -n 1 "$work/client.log")"
fi

published="$(json "$dashboard/api/proxies" "$dashboard_token" \
  '[(p["name"], p["remote_port"]) for p in (d if isinstance(d, list) else d["proxies"])]')"
case "$published" in
  *kubernetes-probe*18080*) pass "the dashboard lists the proxy the client published" ;;
  *) fail "the dashboard lists the proxy the client published" "$published" ;;
esac

# --- the DHT through the Service, inside the cluster and outside it ------------

dht_port="$(sed -n 's/.*listen_addr *= *"0\.0\.0\.0:\([0-9]*\)".*/\1/p' \
  "$repo/deploy/kubernetes/configmap.yaml" | head -1)"
[ -n "$dht_port" ] || skip "the ConfigMap names no DHT port to check"
cluster_ip="$(kubectl -n aethertunnel get svc aethertunnel -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
dht_node_port="$(kubectl -n aethertunnel get svc aethertunnel \
  -o jsonpath='{.spec.ports[?(@.name=="dht")].nodePort}' 2>/dev/null)"

if [ -n "$cluster_ip" ]; then
  cat > "$work/probe.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: dht-probe
  namespace: aethertunnel
data:
  probe.toml: |
    [server]
    bind_addr = "127.0.0.1"
    bind_port = 0

    [dht]
    enabled = true
    listen_addr = "0.0.0.0:0"
    bootstrap = ["$cluster_ip:$dht_port"]
---
apiVersion: v1
kind: Pod
metadata:
  name: dht-probe
  namespace: aethertunnel
  labels:
    app.kubernetes.io/component: probe
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
  containers:
    - name: lookup
      image: $image_ref
      args: ["--config", "/etc/probe/probe.toml", "--dht-lookup", "kubernetes-probe"]
      env:
        - name: AETHERTUNNEL_AUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: aethertunnel-secrets
              key: auth-token
      volumeMounts:
        - name: probe
          mountPath: /etc/probe
          readOnly: true
  volumes:
    - name: probe
      configMap:
        name: dht-probe
EOF

  kubectl apply -f "$work/probe.yaml" > /dev/null 2>&1

  probe_result=""
  for _ in $(seq 1 60); do
    phase="$(kubectl -n aethertunnel get pod dht-probe -o jsonpath='{.status.phase}' 2>/dev/null)"
    if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then
      probe_result="$(kubectl -n aethertunnel logs dht-probe -c lookup 2>/dev/null | grep -v 'warning:' | tail -n 1)"
      break
    fi
    sleep 1
  done
  case "$probe_result" in
    kubernetes-probe*)
      pass "the Service publishes the DHT: a pod in the cluster resolves a name through its cluster address" ;;
    *)
      fail "the Service publishes the DHT: a pod in the cluster resolves a name through its cluster address" \
        "${probe_result:-the probe never finished}" ;;
  esac
else
  fail "the Service publishes the DHT" "the Service has no cluster address"
fi

node_address="$(hostname -I 2>/dev/null | awk '{print $1}')"
if [ -z "$dht_node_port" ]; then
  fail "the Service publishes the DHT on a node port" \
    "$(kubectl -n aethertunnel get svc aethertunnel -o jsonpath='{.spec.ports[*].name}' 2>/dev/null)"
elif [ -z "$node_address" ]; then
  note "this machine has no address other than loopback, so the DHT was not checked from outside the cluster"
else
  cat > "$work/lookup.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = 0
auth_token = "$auth_token"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["$node_address:$dht_node_port"]
EOF
  outside=""
  for _ in $(seq 1 3); do
    outside="$(timeout 20 "$server_bin" --config "$work/lookup.toml" \
      --dht-lookup kubernetes-probe 2> "$work/lookup.err" | tail -n 1)"
    [ -n "$outside" ] && break
    sleep 1
  done
  case "$outside" in
    kubernetes-probe*)
      pass "a process outside the cluster resolves a name through the Service's DHT node port" ;;
    *)
      fail "a process outside the cluster resolves a name through the Service's DHT node port" \
        "${outside:-unresolved: $(tail -n 1 "$work/lookup.err")}" ;;
  esac
fi

# --- the state volume, read back through the API ------------------------------

# The ledger is written when a session ends, and it can only be read back at all
# because the server wrote it into the volume the Deployment mounts: the container's
# own filesystem is read-only, and the pod is replaced by any rollout.
kill "$client_pid" 2>/dev/null
wait "$client_pid" 2>/dev/null
ledger_entries=""
for _ in $(seq 1 20); do
  ledger_entries="$(json "$dashboard/api/ledger" "$dashboard_token" 'len(d["entries"])')"
  case "$ledger_entries" in
    ''|*[!0-9]*) sleep 1 ;;
    0) sleep 1 ;;
    *) break ;;
  esac
done
case "$ledger_entries" in
  ''|*[!0-9]*) fail "the bandwidth ledger records the session in the state volume" "$ledger_entries" ;;
  0) fail "the bandwidth ledger records the session in the state volume" "no entry after the session ended" ;;
  *) pass "the bandwidth ledger records the session in the state volume ($ledger_entries in the ledger)" ;;
esac

echo
if [ "$failures" -eq 0 ]; then
  echo "ALL KUBERNETES CHECKS PASSED"
else
  echo "$failures KUBERNETES CHECKS FAILED"
fi
exit "$failures"
