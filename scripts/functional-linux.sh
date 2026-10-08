#!/usr/bin/env bash
# Drives both binaries through the functional paths that only real processes on a
# real kernel can show: the proxy types that run on this platform, the shared http
# and https listeners and their host routing, a private proxy with its visitors
# (stcp, sudp and xtcp), a half-close that has to survive the relay, the socks5 exit
# and its target policy, finding the server by name through the DHT, the counters
# and the audit log, the rules that refuse a connection before the handshake (a
# denied source, a spent connection burst, a banned source), and the security layers
# together.
#
# It is the Linux counterpart of scripts/smoke-test.ps1, which is written for
# Windows (a second loopback address and icacls-based failure injection are what it
# relies on). The two are separate on purpose: this one runs on Linux (amd64
# and arm64), that one on Windows. It does not cover the vpn section
# (scripts/vpn-linux-test.sh needs root for that).
#
# Everything it needs beyond the two binaries is bash, python3 and the helper in
# scripts/smoketest, which it builds unless a path is given. It binds loopback
# ports only and needs no root.
#
# usage: functional-linux.sh <server binary> <client binary> [smoketest helper]
set -u

SERVER="${1:?the server binary is required}"
CLIENT="${2:?the client binary is required}"
HELPER="${3:-}"

TOKEN="functional-linux-token-0123456789"
DASHBOARD_TOKEN="functional-linux-dashboard"
METRICS_TOKEN="functional-linux-metrics"
STCP_SECRET="functional-linux-stcp-secret"
SUDP_SECRET="functional-linux-sudp-secret"
XTCP_SECRET="functional-linux-xtcp-secret"

PASSES=0
FAILURES=0
PIDS=()

ok() { PASSES=$((PASSES + 1)); echo "PASS  $1"; }
bad() { FAILURES=$((FAILURES + 1)); echo "FAIL  $1 -> $2"; }
check() { if [ "$1" = "$2" ]; then ok "$3"; else bad "$3" "want '$2' got '$1'"; fi; }

cleanup() {
    for pid in "${PIDS[@]:-}"; do
        kill -9 "$pid" 2>/dev/null || true
    done
    rm -rf "${WORK:-/nonexistent}"
}
trap cleanup EXIT

# The binaries are invoked after the working directory has changed, so a path given
# on the command line is resolved here, while the caller's directory is still current.
abspath() {
    case "$1" in
        /*) printf '%s' "$1" ;;
        *) printf '%s/%s' "$(pwd)" "$1" ;;
    esac
}

SERVER="$(abspath "$SERVER")"
CLIENT="$(abspath "$CLIENT")"
[ -z "$HELPER" ] || HELPER="$(abspath "$HELPER")"

for binary in "$SERVER" "$CLIENT"; do
    [ -x "$binary" ] || { echo "not executable: $binary" >&2; exit 2; }
done

WORK="$(mktemp -d)"
# Resolved before the working directory changes, so that the shipped example
# configurations can be found however this script was invoked.
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$WORK" || exit 2

# The ports this run has already handed out. Every allocation below sits inside a
# command substitution, which runs in a subshell, so a shell variable could not
# carry this memory; the file does. Without it two calls can return the same
# ephemeral port — binding port 0 and closing it says nothing about the next
# call — and a later call can land inside a range free_port_range is holding.
PORT_MEMO="$WORK/.ports-handed-out"
: > "$PORT_MEMO"

# A port nobody is listening on. The window between closing this socket and the
# server binding it is small, and the alternative (fixed ports) collides with
# whatever else runs on the machine.
free_port() {
    python3 - "$PORT_MEMO" <<'PY'
import socket, sys

memo = sys.argv[1]
taken = set()
try:
    with open(memo) as f:
        taken = {int(line) for line in f if line.strip()}
except FileNotFoundError:
    pass

for _ in range(200):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    if port in taken:
        continue
    with open(memo, "a") as f:
        f.write("%d\n" % port)
    print(port)
    sys.exit(0)
sys.exit(1)
PY
}

# free_port_range <n>: a base port whose n consecutive ports are all free. A
# remote_ports range expands into one proxy per port, and free_port() alone leaves
# the neighbours unchecked — on a loaded machine one of them can be in use, the
# server refuses that registration, and the proxy count comes up short (seen once
# on arm64: 18 of 19, with the middle port of the range missing).
free_port_range() {
    python3 - "$1" "$PORT_MEMO" <<'PY'
import socket, sys

need = int(sys.argv[1])
memo = sys.argv[2]
taken = set()
try:
    with open(memo) as f:
        taken = {int(line) for line in f if line.strip()}
except FileNotFoundError:
    pass

for _ in range(200):
    held = []
    try:
        base = 0
        for i in range(need):
            s = socket.socket()
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            s.bind(("127.0.0.1", 0 if i == 0 else base + i))
            if i == 0:
                base = s.getsockname()[1]
            held.append(s)
    except OSError:
        for s in held:
            s.close()
        continue
    if any(base + i in taken for i in range(need)):
        # A port this run has already handed to another service: the bind above
        # cannot see that, because the service may not be listening yet.
        for s in held:
            s.close()
        continue
    with open(memo, "a") as f:
        for i in range(need):
            f.write("%d\n" % (base + i))
    for s in held:
        s.close()
    print(base)
    sys.exit(0)
sys.exit(1)
PY
}

# A port can be taken between the allocation above and the server binding it: by
# another process, or by an interrupted earlier run of this script that never got to
# its cleanup. Saying so here is worth much more than the twenty unrelated failures a
# collision produces further down.
require_free_tcp() {
    if ! python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
# SO_REUSEADDR so a socket in TIME_WAIT is not mistaken for a live listener: the
# server can bind over those.
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
    then
        echo "port $1 is already in use: another process, or an earlier run of this" >&2
        echo "script that was interrupted before its cleanup, is still holding it" >&2
        exit 2
    fi
}

# wait_for_tcp <port> <tries>: succeeds once the port accepts a connection.
wait_for_tcp() {
    python3 - "$1" "$2" <<'PY'
import socket, sys, time
port, tries = int(sys.argv[1]), int(sys.argv[2])
for _ in range(tries):
    try:
        socket.create_connection(("127.0.0.1", port), timeout=1).close()
        sys.exit(0)
    except OSError:
        time.sleep(0.25)
sys.exit(1)
PY
}

# wait_for_log <file> <text> <tries>: succeeds once the file holds the text. It is
# how a helper that accepts exactly one connection announces that it is listening:
# a readiness probe that connects would consume that one connection.
wait_for_log() {
    for _ in $(seq 1 "$3"); do
        if grep -q -- "$2" "$1" 2>/dev/null; then
            return 0
        fi
        sleep 0.05
    done
    return 1
}

# api_field_at <port> <token> <path> <python expression over the decoded body, bound to d>
api_field_at() {
    python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, sys, urllib.request
port, token, path, expr = sys.argv[1:5]
req = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path))
req.add_header("Authorization", "Bearer " + token)
with urllib.request.urlopen(req, timeout=10) as resp:
    body = json.loads(resp.read())
print(eval(expr, {"d": body}))
PY
}

# api_field <path> <expression>: the same, read from the deployment under test.
api_field() {
    api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" "$1" "$2"
}

# wait_api_field waits for a dashboard field to reach a value, the way wait_metric does
# for a counter: the teardown paths (a session ending, a member leaving a pool) update
# these asynchronously, so reading once would race with them.
wait_api_field() {
    python3 - "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" "$1" "$2" "$3" <<'PY'
import json, sys, time, urllib.request
port, token, path, expr, want = sys.argv[1:6]
deadline = time.time() + 15
last = ""
while True:
    req = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path))
    req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            last = str(eval(expr, {"d": json.loads(resp.read())}))
    except Exception as exc:
        last = "error: %s" % exc
    if last == want or time.time() > deadline:
        print(last)
        break
    time.sleep(0.25)
PY
}

# metric_at <port> <token> <name>: the value of one series from that server's
# /metrics, or an empty string. The servers the access rules below run on have
# dashboards of their own, so their counters cannot be read from the deployment
# under test.
metric_at() {
    python3 - "$1" "$2" "$3" <<'PY'
import sys, urllib.request
port, token, name = sys.argv[1:4]
req = urllib.request.Request("http://127.0.0.1:%s/metrics" % port)
req.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read().decode()
except Exception:
    print("")
    sys.exit(0)
for line in body.splitlines():
    if line.startswith(name + " "):
        print(line.rsplit(" ", 1)[1].strip())
        break
PY
}

# metric_value <name>: the value of one series from the deployment's /metrics.
metric_value() {
    metric_at "$DASHBOARD_PORT" "$METRICS_TOKEN" "$1"
}

# wait_metric <port> <token> <name> <want>: polls one series until it equals want
# and prints the last value read. A refusal is counted in the handler that goes on
# to close the connection, so a single read can land before the increment; polling
# here is what makes the refusal checks independent of that race.
wait_metric() {
    python3 - "$1" "$2" "$3" "$4" <<'PY'
import sys, time, urllib.request
port, token, name, want = sys.argv[1:5]
deadline = time.time() + 15
while True:
    req = urllib.request.Request("http://127.0.0.1:%s/metrics" % port)
    req.add_header("Authorization", "Bearer " + token)
    try:
        body = urllib.request.urlopen(req, timeout=5).read().decode()
    except Exception as exc:
        last = "no metrics: %s" % exc
    else:
        last = ""
        for line in body.splitlines():
            if line.startswith(name + " "):
                last = line.rsplit(" ", 1)[1].strip()
                break
    if last == want or time.time() > deadline:
        print(last)
        break
    time.sleep(0.2)
PY
}

# --- the local services the tunnels point at ---------------------------------

if [ -z "$HELPER" ]; then
    HELPER="$WORK/smoketest"
    command -v go >/dev/null 2>&1 || { echo "no helper given and go is not installed to build one" >&2; exit 2; }
    (cd "$HERE" && go build -o "$HELPER" ./scripts/smoketest) ||
        { echo "building the smoketest helper failed" >&2; exit 2; }
fi

"$HELPER" > "$WORK/services.json" 2> "$WORK/services.log" &
PIDS+=($!)
for _ in $(seq 1 40); do
    [ -s "$WORK/services.json" ] && break
    sleep 0.25
done
if [ ! -s "$WORK/services.json" ]; then
    echo "the smoketest helper never printed its endpoints" >&2
    cat "$WORK/services.log" >&2
    exit 2
fi

service_port() {
    python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]].split(":")[1])' "$WORK/services.json" "$1"
}
TCP_ECHO_PORT="$(service_port tcp)"
UDP_ECHO_PORT="$(service_port udp)"
HTTP_ECHO_PORT="$(service_port http)"
HALFCLOSE_PORT="$(service_port halfclose)"

# One certificate serves the shared https listener below and the TLS control port
# in the last scenario.
"$HELPER" -cert-dir "$WORK" -cert-hosts "127.0.0.1,localhost" > /dev/null 2>&1
[ -s "$WORK/server.crt" ] || { echo "the helper wrote no certificate" >&2; exit 2; }

echo "== the command line"

server_version="$("$SERVER" --version 2>&1)"
case "$server_version" in
    *"protocol"*) ok "the server reports its version and the protocol: $server_version" ;;
    *) bad "the server reports its version and the protocol" "$server_version" ;;
esac

client_version="$("$CLIENT" --version 2>&1)"
case "$client_version" in
    *"protocol"*) ok "the client reports its version and the protocol: $client_version" ;;
    *) bad "the client reports its version and the protocol" "$client_version" ;;
esac

if "$SERVER" --config "$HERE/server.toml.example" --check > check-server.log 2>&1; then
    ok "the shipped server configuration validates"
else
    bad "the shipped server configuration validates" "$(tail -1 check-server.log)"
fi
if "$CLIENT" --config "$HERE/client.toml.example" --check > check-client.log 2>&1; then
    ok "the shipped client configuration validates"
else
    bad "the shipped client configuration validates" "$(tail -1 check-client.log)"
fi

# The identity key is created on first use, so this both proves the flag works and
# produces the key the private-proxy scenarios need.
cat > identity.toml <<EOF
[client]
server_addr = "127.0.0.1:1"
auth_token = "$TOKEN"

[identity]
enabled = true
key_file = "$WORK/visitor-identity.key"
EOF
identity_out="$("$CLIENT" --identity --config identity.toml 2>&1 | tail -1)"
if [ -s "$WORK/visitor-identity.key" ] && printf '%s' "$identity_out" | grep -qE '^[0-9a-f]{64}$'; then
    ok "the client created an identity key and printed its public half"
else
    bad "the client created an identity key and printed its public half" "$identity_out"
fi

# --- one server, one owner client, three visitors ----------------------------

CONTROL_PORT="$(free_port)"
DASHBOARD_PORT="$(free_port)"
TCP_PROXY_PORT="$(free_port)"
RANGE_BASE_PORT="$(free_port_range 3)"
STATIC_FILE_PORT="$(free_port)"
PROXY_PROTO_PORT="$(free_port)"
PROXY_PROTO_BACKEND_PORT="$(free_port)"
PROXY_PROTO2_PORT="$(free_port)"
PROXY_PROTO2_BACKEND_PORT="$(free_port)"
BW_PORT="$(free_port)"
BW_BACKEND_PORT="$(free_port)"
ALLOWPORTS_CONTROL_PORT="$(free_port)"
ALLOWPORTS_PROXY_PORT="$(free_port)"
UDP_PROXY_PORT="$(free_port)"
HTTP_PORT="$(free_port)"
HTTPS_PORT="$(free_port)"
SOCKS_PORT="$(free_port)"
POOL_PROXY_PORT="$(free_port)"
POOL_OTHER_PORT="$(free_port)"
P2P_PORT="$(free_port)"
DHT_PORT="$(free_port)"
DHT_LOOKUP_PORT="$(free_port)"
DHT_VISITOR_PORT="$(free_port)"
HALFCLOSE_PROXY_PORT="$(free_port)"
STCP_VISITOR_PORT="$(free_port)"
SUDP_VISITOR_PORT="$(free_port)"
XTCP_VISITOR_PORT="$(free_port)"
BAD_VISITOR_PORT="$(free_port)"

cat > server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $CONTROL_PORT
auth_token = "$TOKEN"
http_port = $HTTP_PORT
https_port = $HTTPS_PORT
https_cert_file = "$WORK/server.crt"
https_key_file = "$WORK/server.key"
p2p_port = $P2P_PORT

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $DASHBOARD_PORT
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true
token = "$METRICS_TOKEN"

[audit]
enabled = true
path = "$WORK/audit.jsonl"

# The ledger is on for this deployment so the suite can check the file it writes:
# sessions end throughout the run, and the ledger section below adds one of its own
# whose usage it knows in advance.
[ledger]
enabled = true
path = "$WORK/ledger.jsonl"
signing_key_file = "$WORK/ledger.key"

# A policy for one name: any client that publishes it is subject to this, and the
# visitors below are refused by the server before a tunnel is established.
[[proxies]]
name = "policy-blocked"
type = "http"
deny_cidrs = ["127.0.0.1/32"]

# A name in this network resolves to the endpoint it is published on: a tcp proxy
# to its public port, a private proxy to the control port, which is what lets a
# client with no server_addr find the server.
[dht]
enabled = true
listen_addr = "127.0.0.1:$DHT_PORT"
advertise_host = "127.0.0.1"
signing_key_file = "$WORK/dht.key"
EOF

cat > owner.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "tcp-echo"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $TCP_PROXY_PORT

[[proxies]]
name = "udp-echo"
type = "udp"
local_ip = "127.0.0.1"
local_port = $UDP_ECHO_PORT
remote_port = $UDP_PROXY_PORT

[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["web.smoke.test"]

# Same service, but the proxy carries its own visitor list, which is what the
# server applies after its own policy.
[[proxies]]
name = "list-blocked"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["list-blocked.smoke.test"]
deny_cidrs = ["127.0.0.1/32"]

# The name the server's own policy refuses.
[[proxies]]
name = "policy-blocked"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["policy-blocked.smoke.test"]

# The same service over the shared TLS listener, which selects by Host header too.
[[proxies]]
name = "secure-web"
type = "https"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["secure.smoke.test"]

# A service that answers only after its peer half-closes: the answer is what shows
# the relay did not turn the half-close into a full close.
[[proxies]]
name = "half-close"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $HALFCLOSE_PORT
remote_port = $HALFCLOSE_PROXY_PORT

# The exit a phone or a browser extension points at. A socks5 tunnel has no local
# service to name, so it carries neither local_ip nor local_port, and allow_targets
# is what keeps it from becoming an exit for everything this machine can reach.
[[proxies]]
name = "exit"
type = "socks5"
remote_port = $SOCKS_PORT
allow_targets = ["127.0.0.1/32"]

[[proxies]]
name = "private"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "$STCP_SECRET"
auth_method = "nizk"

[[proxies]]
name = "private-udp"
type = "sudp"
local_ip = "127.0.0.1"
local_port = $UDP_ECHO_PORT
secret_key = "$SUDP_SECRET"
auth_method = "nizk"

# xtcp tries a hole punch and falls back to the relay when it cannot punch
# through. Either way the visitor has to carry the bytes.
[[proxies]]
name = "direct"
type = "xtcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "$XTCP_SECRET"
auth_method = "nizk"

# One member of a pooled proxy. A second client publishes the same name and group
# further down; the pool owns this member's port.
[[proxies]]
name = "pooled"
type = "tcp"
group = "pool-a"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $POOL_PROXY_PORT

# A remote_ports range expands into one proxy per port, each named after its
# port; every other setting of the entry applies to every copy.
[[proxies]]
name = "range"
type = "tcp"
local_port = $TCP_ECHO_PORT
remote_ports = "$RANGE_BASE_PORT-$((RANGE_BASE_PORT + 2))"

# The static_file plugin serves a directory from the client itself; there is no
# local service to dial at all.
[[proxies]]
name = "site"
type = "tcp"
remote_port = $STATIC_FILE_PORT
plugin = "static_file"
plugin_local_path = "$WORK/staticsite"

# The backend behind this proxy reads the first line it receives — the header
# the server prepended — and answers with what it saw.
[[proxies]]
name = "proxied"
type = "tcp"
local_port = $PROXY_PROTO_BACKEND_PORT
remote_port = $PROXY_PROTO_PORT
proxy_protocol = "v1"

[[proxies]]
name = "proxied2"
type = "tcp"
local_port = $PROXY_PROTO2_BACKEND_PORT
remote_port = $PROXY_PROTO2_PORT
proxy_protocol = "v2"

# The bandwidth limiter runs on the client: 40 KB at 5 KB/s takes about seven
# seconds, which no unthrottled path on loopback would.
[[proxies]]
name = "slow"
type = "tcp"
local_port = $BW_BACKEND_PORT
remote_port = $BW_PORT
bandwidth = "5KB"
EOF

cat > visitor.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[identity]
enabled = true
key_file = "$WORK/visitor-identity.key"

[[visitors]]
name = "private"
type = "stcp"
server_name = "private"
secret_key = "$STCP_SECRET"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $STCP_VISITOR_PORT

[[visitors]]
name = "private-udp"
type = "sudp"
server_name = "private-udp"
secret_key = "$SUDP_SECRET"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $SUDP_VISITOR_PORT

[[visitors]]
name = "direct"
type = "xtcp"
server_name = "direct"
secret_key = "$XTCP_SECRET"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $XTCP_VISITOR_PORT
EOF

# A visitor holding the wrong secret, to prove the refusal path and to give the
# audit log something to record.
cat > bad-visitor.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "wrong-secret"
type = "stcp"
server_name = "private"
secret_key = "this-is-not-the-secret"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $BAD_VISITOR_PORT
EOF

for port in "$CONTROL_PORT" "$DASHBOARD_PORT" "$TCP_PROXY_PORT" "$UDP_PROXY_PORT" \
            "$HTTP_PORT" "$HTTPS_PORT" "$SOCKS_PORT" "$POOL_PROXY_PORT" \
            "$HALFCLOSE_PROXY_PORT" "$STCP_VISITOR_PORT" "$XTCP_VISITOR_PORT" \
            "$BAD_VISITOR_PORT" "$DHT_LOOKUP_PORT" "$DHT_VISITOR_PORT"; do
    require_free_tcp "$port"
done

"$SERVER" --config server.toml > server.log 2>&1 &
PIDS+=($!)
"$CLIENT" --config owner.toml > owner.log 2>&1 &
PIDS+=($!)
if wait_for_tcp "$DASHBOARD_PORT" 120; then
    ok "the dashboard answers once the server is up"
else
    bad "the dashboard answers once the server is up" "$(tail -3 server.log)"
fi

# Every proxy the owner publishes has to be there before the transfers mean
# anything: a visitor to a proxy that was never registered fails for the wrong
# reason. The message names what is missing: one number cannot tell a registration
# that was refused from one still in flight or one lost with a reconnect.
#
# This check once reported 18 of 19 on arm64, with the middle port of the
# remote_ports range missing: the range's neighbours were never checked, and one was
# in use on the machine, so the server refused that one registration for good — no
# wait would have fixed it. free_port_range allocates the triple now. The window is
# two minutes anyway, because a loaded machine registers nineteen proxies slowly.
published=""
for _ in $(seq 1 480); do
    published="$(api_field /api/proxies 'len(d["proxies"])' 2>/dev/null || echo 0)"
    [ "$published" = "19" ] && break
    sleep 0.25
done
if [ "$published" = "19" ]; then
    ok "the owner published all nineteen proxies"
else
    bad "the owner published all nineteen proxies" \
        "got $published: $(api_field /api/proxies '[p["name"] for p in d["proxies"]]' 2>/dev/null) | owner: $(tail -3 owner.log | tr '\n' ' ')"
fi

# The backend behind the bandwidth-limited proxy reads exactly 40 KB and sends
# it back; unlike the echo helper it has no short idle deadline, because the
# whole point of this transfer is that it takes a while.
python3 - > bw-backend.log 2>&1 <<BWY &
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", $BW_BACKEND_PORT))
s.listen(1)
conn, _ = s.accept()
data = b""
while len(data) < 40_000:
    chunk = conn.recv(65536)
    if not chunk:
        break
    data += chunk
conn.sendall(data)
conn.close()
BWY
PIDS+=($!)

# The static file site the plugin will serve.
mkdir -p "$WORK/staticsite"
printf 'static-file-content\n' > "$WORK/staticsite/hello.txt"

# The backend behind the proxy-protocol proxy reads one line — the header the
# server prepended — and answers with what it saw.
python3 - > pproto-backend.log 2>&1 <<PPY &
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", $PROXY_PROTO_BACKEND_PORT))
s.listen(1)
# The script waits for this line instead of connecting to find out: this backend
# accepts exactly one connection, and a readiness probe would be that connection.
print("listening", flush=True)
conn, _ = s.accept()
line = conn.makefile().readline().strip()
conn.sendall(("saw: " + line).encode())
conn.close()
PPY
PIDS+=($!)
wait_for_log pproto-backend.log listening 100 || bad "the proxy-protocol backend announced that it is listening" "$(cat pproto-backend.log 2>/dev/null)"

tcp_via() {
    python3 - "$1" <<'PY'
import socket, sys
payload = b"range-check"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
}

check "$(tcp_via "$RANGE_BASE_PORT")" "ok" \
    "the first proxy of a remote_ports range carries bytes"
check "$(tcp_via "$((RANGE_BASE_PORT + 2))")" "ok" \
    "the last proxy of a remote_ports range carries bytes"
static_result="$(python3 - "$STATIC_FILE_PORT" <<'PY'
import sys, urllib.request
try:
    print(urllib.request.urlopen("http://127.0.0.1:%s/hello.txt" % sys.argv[1], timeout=15).read().decode().strip())
except Exception as exc:
    print("failed: %s" % exc)
PY
)"
check "$static_result" "static-file-content" \
    "the static_file plugin serves the directory through the tunnel"

pproto="$(python3 - "$PROXY_PROTO_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    data = b""
    try:
        while b"\n" not in data:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
    finally:
        s.close()
    print(data.decode(errors="replace").strip())
PY
)"
check "$(printf '%s' "$pproto" | grep -c 'saw: PROXY TCP4 127.0.0.1')" "1" \
    "the backend received the visitor's PROXY protocol header: $pproto"

# The v2 form is binary: the backend parses the signature, the command block
# and the addresses, and answers with the visitor it saw.
python3 - > pproto2-backend.log 2>&1 <<PPY &
import socket, struct
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", $PROXY_PROTO2_BACKEND_PORT))
s.listen(1)
print("listening", flush=True)
conn, _ = s.accept()
head = b""
while len(head) < 16:
    chunk = conn.recv(16 - len(head))
    if not chunk:
        break
    head += chunk
ok = head[:12] == bytes([0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A])
length = struct.unpack(">H", head[14:16])[0]
rest = b""
while len(rest) < length:
    chunk = conn.recv(length - len(rest))
    if not chunk:
        break
    rest += chunk
src_ip = ".".join(str(b) for b in rest[:4])
src_port = struct.unpack(">H", rest[8:10])[0]
conn.sendall(("saw-v2: ok=%s tcp4 %s:%d" % (ok, src_ip, src_port)).encode())
conn.close()
PPY
PIDS+=($!)
wait_for_log pproto2-backend.log listening 100 || bad "the proxy-protocol v2 backend announced that it is listening" "$(cat pproto2-backend.log 2>/dev/null)"

pproto2="$(python3 - "$PROXY_PROTO2_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    data = b""
    try:
        while b"\n" not in data:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
    finally:
        s.close()
    print(data.decode(errors="replace").strip())
PY
)"
check "$(printf '%s' "$pproto2" | grep -c 'saw-v2: ok=True tcp4 127.0.0.1:')" "1" \
    "a proxy_protocol v2 backend parsed the binary header: $pproto2"

bw_result="$(python3 - "$BW_PORT" <<'PY'
import socket, sys, time
payload = b"x" * 40_000
start = time.time()
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=120)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(120)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(65536)
        if not chunk:
            break
        got += chunk
    elapsed = time.time() - start
    s.close()
    print("ok %.1f" % elapsed if got == payload else "got %d bytes" % len(got))
PY
)"
check "${bw_result%% *}" "ok" "a bandwidth-limited proxy carries all 40 KB"
bw_elapsed="${bw_result#ok }"
if [ "${bw_result%% *}" = "ok" ] && awk "BEGIN{exit !($bw_elapsed >= 5.0)}"; then
    ok "the bandwidth limiter held 40 KB to at least five seconds ($bw_elapsed s)"
else
    bad "the bandwidth limiter held 40 KB to at least five seconds" "$bw_result"
fi

echo
echo "== the public endpoints"

tcp_result="$(python3 - "$TCP_PROXY_PORT" <<'PY'
import socket, sys
payload = b"tcp-through-the-tunnel"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
)"
check "$tcp_result" "ok" "a tcp proxy carries bytes to the local service and back"

udp_result="$(python3 - "$UDP_PROXY_PORT" <<'PY'
import socket, sys
payload = b"udp-through-the-tunnel"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(15)
try:
    s.sendto(payload, ("127.0.0.1", int(sys.argv[1])))
    data, _ = s.recvfrom(4096)
except OSError as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if data == payload else "got %r" % data)
finally:
    s.close()
PY
)"
check "$udp_result" "ok" "a udp proxy carries a datagram to the local service and back"

# The shared http port routes by the Host header, so this also proves the domains
# list on the owner's http proxy is what selects the tunnel.
http_result="$(python3 - "$HTTP_PORT" <<'PY'
import sys, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "web.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
PY
)"
check "$http_result" "ok" "an http proxy is reachable on the shared port through its domain"

# A name the server holds no policy for has to be refused, not answered by
# whichever tunnel happens to be first.
unknown_host="$(python3 - "$HTTP_PORT" <<'PY'
import sys, urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "no-policy-for-this-name.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read().decode()
except urllib.error.HTTPError as exc:
    print("refused" if exc.code >= 400 else "served %d" % exc.code)
except Exception:
    # A reset or a closed connection is a refusal too.
    print("refused")
else:
    print("served" if "smoketest-http" in body else "unknown answer")
PY
)"
check "$unknown_host" "refused" "a host with no policy behind the shared port is refused"

# The same listener over TLS. The certificate is self-signed, so what is under test
# is the routing: the Host header picks the tunnel, and a name with no proxy behind
# it is refused rather than answered by whichever tunnel came first.
https_result="$(python3 - "$HTTPS_PORT" <<'PY'
import http.client, ssl, sys
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    conn = http.client.HTTPSConnection("127.0.0.1", int(sys.argv[1]), context=ctx, timeout=15)
    conn.request("GET", "/", headers={"Host": "secure.smoke.test"})
    body = conn.getresponse().read().decode()
    conn.close()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
PY
)"
check "$https_result" "ok" "an https proxy is reachable on the shared TLS port through its domain"

https_unknown="$(python3 - "$HTTPS_PORT" <<'PY'
import http.client, ssl, sys
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    conn = http.client.HTTPSConnection("127.0.0.1", int(sys.argv[1]), context=ctx, timeout=15)
    conn.request("GET", "/", headers={"Host": "no-policy-over-tls.smoke.test"})
    resp = conn.getresponse()
    body = resp.read().decode()
    conn.close()
except Exception:
    print("refused")
else:
    print("refused" if resp.status >= 400 else ("served" if "smoketest-http" in body else "unknown answer"))
PY
)"
check "$https_unknown" "refused" "a host with no policy behind the shared TLS port is refused"

echo
echo "== the visitor lists, on both sides"

# Two refusals that look identical to the visitor (403) and have to be told apart by
# what they leave behind: the counter and the audit detail name which side refused.
denied_before="$(metric_value aethertunnel_visitors_denied_by_proxy_total)"
policy_status="$(python3 - "$HTTP_PORT" "policy-blocked.smoke.test" <<'PY'
import sys, urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", sys.argv[2])
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(resp.status)
except urllib.error.HTTPError as exc:
    print(exc.code)
except Exception:
    print("closed")
PY
)"
list_status="$(python3 - "$HTTP_PORT" "list-blocked.smoke.test" <<'PY'
import sys, urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", sys.argv[2])
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(resp.status)
except urllib.error.HTTPError as exc:
    print(exc.code)
except Exception:
    print("closed")
PY
)"
check "$policy_status" "403" "a visitor the server's policy refuses is turned away"
check "$list_status" "403" "a visitor the proxy's own list refuses is turned away"

denied_after="$(metric_value aethertunnel_visitors_denied_by_proxy_total)"
denied_delta=$(( ${denied_after:-0} - ${denied_before:-0} ))
check "$denied_delta" "2" "both refusals are counted as visitors denied by a proxy"

policy_detail="$(python3 - "$WORK/audit.jsonl" "policy-blocked" <<'PY'
import json, sys
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if not line:
        continue
    rec = json.loads(line)
    if rec.get("event") == "proxy_visitor_denied" and rec.get("proxy") == sys.argv[2]:
        print(rec.get("detail", ""))
        break
PY
)"
case "$policy_detail" in
    *"server's policy"*) ok "the audit record says the server's policy refused the visitor" ;;
    *) bad "the audit record says the server's policy refused the visitor" "$policy_detail" ;;
esac

list_detail="$(python3 - "$WORK/audit.jsonl" "list-blocked" <<'PY'
import json, sys
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if not line:
        continue
    rec = json.loads(line)
    if rec.get("event") == "proxy_visitor_denied" and rec.get("proxy") == sys.argv[2]:
        print(rec.get("detail", ""))
        break
PY
)"
case "$list_detail" in
    *"proxy's allow/deny lists"*) ok "the audit record says the proxy's own list refused the visitor" ;;
    *) bad "the audit record says the proxy's own list refused the visitor" "$list_detail" ;;
esac

# Something that is not a SOCKS5 request at all is a protocol error, not a visitor a
# list turned away, and it has its own series so that the two cannot be confused.
malformed_before="$(metric_value aethertunnel_socks5_malformed_requests_total)"
python3 - "$SOCKS_PORT" <<'PY' > /dev/null 2>&1
import socket, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
s.sendall(b"this is not a socks5 greeting")
s.close()
PY
sleep 0.5
malformed_after="$(metric_value aethertunnel_socks5_malformed_requests_total)"
denied_end="$(metric_value aethertunnel_visitors_denied_by_proxy_total)"
malformed_delta=$(( ${malformed_after:-0} - ${malformed_before:-0} ))
check "$malformed_delta" "1" "a connection that is not a SOCKS5 request is counted on its own"
check "$denied_end" "$denied_after" "it is not counted as a visitor denied by a proxy"

# A half-close has to survive the relay: the service answers only once the peer has
# closed its writing side, so losing the FIN loses the answer.
halfclose_result="$(python3 - "$HALFCLOSE_PROXY_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(b"request-then-close-write")
    s.shutdown(socket.SHUT_WR)
    got = b""
    try:
        while True:
            chunk = s.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        print("no answer: %s" % exc)
    else:
        print("ok" if got == b"answered-after-half-close" else "got %r" % got)
    finally:
        s.close()
PY
)"
check "$halfclose_result" "ok" "a half-close reaches the local service and its answer comes back"

echo
echo "== the private proxies and their visitors"

"$CLIENT" --config visitor.toml > visitor.log 2>&1 &
PIDS+=($!)

if wait_for_tcp "$STCP_VISITOR_PORT" 120; then
    ok "the stcp visitor bound its local port"
else
    bad "the stcp visitor bound its local port" "$(tail -3 visitor.log)"
fi

stcp_result="$(python3 - "$STCP_VISITOR_PORT" <<'PY'
import socket, sys
payload = b"stcp-through-the-visitor"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
)"
check "$stcp_result" "ok" "a stcp visitor reaches the private proxy's service"

sudp_result="$(python3 - "$SUDP_VISITOR_PORT" <<'PY'
import socket, sys, time
payload = b"sudp-through-the-visitor"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(2)
# The visitor's UDP socket is ready before the tunnel behind it is: repeat the
# datagram for a while so a slow first exchange is not read as a failure.
deadline = time.time() + 25
while True:
    try:
        s.sendto(payload, ("127.0.0.1", int(sys.argv[1])))
        data, _ = s.recvfrom(4096)
        print("ok" if data == payload else "got %r" % data)
        break
    except OSError as exc:
        if time.time() > deadline:
            print("no answer: %s" % exc)
            break
s.close()
PY
)"
check "$sudp_result" "ok" "a sudp visitor carries a datagram to the private proxy's service"

# The punch takes a moment and may end on either path, so the datagram is retried
# and only the answer decides.
xtcp_result="$(python3 - "$XTCP_VISITOR_PORT" <<'PY'
import socket, sys, time
payload = b"xtcp-through-the-visitor"
deadline = time.time() + 30
last = "no answer"
while time.time() < deadline:
    try:
        s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
    except OSError as exc:
        last = "no connection: %s" % exc
        time.sleep(0.5)
        continue
    s.settimeout(10)
    try:
        s.sendall(payload)
        got = b""
        while len(got) < len(payload):
            chunk = s.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        last = "no answer: %s" % exc
    else:
        if got == payload:
            print("ok")
            s.close()
            sys.exit()
        last = "got %r" % got
    finally:
        s.close()
    time.sleep(0.5)
print(last)
PY
)"
check "$xtcp_result" "ok" "an xtcp visitor reaches the private proxy's service"

# Whichever path the punch took, the server has to have said which. A punch that
# is never resolved is not a working punch, so the outcome is checked, not just
# the bytes above.
punch_outcome="$(grep -cE "reports a direct path|fell back to the relayed path" server.log)"
if [ "${punch_outcome:-0}" -ge 1 ] 2>/dev/null; then
    ok "the server named the path the xtcp visitor took ($(grep -oE 'reports a direct path|fell back to the relayed path' server.log | head -1))"
else
    bad "the server named the path the xtcp visitor took" "neither line is in server.log"
fi

punches="$(metric_value aethertunnel_p2p_punches_total)"
direct="$(metric_value aethertunnel_p2p_direct_total)"
relayed="$(metric_value aethertunnel_p2p_relayed_total)"
outcomes=$(( ${direct:-0} + ${relayed:-0} ))
if [ "${punches:-0}" -ge 1 ] 2>/dev/null && [ "$outcomes" -ge 1 ] 2>/dev/null; then
    ok "every punch attempt was accounted for (punches=$punches direct=$direct relayed=$relayed)"
else
    bad "every punch attempt was accounted for" "punches=$punches direct=$direct relayed=$relayed"
fi

# The refusal path: a visitor with the wrong secret must get no data.
"$CLIENT" --config bad-visitor.toml > bad-visitor.log 2>&1 &
PIDS+=($!)
wait_for_tcp "$BAD_VISITOR_PORT" 40 || true

bad_result="$(python3 - "$BAD_VISITOR_PORT" <<'PY'
import socket, sys, time
deadline = time.time() + 20
while True:
    try:
        s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
    except OSError as exc:
        if time.time() > deadline:
            print("refused: %s" % exc)
            break
        time.sleep(0.5)
        continue
    s.settimeout(8)
    try:
        s.sendall(b"the-wrong-secret-must-not-arrive")
        got = s.recv(4096)
    except OSError:
        print("refused")
    else:
        print("refused" if not got else "served %r" % got)
    finally:
        s.close()
    break
PY
)"
check "$bad_result" "refused" "a visitor with the wrong secret gets no data"

echo
echo "== the socks5 exit"

socks_allowed="$(python3 - "$SOCKS_PORT" "$TCP_ECHO_PORT" <<'PY'
import socket, struct, sys

def open_target(port, host, target_port):
    s = socket.create_connection(("127.0.0.1", int(port)), timeout=15)
    s.settimeout(15)
    s.sendall(b"\x05\x01\x00")
    reply = s.recv(2)
    if len(reply) != 2 or reply[1] != 0:
        s.close()
        return None, "greeting %r" % reply
    s.sendall(b"\x05\x01\x00\x01" + socket.inet_aton(host) + struct.pack(">H", int(target_port)))
    head = s.recv(4)
    if len(head) < 4 or head[1] != 0:
        s.close()
        return None, "reply %r" % head
    if head[3] == 1:
        s.recv(6)
    elif head[3] == 4:
        s.recv(18)
    elif head[3] == 3:
        s.recv(1 + ord(s.recv(1)) + 2)
    return s, None

sock, err = open_target(sys.argv[1], "127.0.0.1", sys.argv[2])
if sock is None:
    print("refused: %s" % err)
else:
    payload = b"socks5-through-the-exit"
    sock.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = sock.recv(4096)
        if not chunk:
            break
        got += chunk
    sock.close()
    print("ok" if got == payload else "got %r" % got)
PY
)"
check "$socks_allowed" "ok" "the socks5 exit reaches a target inside allow_targets"

socks_denied="$(python3 - "$SOCKS_PORT" "$TCP_ECHO_PORT" <<'PY'
import socket, struct, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
s.settimeout(10)
s.sendall(b"\x05\x01\x00")
if s.recv(2)[1] != 0:
    print("refused")
    sys.exit()
# 127.0.0.2 is outside the configured 127.0.0.1/32.
s.sendall(b"\x05\x01\x00\x01" + socket.inet_aton("127.0.0.2") + struct.pack(">H", int(sys.argv[2])))
head = s.recv(4)
if len(head) < 4 or head[1] != 0:
    print("refused")
else:
    try:
        s.sendall(b"this-must-not-arrive")
        got = s.recv(4096)
    except OSError:
        print("refused")
    else:
        print("refused" if not got else "served %r" % got)
s.close()
PY
)"
check "$socks_denied" "refused" "the socks5 exit refuses a target outside allow_targets"

# The same half-close, this time with the socks5 leg in front of it: the relay has
# to carry the FIN through the exit as well as the answer back.
socks_halfclose="$(python3 - "$SOCKS_PORT" "$HALFCLOSE_PORT" <<'PY'
import socket, struct, sys

def open_target(port, host, target_port):
    s = socket.create_connection(("127.0.0.1", int(port)), timeout=15)
    s.settimeout(15)
    s.sendall(b"\x05\x01\x00")
    if s.recv(2)[1] != 0:
        s.close()
        return None
    s.sendall(b"\x05\x01\x00\x01" + socket.inet_aton(host) + struct.pack(">H", int(target_port)))
    head = s.recv(4)
    if len(head) < 4 or head[1] != 0:
        s.close()
        return None
    if head[3] == 1:
        s.recv(6)
    elif head[3] == 4:
        s.recv(18)
    elif head[3] == 3:
        s.recv(1 + ord(s.recv(1)) + 2)
    return s

sock = open_target(sys.argv[1], "127.0.0.1", sys.argv[2])
if sock is None:
    print("the exit refused the target")
else:
    sock.sendall(b"request-then-close-write")
    sock.shutdown(socket.SHUT_WR)
    got = b""
    try:
        while True:
            chunk = sock.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        print("no answer: %s" % exc)
    else:
        print("ok" if got == b"answered-after-half-close" else "got %r" % got)
    finally:
        sock.close()
PY
)"
check "$socks_halfclose" "ok" "a half-close through the socks5 exit still gets its answer"

# The other half of the socks5 exit: a UDP ASSOCIATE. The visitor is told the relay
# address in the reply, then sends a datagram wrapped in the RFC 1928 §7 header; the
# client behind the tunnel dials the target, and the reply comes back with the target
# as its source.
socks_udp="$(python3 - "$SOCKS_PORT" "$UDP_ECHO_PORT" <<'PY'
import socket, struct, sys

sock_port, target_port = int(sys.argv[1]), int(sys.argv[2])

s = socket.create_connection(("127.0.0.1", sock_port), timeout=15)
s.settimeout(15)
s.sendall(b"\x05\x01\x00")
if s.recv(2)[1] != 0:
    print("refused: greeting")
    s.close()
    sys.exit()
s.sendall(b"\x05\x03\x00\x01" + b"\x00\x00\x00\x00" + b"\x00\x00")
head = s.recv(4)
if len(head) < 4 or head[1] != 0:
    print("refused: associate %r" % head)
    s.close()
    sys.exit()
if head[3] == 1:
    buf = s.recv(6)
    relay_ip = socket.inet_ntoa(buf[:4])
    relay_port = struct.unpack(">H", buf[4:6])[0]
elif head[3] == 4:
    buf = s.recv(18)
    relay_ip = socket.inet_ntop(socket.AF_INET6, buf[:16])
    relay_port = struct.unpack(">H", buf[16:18])[0]
elif head[3] == 3:
    length = s.recv(1)[0]
    buf = s.recv(length + 2)
    relay_ip = buf[:length].decode()
    relay_port = struct.unpack(">H", buf[length:length + 2])[0]
else:
    print("refused: address type %d" % head[3])
    s.close()
    sys.exit()

u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.settimeout(15)
payload = b"socks5-udp-through-the-exit"
wrapped = b"\x00\x00\x00\x01" + socket.inet_aton("127.0.0.1") + struct.pack(">H", target_port) + payload
u.sendto(wrapped, (relay_ip, relay_port))
try:
    data, _ = u.recvfrom(4096)
except OSError as exc:
    print("no answer: %s" % exc)
    u.close()
    s.close()
    sys.exit()
if len(data) < 4:
    print("short reply %r" % data)
else:
    atyp = data[3]
    if atyp == 1:
        off = 10
    elif atyp == 4:
        off = 22
    elif atyp == 3:
        off = 4 + 1 + data[4] + 2
    else:
        off = -1
    got = data[off:] if 0 <= off <= len(data) else b""
    print("ok" if got == payload else "got %r" % got)
u.close()
s.close()
PY
)"
check "$socks_udp" "ok" "the socks5 exit relays a UDP ASSOCIATE datagram to the target and back"

# The new path is visible in its own counters: one association, and one datagram in
# each direction at least.
socks_udp_assoc="$(metric_value aethertunnel_socks5_udp_associations_total)"
check "$([ "${socks_udp_assoc:-0}" -ge 1 ] && echo counted || echo not-counted)" "counted" \
    "the socks5 UDP association is counted"
# The reply is written to the visitor just before the counter moves, so poll rather
# than reading once: a single read could land on the forward datagram only.
socks_udp_dgrams="$(wait_metric "$DASHBOARD_PORT" "$METRICS_TOKEN" aethertunnel_socks5_udp_datagrams_total 2)"
check "$socks_udp_dgrams" "2" "the socks5 UDP datagrams are counted in both directions"

echo
echo "== what the server reports"

metrics_status="$(python3 - "$DASHBOARD_PORT" "$METRICS_TOKEN" <<'PY'
import sys, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/metrics" % sys.argv[1])
req.add_header("Authorization", "Bearer " + sys.argv[2])
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read().decode()
        ctype = resp.headers.get("Content-Type", "")
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "aethertunnel_" in body and "text/plain" in ctype else "ctype %r body %r" % (ctype, body[:80]))
PY
)"
check "$metrics_status" "ok" "the metrics endpoint serves the Prometheus exposition format"

# The configured token is what keeps the exposition private: without this check a
# server that answered every anonymous scrape would still pass the read above.
metrics_anonymous="$(python3 - "$DASHBOARD_PORT" <<'PY'
import sys, urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:%s/metrics" % sys.argv[1], timeout=10)
except urllib.error.HTTPError as exc:
    print(exc.code)
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("200")
PY
)"
check "$metrics_anonymous" "401" "the metrics endpoint refuses a request without the token"

transferred="$(api_field /api/status 'd["traffic"]["bytes_in"] + d["traffic"]["bytes_out"]' 2>/dev/null || echo 0)"
if [ "${transferred:-0}" -gt 0 ] 2>/dev/null; then
    ok "the server counted the bytes that crossed it ($transferred)"
else
    bad "the server counted the bytes that crossed it" "$transferred"
fi

streams="$(api_field /api/proxies '[p["total_connections"] for p in d["proxies"] if p["name"] == "tcp-echo"][0]' 2>/dev/null || echo 0)"
if [ "${streams:-0}" -gt 0 ] 2>/dev/null; then
    ok "the tcp proxy recorded the streams it served ($streams)"
else
    bad "the tcp proxy recorded the streams it served" "$streams"
fi

# The audit log is what makes a connection attributable afterwards, so the
# registration and the visitor's acceptance both have to be in it.
audit_events="$(python3 - "$WORK/audit.jsonl" <<'PY'
import json, sys
names = set()
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                names.add(json.loads(line).get("event", ""))
            except ValueError:
                continue
except FileNotFoundError:
    pass
print(",".join(sorted(names)))
PY
)"
case "$audit_events" in
    *proxy_registered*) ok "the audit log recorded the proxy registrations" ;;
    *) bad "the audit log recorded the proxy registrations" "events: $audit_events" ;;
esac
case "$audit_events" in
    *visitor_accepted*) ok "the audit log recorded the visitor's acceptance" ;;
    *) bad "the audit log recorded the visitor's acceptance" "events: $audit_events" ;;
esac

echo
echo "== finding the server by name through the DHT"

# A server-role process that only queries: giving it the running server's own
# listen address would collide with the node already bound there, so the query node
# takes its own port and learns the rest from the bootstrap.
cat > dht-lookup.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $DHT_LOOKUP_PORT
auth_token = "$TOKEN"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$DHT_PORT"]
EOF

# A client-role process with no server address at all: the name is the only way it
# has to find the server, and a private name resolves to the control port.
cat > dht-client.toml <<EOF
[client]
auth_token = "$TOKEN"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$DHT_PORT"]
discover = "private"
EOF

# The announcement is published when a proxy registers, so the lookup is retried
# rather than racing the registration above. Only stdout is read: the discovery
# node logs its bootstrap to stderr, and that warning is not the answer. The budget
# is deliberately small: a lookup that has not resolved by then is not going to, and
# a broken one should not hold the run for minutes.
dht_public=""
dht_control=""
for _ in $(seq 1 12); do
    dht_public="$("$SERVER" --config dht-lookup.toml --dht-lookup tcp-echo 2> dht-public.err | tail -1)"
    dht_control="$("$CLIENT" --config dht-client.toml --discover private 2> dht-control.err | tail -1)"
    case "$dht_public" in *"-> 127.0.0.1:$TCP_PROXY_PORT"*) ;; *) dht_public="";; esac
    case "$dht_control" in *"-> 127.0.0.1:$CONTROL_PORT"*) ;; *) dht_control="";; esac
    [ -n "$dht_public" ] && [ -n "$dht_control" ] && break
    sleep 0.5
done

if [ -n "$dht_public" ]; then
    ok "a name lookup resolves a tcp proxy to the port it is published on ($dht_public)"
else
    bad "a name lookup resolves a tcp proxy to the port it is published on" "$(tr '\n' ' ' < dht-public.err)"
fi
if [ -n "$dht_control" ]; then
    ok "a client-role lookup resolves a private name to the control port ($dht_control)"
else
    bad "a client-role lookup resolves a private name to the control port" "$(tr '\n' ' ' < dht-control.err)"
fi

# A name nobody announced has to fail rather than resolve to something else. The
# whole output is matched rather than its last line: the discovery node may log a
# line of its own at exit (a bootstrap that was still running when the process was
# asked to stop), and which line comes last is not something the answer depends on.
unknown_output="$("$CLIENT" --config dht-client.toml --discover no-such-proxy-anywhere 2>&1)"
case "$unknown_output" in
    *no-such-proxy-anywhere*failed*) ok "an unknown name is reported as unresolved" ;;
    *no-such-proxy-anywhere*) bad "an unknown name is reported as unresolved but not as a failure" "$unknown_output" ;;
    *) bad "an unknown name is reported as unresolved" "$unknown_output" ;;
esac

# The whole point of the record: a client that is told nothing but a name reaches
# the server and carries data. This visitor has no server_addr at all.
cat > dht-visitor.toml <<EOF
[client]
auth_token = "$TOKEN"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$DHT_PORT"]
discover = "private"

[identity]
enabled = true
key_file = "$WORK/visitor-identity.key"

[[visitors]]
name = "by-name"
type = "stcp"
server_name = "private"
secret_key = "$STCP_SECRET"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $DHT_VISITOR_PORT
EOF

"$CLIENT" --config dht-visitor.toml > dht-visitor.log 2>&1 &
PIDS+=($!)
wait_for_tcp "$DHT_VISITOR_PORT" 120 || true

dht_transfer="$(python3 - "$DHT_VISITOR_PORT" <<'PY'
import socket, sys, time
payload = b"found-the-server-by-name"
deadline = time.time() + 40
last = "no connection"
while time.time() < deadline:
    try:
        s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
    except OSError as exc:
        last = "no connection: %s" % exc
        time.sleep(0.5)
        continue
    s.settimeout(15)
    try:
        s.sendall(payload)
        got = b""
        while len(got) < len(payload):
            chunk = s.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        last = "no answer: %s" % exc
    else:
        if got == payload:
            print("ok")
            s.close()
            sys.exit()
        last = "got %r" % got
    finally:
        s.close()
    time.sleep(0.5)
print(last)
PY
)"
check "$dht_transfer" "ok" "a client given only a name reaches the server and carries data"

# The API and the command line describe the same node, and the names it says it
# announces are the ones a lookup can resolve. The key matters in particular: it is what
# an operator hands out for trusted_keys.
check "$(api_field /api/dht 'd["enabled"]')" "True" "/api/dht reports the directory is on"
check "$(api_field /api/dht 'len(d["node_id"])')" "40" \
    "the node identifier is a 160-bit hex id"
check "$(api_field /api/dht 'd["addr"]')" "127.0.0.1:$DHT_PORT" \
    "the directory reports the address it bound"
check "$(api_field /api/dht 'd["namespace"]')" "aethertunnel" \
    "the key namespace is the default one"
check "$(api_field /api/dht 'd["advertise_as"]')" "127.0.0.1" \
    "it advertises the host the configuration names"
check "$(api_field /api/dht 'd["signing_key"]')" \
    "$("$SERVER" --dht-key -config server.toml 2>/dev/null | tail -1)" \
    "-dht-key prints the announcement key the API publishes"
check "$(api_field /api/dht '"tcp-echo" in d["announced"]')" "True" \
    "a connected client's proxy is announced under its name"
check "$(api_field /api/dht 'd["announced"] == sorted(d["announced"])')" "True" \
    "the announced names are reported in a stable order"

echo
echo "== a name is withdrawn when its last client leaves"

# The record follows the group rather than the member: a second client publishing the
# same name must not take the name away when the first one goes, and the name must go
# once the last one leaves. Nothing covered this before — the withdrawal happens only on
# the teardown path, which the lookups above never reach.
DHT_POOL_A_PORT="$(free_port)"
DHT_POOL_B_PORT="$(free_port)"
for dht_member in a b; do
    if [ "$dht_member" = "a" ]; then dht_port="$DHT_POOL_A_PORT"; else dht_port="$DHT_POOL_B_PORT"; fi
    cat > "dht-pool-$dht_member.toml" <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "dht-pooled"
type = "tcp"
group = "dht-pool-g"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $dht_port
EOF
done

"$CLIENT" --config dht-pool-a.toml > dht-pool-a.log 2>&1 &
DHT_POOL_A=$!
PIDS+=("$DHT_POOL_A")
check "$(wait_api_field /api/dht '"dht-pooled" in d["announced"]' True)" "True" \
    "a name is announced once a client serves it"

"$CLIENT" --config dht-pool-b.toml > dht-pool-b.log 2>&1 &
DHT_POOL_B=$!
PIDS+=("$DHT_POOL_B")
check "$(wait_api_field /api/proxies \
    '[p["member_count"] for p in d["proxies"] if p["name"] == "dht-pooled"][0]' 2)" "2" \
    "a second client joins the same name as a pool"

kill -9 "$DHT_POOL_A" 2>/dev/null || true
check "$(wait_api_field /api/proxies \
    '[p["member_count"] for p in d["proxies"] if p["name"] == "dht-pooled"][0]' 1)" "1" \
    "one member leaving leaves the other serving the name"
check "$(api_field /api/dht '"dht-pooled" in d["announced"]')" "True" \
    "and the name stays announced while that member is there"

kill -9 "$DHT_POOL_B" 2>/dev/null || true
check "$(wait_api_field /api/dht '"dht-pooled" in d["announced"]' False)" "False" \
    "the name is withdrawn once the last client leaves"
check "$(api_field /api/dht '"tcp-echo" in d["announced"]')" "True" \
    "and the other clients' names are untouched"

echo
echo "== a proxy pooled between two clients"

# The second member asks for a port of its own. A pool owns one endpoint, so that
# port must stay unbound and the pool must keep reporting the first member's.
cat > member-two.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "pooled"
type = "tcp"
group = "pool-a"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $POOL_OTHER_PORT
EOF

"$CLIENT" --config member-two.toml > member-two.log 2>&1 &
MEMBER_TWO_PID=$!
PIDS+=($MEMBER_TWO_PID)

pool_members=""
for _ in $(seq 1 120); do
    pool_members="$(api_field /api/proxies '[p["member_count"] for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
    [ "$pool_members" = "2" ] && break
    sleep 0.25
done
check "$pool_members" "2" "the pool reports both members"

pool_port="$(api_field /api/proxies '[p["remote_port"] for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
check "$pool_port" "$POOL_PROXY_PORT" "the pool keeps the first member's port, not the late member's"

# The late member's requested port was never bound: nothing answers there.
late_member_port="$(python3 - "$POOL_OTHER_PORT" <<'PY'
import socket, sys
try:
    socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=5).close()
except OSError:
    print("closed")
else:
    print("open")
PY
)"
check "$late_member_port" "closed" "the port the second member asked for is not listening"

# Six connections, and both members have to have served at least one: that is what
# a round-robin pool does, and what a pool that always picks the first member
# would fail.
served_both=""
for _ in $(seq 1 20); do
    python3 - "$POOL_PROXY_PORT" 6 <<'PY' > /dev/null 2>&1
import socket, sys
port = int(sys.argv[1])
for _ in range(int(sys.argv[2])):
    s = socket.create_connection(("127.0.0.1", port), timeout=15)
    s.settimeout(15)
    s.sendall(b"pooled")
    s.recv(64)
    s.close()
PY
    served_both="$(api_field /api/proxies '[sum(1 for m in p["members"] if m["total_connections"] > 0) for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
    [ "$served_both" = "2" ] && break
    sleep 0.5
done
check "$served_both" "2" "both members served some of the connections"

# Losing a member must not lose the proxy. The survivor is then the only one that
# can serve, and the removal has to reach the audit log. The wait reaps the job so
# the shell does not report the kill as a job status of its own.
#
# The audit check below only looks at records written from here on: the log is
# shared with every earlier scenario, and a bare "proxy_removed" was already in it
# (the DHT pool's members), so it could never have failed.
pool_audit_mark=0
[ -f "$WORK/audit.jsonl" ] && pool_audit_mark="$(wc -l < "$WORK/audit.jsonl")"
kill -9 "$MEMBER_TWO_PID" 2>/dev/null || true
wait "$MEMBER_TWO_PID" 2>/dev/null || true
survivor=""
for _ in $(seq 1 120); do
    survivor="$(api_field /api/proxies '[p["member_count"] for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
    [ "$survivor" = "1" ] && break
    sleep 0.25
done
check "$survivor" "1" "the pool drops the member that left"

after_leave="$(python3 - "$POOL_PROXY_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(b"still-here")
    got = s.recv(64)
    s.close()
    print("ok" if got == b"still-here" else "got %r" % got)
PY
)"
check "$after_leave" "ok" "the pool still serves after a member leaves"

if tail -n +"$((pool_audit_mark + 1))" "$WORK/audit.jsonl" 2>/dev/null \
        | grep '"event":"proxy_removed"' | grep -q '"proxy":"pooled"'; then
    ok "the audit log recorded the departure"
else
    bad "the audit log recorded the departure" "$(tail -1 "$WORK/audit.jsonl" 2>/dev/null)"
fi

echo
echo "== a pool whose member's service is down"

# The strategy is a server-wide setting, so this needs a server of its own. One
# member's local service is dead: it never answers, so it never gets a latency
# measurement, and the latency strategy used to hand it every visit (31 of 30
# attempts). What a pool costs in that state is the failing member's own counter,
# which is what this reads.
LAT_CONTROL="$(free_port)"
LAT_DASHBOARD="$(free_port)"
LAT_POOL="$(free_port)"
LAT_DEAD_PORT="$(free_port)"

cat > latency-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $LAT_CONTROL
auth_token = "$TOKEN"
load_balance = "latency"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $LAT_DASHBOARD
token = "$DASHBOARD_TOKEN"
EOF

cat > latency-live.toml <<EOF
[client]
server_addr = "127.0.0.1:$LAT_CONTROL"
auth_token = "$TOKEN"

[[proxies]]
name = "pooled"
type = "tcp"
group = "latency-pool"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $LAT_POOL
EOF

cat > latency-dead.toml <<EOF
[client]
server_addr = "127.0.0.1:$LAT_CONTROL"
auth_token = "$TOKEN"

[[proxies]]
name = "pooled"
type = "tcp"
group = "latency-pool"
local_ip = "127.0.0.1"
local_port = $LAT_DEAD_PORT
remote_port = $((LAT_POOL + 1))
EOF

# This section reads the dashboard of the server it starts, so it names it in every call
# rather than reassigning DASHBOARD_PORT: the helpers are shared with the rest of the
# suite, and repointing them here is what sent the sections below to this server.
LAT_FIELD() {
    api_field_at "$LAT_DASHBOARD" "$DASHBOARD_TOKEN" "$1" "$2"
}
"$SERVER" --config latency-server.toml > latency-server.log 2>&1 &
PIDS+=($!)
wait_for_tcp "$LAT_DASHBOARD" 120 || true
"$CLIENT" --config latency-live.toml > latency-live.log 2>&1 &
PIDS+=($!)
"$CLIENT" --config latency-dead.toml > latency-dead.log 2>&1 &
PIDS+=($!)

latency_members=""
for _ in $(seq 1 120); do
    latency_members="$(LAT_FIELD /api/proxies '[p["member_count"] for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
    [ "$latency_members" = "2" ] && break
    sleep 0.25
done
check "$latency_members" "2" "the latency pool reports both members"

# The pool's endpoint is whichever member registered first, so it is read back
# rather than assumed.
latency_pool="$(LAT_FIELD /api/proxies '[p["remote_port"] for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 0)"
latency_served="$(python3 - "$latency_pool" 10 <<'PY'
import socket, sys
port, visits = int(sys.argv[1]), int(sys.argv[2])
served = 0
for _ in range(visits):
    try:
        s = socket.create_connection(("127.0.0.1", port), timeout=15)
        s.settimeout(15)
        s.sendall(b"latency")
        if s.recv(32) == b"latency":
            served += 1
        s.close()
    except OSError:
        pass
print(served)
PY
)"
check "$latency_served" "10" "every visit is served although one member's service is down"

# One attempt may land on the dead member before anything has been measured; the
# visits after that must go to the member that answers.
dead_attempts="$(LAT_FIELD /api/proxies '[max(m["consecutive_failures"] for m in p["members"]) for p in d["proxies"] if p["name"] == "pooled"][0]' 2>/dev/null || echo 99)"
if [ "${dead_attempts:-99}" -le 2 ] 2>/dev/null; then
    ok "the latency strategy left the member that never answers behind ($dead_attempts wasted attempt(s) in 10 visits)"
else
    bad "the latency strategy left the member that never answers behind" "$dead_attempts wasted attempt(s) in 10 visits"
fi

# Ranking the dead member last must not mean burying it: when its service comes
# back, the pool has to find it again. The listener that answers "RECOVERED" is what
# makes the two members tell themselves apart in a reply.
cat > recovered.py <<'PY'
import socket, sys
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", int(sys.argv[1])))
listener.listen(8)
while True:
    conn, _ = listener.accept()
    try:
        conn.recv(64)
        conn.sendall(b"RECOVERED")
    except OSError:
        pass
    finally:
        conn.close()
PY
python3 recovered.py "$LAT_DEAD_PORT" > /dev/null 2>&1 &
PIDS+=($!)
wait_for_tcp "$LAT_DEAD_PORT" 40 || bad "the recovered service came up" "the port never accepted a connection"

recovered_served="$(python3 - "$latency_pool" 25 <<'PY'
import socket, sys
port, visits = int(sys.argv[1]), int(sys.argv[2])
answered = 0
for _ in range(visits):
    try:
        s = socket.create_connection(("127.0.0.1", port), timeout=15)
    except OSError:
        continue
    try:
        s.settimeout(15)
        s.sendall(b"latency")
        if s.recv(32) == b"RECOVERED":
            answered += 1
    except OSError:
        pass
    finally:
        s.close()
print(answered)
PY
)"
if [ "${recovered_served:-0}" -ge 1 ] 2>/dev/null; then
    ok "the member whose service came back is used again ($recovered_served of 25 visits)"
else
    bad "the member whose service came back is used again" "it served none of 25 visits"
fi

echo
echo "== what the server refuses before a handshake"

# The rules that are decided before a client is let in get a server of their own: a
# source in deny_cidrs, and a source that has spent its connection burst. On the
# deployment above either would refuse or starve every check that comes after it.
# 127.0.0.2 is this machine's second loopback address, which is what makes a refused
# source available without inventing one.
GUARD_CONTROL="$(free_port)"
GUARD_DASHBOARD="$(free_port)"

cat > guard-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $GUARD_CONTROL
auth_token = "$TOKEN"
# The two connections inside the burst below are let through to the handshake and
# then say nothing, so a short handshake timeout keeps them from lingering.
handshake_timeout_seconds = 2
deny_cidrs = ["127.0.0.2/32"]
# Far below one token per second on purpose: the burst goes through and every
# attempt after it is refused however much time passes between attempts, so the
# outcome does not depend on how loaded the machine is.
rate_limit_per_second = 0.01
rate_limit_burst = 2

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $GUARD_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = "$WORK/guard-audit.jsonl"
EOF

"$SERVER" --config guard-server.toml > guard-server.log 2>&1 &
PIDS+=($!)

# Readiness is waited for on the dashboard rather than on the control port:
# wait_for_tcp detects it by connecting, and every connection to this server spends
# a token from the burst the check below is about to measure. The dashboard is a
# listener of its own and is not subject to the access rules.
guard_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$GUARD_DASHBOARD" 1 2>/dev/null; then
        guard_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$guard_ready" = "yes" ]; then
    # Three connections from a source the deny list names. Nothing is sent: the rule
    # is applied to the accepted socket, before a frame is read.
    guard_refused="$(python3 - "$GUARD_CONTROL" <<'PY'
import socket, sys, time
refused = 0
for _ in range(3):
    s = socket.socket()
    s.bind(("127.0.0.2", 0))
    try:
        s.connect(("127.0.0.1", int(sys.argv[1])))
    except OSError:
        refused += 1
        s.close()
        continue
    # The refusal is the connection being closed without an answer, which arrives
    # either as an empty read or as a reset. The read is bounded, so a connection
    # that is merely left open is not counted: without that, this check would pass
    # whenever the server stopped refusing and the close it waited for came from the
    # handshake timeout instead.
    s.settimeout(1)
    try:
        if s.recv(1) == b"":
            refused += 1
    except socket.timeout:
        pass
    except OSError:
        refused += 1
    s.close()
    time.sleep(0.1)
print(refused)
PY
)"
    check "$guard_refused" "3" "a source in the deny list is turned away as soon as it connects"
    check "$(wait_metric "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_connections_denied_by_acl_total 3)" "3" \
        "the deny-list refusal is counted once per connection"
    check "$(metric_at "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_rejected_total)" "3" \
        "each deny-list refusal also lands in the shared rejection counter"
    check "$(metric_at "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total)" "0" \
        "a refused source is not counted as an accepted control connection"
    check "$(grep -c 'denied by access control' guard-server.log 2>/dev/null || true)" "3" \
        "the server logged the refusal as an access-control decision"

    guard_acl_audit="$(python3 - "$WORK/guard-audit.jsonl" <<'PY'
import json, sys
named, detail = 0, ""
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if record.get("event") == "acl_denied" and record.get("remote", "").startswith("127.0.0.2"):
                named += 1
                detail = record.get("detail", "")
except FileNotFoundError:
    pass
print("%d %s" % (named, detail))
PY
)"
    check "$guard_acl_audit" "3 source address rejected by allow/deny lists" \
        "the audit log names the refused source and the rule that refused it"

    # The burst is two connections and the rate is one hundredth of a token per
    # second, so no token is refilled inside this check: the first two attempts are
    # let through and every later one is refused. The counter is cumulative for the
    # life of the server, so it is read as a delta.
    burst_before="$(metric_at "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_connections_rate_limited_total)"
    burst_before="${burst_before:-0}"
    python3 - "$GUARD_CONTROL" 2 <<'PY'
import socket, sys, time
for _ in range(int(sys.argv[2])):
    try:
        socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=2).close()
    except OSError:
        pass
    time.sleep(0.1)
PY
    sleep 1
    check "$(metric_at "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_connections_rate_limited_total)" "$burst_before" \
        "a connection inside the burst is not refused by the rate limit"

    python3 - "$GUARD_CONTROL" 3 <<'PY'
import socket, sys, time
for _ in range(int(sys.argv[2])):
    try:
        socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=2).close()
    except OSError:
        pass
    time.sleep(0.1)
PY
    check "$(wait_metric "$GUARD_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_connections_rate_limited_total "$((burst_before + 3))")" \
        "$((burst_before + 3))" "every attempt past the burst is refused by the rate limit"
    check "$(grep -c '"event":"rate_limited"' "$WORK/guard-audit.jsonl" 2>/dev/null || true)" "3" \
        "every rate-limit refusal is in the audit log"
    check "$(grep -c 'denied by rate limit' guard-server.log 2>/dev/null || true)" "3" \
        "the server logged each rate-limit refusal"
else
    bad "the access-control server came up" "$(tail -2 guard-server.log)"
fi

# A server of its own for the ban as well: banning a loopback source on either of
# the servers above would refuse everything that follows. Its ban window is a few
# seconds long, so the same check can watch the ban end.
BAN_CONTROL="$(free_port)"
BAN_DASHBOARD="$(free_port)"
BAN_PROXY_PORT="$(free_port)"

cat > ban-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $BAN_CONTROL
auth_token = "$TOKEN"
ban_after_failures = 3
ban_seconds = 10

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $BAN_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = "$WORK/ban-audit.jsonl"
EOF

# The failing client retries every second, which is what accumulates three failures
# inside the check. Nothing about it but its token is wrong, so what the counters
# below measure is the ban and not a client that could never have connected.
cat > ban-bad-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$BAN_CONTROL"
auth_token = "the-wrong-token"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "ban-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $BAN_PROXY_PORT
EOF

# The same client with the right token: started while the source is banned, it is
# the control that shows the earlier refusals were the ban.
cat > ban-good-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$BAN_CONTROL"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "ban-good"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $BAN_PROXY_PORT
EOF

"$SERVER" --config ban-server.toml > ban-server.log 2>&1 &
PIDS+=($!)
ban_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$BAN_DASHBOARD" 1 2>/dev/null; then
        ban_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$ban_ready" = "yes" ]; then
    "$CLIENT" --config ban-bad-client.toml > ban-bad-client.log 2>&1 &
    PIDS+=($!)
    check "$(wait_metric "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_sources_banned_total 1)" "1" \
        "a source that keeps failing authentication is banned"

    ban_failures="$(metric_at "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_auth_failures_total)"
    if [ "${ban_failures:-0}" -ge 3 ] 2>/dev/null; then
        ok "the failures that earned the ban are counted ($ban_failures)"
    else
        bad "the failures that earned the ban are counted" "$ban_failures"
    fi

    # The record is what makes a brute-force attempt visible after the fact, so each
    # failure has to be written down with the source it came from.
    ban_audited="$(python3 - "$WORK/ban-audit.jsonl" <<'PY'
import json, sys
count = 0
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if record.get("event") == "auth_failed" and record.get("outcome") == "denied" and record.get("remote"):
                count += 1
except FileNotFoundError:
    pass
print(count)
PY
)"
    if [ "${ban_audited:-0}" -ge 3 ] 2>/dev/null; then
        ok "every failed authentication is audited with its source and outcome ($ban_audited)"
    else
        bad "every failed authentication is audited with its source and outcome" "$ban_audited"
    fi
    check "$(grep -c 'banned after 3 failed attempt' "$WORK/ban-audit.jsonl" 2>/dev/null || true)" "1" \
        "the ban is recorded with the number of failures that earned it"

    # The client with the right token is started while the source is banned. No
    # session may appear while the ban stands, however often it retries.
    "$CLIENT" --config ban-good-client.toml > ban-good-client.log 2>&1 &
    PIDS+=($!)
    sleep 3
    check "$(metric_at "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total)" "0" \
        "a client with the right token is refused while its source is banned"
    ban_refused="$(metric_at "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_banned_connections_refused_total)"
    if [ "${ban_refused:-0}" -ge 1 ] 2>/dev/null; then
        ok "the refusals of the banned source are counted ($ban_refused)"
    else
        bad "the refusals of the banned source are counted" "$ban_refused"
    fi
    check "$(grep -c 'as session' ban-good-client.log 2>/dev/null || true)" "0" \
        "the client refused by the ban logged no session"

    # A ban is a window rather than a removal: once ban_seconds have passed the same
    # client gets in, which is what shows the refusal above was the ban and not a
    # client that could never connect.
    check "$(wait_metric "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total 1)" "1" \
        "the source is let in again when the ban expires"
    ban_session="$(grep -c 'as session' ban-good-client.log 2>/dev/null || true)"
    if [ "${ban_session:-0}" -ge 1 ] 2>/dev/null; then
        ok "the client that waited out the ban established its session"
    else
        bad "the client that waited out the ban established its session" "$ban_session"
    fi
else
    bad "the ban server came up" "$(tail -2 ban-server.log)"
fi

# A third server for ban_ignore_cidrs: the ignore list exists so that a source
# behind a load balancer is never banned, which is the opposite of what the ban
# server above has to show, so the two cannot share one configuration.
IGNORE_CONTROL="$(free_port)"
IGNORE_DASHBOARD="$(free_port)"
IGNORE_PROXY_PORT="$(free_port)"

cat > ignore-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $IGNORE_CONTROL
auth_token = "$TOKEN"
ban_after_failures = 2
ban_seconds = 30
ban_ignore_cidrs = ["127.0.0.1/32"]

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $IGNORE_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = "$WORK/ignore-audit.jsonl"
EOF

cat > ignore-bad-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$IGNORE_CONTROL"
auth_token = "the-wrong-token"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "ignore-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $IGNORE_PROXY_PORT
EOF

"$SERVER" --config ignore-server.toml > ignore-server.log 2>&1 &
PIDS+=($!)
ignore_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$IGNORE_DASHBOARD" 1 2>/dev/null; then
        ignore_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$ignore_ready" = "yes" ]; then
    "$CLIENT" --config ignore-bad-client.toml > ignore-bad-client.log 2>&1 &
    PIDS+=($!)
    # Past the threshold of two with room to spare, so that what the next check
    # reads is a source that has earned a ban rather than one that is about to.
    sleep 5
    ignore_failures="$(metric_at "$IGNORE_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_auth_failures_total)"
    if [ "${ignore_failures:-0}" -ge 3 ] 2>/dev/null; then
        ok "the ignored source kept failing ($ignore_failures attempts)"
    else
        bad "the ignored source kept failing" "$ignore_failures"
    fi
    check "$(metric_at "$IGNORE_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_sources_banned_total)" "0" \
        "a source named by ban_ignore_cidrs is not banned"
    check "$(metric_at "$IGNORE_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_banned_connections_refused_total)" "0" \
        "an ignored source is never refused as banned"
else
    bad "the ignore-list server came up" "$(tail -2 ignore-server.log)"
fi

echo
echo "== the server at its connection limit"

# The session limit is the one refusal that is decided after authentication: the
# client is believed and then told there is no room. A server of its own, because
# max_connections = 1 would leave nothing for the checks above.
CAP_CONTROL="$(free_port)"
CAP_DASHBOARD="$(free_port)"
CAP_PROXY_PORT="$(free_port)"

cat > cap-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $CAP_CONTROL
auth_token = "$TOKEN"
max_connections = 1

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $CAP_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = "$WORK/cap-audit.jsonl"
EOF

# Two clients that both hold a valid token: the first takes the only session, the
# second has to be turned away, and its retries are what the checks read.
for name in a b; do
    cat > "cap-client-$name.toml" <<EOF
[client]
server_addr = "127.0.0.1:$CAP_CONTROL"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "cap-$name"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $CAP_PROXY_PORT
EOF
done

"$SERVER" --config cap-server.toml > cap-server.log 2>&1 &
PIDS+=($!)
cap_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$CAP_DASHBOARD" 1 2>/dev/null; then
        cap_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$cap_ready" = "yes" ]; then
    "$CLIENT" --config cap-client-a.toml > cap-client-a.log 2>&1 &
    PIDS+=($!)
    check "$(wait_metric "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total 1)" "1" \
        "the first client takes the only session the server allows"
    "$CLIENT" --config cap-client-b.toml > cap-client-b.log 2>&1 &
    CAP_B=$!
    PIDS+=($CAP_B)
    sleep 4
    check "$(metric_at "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total)" "1" \
        "the session past the limit is not admitted"
    cap_refused="$(metric_at "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_rejected_total)"
    if [ "${cap_refused:-0}" -ge 1 ] 2>/dev/null; then
        ok "the refusal at capacity is counted ($cap_refused)"
    else
        bad "the refusal at capacity is counted" "$cap_refused"
    fi
    cap_audited="$(grep -c 'at its connection limit' "$WORK/cap-audit.jsonl" 2>/dev/null || true)"
    if [ "${cap_audited:-0}" -ge 1 ] 2>/dev/null; then
        ok "the audit log records the refusal at capacity"
    else
        bad "the audit log records the refusal at capacity" "$cap_audited"
    fi
    cap_told="$(grep -c 'at its connection limit' cap-client-b.log 2>/dev/null || true)"
    if [ "${cap_told:-0}" -ge 1 ] 2>/dev/null; then
        ok "the client that did not fit is told why it was refused"
    else
        bad "the client that did not fit is told why it was refused" "$cap_told"
    fi

    # The client that did not fit is stopped here: it retries every second, and each
    # retry adds one to the shared rejection counter, which is exactly the counter the
    # next check reads as a delta. It is asked to stop rather than killed, so the shell
    # does not report a terminated job in the middle of the output.
    if [ -n "${CAP_B:-}" ]; then
        kill "$CAP_B" 2>/dev/null || true
        wait "$CAP_B" 2>/dev/null || true
    fi

    # A first frame that is not a usable request is answered with a refusal, so it has
    # to leave the same trace as every other refusal. This is the one server here with
    # neither a rate limit nor a ban, so a connection from loopback reaches the frame
    # reader; the two frames below are what a scan of the control port looks like.
    cap_rejected_before="$(metric_at "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_rejected_total)"
    cap_rejected_before="${cap_rejected_before:-0}"
    cap_frames="$(python3 - "$CAP_CONTROL" <<'PY'
import socket, struct, sys
port = int(sys.argv[1])

def send(first_byte, payload):
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    s.sendall(bytes([first_byte, 0]) + struct.pack(">I", len(payload)) + payload)
    s.settimeout(5)
    try:
        answer = s.recv(4096)
    except OSError:
        answer = b""
    s.close()
    return answer

answered = 0
# A frame that claims to be an authentication request but carries no usable payload.
if b"malformed auth request" in send(1, b"this is not json"):
    answered += 1
# A frame type that may not start a connection at all.
if b"first frame must be" in send(3, b"{}"):
    answered += 1
print(answered)
PY
)"
    check "$cap_frames" "2" "a first frame that is not a usable request is answered with the reason"
    check "$(wait_metric "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_unusable_request_frames_total 2)" "2" \
        "both unusable first frames are counted on their own"
    check "$(metric_at "$CAP_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_rejected_total)" "$((cap_rejected_before + 2))" \
        "an unusable first frame also moves the shared rejection counter"
    check "$(grep -c 'malformed auth request' "$WORK/cap-audit.jsonl" 2>/dev/null || true)" "1" \
        "the malformed request is in the audit log with its reason"
    check "$(grep -c 'which cannot start a connection' "$WORK/cap-audit.jsonl" 2>/dev/null || true)" "1" \
        "the frame type that cannot start a connection is in the audit log too"
    check "$(grep -c 'refusing .*: malformed auth request' cap-server.log 2>/dev/null || true)" "1" \
        "the server logged the malformed request"
else
    bad "the capacity-limit server came up" "$(tail -2 cap-server.log)"
fi

echo
echo "== encryption, post-quantum, TLS, identity and disguise together"

# One more pair with every security layer on at once. The server knows the owner's
# public identity key and refuses anyone else, so the transfer proves the gate let
# this client through, and each layer logs its own line: a layer that silently
# switched itself off is visible here.
SEC_CONTROL="$(free_port)"
SEC_DASHBOARD="$(free_port)"
SEC_PROXY="$(free_port)"

cat > secure-identity.toml <<EOF
[client]
server_addr = "127.0.0.1:1"
auth_token = "$TOKEN"

[identity]
enabled = true
key_file = "$WORK/owner-identity.key"
EOF
SEC_IDENTITY="$("$CLIENT" --identity --config secure-identity.toml 2>&1 | tail -1)"

cat > secure-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $SEC_CONTROL
auth_token = "$TOKEN"

[encryption]
enabled = true
algorithm = "xchacha20-poly1305"
passphrase = "functional-linux-passphrase"
salt = "functional-linux-salt"
post_quantum = true

[transport]
enable_tls = true
cert_file = "$WORK/server.crt"
key_file = "$WORK/server.key"

[identity]
enabled = true
require_identity = true
allowed_keys = ["$SEC_IDENTITY"]

[obfuscation]
enabled = true
pad_to = 256
disguise = "tls-record"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $SEC_DASHBOARD
token = "$DASHBOARD_TOKEN"
EOF

cat > secure-owner.toml <<EOF
[client]
server_addr = "127.0.0.1:$SEC_CONTROL"
auth_token = "$TOKEN"

# enable_tls and ca_file live in [transport]: the client has to be told to speak
# TLS to the control port, and a trust anchor given anywhere else is ignored.
[transport]
enable_tls = true
ca_file = "$WORK/server.crt"

[encryption]
enabled = true
algorithm = "xchacha20-poly1305"
passphrase = "functional-linux-passphrase"
salt = "functional-linux-salt"
post_quantum = true

[obfuscation]
enabled = true
pad_to = 256
disguise = "tls-record"

[identity]
enabled = true
key_file = "$WORK/owner-identity.key"

[[proxies]]
name = "secure-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $SEC_PROXY
EOF

"$SERVER" --config secure-server.toml > secure-server.log 2>&1 &
PIDS+=($!)
"$CLIENT" --config secure-owner.toml > secure-owner.log 2>&1 &
PIDS+=($!)

secure_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$SEC_PROXY" 1 2>/dev/null; then
        secure_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$secure_ready" = "yes" ]; then
    secure_result="$(python3 - "$SEC_PROXY" <<'PY'
import socket, sys
payload = b"through-every-layer"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=20)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(20)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
)"
    check "$secure_result" "ok" "a transfer survives encryption, post-quantum, TLS, identity and disguise"
else
    bad "the secured pair came up" "$(tail -2 secure-server.log) | $(tail -2 secure-owner.log)"
    echo "--- secure-server.log ---"; tail -8 secure-server.log
    echo "--- secure-owner.log ---"; tail -8 secure-owner.log
fi

# Each layer announces itself, and the lines differ per layer, so this catches a
# layer that stopped being applied rather than one that stopped working.
for layer in "post-quantum:post-quantum" "tls:wrapped in TLS" "identity:require_identity=true" "disguise:connection disguise"; do
    label="${layer%%:*}"
    needle="${layer#*:}"
    if grep -qF "$needle" secure-server.log secure-owner.log 2>/dev/null; then
        ok "the $label layer announced itself in the logs"
    else
        bad "the $label layer announced itself in the logs" "no line containing '$needle'"
    fi
done

echo
echo "== a session disguise that is a real TLS handshake"

# tls-session wraps every connection in a genuine TLS handshake with a freshly
# minted self-signed certificate, which is what the record disguise deliberately
# lacks. The transfer proves the tunnel runs inside it, a TLS-speaking probe must
# see a session that is one, and a peer that sends record bytes without a
# handshake must be refused.
SES_CONTROL="$(free_port)"
SES_DASHBOARD="$(free_port)"
SES_PROXY="$(free_port)"

cat > session-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $SES_CONTROL
auth_token = "$TOKEN"

[obfuscation]
enabled = true
disguise = "tls-session"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $SES_DASHBOARD
EOF

cat > session-owner.toml <<EOF
[client]
server_addr = "127.0.0.1:$SES_CONTROL"
auth_token = "$TOKEN"

[obfuscation]
enabled = true
disguise = "tls-session"

[[proxies]]
name = "session-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $SES_PROXY
EOF

"$SERVER" --config session-server.toml > session-server.log 2>&1 &
PIDS+=($!)
"$CLIENT" --config session-owner.toml > session-owner.log 2>&1 &
PIDS+=($!)

session_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$SES_PROXY" 1 2>/dev/null; then
        session_ready="yes"
        break
    fi
    sleep 0.5
done

if [ "$session_ready" = "yes" ]; then
    session_result="$(python3 - "$SES_PROXY" <<'PY'
import socket, sys
payload = b"through-a-real-tls-session"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=20)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(20)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
)"
    check "$session_result" "ok" "a transfer survives the session disguise"
else
    bad "the session-disguised pair came up" "$(tail -2 session-server.log) | $(tail -2 session-owner.log)"
    echo "--- session-server.log ---"; tail -8 session-server.log
    echo "--- session-owner.log ---"; tail -8 session-owner.log
fi

# A detector that models TLS sessions must see a session that is one: a completed
# handshake, a modern protocol version, and the fresh anonymous certificate.
probe_result="$(python3 - "$SES_CONTROL" <<'PY'
import socket, ssl, sys
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    raw = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
except OSError as exc:
    print("probe failed: %s" % exc)
else:
    try:
        with ctx.wrap_socket(raw, server_hostname="aethertunnel") as s:
            cert = s.getpeercert(binary_form=True)
            good = bool(cert) and s.version() in ("TLSv1.2", "TLSv1.3")
            print("ok" if good else "version %s, certificate %s" % (s.version(), bool(cert)))
    except Exception as exc:
        print("probe failed: %s" % exc)
PY
)"
check "$probe_result" "ok" "a TLS-speaking probe sees a real session on the control port"

# Record-shaped bytes without a handshake are not TLS: the connection closes
# (possibly with a TLS alert) instead of the tunnel starting.
garbage_result="$(python3 - "$SES_CONTROL" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
except OSError as exc:
    print("refused")
else:
    try:
        s.settimeout(10)
        s.sendall(b"\x17\x03\x03\x00\x01x")
        data = s.recv(64)
        if not data or data[0] == 0x15:
            print("refused")
        else:
            print("answered %r" % data[:16])
    except OSError:
        print("refused")
    finally:
        s.close()
PY
)"
check "$garbage_result" "refused" "a peer that never speaks TLS is refused"

if grep -qF "connection disguise: tls-session" session-server.log; then
    ok "the session disguise announced itself in the log"
else
    bad "the session disguise announced itself in the log" "no line containing 'connection disguise: tls-session'"
fi

echo
echo "== the bandwidth ledger"

# The ledger is the record of usage that leaves this server, and it was the one
# documented feature with no end-to-end check anywhere in the repository. It is written
# on the teardown path — one entry per proxy a client published, when the session ends —
# so this section moves a payload of a known size through a proxy of its own, ends that
# session, and then reads the file back: through the API, offline with only the public
# key, and with a byte edited by someone who should not have touched it.

LEDGER_PROBE_PORT="$(free_port)"

cat > ledger-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "ledger-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $LEDGER_PROBE_PORT
EOF

check "$(api_field /api/ledger 'd.get("enabled", False)')" "True" "/api/ledger reports the ledger is on"
ledger_key="$(api_field /api/ledger 'd.get("public_key", "")')"
check "$(printf '%s' "$ledger_key" | wc -c | tr -d ' ')" "64" \
    "the ledger publishes a 64-character hex verification key"
check "$(api_field /api/ledger 'd.get("path", "")')" "$WORK/ledger.jsonl" \
    "the file it publishes is the one [ledger] names"

"$CLIENT" --config ledger-client.toml > ledger-client.log 2>&1 &
LEDGER_CLIENT_PID=$!
PIDS+=("$LEDGER_CLIENT_PID")

ledger_registered="no"
for _ in $(seq 1 80); do
    ledger_client_id="$(api_field /api/proxies \
        '[p["client_id"] for p in d["proxies"] if p["name"] == "ledger-probe"][0]' 2>/dev/null || true)"
    if [ -n "$ledger_client_id" ] && [ "$ledger_client_id" != "None" ]; then
        ledger_registered="yes"
        break
    fi
    sleep 0.25
done
if [ "$ledger_registered" = "yes" ]; then
    ok "the client that publishes ledger-probe registered"
else
    bad "the client that publishes ledger-probe registered" "$(tail -2 ledger-client.log)"
fi

# The payload is echoed back, so both directions carry the same number of bytes and
# the entry has to bill exactly that: the ledger is compared against what the tunnel
# moved, not against itself.
ledger_sent="$(python3 - "$LEDGER_PROBE_PORT" <<'PY'
import socket, sys
payload = b"ledger-probe-payload" * 2
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print(len(payload) if got == payload else "got %r" % got)
PY
)"
check "$ledger_sent" "40" "the ledger probe's payload comes back through the tunnel"

# Ending the session is what writes the entry, so the client is stopped here.
kill -9 "$LEDGER_CLIENT_PID" 2>/dev/null || true

ledger_entries="0"
for _ in $(seq 1 80); do
    ledger_entries="$(api_field /api/ledger \
        'sum(1 for e in d.get("entries", []) if e["proxy"] == "ledger-probe")' 2>/dev/null || echo 0)"
    [ "$ledger_entries" = "1" ] && break
    sleep 0.25
done
check "$ledger_entries" "1" "the session that ended is in the ledger"
check "$(api_field /api/ledger \
    '[e["client_id"] for e in d.get("entries", []) if e["proxy"] == "ledger-probe"][0]')" \
    "$ledger_client_id" "the entry names the client whose session ended"
check "$(api_field /api/ledger \
    '[e["bytes_in"] for e in d.get("entries", []) if e["proxy"] == "ledger-probe"][0]')" \
    "$ledger_sent" "the entry bills the bytes that went in"
check "$(api_field /api/ledger \
    '[e["bytes_out"] for e in d.get("entries", []) if e["proxy"] == "ledger-probe"][0]')" \
    "$ledger_sent" "and the bytes that came back out"

ledger_count="$(api_field /api/ledger 'd.get("count", 0)')"
ledger_head="$(api_field /api/ledger 'd.get("head", "")')"
ledger_file_head="$(python3 - "$WORK/ledger.jsonl" <<'PY'
import json, sys
last = ""
for line in open(sys.argv[1], encoding="utf-8"):
    if line.strip():
        last = json.loads(line)["hash"]
print(last)
PY
)"
check "$ledger_file_head" "$ledger_head" \
    "the head the API publishes is the last hash in the file"
check "$(grep -c 'bandwidth ledger: recorded entry' server.log 2>/dev/null || true)" "$ledger_count" \
    "the server logged each entry it wrote"

# Offline verification: the file and the public key, nothing else. This is the command
# an auditor runs on a machine that has never seen this server.
ledger_verify="$("$SERVER" --verify-ledger "$WORK/ledger.jsonl" --ledger-key "$ledger_key" 2>&1)"
check "$(printf '%s\n' "$ledger_verify" | head -1)" \
    "$WORK/ledger.jsonl: $ledger_count entries verified" \
    "the ledger file verifies offline with only the public key"
check "$(printf '%s\n' "$ledger_verify" | sed -n '2p')" "chain head: $ledger_head" \
    "and it reports the chain head the API published"

# A second key of the right shape must not verify this chain.
wrong_key="$(python3 - "$ledger_key" <<'PY'
import sys
key = sys.argv[1]
print(("1" if key[0] != "1" else "2") + key[1:])
PY
)"
ledger_wrong="$("$SERVER" --verify-ledger "$WORK/ledger.jsonl" --ledger-key "$wrong_key" 2>&1)"
check "$(printf '%s' "$ledger_wrong" | grep -c 'entries verified')" "0" \
    "a different key of the same shape does not verify the chain"
check "$(printf '%s' "$ledger_wrong" | grep -c 'ledger verification failed')" "1" \
    "and says that verification failed"

# One changed byte inside an entry: the hash chain and the signature both cover it, and
# a ledger that still verifies after an edit is not evidence of anything.
cp "$WORK/ledger.jsonl" "$WORK/ledger-edited.jsonl"
python3 - "$WORK/ledger-edited.jsonl" <<'PY'
import json, sys
path = sys.argv[1]
lines = [line for line in open(path, encoding="utf-8") if line.strip()]
record = json.loads(lines[0])
record["bytes_in"] = int(record["bytes_in"]) + 1
lines[0] = json.dumps(record) + "\n"
open(path, "w", encoding="utf-8").writelines(lines)
PY
ledger_edited="$("$SERVER" --verify-ledger "$WORK/ledger-edited.jsonl" --ledger-key "$ledger_key" 2>&1)"
check "$(printf '%s' "$ledger_edited" | grep -c 'entries verified')" "0" \
    "an entry with one byte changed does not verify"
check "$(printf '%s' "$ledger_edited" | grep -c 'ledger verification failed')" "1" \
    "and that failure is reported"

# The proof prefix: one period's usage handed to an auditor without the rest of the
# chain. It has to verify on its own, with the head of the entry it stops at.
"$SERVER" --ledger-proof "$WORK/ledger.jsonl" --proof-index 0 > "$WORK/ledger-proof.jsonl" 2>"$WORK/ledger-proof.err"
proof_status=$?
check "$proof_status" "0" "a proof for the first entry is written"
check "$(wc -l < "$WORK/ledger-proof.jsonl" | tr -d ' ')" "1" "the proof holds that one entry"
first_entry_hash="$(python3 - "$WORK/ledger.jsonl" <<'PY'
import json, sys
print(json.loads(open(sys.argv[1], encoding="utf-8").readline())["hash"])
PY
)"
proof_verify="$("$SERVER" --verify-ledger "$WORK/ledger-proof.jsonl" --ledger-key "$ledger_key" 2>&1)"
check "$(printf '%s\n' "$proof_verify" | head -1)" \
    "$WORK/ledger-proof.jsonl: 1 entries verified" \
    "the proof verifies on its own with the same public key"
check "$(printf '%s\n' "$proof_verify" | sed -n '2p')" "chain head: $first_entry_hash" \
    "and its chain head is the hash of the entry it stops at"
"$SERVER" --ledger-proof "$WORK/ledger.jsonl" --proof-index 99999 > /dev/null 2>"$WORK/ledger-proof-over.err"
check "$?" "1" "a proof past the end of the chain is refused"
check "$(grep -c 'ledger proof failed' "$WORK/ledger-proof-over.err" || true)" "1" \
    "and it says why"

echo
echo "== the Kubernetes ConfigMap's server, with credentials only from the environment"

# deploy/kubernetes/ ships a ConfigMap whose server.toml holds no credential: the
# Deployment supplies AETHERTUNNEL_AUTH_TOKEN and AETHERTUNNEL_DASHBOARD_TOKEN, and
# config.ApplyEnv puts them into the loaded configuration. A unit test loads that file;
# nothing ever started it. This does: the shipped configuration with the two variables
# set, a client that authenticates with the environment's token, and a payload carried
# through a proxy that client publishes.
#
# Two things are localised so the check can run outside a cluster: the bind addresses
# (a container binds 0.0.0.0, this run binds loopback) and the absolute state paths
# (the container provides /var/lib/aethertunnel, this run uses its work directory).
# Credentials, limits, obfuscation and everything else stay as shipped.
CM_CONTROL="$(free_port)"
CM_DASHBOARD="$(free_port)"
CM_P2P="$(free_port)"
CM_DHT="$(free_port)"
CM_PROXY="$(free_port)"
CM_AUTH="configmap-auth-token-0123456789abcdef"
CM_DASH_TOKEN="configmap-dashboard-token"
# The shipped configuration points its state at /var/lib/aethertunnel; localising
# that to the shared work directory made the audit check below read the *main*
# server's log, which has been non-empty since the first scenario, and left this
# server appending into the main ledger's chain with a different key. A directory
# of its own keeps both servers' state separate.
CM_STATE="$WORK/configmap"
mkdir -p "$CM_STATE"

cm_check="$(python3 - "$HERE/deploy/kubernetes/configmap.yaml" \
    "$WORK/cm-server.toml" "$CM_STATE" "$CM_CONTROL" "$CM_DASHBOARD" "$CM_P2P" "$CM_DHT" <<'PY'
import io, re, sys
src, dst, work, control, dash, p2p, dht = sys.argv[1:8]
lines = io.open(src, encoding="utf-8").read().splitlines()
out, inside = [], False
for line in lines:
    if line.strip() == "server.toml: |":
        inside = True
        continue
    if inside:
        if line.strip() == "":
            out.append("")
            continue
        if not line.startswith("    "):
            break
        out.append(line[4:])
shipped = "\n".join(out) + "\n"
io.open(dst + ".shipped", "w", encoding="utf-8").write(shipped)

# What the check is about: the file carries no credential of its own.
leaked = [ln.strip() for ln in shipped.splitlines()
          if re.match(r"\s*(auth_token|token|passphrase)\s*=", ln)]
print("credentials=%s" % ("yes" if leaked else "no"))
if leaked:
    print("leaked: %s" % leaked)
    sys.exit(0)

localised = shipped.replace("/var/lib/aethertunnel", work)
localised = localised.replace('bind_addr = "0.0.0.0"', 'bind_addr = "127.0.0.1"')
localised = re.sub(r"(?m)^bind_port = 7001$", "bind_port = " + control, localised)
localised = re.sub(r"(?m)^port = 7500$", "port = " + dash, localised)
localised = re.sub(r"(?m)^p2p_port = 7002$", "p2p_port = " + p2p, localised)
# Whatever port the ConfigMap gives the DHT, and not the number it happens to have: the
# deployment gives the DHT one of its own now, and a literal here would leave the shipped
# port in place, where it collides with anything else on the machine.
localised = re.sub(r'(?m)^listen_addr = "0\.0\.0\.0:\d+"$',
                   'listen_addr = "127.0.0.1:%s"' % dht, localised)
io.open(dst, "w", encoding="utf-8").write(localised)
print("extracted %d lines" % len(localised.splitlines()))
PY
)"
echo "   $cm_check"
check "$(printf '%s' "$cm_check" | sed -n 's/^credentials=//p')" "no" \
    "the shipped ConfigMap holds no credential of its own"

if "$SERVER" --config "$WORK/cm-server.toml.shipped" --check > "$WORK/cm-check.log" 2>&1; then
    bad "the ConfigMap's configuration is incomplete on its own" "it validated without a credential"
else
    check "$(grep -c 'server.auth_token is required' "$WORK/cm-check.log" || true)" "1" \
        "the ConfigMap's configuration has no credential of its own: the environment supplies it"
fi
if AETHERTUNNEL_AUTH_TOKEN="$CM_AUTH" AETHERTUNNEL_DASHBOARD_TOKEN="$CM_DASH_TOKEN" \
    "$SERVER" --config "$WORK/cm-server.toml.shipped" --check > "$WORK/cm-check-env.log" 2>&1; then
    ok "and validates as soon as the environment supplies them"
else
    bad "and validates as soon as the environment supplies them" "$(tail -2 "$WORK/cm-check-env.log")"
fi

AETHERTUNNEL_AUTH_TOKEN="$CM_AUTH" AETHERTUNNEL_DASHBOARD_TOKEN="$CM_DASH_TOKEN" \
    "$SERVER" --config "$WORK/cm-server.toml" > cm-server.log 2>&1 &
PIDS+=($!)

cm_ready="no"
for _ in $(seq 1 80); do
    if wait_for_tcp "$CM_DASHBOARD" 1 2>/dev/null; then cm_ready="yes"; break; fi
    sleep 0.25
done
if [ "$cm_ready" = "yes" ]; then
    ok "the server starts with its credentials from the environment only"
else
    bad "the server starts with its credentials from the environment only" "$(tail -3 cm-server.log)"
fi

cm_no_token="$(python3 - "$CM_DASHBOARD" <<'PY'
import sys, urllib.error, urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:%s/api/status" % sys.argv[1], timeout=5)
    print("200")
except urllib.error.HTTPError as exc:
    print(exc.code)
except Exception as exc:
    print("error: %s" % exc)
PY
)"
check "$cm_no_token" "401" "the dashboard refuses a request without the environment's token"
check "$(api_field_at "$CM_DASHBOARD" "$CM_DASH_TOKEN" /api/status 'd["auth_required"]')" "True" \
    "and answers with it, reporting that a token is required"

# The ConfigMap turns the TLS-record disguise on, so this client has to match it: the
# server refuses a plain connection to the same port (checked below with the second
# client, which leaves the section out on purpose).
cat > cm-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$CM_CONTROL"
auth_token = "$CM_AUTH"

[obfuscation]
enabled = true
pad_to = 256
disguise = "tls-record"

[[proxies]]
name = "configmap-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $CM_PROXY
EOF

"$CLIENT" --config cm-client.toml > cm-client.log 2>&1 &
PIDS+=($!)

cm_transfer="$(python3 - "$CM_PROXY" <<'PY'
import socket, sys, time
payload = b"through-the-configmap-server"
deadline = time.time() + 30
last = "no connection"
while time.time() < deadline:
    try:
        s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
    except OSError as exc:
        last = "no connection: %s" % exc
        time.sleep(0.5)
        continue
    try:
        s.settimeout(10)
        s.sendall(payload)
        got = b""
        while len(got) < len(payload):
            chunk = s.recv(4096)
            if not chunk:
                break
            got += chunk
    except OSError as exc:
        last = "no answer: %s" % exc
    else:
        if got == payload:
            print("ok")
            s.close()
            sys.exit()
        last = "got %r" % got
    finally:
        s.close()
    time.sleep(0.5)
print(last)
PY
)"
check "$cm_transfer" "ok" \
    "a client that authenticates with the environment's token carries bytes"
check "$(python3 - "$CM_STATE/audit.jsonl" <<'PY'
import os, sys
print("yes" if os.path.exists(sys.argv[1]) and os.path.getsize(sys.argv[1]) > 0 else "no")
PY
)" "yes" "the audit log lands where the ConfigMap points it"

# The disguise the ConfigMap enables is not decoration: a client that does not speak it
# is refused before authentication, which is what the server's own log records.
CM_PLAIN_PORT="$(free_port)"
cat > cm-client-plain.toml <<EOF
[client]
server_addr = "127.0.0.1:$CM_CONTROL"
auth_token = "$CM_AUTH"

[[proxies]]
name = "plain-probe"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $CM_PLAIN_PORT
EOF

"$CLIENT" --config cm-client-plain.toml > cm-client-plain.log 2>&1 &
CM_PLAIN_PID=$!
sleep 3
kill -9 "$CM_PLAIN_PID" 2>/dev/null || true
cm_refusals="$(grep -c 'not carrying record-framed data' cm-server.log 2>/dev/null || true)"
check "$([ "${cm_refusals:-0}" -ge 1 ] && echo refused || echo not-refused)" "refused" \
    "a client that does not speak the ConfigMap's disguise is refused"

echo
echo "== the dashboard in a real browser"

# Everything above drives the server over the API and the tunnels; this drives the page
# itself, in the browser the page is meant for. It is the only check of the panel —
# a single HTML file with inline CSS and JS that nothing else here runs — and it needs
# python3 (already required) and a Chrome-like binary. Without one it is skipped rather
# than failed, so the suite still runs on a machine that has no browser.
panel_browser=""
for candidate in google-chrome google-chrome-stable chromium chromium-browser; do
    if command -v "$candidate" >/dev/null 2>&1; then
        panel_browser="$candidate"
        break
    fi
done

if [ -z "$panel_browser" ]; then
    echo "SKIP  the dashboard checks: no chrome, chromium or google-chrome on this machine"
else
    # Two more clients publish one name as a pool, so the member column the panel
    # renders is not exercised on a deployment that has no pool. The first of them also
    # publishes a name no phone screen can fit on one line: the panel promises that the
    # tables become cards below 700 px and that nothing is cut off at 360 px, and a name
    # like this is the case that promise is about.
    PANEL_POOL_PORT="$(free_port)"
    PANEL_POOL_OTHER="$(free_port)"
    PANEL_LONG_PORT="$(free_port)"
    for member in one two; do
        if [ "$member" = "one" ]; then port="$PANEL_POOL_PORT"; else port="$PANEL_POOL_OTHER"; fi
        cat > "panel-pool-$member.toml" <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 30

[[proxies]]
name = "panel-pool"
type = "tcp"
group = "panel-pool"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $port
EOF
        if [ "$member" = "one" ]; then
            cat >> "panel-pool-$member.toml" <<EOF

[[proxies]]
name = "panel-layout-long-name-without-any-spaces-at-all-0123456789"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $PANEL_LONG_PORT
EOF
        fi
        "$CLIENT" --config "panel-pool-$member.toml" > "panel-pool-$member.log" 2>&1 &
        PIDS+=($!)
    done
    panel_members=""
    for _ in $(seq 1 120); do
        panel_members="$(api_field /api/proxies '[p["member_count"] for p in d["proxies"] if p["name"] == "panel-pool"][0]' 2>/dev/null || echo 0)"
        [ "$panel_members" = "2" ] && break
        sleep 0.25
    done
    if [ "$panel_members" != "2" ]; then
        bad "the pool the dashboard section needs came up" "member_count=$panel_members"
    fi

    # The ledger and directory views have two states each and this deployment only has
    # one of them: the capacity server above has neither a [ledger] nor a [dht] section,
    # so its dashboard is where the "off" states are checked. It is still running.
    panel_bare=()
    if wait_for_tcp "$CAP_DASHBOARD" 1 2>/dev/null; then
        panel_bare=(--bare-url "http://127.0.0.1:$CAP_DASHBOARD")
    else
        echo "SKIP  the off-state dashboard: the capacity server is not answering"
    fi

    # The tokens travel in the environment, not on the command line, so they stay out
    # of the process list.
    panel_output="$(AETHERTUNNEL_PANEL_TOKEN="$DASHBOARD_TOKEN" \
        AETHERTUNNEL_PANEL_BARE_TOKEN="$DASHBOARD_TOKEN" \
        python3 "$HERE/scripts/panel-checks.py" \
        --url "http://127.0.0.1:$DASHBOARD_PORT" \
        --audit "$WORK/audit.jsonl" --chrome "$(command -v "$panel_browser")" \
        ${panel_bare[@]+"${panel_bare[@]}"} 2>&1)"
    panel_status=$?
    printf '%s\n' "$panel_output"
    # The script prints the suite's own PASS/FAIL shape, so its checks join the totals
    # and nothing has to be counted twice.
    panel_passes="$(printf '%s\n' "$panel_output" | grep -c '^PASS  ' || true)"
    panel_failed="$(printf '%s\n' "$panel_output" | grep -c '^FAIL  ' || true)"
    # A checker that died before printing anything (no browser could be started, say) has
    # to fail here: its output would otherwise read as a section that simply had nothing
    # to report.
    if [ "$panel_status" -ne 0 ] && [ "$panel_failed" -eq 0 ]; then
        bad "the dashboard checker ran to completion" "it exited $panel_status"
    fi
    PASSES=$((PASSES + panel_passes))
    FAILURES=$((FAILURES + panel_failed))
fi

echo
# ── allow_ports: the server refuses a remote port outside the ranges it allows.

ALLOWPORTS_DIR="$WORK/allowports"
mkdir -p "$ALLOWPORTS_DIR"
cat > "$ALLOWPORTS_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $ALLOWPORTS_CONTROL_PORT
auth_token = "$TOKEN"
allow_ports = ["20000-20009"]
EOF
cat > "$ALLOWPORTS_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$ALLOWPORTS_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "inside"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = 20001

[[proxies]]
name = "outside"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = 20011
EOF
"$SERVER" --config "$ALLOWPORTS_DIR/server.toml" > "$ALLOWPORTS_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$ALLOWPORTS_DIR/client.toml" > "$ALLOWPORTS_DIR/client.log" 2>&1 &
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 240); do
    if grep -q 'outside allow_ports' "$ALLOWPORTS_DIR/server.log" 2>/dev/null \
        && grep -q 'outside allow_ports' "$ALLOWPORTS_DIR/client.log" 2>/dev/null \
        && grep -q '"inside"' "$ALLOWPORTS_DIR/client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the server refused a remote port outside allow_ports and told the client"


echo
# ── frp parity, round two: subdomains, dial_via, health-check withdrawal.

FEATURES_DIR="$WORK/features"
mkdir -p "$FEATURES_DIR"
FEATURES_CONTROL_PORT="$(free_port)"
FEATURES_HTTP_PORT="$(free_port)"
FEATURES_HEALTH_BACKEND_PORT="$(free_port)"
FEATURES_HEALTH_PUBLIC_PORT="$(free_port)"
FEATURES_DIALVIA_PUBLIC_PORT="$(free_port)"
FEATURES_CONNECT_PORT="$(free_port)"

# A guarded backend the health check watches. The scenario kills it and brings
# it back, so it is its own process the script can restart.
cat > "$FEATURES_DIR/health_backend.py" <<'PY'
import socket, sys
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", int(sys.argv[1])))
srv.listen(16)
while True:
    try:
        conn, _ = srv.accept()
    except OSError:
        break
    try:
        conn.sendall(b"guarded-backend\n")
    finally:
        conn.close()
PY

# The intermediary dial_via points at: an HTTP CONNECT proxy that forwards every
# accepted tunnel to the server's control port, and writes down what it was
# asked to CONNECT to.
cat > "$FEATURES_DIR/connect_proxy.py" <<'PY'
import socket, sys, threading
port, host, upstream_port, log = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), sys.argv[4]
upstream = (host, upstream_port)
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(16)

def pipe(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except OSError:
        pass
    try:
        b.shutdown(socket.SHUT_WR)
    except OSError:
        pass

while True:
    conn, _ = srv.accept()
    try:
        request = b""
        while b"\r\n\r\n" not in request:
            chunk = conn.recv(65536)
            if not chunk:
                raise OSError("peer went away")
            request += chunk
        line = request.split(b"\r\n", 1)[0].decode(errors="replace")
        if not line.startswith("CONNECT "):
            raise OSError("not a CONNECT request")
        with open(log, "a") as f:
            f.write(line + "\n")
        upstream_sock = socket.create_connection(upstream)
    except Exception as exc:
        with open(log, "a") as f:
            f.write("refused: %s\n" % exc)
        try:
            conn.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
        except OSError:
            pass
        conn.close()
        continue
    conn.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
    threading.Thread(target=pipe, args=(conn, upstream_sock), daemon=True).start()
    threading.Thread(target=pipe, args=(upstream_sock, conn), daemon=True).start()
PY

cat > "$FEATURES_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $FEATURES_CONTROL_PORT
auth_token = "$TOKEN"
http_port = $FEATURES_HTTP_PORT
subdomain_host = "feats.smoke.test"
EOF

# Client A carries the subdomain and the health-checked proxy. The guarded
# backend's probe schedule is one second, so the withdrawal lands quickly.
cat > "$FEATURES_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$FEATURES_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "shopfront"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
subdomain = "shop"

[[proxies]]
name = "guarded"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $FEATURES_HEALTH_BACKEND_PORT
remote_port = $FEATURES_HEALTH_PUBLIC_PORT
health_check = { type = "tcp", interval_s = 1, timeout_s = 1, max_failed = 1 }
EOF

# Client B reaches the same server through the CONNECT proxy: dial_via in
# front of the control connection.
cat > "$FEATURES_DIR/dialvia-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$FEATURES_CONTROL_PORT"
auth_token = "$TOKEN"
dial_via = "http://127.0.0.1:$FEATURES_CONNECT_PORT"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "through-the-proxy"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $FEATURES_DIALVIA_PUBLIC_PORT
EOF

python3 "$FEATURES_DIR/health_backend.py" "$FEATURES_HEALTH_BACKEND_PORT" > /dev/null 2>&1 &
HEALTH_BACKEND_PID=$!
PIDS+=($!)
# The client's first probe runs as soon as the process is up, so the backend it
# watches gets a head start.
sleep 0.5
python3 "$FEATURES_DIR/connect_proxy.py" "$FEATURES_CONNECT_PORT" 127.0.0.1 "$FEATURES_CONTROL_PORT" "$FEATURES_DIR/connect.log" > /dev/null 2>&1 &
PIDS+=($!)
"$SERVER" --config "$FEATURES_DIR/server.toml" > "$FEATURES_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$FEATURES_DIR/client.toml" > "$FEATURES_DIR/client.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$FEATURES_DIR/dialvia-client.toml" > "$FEATURES_DIR/dialvia-client.log" 2>&1 &
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 120); do
    if grep -q 'shopfront' "$FEATURES_DIR/client.log" 2>/dev/null \
        && grep -q 'guarded' "$FEATURES_DIR/client.log" 2>/dev/null \
        && grep -q 'through-the-proxy' "$FEATURES_DIR/dialvia-client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "both feature clients are up: the subdomain, the guarded proxy and the dial_via tunnel"

# The health_via helper reads the guarded backend's one-line answer; a port the
# server stopped publishing answers "refused" instead of opening at all.
health_via() {
    python3 - "$1" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=5)
except OSError:
    print("refused")
else:
    s.settimeout(5)
    try:
        line = b""
        while b"\n" not in line:
            chunk = s.recv(4096)
            if not chunk:
                break
            line += chunk
        s.close()
        print("ok" if b"guarded-backend" in line else "got %r" % line)
    except OSError as exc:
        print("broken: %s" % exc)
PY
}

subdomain_result="$(python3 - "$FEATURES_HTTP_PORT" <<'PY'
import sys, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "shop.feats.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
PY
)"
check "$subdomain_result" "ok" \
    "a proxy with a subdomain is served as shop.feats.smoke.test"

subdomain_unknown="$(python3 - "$FEATURES_HTTP_PORT" <<'PY'
import sys, urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "not-taken.feats.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except urllib.error.HTTPError as exc:
    print("refused" if exc.code == 404 else "status %d" % exc.code)
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("answered")
PY
)"
check "$subdomain_unknown" "refused" \
    "a subdomain no client asked for stays unrouted"

check "$(tcp_via "$FEATURES_DIALVIA_PUBLIC_PORT")" "ok" \
    "a client that dials through dial_via carries bytes"
connect_seen=0
for _ in $(seq 1 20); do
    if grep -qF "CONNECT 127.0.0.1:$FEATURES_CONTROL_PORT" "$FEATURES_DIR/connect.log" 2>/dev/null; then
        connect_seen=1
        break
    fi
    sleep 0.5
done
check "$connect_seen" "1" "the CONNECT proxy saw the client dial the server through it"

healthy_up=0
for _ in $(seq 1 30); do
    if [ "$(health_via "$FEATURES_HEALTH_PUBLIC_PORT")" = "ok" ]; then
        healthy_up=1
        break
    fi
    sleep 0.5
done
check "$healthy_up" "1" "a healthy guarded proxy serves its public port"

kill "$HEALTH_BACKEND_PID" 2>/dev/null || true
withdrawn=0
for _ in $(seq 1 40); do
    if [ "$(health_via "$FEATURES_HEALTH_PUBLIC_PORT")" = "refused" ] \
        && grep -q 'withdrawing the tunnel' "$FEATURES_DIR/client.log" 2>/dev/null; then
        withdrawn=1
        break
    fi
    sleep 0.5
done
check "$withdrawn" "1" \
    "a failed health check withdraws the proxy: the public port is closed"

python3 "$FEATURES_DIR/health_backend.py" "$FEATURES_HEALTH_BACKEND_PORT" > /dev/null 2>&1 &
HEALTH_BACKEND_PID=$!
PIDS+=($!)
recovered=0
for _ in $(seq 1 40); do
    if [ "$(health_via "$FEATURES_HEALTH_PUBLIC_PORT")" = "ok" ] \
        && grep -q 'the tunnel is published again' "$FEATURES_DIR/client.log" 2>/dev/null; then
        recovered=1
        break
    fi
    sleep 0.5
done
check "$recovered" "1" \
    "a recovered health check publishes the proxy again"

subdomain_still_up="$(python3 - "$FEATURES_HTTP_PORT" <<'PY'
import sys, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "shop.feats.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
PY
)"
check "$subdomain_still_up" "ok" \
    "the withdrawal of one proxy leaves the others published"


echo
# ── frp parity, round three: basic auth, tcpmux, SIGHUP hot reload.

F3_DIR="$WORK/features3"
mkdir -p "$F3_DIR"
F3_CONTROL_PORT="$(free_port)"
F3_HTTP_PORT="$(free_port)"
F3_MUX_PORT="$(free_port)"
F3_ECHO_PORT="$(free_port)"
F3_BASE_PUBLIC_PORT="$(free_port)"
F3_ADDED_PUBLIC_PORT="$(free_port)"

# One echo service answers both the tcpmux tunnel and the reload tunnels.
cat > "$F3_DIR/echo.py" <<'PY'
import socket, sys, threading
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", int(sys.argv[1])))
srv.listen(16)
def pipe(conn):
    try:
        while True:
            data = conn.recv(65536)
            if not data:
                break
            conn.sendall(data)
    except OSError:
        pass
    finally:
        conn.close()
while True:
    try:
        conn, _ = srv.accept()
    except OSError:
        break
    threading.Thread(target=pipe, args=(conn,), daemon=True).start()
PY

cat > "$F3_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $F3_CONTROL_PORT
auth_token = "$TOKEN"
http_port = $F3_HTTP_PORT
tcpmux_port = $F3_MUX_PORT
EOF

# The reload client's configuration: basic-authed http, a tcpmux tunnel and a
# tcp tunnel the reload below will remove.
cat > "$F3_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$F3_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "authed"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["authed.smoke.test"]
http_user = "ops"
http_password = "s3cret"

[[proxies]]
name = "muxed"
type = "tcpmux"
multiplexer = "httpconnect"
local_ip = "127.0.0.1"
local_port = $F3_ECHO_PORT
domains = ["muxed"]

[[proxies]]
name = "base"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $F3_ECHO_PORT
remote_port = $F3_BASE_PUBLIC_PORT
EOF

python3 "$F3_DIR/echo.py" "$F3_ECHO_PORT" > /dev/null 2>&1 &
PIDS+=($!)
"$SERVER" --config "$F3_DIR/server.toml" > "$F3_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$F3_DIR/client.toml" > "$F3_DIR/client.log" 2>&1 &
F3_CLIENT_PID=$!
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 120); do
    if grep -q 'authed' "$F3_DIR/client.log" 2>/dev/null \
        && grep -q 'muxed' "$F3_DIR/client.log" 2>/dev/null \
        && grep -q 'base' "$F3_DIR/client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the round-three client is up: authed, muxed and base"

auth_refused="$(python3 - "$F3_HTTP_PORT" <<'PY'
import sys, urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "authed.smoke.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        return_code = resp.status
except urllib.error.HTTPError as exc:
    header = exc.headers.get("WWW-Authenticate", "")
    print("401" if exc.code == 401 and "Basic" in header else "status %d %s" % (exc.code, header))
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("answered %d" % return_code)
PY
)"
check "$auth_refused" "401" "a basic-authed hostname challenges a visitor without credentials"

auth_ok="$(python3 - "$F3_HTTP_PORT" <<'PY'
import sys, urllib.request, base64
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "authed.smoke.test")
req.add_header("Authorization", "Basic " + base64.b64encode(b"ops:s3cret").decode())
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
PY
)"
check "$auth_ok" "ok" "the right basic-auth credentials reach the tunnel"

mux_result="$(python3 - "$F3_MUX_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(b"CONNECT muxed:1 HTTP/1.1\r\nHost: muxed:1\r\n\r\n")
    answer = b""
    while b"\r\n\r\n" not in answer:
        chunk = s.recv(4096)
        if not chunk:
            break
        answer += chunk
    if b" 200 " not in answer.split(b"\r\n", 1)[0]:
        print("mux answered %r" % answer.split(b"\r\n", 1)[0])
    else:
        payload = b"mux-check"
        s.sendall(payload)
        got = b""
        while len(got) < len(payload):
            chunk = s.recv(4096)
            if not chunk:
                break
            got += chunk
        print("ok" if got == payload else "got %r" % got)
PY
)"
check "$mux_result" "ok" "a tcpmux tunnel is reached through a CONNECT on the shared port"

mux_unknown="$(python3 - "$F3_MUX_PORT" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(b"CONNECT nobody:1 HTTP/1.1\r\nHost: nobody:1\r\n\r\n")
    answer = b""
    while b"\r\n\r\n" not in answer:
        chunk = s.recv(4096)
        if not chunk:
            break
        answer += chunk
    first = answer.split(b"\r\n", 1)[0]
    print("refused" if b" 404 " in first else "answered %r" % first)
PY
)"
check "$mux_unknown" "refused" "a hostname no tcpmux proxy publishes is answered 404"

# The hot reload: add a tunnel, remove another, and let SIGHUP apply the edit
# to the running client.
cat > "$F3_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$F3_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "authed"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["authed.smoke.test"]
http_user = "ops"
http_password = "s3cret"

[[proxies]]
name = "muxed"
type = "tcpmux"
multiplexer = "httpconnect"
local_ip = "127.0.0.1"
local_port = $F3_ECHO_PORT
domains = ["muxed"]

[[proxies]]
name = "added"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $F3_ECHO_PORT
remote_port = $F3_ADDED_PUBLIC_PORT
EOF
kill -HUP "$F3_CLIENT_PID"

reloaded=0
for _ in $(seq 1 40); do
    if grep -q 'reload: 1 proxy(es) added, 0 changed, 1 removed' "$F3_DIR/client.log" 2>/dev/null; then
        reloaded=1
        break
    fi
    sleep 0.5
done
check "$reloaded" "1" "SIGHUP applies the edited configuration to the running client"

added_serves=0
for _ in $(seq 1 30); do
    if [ "$(tcp_via "$F3_ADDED_PUBLIC_PORT")" = "ok" ]; then
        added_serves=1
        break
    fi
    sleep 0.5
done
check "$added_serves" "1" "a tunnel the reload added is serving without a restart"

base_gone=0
for _ in $(seq 1 30); do
    probe="$(python3 -c "
import socket
try:
    socket.create_connection(('127.0.0.1', $F3_BASE_PUBLIC_PORT), timeout=3).close()
    print('open')
except OSError:
    print('refused')
")"
    if [ "$probe" = "refused" ]; then
        base_gone=1
        break
    fi
    sleep 0.5
done
check "$base_gone" "1" "a tunnel the reload removed really closed its public port"


echo
# ── frp parity, round four: use_compression and the https2http plugin.

F4_DIR="$WORK/features4"
mkdir -p "$F4_DIR"
F4_CONTROL_PORT="$(free_port)"
F4_ECHO_PORT="$(free_port)"
F4_PLAIN_PUBLIC_PORT="$(free_port)"
F4_SEALED_PUBLIC_PORT="$(free_port)"
F4_TLS_PUBLIC_PORT="$(free_port)"

# A self-signed certificate the https2http plugin terminates the visitor's TLS
# with; the python visitor below does not verify it, so any valid pair works.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$F4_DIR/site.key" \
    -out "$F4_DIR/site.crt" -days 2 -subj "/CN=127.0.0.1" > /dev/null 2>&1

cat > "$F4_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $F4_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$F4_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$F4_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "squeezed"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $F4_ECHO_PORT
remote_port = $F4_PLAIN_PUBLIC_PORT
use_compression = true

# The second byte-stream proxy carries its own AEAD layer: [encryption] is off
# for this whole deployment, so the only thing protecting this stream is the key
# both ends derive from the token and this proxy's name.
[[proxies]]
name = "sealed"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $F4_ECHO_PORT
remote_port = $F4_SEALED_PUBLIC_PORT
use_encryption = true

[[proxies]]
name = "https-site"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
remote_port = $F4_TLS_PUBLIC_PORT
plugin = "https2http"
plugin_cert_file = "$F4_DIR/site.crt"
plugin_key_file = "$F4_DIR/site.key"
EOF

# The compressed tunnel's local service: the same echo the earlier sections
# use, written here so the round stands on its own.
cat > "$F4_DIR/echo.py" <<'PY'
import socket, sys, threading
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", int(sys.argv[1])))
srv.listen(16)
def pipe(conn):
    try:
        while True:
            data = conn.recv(65536)
            if not data:
                break
            conn.sendall(data)
    except OSError:
        pass
    finally:
        conn.close()
while True:
    try:
        conn, _ = srv.accept()
    except OSError:
        break
    threading.Thread(target=pipe, args=(conn,), daemon=True).start()
PY

python3 "$F4_DIR/echo.py" "$F4_ECHO_PORT" > /dev/null 2>&1 &
PIDS+=($!)
"$SERVER" --config "$F4_DIR/server.toml" > "$F4_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$F4_DIR/client.toml" > "$F4_DIR/client.log" 2>&1 &
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 120); do
    if grep -q 'squeezed' "$F4_DIR/client.log" 2>/dev/null \
        && grep -q 'sealed' "$F4_DIR/client.log" 2>/dev/null \
        && grep -q 'https-site' "$F4_DIR/client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the round-four client is up: the compressed tunnel, the sealed tunnel and the https2http site"

compressed_ok=0
for _ in $(seq 1 30); do
    if [ "$(tcp_via "$F4_PLAIN_PUBLIC_PORT")" = "ok" ]; then
        compressed_ok=1
        break
    fi
    sleep 0.5
done
check "$compressed_ok" "1" "a use_compression tunnel carries bytes end to end"

# A use_encryption proxy on a session that has no cipher of its own: the round
# trip can only work if both ends derived the same per-proxy key from the token
# and the proxy's name, so this checks the derivation as much as the payload.
sealed_ok=0
for _ in $(seq 1 30); do
    if [ "$(tcp_via "$F4_SEALED_PUBLIC_PORT")" = "ok" ]; then
        sealed_ok=1
        break
    fi
    sleep 0.5
done
check "$sealed_ok" "1" "a use_encryption tunnel carries bytes end to end with the proxy's own key"

https_ok=0
for _ in $(seq 1 30); do
    answer="$(python3 - "$F4_TLS_PUBLIC_PORT" <<'PY'
import socket, ssl, sys
try:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    s = ctx.wrap_socket(socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15))
except Exception as exc:
    print("no tls: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(b"GET / HTTP/1.1\r\nHost: site.example\r\nConnection: close\r\n\r\n")
    body = b""
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        body += chunk
    print("ok" if b"smoketest-http" in body else "got %r" % body[:200])
PY
)"
    if [ "$answer" = "ok" ]; then
        https_ok=1
        break
    fi
    sleep 0.5
done
check "$https_ok" "1" "the https2http plugin terminates the visitor's TLS with the client's certificate"


echo
# ── frp parity, round five: max_ports_per_client, includes, http_proxy, http2https.

F5_DIR="$WORK/features5"
mkdir -p "$F5_DIR"
F5_CONTROL_PORT="$(free_port)"
F5_CAP_CONTROL_PORT="$(free_port)"
F5_TLS_ORIGIN_PORT="$(free_port)"
F5_PROXY_PUBLIC_PORT="$(free_port)"
F5_TLS_PUBLIC_PORT="$(free_port)"

# A self-signed certificate for the local HTTPS origin the http2https plugin
# dials; the plugin skips verification, so any valid pair works.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$F5_DIR/origin.key" \
    -out "$F5_DIR/origin.crt" -days 2 -subj "/CN=127.0.0.1" > /dev/null 2>&1

cat > "$F5_DIR/origin.py" <<PY
import http.server, ssl, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"tls-origin-service\n")
    def log_message(self, *args):
        pass
srv = http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain("$F5_DIR/origin.crt", "$F5_DIR/origin.key")
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
PY

cat > "$F5_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $F5_CONTROL_PORT
auth_token = "$TOKEN"
EOF

# The cap server: one public port per client session.
cat > "$F5_DIR/capped-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $F5_CAP_CONTROL_PORT
auth_token = "$TOKEN"
max_ports_per_client = 1
EOF

# The capped client asks for two public ports: the second is refused by name.
cat > "$F5_DIR/capped-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$F5_CAP_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "capped-one"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $F5_TLS_ORIGIN_PORT
remote_port = $(free_port)

[[proxies]]
name = "capped-two"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $(free_port)
EOF

# The plugin client's proxies live in fragments the main file includes.
cat > "$F5_DIR/plugin-client.toml" <<EOF
includes = ["fragments/*.toml"]

[client]
server_addr = "127.0.0.1:$F5_CONTROL_PORT"
auth_token = "$TOKEN"
EOF
mkdir -p "$F5_DIR/fragments"
cat > "$F5_DIR/fragments/http-proxy.toml" <<EOF
[[proxies]]
name = "http-exit"
type = "tcp"
local_port = $HTTP_ECHO_PORT
remote_port = $F5_PROXY_PUBLIC_PORT
plugin = "http_proxy"
allow_targets = ["127.0.0.0/8"]
plugin_http_user = "ops"
plugin_http_password = "s3cret"
EOF
cat > "$F5_DIR/fragments/tls-bridge.toml" <<EOF
[[proxies]]
name = "tls-bridge"
type = "tcp"
local_port = $F5_TLS_ORIGIN_PORT
remote_port = $F5_TLS_PUBLIC_PORT
plugin = "http2https"
host_header_rewrite = "rewritten.example"
EOF

# The local TLS origin the http2https plugin dials.
python3 "$F5_DIR/origin.py" "$F5_TLS_ORIGIN_PORT" > /dev/null 2>&1 &
PIDS+=($!)
"$SERVER" --config "$F5_DIR/server.toml" > "$F5_DIR/server.log" 2>&1 &
PIDS+=($!)
"$SERVER" --config "$F5_DIR/capped-server.toml" > "$F5_DIR/capped-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$F5_DIR/capped-client.toml" > "$F5_DIR/capped-client.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$F5_DIR/plugin-client.toml" > "$F5_DIR/plugin-client.log" 2>&1 &
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 120); do
    if grep -q 'exceed' "$F5_DIR/capped-client.log" 2>/dev/null \
        && grep -q 'http-exit' "$F5_DIR/plugin-client.log" 2>/dev/null \
        && grep -q 'tls-bridge' "$F5_DIR/plugin-client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the round-five clients are up: the cap refused, both plugins registered"

cap_refused=0
for _ in $(seq 1 20); do
    if grep -q 'max_ports_per_client' "$F5_DIR/capped-client.log" 2>/dev/null; then
        cap_refused=1
        break
    fi
    sleep 0.5
done
check "$cap_refused" "1" "a registration past max_ports_per_client is refused and the client is told"

proxy_407="$(python3 - "$F5_PROXY_PUBLIC_PORT" <<'PY'
import sys, urllib.request, urllib.error
opener = urllib.request.build_opener(urllib.request.ProxyHandler(
    {"http": "http://127.0.0.1:%s" % sys.argv[1]}))
try:
    resp = opener.open("http://127.0.0.1:%s/" % sys.argv[1], timeout=15)
    print("answered %d" % resp.status)
except urllib.error.HTTPError as exc:
    print("407" if exc.code == 407 else "status %d" % exc.code)
except Exception as exc:
    print("no answer: %s" % exc)
PY
)"
check "$proxy_407" "407" "the http_proxy plugin demands credentials when none are given"

proxy_ok="$(python3 - "$F5_PROXY_PUBLIC_PORT" "$HTTP_ECHO_PORT" <<'PY'
import base64, socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    token = base64.b64encode(b"ops:s3cret").decode()
    target = "http://127.0.0.1:%s/" % sys.argv[2]
    s.sendall(("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n"
               % (target, sys.argv[2], token)).encode())
    body = b""
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        body += chunk
    print("ok" if b"smoketest-http" in body else "got %r" % body[:200])
PY
)"
check "$proxy_ok" "ok" "credentials let the http_proxy plugin fetch through the client's network"

tls_ok=0
for _ in $(seq 1 30); do
    answer="$(python3 - "$F5_TLS_PUBLIC_PORT" <<'PY'
import sys, urllib.request
try:
    resp = urllib.request.urlopen("http://127.0.0.1:%s/" % sys.argv[1], timeout=15)
    body = resp.read().decode()
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print("ok" if "tls-origin-service" in body else "got %r" % body[:200])
PY
)"
    if [ "$answer" = "ok" ]; then
        tls_ok=1
        break
    fi
    sleep 0.5
done
check "$tls_ok" "1" "the http2https plugin answers plain HTTP with its local TLS service: $answer"


# ── frp parity, round six: SNI passthrough and the client admin API.

F6_DIR="$WORK/features6"
mkdir -p "$F6_DIR"
F6_CONTROL_PORT="$(free_port)"
F6_PASSTHROUGH_PORT="$(free_port)"
F6_ADMIN_PORT="$(free_port)"
F6_RELOAD_PUBLIC_PORT="$(free_port)"

# The certificate the CLIENT terminates the passthrough visitor's TLS with.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$F6_DIR/site.key" \
    -out "$F6_DIR/site.crt" -days 2 -subj "/CN=sni.local.test" > /dev/null 2>&1

cat > "$F6_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $F6_CONTROL_PORT
auth_token = "$TOKEN"
https_passthrough_port = $F6_PASSTHROUGH_PORT
EOF

cat > "$F6_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$F6_CONTROL_PORT"
auth_token = "$TOKEN"

[client.admin]
enabled = true
port = $F6_ADMIN_PORT
user = "ops"
password = "s3cret"

[[proxies]]
name = "sni-site"
type = "https"
domains = ["sni.local.test"]
tls_passthrough = true
plugin = "https2http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
plugin_cert_file = "$F6_DIR/site.crt"
plugin_key_file = "$F6_DIR/site.key"
EOF

"$SERVER" --config "$F6_DIR/server.toml" > "$F6_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$F6_DIR/client.toml" > "$F6_DIR/client.log" 2>&1 &
PIDS+=($!)

ok_awaited=0
for _ in $(seq 1 120); do
    if grep -q 'sni-site' "$F6_DIR/client.log" 2>/dev/null \
        && grep -q 'passthrough' "$F6_DIR/server.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the round-six client is up: the https tunnel rides the passthrough listener"

sni_ok=0
for _ in $(seq 1 30); do
    answer="$(python3 - "$F6_PASSTHROUGH_PORT" <<'PY'
import socket, ssl, sys
try:
    raw = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    tls = ctx.wrap_socket(raw, server_hostname="sni.local.test")
except Exception as exc:
    print("no handshake: %s" % exc)
else:
    tls.settimeout(15)
    tls.sendall(b"GET / HTTP/1.1\r\nHost: sni.local.test\r\nConnection: close\r\n\r\n")
    body = b""
    while True:
        chunk = tls.recv(65536)
        if not chunk:
            break
        body += chunk
    tls.close()
    print("ok" if b"smoketest-http" in body else "got %r" % body[:200])
PY
)"
    if [ "$answer" = "ok" ]; then
        sni_ok=1
        break
    fi
    sleep 0.5
done
check "$sni_ok" "1" "SNI passthrough relays the visitor's TLS untouched and the client's own certificate answers"

sni_unknown="$(python3 - "$F6_PASSTHROUGH_PORT" <<'PY'
import socket, ssl, sys
try:
    raw = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    tls = ctx.wrap_socket(raw, server_hostname="nobody.local.test")
except Exception:
    print("refused")
else:
    tls.close()
    print("open")
PY
)"
check "$sni_unknown" "refused" "an SNI nobody published is refused on the passthrough listener"

healthz="$(python3 - "$F6_ADMIN_PORT" <<'PY'
import sys, urllib.request
try:
    resp = urllib.request.urlopen("http://127.0.0.1:%s/healthz" % sys.argv[1], timeout=15)
    print(resp.read().decode().strip())
except Exception as exc:
    print("no answer: %s" % exc)
PY
)"
check "$healthz" "ok" "the client admin /healthz answers without credentials"

status_anon="$(python3 - "$F6_ADMIN_PORT" <<'PY'
import sys, urllib.error, urllib.request
try:
    resp = urllib.request.urlopen("http://127.0.0.1:%s/api/status" % sys.argv[1], timeout=15)
    print("answered %d" % resp.status)
except urllib.error.HTTPError as exc:
    print("401" if exc.code == 401 else "status %d" % exc.code)
except Exception as exc:
    print("no answer: %s" % exc)
PY
)"
check "$status_anon" "401" "the admin /api/status demands credentials"

status_confirmed="$(python3 - "$F6_ADMIN_PORT" <<'PY'
import base64, json, sys, urllib.error, urllib.request
token = base64.b64encode(b"ops:s3cret").decode()
request = urllib.request.Request(
    "http://127.0.0.1:%s/api/status" % sys.argv[1],
    headers={"Authorization": "Basic " + token})
try:
    report = json.loads(urllib.request.urlopen(request, timeout=15).read().decode())
except Exception as exc:
    print("no answer: %s" % exc)
else:
    confirmed = [p["name"] for p in report.get("proxies", []) if p.get("confirmed")]
    print("sni-site" if "sni-site" in confirmed else "confirmed=%r" % confirmed)
PY
)"
check "$status_confirmed" "sni-site" "the admin /api/status reports the passthrough tunnel as confirmed"

cat >> "$F6_DIR/client.toml" <<EOF

[[proxies]]
name = "reloaded-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $F6_RELOAD_PUBLIC_PORT
EOF

reload_status="$(python3 - "$F6_ADMIN_PORT" <<'PY'
import base64, json, sys, urllib.request
token = base64.b64encode(b"ops:s3cret").decode()
request = urllib.request.Request(
    "http://127.0.0.1:%s/api/reload" % sys.argv[1],
    headers={"Authorization": "Basic " + token}, data=b"")
try:
    answer = json.loads(urllib.request.urlopen(request, timeout=15).read().decode())
except Exception as exc:
    print("no answer: %s" % exc)
else:
    print(answer.get("result", "no result"))
PY
)"
check "$reload_status" "ok" "POST /api/reload re-reads the client's configuration file"

reload_confirmed=0
for _ in $(seq 1 60); do
    answer="$(python3 - "$F6_ADMIN_PORT" <<'PY'
import base64, json, sys, urllib.request
token = base64.b64encode(b"ops:s3cret").decode()
request = urllib.request.Request(
    "http://127.0.0.1:%s/api/status" % sys.argv[1],
    headers={"Authorization": "Basic " + token})
try:
    report = json.loads(urllib.request.urlopen(request, timeout=15).read().decode())
except Exception as exc:
    print("no answer: %s" % exc)
else:
    confirmed = [p["name"] for p in report.get("proxies", []) if p.get("confirmed")]
    print("reloaded-tcp" if "reloaded-tcp" in confirmed else "pending")
PY
)"
    if [ "$answer" = "reloaded-tcp" ]; then
        reload_confirmed=1
        break
    fi
    sleep 0.5
done
check "$reload_confirmed" "1" "the tunnel added through /api/reload is confirmed by the server"

reload_roundtrip="$(python3 - "$F6_RELOAD_PUBLIC_PORT" <<'PY'
import socket, sys
payload = b"ping-reloaded"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    echoed = b""
    while len(echoed) < len(payload):
        chunk = s.recv(65536)
        if not chunk:
            break
        echoed += chunk
    s.close()
    print("ok" if echoed == payload else "echoed %r" % echoed)
PY
)"
check "$reload_roundtrip" "ok" "the reloaded tunnel's public port carries traffic end to end"


# ── frp parity, round seven: the websocket transport, data connections parked
# in advance, and a client that gives up on a refused first login.

R7_DIR="$WORK/features7"
mkdir -p "$R7_DIR"
R7_CONTROL_PORT="$(free_port)"
R7_METRICS_PORT="$(free_port)"
R7_WEBSOCKET_PORT="$(free_port)"
R7_PLAIN_PORT="$(free_port)"
R7_POOL_PORT="$(free_port)"

cat > "$R7_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $R7_CONTROL_PORT
auth_token = "$TOKEN"

# A metrics listener of its own: the counters this round reads belong to this
# server, and the deployment under test has a dashboard of its own.
[metrics]
enabled = true
bind_addr = "127.0.0.1"
port = $R7_METRICS_PORT
token = "$METRICS_TOKEN"
EOF

# transport.protocol = "websocket" wraps every connection this client opens —
# control, data and visitor — in RFC 6455 binary frames. The server needs no
# setting for it: it recognises the upgrade on the shared port.
cat > "$R7_DIR/websocket-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$R7_CONTROL_PORT"
auth_token = "$TOKEN"

[transport]
protocol = "websocket"

[[proxies]]
name = "websocket-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $R7_WEBSOCKET_PORT
EOF

# A second client with the default shape on the same port: the server's sniffing
# has to leave every connection that is not an upgrade exactly as it was.
cat > "$R7_DIR/plain-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$R7_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "plain-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $R7_PLAIN_PORT
EOF

# pool_count keeps data connections parked at the server, so a visitor's stream
# does not wait for a dial (a TLS handshake, when the transport is TLS).
cat > "$R7_DIR/pool-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$R7_CONTROL_PORT"
auth_token = "$TOKEN"
pool_count = 2

[[proxies]]
name = "pooled-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $R7_POOL_PORT
EOF

"$SERVER" --config "$R7_DIR/server.toml" > "$R7_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$R7_DIR/websocket-client.toml" > "$R7_DIR/websocket-client.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$R7_DIR/plain-client.toml" > "$R7_DIR/plain-client.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$R7_DIR/pool-client.toml" > "$R7_DIR/pool-client.log" 2>&1 &
PIDS+=($!)

# r7_roundtrip <port> <payload>: sends the payload to a public port and prints
# "ok" when the echo service behind it sends it back unchanged.
r7_roundtrip() {
    python3 - "$1" "$2" <<'PY'
import socket, sys
payload = sys.argv[2].encode()
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=20)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(20)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
}

r7_ready=""
for _ in $(seq 1 160); do
    if wait_for_tcp "$R7_WEBSOCKET_PORT" 1 2>/dev/null \
        && wait_for_tcp "$R7_PLAIN_PORT" 1 2>/dev/null \
        && wait_for_tcp "$R7_POOL_PORT" 1 2>/dev/null; then
        r7_ready="yes"
        break
    fi
    sleep 0.25
done

if [ -n "$r7_ready" ]; then
    ok "a websocket client, a plain client and a pooled client all publish on one server"
else
    bad "the websocket, plain and pooled clients published" \
        "websocket: $(tail -2 "$R7_DIR/websocket-client.log") | plain: $(tail -2 "$R7_DIR/plain-client.log") | pool: $(tail -2 "$R7_DIR/pool-client.log")"
fi

# A stream through each shape is what proves the upgrade and the framing are not
# just accepted but carried: the payload and its echo travel inside binary frames.
check "$(r7_roundtrip "$R7_WEBSOCKET_PORT" through-websocket-frames)" "ok" \
    "a stream rides the websocket transport end to end"
check "$(r7_roundtrip "$R7_PLAIN_PORT" through-the-same-port)" "ok" \
    "a plain client shares the server's port with a websocket client"

if grep -qF 'RFC 6455' "$R7_DIR/websocket-client.log"; then
    ok "the websocket client says in its log which shape it is on the wire"
else
    bad "the websocket client says in its log which shape it is on the wire" "$(tail -3 "$R7_DIR/websocket-client.log")"
fi
if grep -qF 'RFC 6455' "$R7_DIR/plain-client.log"; then
    bad "the plain client stays silent about websocket framing" "$(grep -F 'RFC 6455' "$R7_DIR/plain-client.log" | head -1)"
else
    ok "the plain client stays silent about websocket framing"
fi

# The same framing with TLS outside it, the wss arrangement: the upgrade request
# is written inside the TLS session, so a server that terminates TLS before it
# looks for the upgrade is what this pair proves.
R7_TLS_CONTROL_PORT="$(free_port)"
R7_TLS_PUBLIC_PORT="$(free_port)"

cat > "$R7_DIR/tls-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $R7_TLS_CONTROL_PORT
auth_token = "$TOKEN"

[transport]
enable_tls = true
cert_file = "$WORK/server.crt"
key_file = "$WORK/server.key"
EOF

cat > "$R7_DIR/wss-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$R7_TLS_CONTROL_PORT"
auth_token = "$TOKEN"

[transport]
enable_tls = true
ca_file = "$WORK/server.crt"
protocol = "websocket"

[[proxies]]
name = "wss-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $R7_TLS_PUBLIC_PORT
EOF

"$SERVER" --config "$R7_DIR/tls-server.toml" > "$R7_DIR/tls-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$R7_DIR/wss-client.toml" > "$R7_DIR/wss-client.log" 2>&1 &
PIDS+=($!)

r7_wss_ready=""
for _ in $(seq 1 120); do
    if wait_for_tcp "$R7_TLS_PUBLIC_PORT" 1 2>/dev/null; then
        r7_wss_ready="yes"
        break
    fi
    sleep 0.25
done
if [ -n "$r7_wss_ready" ]; then
    ok "a websocket client publishes through a TLS control port (the wss shape)"
else
    bad "the websocket client published through a TLS control port" \
        "server: $(tail -2 "$R7_DIR/tls-server.log") | client: $(tail -2 "$R7_DIR/wss-client.log")"
fi
check "$(r7_roundtrip "$R7_TLS_PUBLIC_PORT" through-tls-and-websocket)" "ok" \
    "a stream rides websocket framing inside a TLS session"

# No upgrade failed while both clients ran, which is the server half of the
# arrangement: it has to answer the websocket request and pass the other one.
check "$(grep -c 'websocket upgrade.*failed' "$R7_DIR/server.log" 2>/dev/null || true)" "0" \
    "no websocket upgrade failed while both shapes ran against one port"

# The upgrade path is answered rather than passed through: a request that names it
# but cannot complete it gets a 400. This one probe is the reason the check above
# counts the failures before it (the server logs every failed upgrade).
r7_upgrade="$(python3 - "$R7_CONTROL_PORT" <<'PY'
import socket, sys
# The tunnel's upgrade path with the headers that make it a websocket request
# except the key, which is what the server checks last.
request = ("GET /~!aethertunnel HTTP/1.1\r\nHost: 127.0.0.1\r\n"
           "Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(10)
    s.sendall(request.encode())
    answer = b""
    while b"\r\n\r\n" not in answer and len(answer) < 4096:
        chunk = s.recv(1024)
        if not chunk:
            break
        answer += chunk
    s.close()
    print(answer.split(b"\r\n", 1)[0].decode(errors="replace") if answer else "no answer")
PY
)"
check "$r7_upgrade" "HTTP/1.1 400 Bad Request" \
    "the server answers a websocket request on the shared port that cannot be completed"

# A GET that is not the tunnel's upgrade takes the path every other connection
# takes: it goes to the framer, which refuses it without an HTTP answer.
r7_plain_get="$(python3 - "$R7_CONTROL_PORT" <<'PY'
import socket, sys
request = "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(10)
    s.sendall(request.encode())
    answer = b""
    try:
        while len(answer) < 4096:
            chunk = s.recv(1024)
            if not chunk:
                break
            answer += chunk
    except OSError:
        pass
    s.close()
    print(answer.split(b"\r\n", 1)[0].decode(errors="replace") if answer else "closed")
PY
)"
check "$r7_plain_get" "closed" \
    "a GET that is not the tunnel's upgrade reaches the framer, which refuses it without an HTTP answer"

# The pool: the client parks two data connections before any visitor arrives, and a
# stream that arrives afterwards is served on one of them instead of waiting for a
# dial. Both halves are counted by the server, which is where the proof is.
r7_parked=0
for _ in $(seq 1 60); do
    r7_parked="$(metric_at "$R7_METRICS_PORT" "$METRICS_TOKEN" aethertunnel_pool_parked_total)"
    if [ "${r7_parked:-0}" -ge 2 ] 2>/dev/null; then
        break
    fi
    sleep 0.25
done
if [ "${r7_parked:-0}" -ge 2 ] 2>/dev/null; then
    ok "the pooled client parks two data connections before they are needed ($r7_parked parked)"
else
    bad "the pooled client parked two data connections in advance" "aethertunnel_pool_parked_total=$r7_parked"
fi

# How many streams the server has already served on a parked connection. The
# readiness probe above is one of them when a connection was parked in time, so the
# check below is that this number grows by the stream it sends, not that it is one.
r7_used_before="$(metric_at "$R7_METRICS_PORT" "$METRICS_TOKEN" aethertunnel_pool_used_total)"
r7_used_before="${r7_used_before:-0}"
check "$(r7_roundtrip "$R7_POOL_PORT" through-a-parked-connection)" "ok" \
    "a stream carried on a connection parked in advance reaches the visitor"
check "$(wait_metric "$R7_METRICS_PORT" "$METRICS_TOKEN" aethertunnel_pool_used_total $((r7_used_before + 1)))" \
    "$((r7_used_before + 1))" \
    "the server served that stream on a connection parked in advance"

# login_fail_exit: a refused first login ends the process instead of retrying
# forever, which is what a supervisor unit waits for. Last in this section: the
# refusal is an authentication failure, and the ban policy counts those per source.
cat > "$R7_DIR/wrong-token.toml" <<EOF
[client]
server_addr = "127.0.0.1:$R7_CONTROL_PORT"
auth_token = "not-the-token"
login_fail_exit = true

[[proxies]]
name = "never-published"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
EOF

"$CLIENT" --config "$R7_DIR/wrong-token.toml" > "$R7_DIR/wrong-token.log" 2>&1
r7_fail_exit=$?
check "$r7_fail_exit" "1" "a client whose first login is refused exits with a failure status"
if grep -qF 'login_fail_exit is set' "$R7_DIR/wrong-token.log"; then
    ok "the client says why it gave up instead of retrying"
else
    bad "the client says why it gave up instead of retrying" "$(tail -3 "$R7_DIR/wrong-token.log")"
fi


# ── frp parity, round eight: the Groth16 proof of the proxy secret
# (auth_method = "snark").

S8_DIR="$WORK/features8"
mkdir -p "$S8_DIR"
S8_CONTROL_PORT="$(free_port)"
S8_VISITOR_PORT="$(free_port)"
S8_BAD_VISITOR_PORT="$(free_port)"
S8_MISMATCH_VISITOR_PORT="$(free_port)"
S8_SECRET="functional-linux-snark-secret"

cat > "$S8_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $S8_CONTROL_PORT
auth_token = "$TOKEN"
EOF

# The owner publishes a private proxy whose visitors prove knowledge of the secret
# with a Groth16 proof instead of a Schnorr proof.
cat > "$S8_DIR/owner.toml" <<EOF
[client]
server_addr = "127.0.0.1:$S8_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "private-snark"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "$S8_SECRET"
auth_method = "snark"
EOF

cat > "$S8_DIR/visitor.toml" <<EOF
[client]
server_addr = "127.0.0.1:$S8_CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "snark-visitor"
type = "stcp"
server_name = "private-snark"
secret_key = "$S8_SECRET"
auth_method = "snark"
bind_addr = "127.0.0.1"
bind_port = $S8_VISITOR_PORT
EOF

# Two visitors that must be refused: one holding the wrong secret, and one holding
# the right secret but answering with the other proof. The server checks the answer
# against the method the proxy registered, so neither is an accepted visitor.
cat > "$S8_DIR/wrong-secret.toml" <<EOF
[client]
server_addr = "127.0.0.1:$S8_CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "snark-wrong-secret"
type = "stcp"
server_name = "private-snark"
secret_key = "not-the-snark-secret"
auth_method = "snark"
bind_addr = "127.0.0.1"
bind_port = $S8_BAD_VISITOR_PORT
EOF

cat > "$S8_DIR/mismatched.toml" <<EOF
[client]
server_addr = "127.0.0.1:$S8_CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "snark-mismatched"
type = "stcp"
server_name = "private-snark"
secret_key = "$S8_SECRET"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $S8_MISMATCH_VISITOR_PORT
EOF

"$SERVER" --config "$S8_DIR/server.toml" > "$S8_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$S8_DIR/owner.toml" > "$S8_DIR/owner.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$S8_DIR/visitor.toml" > "$S8_DIR/visitor.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$S8_DIR/wrong-secret.toml" > "$S8_DIR/wrong-secret.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$S8_DIR/mismatched.toml" > "$S8_DIR/mismatched.log" 2>&1 &
PIDS+=($!)

# s8_refused <port>: prints "refused" when a visitor's local port takes the
# connection and sends nothing back — what both a wrong secret and a proof of the
# wrong method look like from the caller's side.
s8_refused() {
    python3 - "$1" <<'PY'
import socket, sys, time
deadline = time.time() + 25
while True:
    try:
        s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
    except OSError as exc:
        if time.time() > deadline:
            print("no listener: %s" % exc)
            break
        time.sleep(0.5)
        continue
    s.settimeout(10)
    try:
        s.sendall(b"a proof that does not verify must carry no data")
        got = s.recv(4096)
    except OSError:
        print("refused")
    else:
        print("refused" if not got else "served %r" % got)
    finally:
        s.close()
    break
PY
}

if wait_for_tcp "$S8_VISITOR_PORT" 120 \
    && wait_for_tcp "$S8_BAD_VISITOR_PORT" 120 \
    && wait_for_tcp "$S8_MISMATCH_VISITOR_PORT" 120; then
    ok "the snark visitors bound their local ports"
else
    bad "the snark visitors bound their local ports" \
        "visitor: $(tail -2 "$S8_DIR/visitor.log") | wrong secret: $(tail -2 "$S8_DIR/wrong-secret.log")"
fi

# A visitor's local listener comes up on its own, before the owner's proxy is
# registered: a visitor that arrives first is told no such proxy exists, and every
# refusal below would pass for the wrong reason. So wait for the registration.
if wait_for_log "$S8_DIR/server.log" 'proxy "private-snark" (stcp) registered as private' 120; then
    ok "the server registered the snark proxy as a private one"
else
    bad "the server registered the snark proxy as a private one" "$(tail -2 "$S8_DIR/server.log")"
fi

# The proof is made by the visitor's client and checked by the server, so a working
# transfer is the whole path: the keys embedded in pkg/snarkauth, the client's
# dispatch on auth_method, the challenge and the server's verifier.
check "$(tcp_via "$S8_VISITOR_PORT")" "ok" \
    "a visitor that proved knowledge with a Groth16 proof reaches the private service"

check "$(s8_refused "$S8_BAD_VISITOR_PORT")" "refused" \
    "a visitor holding the wrong secret gets no data"
check "$(s8_refused "$S8_MISMATCH_VISITOR_PORT")" "refused" \
    "a visitor answering with the other proof method gets no data"

if grep -qF 'the proof does not verify' "$S8_DIR/mismatched.log"; then
    ok "the mismatched visitor's client says the proof was rejected"
else
    bad "the mismatched visitor's client says the proof was rejected" "$(tail -3 "$S8_DIR/mismatched.log")"
fi


# ── frp parity, round nine: the WebRTC data-channel visitor
# (a visitor's `transport = "webrtc"`).

W9_DIR="$WORK/features9"
mkdir -p "$W9_DIR"
W9_CONTROL_PORT="$(free_port)"
W9_VISITOR_PORT="$(free_port)"
W9_SECRET="functional-linux-webrtc-secret"

cat > "$W9_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $W9_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$W9_DIR/owner.toml" <<EOF
[client]
server_addr = "127.0.0.1:$W9_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "private-channel"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "$W9_SECRET"
EOF

# The visitor's local listener is the one every other visitor has. What changes is
# where the bytes go: an offer and an answer on the control connection, then DTLS
# over ICE on a data channel, so the relay carries signaling only.
cat > "$W9_DIR/visitor.toml" <<EOF
[client]
server_addr = "127.0.0.1:$W9_CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "channel"
type = "stcp"
server_name = "private-channel"
secret_key = "$W9_SECRET"
transport = "webrtc"
bind_addr = "127.0.0.1"
bind_port = $W9_VISITOR_PORT
EOF

"$SERVER" --config "$W9_DIR/server.toml" > "$W9_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$W9_DIR/owner.toml" > "$W9_DIR/owner.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$W9_DIR/visitor.toml" > "$W9_DIR/visitor.log" 2>&1 &
PIDS+=($!)

if wait_for_tcp "$W9_VISITOR_PORT" 120; then
    ok "the webrtc visitor bound its local port"
else
    bad "the webrtc visitor bound its local port" "$(tail -3 "$W9_DIR/visitor.log")"
fi

# The visitor's listener comes up before the owner's proxy is registered, and a
# caller that arrives first is refused for that reason instead of a channel one.
if wait_for_log "$W9_DIR/server.log" 'proxy "private-channel" (stcp) registered as private' 120; then
    ok "the server registered the proxy the channel visitor asks for"
else
    bad "the server registered the proxy the channel visitor asks for" "$(tail -2 "$W9_DIR/server.log")"
fi

# The caller sees an ordinary TCP listener; its bytes reach the private service
# through the owner, and the channel is what carries them there.
check "$(tcp_via "$W9_VISITOR_PORT")" "ok" \
    "a caller's bytes reach the private service over the data channel"

# The channel rather than the relay: the server writes this line only once the
# data channel is open, so a visitor that quietly fell back to the relayed path
# would carry the bytes above without it.
if wait_for_log "$W9_DIR/server.log" 'data channel established' 40; then
    ok "the server says the visitor moved onto its data channel"
else
    bad "the server says the visitor moved onto its data channel" "$(tail -3 "$W9_DIR/server.log")"
fi

# The visitor's own report says the same thing: a relayed session is not direct.
if wait_for_log "$W9_DIR/visitor.log" 'direct true' 40; then
    ok "the visitor's client reports a direct session"
else
    bad "the visitor's client reports a direct session" "$(tail -3 "$W9_DIR/visitor.log")"
fi


# ── frp parity, round ten: the server's [[http_plugins]] webhooks.

P10_DIR="$WORK/features10"
mkdir -p "$P10_DIR"
P10_PLUGIN_PORT="$(free_port)"
P10_CONTROL_PORT="$(free_port)"
P10_PROXY_PORT="$(free_port)"
P10_REJECT_PORT="$(free_port)"
P10_DEAD_PORT="$(free_port)"
P10_DEAD_ADDR_PORT="$(free_port)"

# The webhook receiver: it keeps every envelope it is asked about, and answers
# what the path says — /accept allows, /reject refuses the login with a reason.
# One receiver serves both servers below, so the recorded calls carry the path.
cat > "$P10_DIR/plugin.py" <<'PY'
import http.server, json, sys, threading

calls, port = sys.argv[1], int(sys.argv[2])
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length).decode())
        with lock:
            with open(calls, "a") as log:
                log.write(json.dumps({"path": self.path, "body": body}) + "\n")
        if self.path.startswith("/reject"):
            answer = {"reject": True, "reject_reason": "the webhook said no"}
        else:
            answer = {}
        payload = json.dumps(answer).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

python3 "$P10_DIR/plugin.py" "$P10_DIR/calls.jsonl" "$P10_PLUGIN_PORT" > "$P10_DIR/plugin.log" 2>&1 &
PIDS+=($!)
wait_for_tcp "$P10_PLUGIN_PORT" 40 || bad "the webhook receiver came up" "$(cat "$P10_DIR/plugin.log")"

# p10_calls <python expression over the recorded envelopes, bound to c>: one
# question about what the webhook was told.
p10_calls() {
    python3 - "$P10_DIR/calls.jsonl" "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1]) as log:
        c = [json.loads(line) for line in log if line.strip()]
except OSError:
    c = []
print(eval(sys.argv[2]))
PY
}

cat > "$P10_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $P10_CONTROL_PORT
auth_token = "$TOKEN"

# Every operation this section is about, on a webhook that allows.
[[http_plugins]]
name = "gate"
addr = "http://127.0.0.1:$P10_PLUGIN_PORT"
path = "/accept"
ops = ["login", "newProxy", "newUserConn"]
EOF

cat > "$P10_DIR/server-reject.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $P10_REJECT_PORT
auth_token = "$TOKEN"

[[http_plugins]]
name = "gate"
addr = "http://127.0.0.1:$P10_PLUGIN_PORT"
path = "/reject"
ops = ["login"]
EOF

# A webhook nothing answers: a gate that is down closes the door rather than
# holding it open, which is the choice this contract makes.
cat > "$P10_DIR/server-dead.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $P10_DEAD_PORT
auth_token = "$TOKEN"

[[http_plugins]]
name = "gone"
addr = "http://127.0.0.1:$P10_DEAD_ADDR_PORT"
path = "/plugin"
ops = ["login"]
timeout_secs = 2
EOF

cat > "$P10_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$P10_CONTROL_PORT"
auth_token = "$TOKEN"

# The webhook sees these: they are how an operator tells its gate which
# deployment is asking.
metas = { site = "berlin" }

[[proxies]]
name = "gated-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $P10_PROXY_PORT
EOF

cat > "$P10_DIR/rejected.toml" <<EOF
[client]
server_addr = "127.0.0.1:$P10_REJECT_PORT"
auth_token = "$TOKEN"
login_fail_exit = true

[[proxies]]
name = "never-published"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
EOF

cat > "$P10_DIR/unreachable.toml" <<EOF
[client]
server_addr = "127.0.0.1:$P10_DEAD_PORT"
auth_token = "$TOKEN"
login_fail_exit = true

[[proxies]]
name = "never-published"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
EOF

"$SERVER" --config "$P10_DIR/server.toml" > "$P10_DIR/server.log" 2>&1 &
PIDS+=($!)
"$SERVER" --config "$P10_DIR/server-reject.toml" > "$P10_DIR/server-reject.log" 2>&1 &
PIDS+=($!)
"$SERVER" --config "$P10_DIR/server-dead.toml" > "$P10_DIR/server-dead.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$P10_DIR/client.toml" > "$P10_DIR/client.log" 2>&1 &
PIDS+=($!)

if wait_for_tcp "$P10_PROXY_PORT" 120; then
    ok "a client logs in through a webhook that allows"
else
    bad "a client logs in through a webhook that allows" \
        "client: $(tail -2 "$P10_DIR/client.log") | server: $(tail -2 "$P10_DIR/server.log")"
fi

check "$(tcp_via "$P10_PROXY_PORT")" "ok" "the tunnel behind the allowed login carries bytes"

check "$(p10_calls 'any(x["body"]["op"] == "login" for x in c)')" "True" \
    "the server asked the webhook about the login"
check "$(p10_calls '[x["body"]["content"]["user"]["metas"].get("site") for x in c if x["body"]["op"] == "login"][0]')" "berlin" \
    "the login the webhook saw carries the client's metas"
check "$(p10_calls 'any(x["body"]["op"] == "newProxy" and x["body"]["content"]["name"] == "gated-tcp" for x in c)')" "True" \
    "the server asked the webhook about the registration, by name"
check "$(p10_calls 'any(x["body"]["op"] == "newUserConn" and x["body"]["content"]["proxy_name"] == "gated-tcp" for x in c)')" "True" \
    "the server asked the webhook about the visitor connection"

# A webhook that refuses the login refuses the session, and its own reason is
# what the client is told.
"$CLIENT" --config "$P10_DIR/rejected.toml" > "$P10_DIR/rejected.log" 2>&1
p10_rejected=$?
check "$p10_rejected" "1" "a client the webhook refuses exits with a failure status"
if grep -qF 'the webhook said no' "$P10_DIR/rejected.log"; then
    ok "the refused client is told the webhook's own reason"
else
    bad "the refused client is told the webhook's own reason" "$(tail -3 "$P10_DIR/rejected.log")"
fi

# A webhook nothing answers refuses as well: the gate closes the door.
"$CLIENT" --config "$P10_DIR/unreachable.toml" > "$P10_DIR/unreachable.log" 2>&1
p10_unreachable=$?
check "$p10_unreachable" "1" "a client whose gate cannot be reached is refused"
if grep -qF 'send login request to plugin error' "$P10_DIR/unreachable.log"; then
    ok "the refused client is told the gate could not be reached"
else
    bad "the refused client is told the gate could not be reached" "$(tail -3 "$P10_DIR/unreachable.log")"
fi


# ── frp parity, round eleven: the SSH tunnel gateway, driven by the ssh binary.
#
# The Go test drives this with golang.org/x/crypto/ssh; here it is the OpenSSH
# client, which is what an operator has. It is skipped rather than failed where
# ssh is not installed.

if command -v ssh >/dev/null 2>&1 && command -v ssh-keygen >/dev/null 2>&1; then
    G11_DIR="$WORK/features11"
    mkdir -p "$G11_DIR"
    G11_SSH_PORT="$(free_port)"
    G11_CONTROL_PORT="$(free_port)"
    G11_PUBLIC_PORT="$(free_port)"
    G11_OUTSIDE_PORT="$(free_port)"

    # The client key the gateway authorizes, and a second one it does not.
    ssh-keygen -q -t ed25519 -N "" -f "$G11_DIR/id_ed25519" > /dev/null 2>&1
    cp "$G11_DIR/id_ed25519.pub" "$G11_DIR/authorized_keys"
    ssh-keygen -q -t ed25519 -N "" -f "$G11_DIR/other_key" > /dev/null 2>&1

    cat > "$G11_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G11_CONTROL_PORT
auth_token = "$TOKEN"
# One port is allowed, so a publish outside it is refused by the same policy a
# client's registration answers to.
allow_ports = ["$G11_PUBLIC_PORT-$G11_PUBLIC_PORT"]

[server.ssh_tunnel_gateway]
bind_addr = "127.0.0.1"
bind_port = $G11_SSH_PORT
auto_gen_key_file = "$G11_DIR/host_key"
authorized_keys_file = "$G11_DIR/authorized_keys"
max_proxies = 2
idle_timeout_seconds = 60
EOF

    # ssh options shared by every call: never prompt, do not pin the host key
    # (the gateway generates one on first use), and fail if the forwarding rule
    # is refused. G11_SSH_LIMIT bounds a call that must not stay up, which the
    # publishing calls leave empty on purpose.
    #
    # The command is built as an array so g11_ssh_exec below can exec it: bash runs
    # a backgrounded shell function in a subshell, so $! named the subshell and the
    # recorded pid was not the ssh process at all.
    g11_ssh_cmd() {
        local limit=""
        [ -n "${G11_SSH_LIMIT:-}" ] && limit="timeout $G11_SSH_LIMIT"
        # shellcheck disable=SC2206 # the limit is intentionally split into
        # "timeout" and its seconds, exactly as the unquoted form did.
        g11_command=($limit ssh -i "$1" -o IdentitiesOnly=yes -o BatchMode=yes \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=10 \
            -o ExitOnForwardFailure=yes -p "$G11_SSH_PORT" \
            -R "127.0.0.1:0:127.0.0.1:$TCP_ECHO_PORT" \
            v0@127.0.0.1 "${@:2}")
    }
    g11_ssh() {
        g11_ssh_cmd "$@"
        "${g11_command[@]}"
    }
    # For the calls that are backgrounded: exec replaces the subshell, so the pid
    # the cleanup records is the ssh client itself and killing it kills the client.
    g11_ssh_exec() {
        g11_ssh_cmd "$@"
        exec "${g11_command[@]}"
    }

    "$SERVER" --config "$G11_DIR/server.toml" > "$G11_DIR/server.log" 2>&1 &
    PIDS+=($!)
    if wait_for_tcp "$G11_SSH_PORT" 120; then
        ok "the ssh tunnel gateway listens on its own port"
    else
        bad "the ssh tunnel gateway listens on its own port" "$(tail -3 "$G11_DIR/server.log")"
    fi

    G11_SSH_LIMIT= g11_ssh_exec "$G11_DIR/id_ed25519" tcp --proxy_name ssh-gateway \
        --remote_port "$G11_PUBLIC_PORT" --token "$TOKEN" > "$G11_DIR/ssh.log" 2>&1 &
    PIDS+=($!)
    if wait_for_tcp "$G11_PUBLIC_PORT" 120; then
        ok "an ssh client published a tunnel through the gateway"
    else
        bad "an ssh client published a tunnel through the gateway" \
            "ssh: $(tr '\n' ' ' < "$G11_DIR/ssh.log") | server: $(tail -2 "$G11_DIR/server.log")"
    fi

    check "$(tcp_via "$G11_PUBLIC_PORT")" "ok" \
        "the published port carries bytes to the service the ssh client dials"

    if wait_for_log "$G11_DIR/ssh.log" 'RemoteAddress:' 40; then
        ok "the ssh client is told where its tunnel is published"
    else
        bad "the ssh client is told where its tunnel is published" "$(tr '\n' ' ' < "$G11_DIR/ssh.log")"
    fi

    # The tunnel is a session like a client's, so the server records it as one.
    if grep -q 'ssh-gateway' "$G11_DIR/server.log"; then
        ok "the server logs the gateway's tunnel like a client's"
    else
        bad "the server logs the gateway's tunnel like a client's" "$(tail -2 "$G11_DIR/server.log")"
    fi

    # A key that is not in authorized_keys cannot publish: the gateway must not
    # fall back to accepting anyone.
    G11_SSH_LIMIT=30 g11_ssh "$G11_DIR/other_key" tcp --proxy_name refused-key \
        --remote_port "$G11_PUBLIC_PORT" --token "$TOKEN" > "$G11_DIR/other-key.log" 2>&1
    if [ $? -eq 0 ]; then
        bad "an ssh key outside authorized_keys is refused" "the ssh client exited 0"
    else
        if grep -q 'Permission denied' "$G11_DIR/other-key.log"; then
            ok "an ssh key outside authorized_keys is refused"
        else
            bad "an ssh key outside authorized_keys is refused" "$(tr '\n' ' ' < "$G11_DIR/other-key.log")"
        fi
    fi

    # server.allow_ports applies to the gateway the way it applies to a client's
    # registration. The session stays open so the operator can fix the command,
    # so the process is killed once the reason is in its output.
    G11_SSH_LIMIT= g11_ssh_exec "$G11_DIR/id_ed25519" tcp --proxy_name outside-allow-ports \
        --remote_port "$G11_OUTSIDE_PORT" --token "$TOKEN" > "$G11_DIR/outside.log" 2>&1 &
    G11_OUTSIDE_PID=$!
    PIDS+=($G11_OUTSIDE_PID)
    if wait_for_log "$G11_DIR/outside.log" 'outside allow_ports' 80; then
        ok "a publish outside allow_ports is refused with the policy's reason"
    else
        bad "a publish outside allow_ports is refused with the policy's reason" "$(tr '\n' ' ' < "$G11_DIR/outside.log")"
    fi
    kill -9 "$G11_OUTSIDE_PID" 2>/dev/null || true
else
    echo "SKIP  the ssh gateway checks: no ssh and ssh-keygen on this machine"
fi


# ── frp parity, round twelve: log.level, and the client store.

G12_DIR="$WORK/features12"
mkdir -p "$G12_DIR"
G12_CONTROL_PORT="$(free_port)"
G12_ADMIN_PORT="$(free_port)"
G12_PUBLIC_PORT="$(free_port)"
G12_STORED_PORT="$(free_port)"

# A server whose log keeps warnings only: a refused login has to be in it, the
# startup lines must not be.
cat > "$G12_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G12_CONTROL_PORT
auth_token = "$TOKEN"

[log]
level = "warn"
EOF

# A client that gives up on a refused first login, with the same floor: its own
# give-up line is a warning, its startup line is info.
cat > "$G12_DIR/wrong-token.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G12_CONTROL_PORT"
auth_token = "not-the-token"
login_fail_exit = true

[log]
level = "warn"
EOF

"$SERVER" --config "$G12_DIR/server.toml" > "$G12_DIR/server.log" 2>&1 &
G12_SERVER_PID=$!
PIDS+=($G12_SERVER_PID)
if wait_for_tcp "$G12_CONTROL_PORT" 120; then
    ok "a server with log.level = \"warn\" comes up"
else
    bad "a server with log.level = \"warn\" comes up" "$(cat "$G12_DIR/server.log")"
fi

"$CLIENT" --config "$G12_DIR/wrong-token.toml" > "$G12_DIR/wrong-token.log" 2>&1
g12_client_exit=$?
check "$g12_client_exit" "1" "a client whose first login is refused exits with a failure status"

if grep -qF 'authentication failed for' "$G12_DIR/server.log"; then
    ok "a warning line (the refusal) is written at log.level = \"warn\""
else
    bad "a warning line (the refusal) is written at log.level = \"warn\"" "$(cat "$G12_DIR/server.log")"
fi
if grep -qF 'listening on' "$G12_DIR/server.log"; then
    bad "an info line (the listener) is dropped at log.level = \"warn\"" "$(grep -F 'listening on' "$G12_DIR/server.log" | head -1)"
else
    ok "an info line (the listener) is dropped at log.level = \"warn\""
fi
if grep -qF 'login_fail_exit is set' "$G12_DIR/wrong-token.log" \
    && ! grep -qF 'AetherTunnel client' "$G12_DIR/wrong-token.log"; then
    ok "the client's own floor keeps its give-up line and drops its startup line"
else
    bad "the client's own floor keeps its give-up line and drops its startup line" "$(cat "$G12_DIR/wrong-token.log")"
fi

# The store: entries created through the admin API, persisted to a file, and
# restored when the client comes back.
cat > "$G12_DIR/store-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G12_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$G12_DIR/store-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G12_CONTROL_PORT"
auth_token = "$TOKEN"

[client.admin]
enabled = true
bind_addr = "127.0.0.1"
port = $G12_ADMIN_PORT
user = "ops"
password = "s3cret"

[store]
path = "$G12_DIR/store.json"
EOF

"$SERVER" --config "$G12_DIR/store-server.toml" > "$G12_DIR/store-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G12_DIR/store-client.toml" > "$G12_DIR/store-client.log" 2>&1 &
G12_STORE_CLIENT_PID=$!
PIDS+=($G12_STORE_CLIENT_PID)

# g12_admin <method> <path> [body]: one call to the client's admin API with its
# basic credentials, printing "status <code>" and the body on one line.
g12_admin() {
    python3 - "$G12_ADMIN_PORT" "$1" "$2" "${3:-}" <<'PY'
import base64, sys, urllib.error, urllib.request

port, method, path, body = sys.argv[1:5]
request = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path), method=method,
                                 data=body.encode() if body else None)
request.add_header("Authorization", "Basic " + base64.b64encode(b"ops:s3cret").decode())
request.add_header("Content-Type", "application/json")
try:
    answer = urllib.request.urlopen(request, timeout=15)
    print("status", answer.status, answer.read().decode().strip())
except urllib.error.HTTPError as exc:
    print("status", exc.code, exc.read().decode().strip())
except Exception as exc:
    print("no answer:", exc)
PY
}

if wait_for_log "$G12_DIR/store-client.log" 'admin api on' 120; then
    ok "the client's admin API answers when the store is configured"
else
    bad "the client's admin API answers when the store is configured" "$(tail -3 "$G12_DIR/store-client.log")"
fi

# An entry created at runtime, with a name and a public port nothing else uses.
g12_put="$(g12_admin PUT /api/store/proxy/runtime-tcp \
    "{\"type\":\"tcp\",\"local_ip\":\"127.0.0.1\",\"local_port\":$TCP_ECHO_PORT,\"remote_port\":$G12_PUBLIC_PORT}")"
case "$g12_put" in
    status\ 200*) ok "a proxy entry created through the store's API is accepted" ;;
    *) bad "a proxy entry created through the store's API is accepted" "$g12_put" ;;
esac

if wait_for_tcp "$G12_PUBLIC_PORT" 120; then
    ok "the runtime entry is published without a restart"
else
    bad "the runtime entry is published without a restart" "$(g12_admin GET /api/status) | $(tail -3 "$G12_DIR/store-client.log")"
fi
check "$(tcp_via "$G12_PUBLIC_PORT")" "ok" "the runtime entry's port carries bytes"

case "$(g12_admin GET /api/store/proxy/runtime-tcp)" in
    status\ 200*) ok "the stored entry reads back through the API" ;;
    *) bad "the stored entry reads back through the API" "$(g12_admin GET /api/store/proxy/runtime-tcp)" ;;
esac

# An entry the configuration decoder refuses is refused here too, rather than
# being written and discovered at the next restart.
g12_bad="$(g12_admin PUT /api/store/proxy/bad-entry \
    "{\"type\":\"tcp\",\"local_ip\":\"127.0.0.1\",\"local_port\":$TCP_ECHO_PORT,\"remote_port\":$G12_STORED_PORT,\"no_such_key\":1}")"
case "$g12_bad" in
    status\ 400*) ok "a runtime entry with an unknown key is refused" ;;
    *) bad "a runtime entry with an unknown key is refused" "$g12_bad" ;;
esac

# Persistence: the entry survives the client, which is the point of the file.
kill -9 "$G12_STORE_CLIENT_PID" 2>/dev/null || true
# Reap it, so the shell does not print a job-termination notice in the middle of
# the suite's output.
wait "$G12_STORE_CLIENT_PID" 2>/dev/null || true
sleep 1
"$CLIENT" --config "$G12_DIR/store-client.toml" > "$G12_DIR/store-client2.log" 2>&1 &
PIDS+=($!)
if wait_for_log "$G12_DIR/store-client2.log" 'runtime proxy entry' 120 && wait_for_tcp "$G12_PUBLIC_PORT" 120; then
    ok "the stored entry is restored and published after a restart"
else
    bad "the stored entry is restored and published after a restart" "$(tail -3 "$G12_DIR/store-client2.log")"
fi

# Deleting it withdraws the proxy and closes the public port.
case "$(g12_admin DELETE /api/store/proxy/runtime-tcp)" in
    status\ 200*) ok "the stored entry is deleted through the API" ;;
    *) bad "the stored entry is deleted through the API" "$(g12_admin DELETE /api/store/proxy/runtime-tcp)" ;;
esac

g12_closed=0
for _ in $(seq 1 60); do
    if ! wait_for_tcp "$G12_PUBLIC_PORT" 1 2>/dev/null; then
        g12_closed=1
        break
    fi
    sleep 0.25
done
check "$g12_closed" "1" "deleting the entry closes its public port"


# ── frp parity, round thirteen: token files, the published-port bind address and
# timestamp-free logs. Three keys the VS-FRP table lists as reproduced whose only
# coverage was a unit test, so each is driven here through the real binaries.

G13_DIR="$WORK/features13"
mkdir -p "$G13_DIR"
G13_CONTROL_PORT="$(free_port)"
G13_PUBLIC_PORT="$(free_port)"
G13_FAIL_PORT="$(free_port)"

# The token is a file on both ends, written the way an editor leaves it: with a
# trailing newline. A successful authentication therefore also proves the line,
# and not the file's last byte, is the secret.
printf '%s\n' "$TOKEN" > "$G13_DIR/token"
chmod 600 "$G13_DIR/token"

cat > "$G13_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G13_CONTROL_PORT
proxy_bind_addr = "127.0.0.2"
auth_token_file = "$G13_DIR/token"

[log]
disable_timestamp = true
EOF

cat > "$G13_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G13_CONTROL_PORT"
auth_token_file = "$G13_DIR/token"

[log]
disable_timestamp = true

[[proxies]]
name = "tokenfile"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G13_PUBLIC_PORT
EOF

"$SERVER" --config "$G13_DIR/server.toml" > "$G13_DIR/server.log" 2>&1 &
PIDS+=($!)
if wait_for_tcp "$G13_CONTROL_PORT" 120; then
    ok "a server starts with its token read from auth_token_file"
else
    bad "a server starts with its token read from auth_token_file" "$(cat "$G13_DIR/server.log")"
fi

"$CLIENT" --config "$G13_DIR/client.toml" > "$G13_DIR/client.log" 2>&1 &
PIDS+=($!)
if wait_for_log "$G13_DIR/client.log" 'server confirms' 120; then
    ok "a client authenticates with the same token file, newline and all"
else
    bad "a client authenticates with the same token file, newline and all" "$(tail -3 "$G13_DIR/client.log")"
fi

# g13_connects <host> <port> prints "open" or "closed".
g13_connects() {
    python3 - "$1" "$2" <<'PY'
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5).close()
    print("open")
except OSError:
    print("closed")
PY
}

# g13_tcp_via <host> <port>: the range-check round trip against a named address,
# which tcp_via cannot do because it always dials 127.0.0.1.
g13_tcp_via() {
    python3 - "$1" "$2" <<'PY'
import socket, sys
payload = b"range-check"
try:
    s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
else:
    s.settimeout(15)
    s.sendall(payload)
    got = b""
    while len(got) < len(payload):
        chunk = s.recv(4096)
        if not chunk:
            break
        got += chunk
    s.close()
    print("ok" if got == payload else "got %r" % got)
PY
}

# proxy_bind_addr sends the published port to 127.0.0.2 while the control port
# stays on bind_addr's 127.0.0.1: the two are told apart by where the port answers.
g13_up=0
for _ in $(seq 1 120); do
    if [ "$(g13_connects 127.0.0.2 "$G13_PUBLIC_PORT")" = "open" ]; then
        g13_up=1
        break
    fi
    sleep 0.25
done
check "$g13_up" "1" "the tunnel is published on proxy_bind_addr's address"
check "$(g13_tcp_via 127.0.0.2 "$G13_PUBLIC_PORT")" "ok" \
    "the published port carries bytes on proxy_bind_addr"
check "$(g13_connects 127.0.0.1 "$G13_PUBLIC_PORT")" "closed" \
    "proxy_bind_addr keeps the published port off bind_addr"

# disable_timestamp: the listener line is there, and no line starts with the
# date-and-time prefix the default flags add.
if grep -q 'listening on' "$G13_DIR/server.log" \
    && ! grep -qE '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} ' "$G13_DIR/server.log"; then
    ok "disable_timestamp writes lines with no timestamp"
else
    bad "disable_timestamp writes lines with no timestamp" "$(head -2 "$G13_DIR/server.log")"
fi

# An empty token file is refused rather than treated as an empty secret, which
# would surface much later as an authentication failure naming nothing.
: > "$G13_DIR/empty-token"
cat > "$G13_DIR/empty-token.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G13_FAIL_PORT
auth_token_file = "$G13_DIR/empty-token"
EOF
g13_empty_out="$("$SERVER" --config "$G13_DIR/empty-token.toml" 2>&1)"
g13_empty_exit=$?
if [ "$g13_empty_exit" -ne 0 ] && printf '%s' "$g13_empty_out" | grep -qF 'is empty'; then
    ok "an empty auth_token_file is refused instead of starting without a token"
else
    bad "an empty auth_token_file is refused instead of starting without a token" \
        "exit $g13_empty_exit: $g13_empty_out"
fi


# ── frp parity, round fourteen: vhost path routing (locations) and the per-proxy
# request/response header injection. Both were listed as reproduced with unit
# tests only; here two http proxies share one domain and are told apart by path.

G14_DIR="$WORK/features14"
mkdir -p "$G14_DIR"
G14_CONTROL_PORT="$(free_port)"
G14_HTTP_PORT="$(free_port)"
G14_API_PORT="$(free_port)"
G14_REST_PORT="$(free_port)"

# A backend that names itself and reports the request headers it received, so an
# injected request header is visible in the body and the two proxies' backends
# cannot be confused for one another.
cat > "$G14_DIR/backend.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

tag, port = sys.argv[1], int(sys.argv[2])

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"tag": tag, "path": self.path,
                           "headers": {k: v for k, v in self.headers.items()}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

python3 "$G14_DIR/backend.py" api "$G14_API_PORT" > "$G14_DIR/api.log" 2>&1 &
PIDS+=($!)
python3 "$G14_DIR/backend.py" rest "$G14_REST_PORT" > "$G14_DIR/rest.log" 2>&1 &
PIDS+=($!)
wait_for_tcp "$G14_API_PORT" 40 && wait_for_tcp "$G14_REST_PORT" 40

cat > "$G14_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G14_CONTROL_PORT
http_port = $G14_HTTP_PORT
auth_token = "$TOKEN"
EOF

cat > "$G14_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G14_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "api"
type = "http"
domains = ["route.test"]
locations = ["/api"]
local_port = $G14_API_PORT
request_headers = { "X-Inject" = "one" }
response_headers = { "X-Resp" = "one" }

[[proxies]]
name = "rest"
type = "http"
domains = ["route.test"]
local_port = $G14_REST_PORT
EOF

"$SERVER" --config "$G14_DIR/server.toml" > "$G14_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G14_DIR/client.toml" > "$G14_DIR/client.log" 2>&1 &
PIDS+=($!)
if wait_for_log "$G14_DIR/client.log" 'server confirms 2 tunnel' 120; then
    ok "two http proxies share one domain, one claiming a path prefix"
else
    bad "two http proxies share one domain, one claiming a path prefix" "$(tail -3 "$G14_DIR/client.log")"
fi

# g14_probe <path> prints "<backend tag> <path> <X-Inject> <X-Resp>", with "-"
# for a header that did not arrive.
g14_probe() {
    python3 - "$G14_HTTP_PORT" "$1" <<'PY'
import json, sys, urllib.request

port, path = sys.argv[1], sys.argv[2]
request = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path))
request.add_header("Host", "route.test")
with urllib.request.urlopen(request, timeout=15) as answer:
    body = json.loads(answer.read())
print("%s %s %s %s" % (body["tag"], body["path"],
                       body["headers"].get("X-Inject", "-"),
                       answer.headers.get("X-Resp", "-")))
PY
}

check "$(g14_probe /api/x)" "api /api/x one one" \
    "a path under locations reaches that proxy, with its request and response headers"
check "$(g14_probe /elsewhere)" "rest /elsewhere - -" \
    "a path outside every locations prefix falls to the proxy that declares none"
check "$(g14_probe /apix)" "api /apix one one" \
    "locations matches a path prefix, not a whole segment"


# ── frp parity, round fifteen: a visitor's fallback_to. The key was listed as
# reproduced with unit tests only; here the tunnel really goes away mid-flight
# and the caller has to arrive at the fallback address with nothing else changing.

G15_DIR="$WORK/features15"
mkdir -p "$G15_DIR"
G15_CONTROL_PORT="$(free_port)"
G15_TUNNEL_PORT="$(free_port)"
G15_FALLBACK_PORT="$(free_port)"
G15_VISITOR_PORT="$(free_port)"

# Two backends that prefix the tag to whatever they are sent, so the answer
# says which of the two the caller reached.
cat > "$G15_DIR/echo.py" <<'PY'
import socket, sys, threading

tag, port = sys.argv[1], int(sys.argv[2])
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(16)

def serve(conn):
    try:
        conn.settimeout(10)
        data = conn.recv(4096)
        if data:
            conn.sendall(tag.encode() + b":" + data)
    except OSError:
        pass
    finally:
        conn.close()

while True:
    connection, _ = srv.accept()
    threading.Thread(target=serve, args=(connection,), daemon=True).start()
PY

python3 "$G15_DIR/echo.py" tunnel "$G15_TUNNEL_PORT" > "$G15_DIR/tunnel.log" 2>&1 &
PIDS+=($!)
python3 "$G15_DIR/echo.py" fallback "$G15_FALLBACK_PORT" > "$G15_DIR/fallback.log" 2>&1 &
PIDS+=($!)
wait_for_tcp "$G15_TUNNEL_PORT" 40 && wait_for_tcp "$G15_FALLBACK_PORT" 40

cat > "$G15_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G15_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$G15_DIR/publisher.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G15_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "private"
type = "stcp"
secret_key = "s3cret"
local_ip = "127.0.0.1"
local_port = $G15_TUNNEL_PORT
EOF

cat > "$G15_DIR/visitor.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G15_CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "v"
type = "stcp"
server_name = "private"
secret_key = "s3cret"
bind_addr = "127.0.0.1"
bind_port = $G15_VISITOR_PORT
fallback_to = "127.0.0.1:$G15_FALLBACK_PORT"
fallback_timeout_ms = 1000
EOF

"$SERVER" --config "$G15_DIR/server.toml" > "$G15_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G15_DIR/publisher.toml" > "$G15_DIR/publisher.log" 2>&1 &
G15_PUBLISHER_PID=$!
PIDS+=($G15_PUBLISHER_PID)
"$CLIENT" --config "$G15_DIR/visitor.toml" > "$G15_DIR/visitor.log" 2>&1 &
PIDS+=($!)

wait_for_log "$G15_DIR/publisher.log" 'server confirms' 120
if wait_for_log "$G15_DIR/visitor.log" 'listening on' 120; then
    ok "a private visitor with fallback_to binds its caller-facing port"
else
    bad "a private visitor with fallback_to binds its caller-facing port" "$(tail -3 "$G15_DIR/visitor.log")"
fi

# g15_say <port> <payload> prints the tagged answer the caller received.
g15_say() {
    python3 - "$1" "$2" <<'PY'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
    s.settimeout(15)
    s.sendall(sys.argv[2].encode())
    print(s.recv(4096).decode())
    s.close()
except OSError as exc:
    print("no answer: %s" % exc)
PY
}

check "$(g15_say "$G15_VISITOR_PORT" hello)" "tunnel:hello" \
    "a private visitor reaches the tunnel's backend while its publisher is online"

# The publisher leaves; the proxy is gone, so the next caller cannot be tunnelled
# and must land on fallback_to instead.
kill "$G15_PUBLISHER_PID" 2>/dev/null || true
wait "$G15_PUBLISHER_PID" 2>/dev/null || true

g15_fell_back=""
for _ in $(seq 1 40); do
    answer="$(g15_say "$G15_VISITOR_PORT" hello)"
    if [ "$answer" = "fallback:hello" ]; then
        g15_fell_back="$answer"
        break
    fi
    sleep 0.5
done
check "$g15_fell_back" "fallback:hello" \
    "the same caller reaches fallback_to once the tunnel is gone"
if wait_for_log "$G15_DIR/visitor.log" 'fallback session finished' 40; then
    ok "the visitor logs the fallback session"
else
    bad "the visitor logs the fallback session" "$(tail -3 "$G15_DIR/visitor.log")"
fi


# ── frp parity, round sixteen: selecting which entries run, and how much of a
# refusal the client is told. Both had unit tests only.

G16_DIR="$WORK/features16"
mkdir -p "$G16_DIR"
G16_CONTROL_PORT="$(free_port)"
G16_ON_PORT="$(free_port)"
G16_OFF_PORT="$(free_port)"
G16_START_A_PORT="$(free_port)"
G16_START_B_PORT="$(free_port)"

cat > "$G16_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G16_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$G16_DIR/enabled.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G16_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "on"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G16_ON_PORT

[[proxies]]
name = "off"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G16_OFF_PORT
enabled = false
EOF

cat > "$G16_DIR/start.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G16_CONTROL_PORT"
auth_token = "$TOKEN"
start = ["b"]

[[proxies]]
name = "a"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G16_START_A_PORT

[[proxies]]
name = "b"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G16_START_B_PORT
EOF

"$SERVER" --config "$G16_DIR/server.toml" > "$G16_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G16_DIR/enabled.toml" > "$G16_DIR/enabled.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G16_DIR/start.toml" > "$G16_DIR/start.log" 2>&1 &
PIDS+=($!)

# Wait for the enabled proxy first, so the closed-port check below cannot pass
# merely because the client had not started yet.
if wait_for_tcp "$G16_ON_PORT" 120; then
    check "$(tcp_via "$G16_ON_PORT")" "ok" "a proxy with enabled = true is published and carries bytes"
else
    bad "a proxy with enabled = true is published and carries bytes" "$(tail -3 "$G16_DIR/enabled.log")"
fi
if ! wait_for_tcp "$G16_OFF_PORT" 4 2>/dev/null; then
    ok "its enabled = false neighbour opens no port"
else
    bad "its enabled = false neighbour opens no port" "something is listening on $G16_OFF_PORT"
fi

if wait_for_log "$G16_DIR/start.log" 'selects 1 of 2' 120 && wait_for_tcp "$G16_START_B_PORT" 120; then
    check "$(tcp_via "$G16_START_B_PORT")" "ok" "client.start publishes the tunnel it names"
else
    bad "client.start publishes the tunnel it names" "$(tail -3 "$G16_DIR/start.log")"
fi
if ! wait_for_tcp "$G16_START_A_PORT" 4 2>/dev/null; then
    ok "client.start leaves the tunnel it does not name unpublished"
else
    bad "client.start leaves the tunnel it does not name unpublished" "something is listening on $G16_START_A_PORT"
fi

# detailed_errors_to_client: the same refusal the default server describes in
# full reaches the client as a short summary, while the server's own log keeps
# the detail (which the allow_ports section above already checks for the default).
G16_TERSE_CONTROL_PORT="$(free_port)"
cat > "$G16_DIR/terse-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G16_TERSE_CONTROL_PORT
auth_token = "$TOKEN"
allow_ports = ["20000-20009"]
detailed_errors_to_client = false
EOF

cat > "$G16_DIR/terse-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G16_TERSE_CONTROL_PORT"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1

[[proxies]]
name = "outside"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = 20011
EOF

"$SERVER" --config "$G16_DIR/terse-server.toml" > "$G16_DIR/terse-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G16_DIR/terse-client.toml" > "$G16_DIR/terse-client.log" 2>&1 &
PIDS+=($!)

g16_terse=0
for _ in $(seq 1 240); do
    if grep -q 'cannot register the proxy "outside"' "$G16_DIR/terse-client.log" 2>/dev/null; then
        g16_terse=1
        break
    fi
    sleep 0.5
done
check "$g16_terse" "1" "a server with detailed_errors_to_client = false sends the short summary"
if ! grep -q 'outside allow_ports' "$G16_DIR/terse-client.log" 2>/dev/null; then
    ok "the short summary does not name the server's allow_ports"
else
    bad "the short summary does not name the server's allow_ports" \
        "$(grep -F 'outside allow_ports' "$G16_DIR/terse-client.log" | head -1)"
fi
if grep -q 'outside allow_ports' "$G16_DIR/terse-server.log" 2>/dev/null; then
    ok "the server's own log keeps the full reason the client was not told"
else
    bad "the server's own log keeps the full reason the client was not told" "$(tail -3 "$G16_DIR/terse-server.log")"
fi


# ── frp parity, round seventeen: the four remaining network keys -- the client's
# source address and resolver, the server's datagram cap, and the vhost response
# deadline. All four had unit tests only.

G17_DIR="$WORK/features17"
mkdir -p "$G17_DIR"
G17_CONTROL_PORT="$(free_port)"
G17_SRC_PORT="$(free_port)"
G17_DNS_PORT="$(free_port)"
G17_RESOLVER_PORT="$(free_port)"
G17_UDP_CONTROL_PORT="$(free_port)"
G17_UDP_PUBLIC_PORT="$(free_port)"
G17_UDP_LOCAL_PORT="$(free_port)"
G17_VHOST_CONTROL_PORT="$(free_port)"
G17_VHOST_HTTP_PORT="$(free_port)"
G17_VHOST_LOCAL_PORT="$(free_port)"

# A resolver that answers exactly one name with 127.0.0.1, so a positive
# dns_server check does not need a real DNS service.
cat > "$G17_DIR/resolver.py" <<'PY'
import socket, struct, sys

port, name, address = int(sys.argv[1]), sys.argv[2], sys.argv[3]
srv = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
srv.bind(("127.0.0.1", port))

def question_end(data):
    off = 12
    while data[off]:
        off += 1 + data[off]
    return off + 1

while True:
    data, addr = srv.recvfrom(4096)
    end = question_end(data)
    qtype = struct.unpack("!H", data[end:end+2])[0]
    header = struct.pack("!HHHHHH", struct.unpack("!H", data[0:2])[0],
                         0x8180 if qtype == 1 else 0x8183, 1, 1 if qtype == 1 else 0, 0, 0)
    answer = b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 30, 4) + socket.inet_aton(address)
    srv.sendto(header + data[12:end+4] + (answer if qtype == 1 else b""), addr)
PY

# A datagram echo that tags its answer, and a service that accepts a request and
# then says nothing until vhost_http_timeout has had its say.
cat > "$G17_DIR/udp_echo.py" <<'PY'
import socket, sys

port = int(sys.argv[1])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", port))
while True:
    data, addr = s.recvfrom(65535)
    s.sendto(b"echo:" + data, addr)
PY

cat > "$G17_DIR/slow_http.py" <<'PY'
import socket, sys, threading, time

port = int(sys.argv[1])
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(16)

def hold(conn):
    time.sleep(10)
    conn.close()

while True:
    connection, _ = srv.accept()
    threading.Thread(target=hold, args=(connection,), daemon=True).start()
PY

python3 "$G17_DIR/resolver.py" "$G17_RESOLVER_PORT" server.test 127.0.0.1 > "$G17_DIR/resolver.log" 2>&1 &
PIDS+=($!)
python3 "$G17_DIR/udp_echo.py" "$G17_UDP_LOCAL_PORT" > "$G17_DIR/udp_echo.log" 2>&1 &
PIDS+=($!)
python3 "$G17_DIR/slow_http.py" "$G17_VHOST_LOCAL_PORT" > "$G17_DIR/slow_http.log" 2>&1 &
PIDS+=($!)

cat > "$G17_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G17_CONTROL_PORT
auth_token = "$TOKEN"
EOF

cat > "$G17_DIR/source.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G17_CONTROL_PORT"
auth_token = "$TOKEN"
connect_server_local_ip = "127.0.0.2"

[[proxies]]
name = "from2"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G17_SRC_PORT
EOF

cat > "$G17_DIR/resolved.toml" <<EOF
[client]
server_addr = "server.test:$G17_CONTROL_PORT"
dns_server = "127.0.0.1:$G17_RESOLVER_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "byname"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G17_DNS_PORT
EOF

"$SERVER" --config "$G17_DIR/server.toml" > "$G17_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G17_DIR/source.toml" > "$G17_DIR/source.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G17_DIR/resolved.toml" > "$G17_DIR/resolved.log" 2>&1 &
PIDS+=($!)

if wait_for_tcp "$G17_SRC_PORT" 120; then
    check "$(tcp_via "$G17_SRC_PORT")" "ok" "a client that bound connect_server_local_ip still carries bytes"
else
    bad "a client that bound connect_server_local_ip still carries bytes" "$(tail -3 "$G17_DIR/source.log")"
fi
if grep -q 'client 127.0.0.2:' "$G17_DIR/server.log"; then
    ok "the server sees the source address connect_server_local_ip names"
else
    bad "the server sees the source address connect_server_local_ip names" "$(grep -F 'connected as' "$G17_DIR/server.log" | tail -2)"
fi

if wait_for_tcp "$G17_DNS_PORT" 120; then
    check "$(tcp_via "$G17_DNS_PORT")" "ok" "dns_server resolves the server's name through the configured resolver"
else
    bad "dns_server resolves the server's name through the configured resolver" "$(tail -3 "$G17_DIR/resolved.log")"
fi

# g17_udp_say sends a payload and prints the tagged answer, or "no answer".
g17_udp_say() {
    python3 - "$1" "$2" <<'PY'
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(4)
s.sendto(sys.argv[2].encode(), ("127.0.0.1", int(sys.argv[1])))
try:
    print(s.recvfrom(65535)[0].decode())
except socket.timeout:
    print("no answer")
PY
}

cat > "$G17_DIR/udp-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G17_UDP_CONTROL_PORT
auth_token = "$TOKEN"
udp_packet_size = 8
EOF

cat > "$G17_DIR/udp-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G17_UDP_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "dgram"
type = "udp"
local_ip = "127.0.0.1"
local_port = $G17_UDP_LOCAL_PORT
remote_port = $G17_UDP_PUBLIC_PORT
EOF

"$SERVER" --config "$G17_DIR/udp-server.toml" > "$G17_DIR/udp-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G17_DIR/udp-client.toml" > "$G17_DIR/udp-client.log" 2>&1 &
PIDS+=($!)

# A udp proxy has no TCP listener to wait on, so readiness is the first small
# datagram coming back.
g17_udp_up=0
for _ in $(seq 1 120); do
    if [ "$(g17_udp_say "$G17_UDP_PUBLIC_PORT" abcd)" = "echo:abcd" ]; then
        g17_udp_up=1
        break
    fi
    sleep 0.5
done
check "$g17_udp_up" "1" "udp_packet_size relays a datagram within the cap"
check "$(g17_udp_say "$G17_UDP_PUBLIC_PORT" this-is-well-over-eight-bytes)" "no answer" \
    "udp_packet_size drops a datagram over the cap"

cat > "$G17_DIR/vhost-server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G17_VHOST_CONTROL_PORT
http_port = $G17_VHOST_HTTP_PORT
auth_token = "$TOKEN"
vhost_http_timeout = 1
EOF

cat > "$G17_DIR/vhost-client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G17_VHOST_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "slow"
type = "http"
domains = ["slow.test"]
local_ip = "127.0.0.1"
local_port = $G17_VHOST_LOCAL_PORT
EOF

"$SERVER" --config "$G17_DIR/vhost-server.toml" > "$G17_DIR/vhost-server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G17_DIR/vhost-client.toml" > "$G17_DIR/vhost-client.log" 2>&1 &
PIDS+=($!)
wait_for_log "$G17_DIR/vhost-client.log" 'server confirms' 120

# g17_vhost_probe prints "<status> fast|slow", so a 502 that arrives after the
# backend gave up ten seconds later cannot pass as the timeout working.
g17_vhost_probe() {
    python3 - "$1" <<'PY'
import sys, time, urllib.error, urllib.request

request = urllib.request.Request("http://127.0.0.1:%s/x" % sys.argv[1])
request.add_header("Host", "slow.test")
start = time.time()
try:
    answer = urllib.request.urlopen(request, timeout=15)
    code = answer.status
except urllib.error.HTTPError as exc:
    code = exc.code
except Exception as exc:
    code = "error %s" % exc
print("%s %s" % (code, "fast" if time.time() - start < 5 else "slow"))
PY
}
check "$(g17_vhost_probe "$G17_VHOST_HTTP_PORT")" "502 fast" \
    "vhost_http_timeout answers 502 instead of waiting for headers"


# ── frp parity, round eighteen: the heartbeat the server dictates, and the
# per-proxy annotations the dashboard shows. The first had unit tests only; the
# second was read by the dashboard and nothing else.

G18_DIR="$WORK/features18"
mkdir -p "$G18_DIR"
G18_CONTROL_PORT="$(free_port)"
G18_DASH_PORT="$(free_port)"
G18_PUBLIC_PORT="$(free_port)"

cat > "$G18_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G18_CONTROL_PORT
auth_token = "$TOKEN"
heartbeat_seconds = 2

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $G18_DASH_PORT
token = "dashboard-token"
EOF

cat > "$G18_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G18_CONTROL_PORT"
auth_token = "$TOKEN"
heartbeat_seconds = 9

[[proxies]]
name = "labelled"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G18_PUBLIC_PORT
annotations = { owner = "team-a", env = "prod" }
EOF

"$SERVER" --config "$G18_DIR/server.toml" > "$G18_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G18_DIR/client.toml" > "$G18_DIR/client.log" 2>&1 &
PIDS+=($!)

if wait_for_log "$G18_DIR/client.log" 'every 2s' 120; then
    ok "the server's heartbeat_seconds governs the session"
else
    bad "the server's heartbeat_seconds governs the session" "$(tail -3 "$G18_DIR/client.log")"
fi
if grep -q 'has no effect on this session' "$G18_DIR/client.log"; then
    ok "the client's differing heartbeat_seconds is reported as not applying"
else
    bad "the client's differing heartbeat_seconds is reported as not applying" "$(grep -F 'heartbeat' "$G18_DIR/client.log" | tail -2)"
fi
if wait_for_tcp "$G18_PUBLIC_PORT" 120; then
    check "$(tcp_via "$G18_PUBLIC_PORT")" "ok" "the session carries bytes under the server's heartbeat"
else
    bad "the session carries bytes under the server's heartbeat" "$(tail -3 "$G18_DIR/client.log")"
fi

# g18_annotations reads the labelled proxy's annotations back out of /api/proxies.
g18_annotations() {
    python3 - "$G18_DASH_PORT" <<'PY'
import json, sys, urllib.request

request = urllib.request.Request("http://127.0.0.1:%s/api/proxies" % sys.argv[1])
request.add_header("Authorization", "Bearer dashboard-token")
with urllib.request.urlopen(request, timeout=10) as answer:
    body = json.loads(answer.read())
rows = body if isinstance(body, list) else body.get("proxies", [])
for row in rows:
    if row.get("name") == "labelled":
        annotations = row.get("annotations") or {}
        print("%s=%s" % (annotations.get("owner", "-"), annotations.get("env", "-")))
        break
else:
    print("no row")
PY
}
check "$(g18_annotations)" "team-a=prod" "per-proxy annotations reach the dashboard API"

# Switches that point nowhere are named rather than silently doing nothing: a
# dashboard token with the dashboard switched off, and metrics enabled with no
# listener to answer it.
cat > "$G18_DIR/pointless-dashboard.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G18_CONTROL_PORT
auth_token = "$TOKEN"

[dashboard]
bind_addr = "127.0.0.1"
port = $G18_DASH_PORT
token = "dashboard-token"
EOF
if "$SERVER" --check -config "$G18_DIR/pointless-dashboard.toml" 2>&1 \
    | grep -q 'dashboard.token is set but dashboard.enabled is false'; then
    ok "a dashboard token with the dashboard switched off is warned about"
else
    bad "a dashboard token with the dashboard switched off is warned about" \
        "$("$SERVER" --check -config "$G18_DIR/pointless-dashboard.toml" 2>&1 | tail -3)"
fi

cat > "$G18_DIR/pointless-metrics.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G18_CONTROL_PORT
auth_token = "$TOKEN"

[metrics]
enabled = true
EOF
if "$SERVER" --check -config "$G18_DIR/pointless-metrics.toml" 2>&1 \
    | grep -q 'metrics.enabled is set but nothing serves the endpoint'; then
    ok "metrics enabled with no listener to answer it is warned about"
else
    bad "metrics enabled with no listener to answer it is warned about" \
        "$("$SERVER" --check -config "$G18_DIR/pointless-metrics.toml" 2>&1 | tail -3)"
fi


# ── frp parity, round nineteen: a configuration pasted from frp is told what to
# write instead of only being listed as unknown keys. frp's keys are camelCase and
# its older files keep the client's settings in [common], so a migration meets
# this message first.

G19_DIR="$WORK/features19"
mkdir -p "$G19_DIR"
cat > "$G19_DIR/frpc.toml" <<'EOF'
[common]
serverAddr = "example.com"
serverPort = 7000
token = "0123456789abcdef0123456789abcdef"
transport.tls.enable = true
heartbeatInterval = 30
log.maxDays = 3

[[proxies]]
name = "ssh"
type = "tcp"
localIP = "127.0.0.1"
localPort = 22
remotePort = 6000
customDomains = ["a.example.com"]
EOF

g19_check="$("$CLIENT" --check -config "$G19_DIR/frpc.toml" 2>&1)"
g19_ok=1
for want in serverAddr customDomains localIP transport.enable_tls \
    client.server_addr domains local_ip '[client] or [server]'; do
    printf '%s' "$g19_check" | grep -qF -- "$want" || g19_ok=0
done
check "$g19_ok" "1" "an frp configuration is told what to write instead of the frp names"

g19_strict="$("$CLIENT" --check --reject-unknown-keys -config "$G19_DIR/frpc.toml" 2>&1)"
if printf '%s' "$g19_strict" | grep -qF -- 'customDomains' \
    && printf '%s' "$g19_strict" | grep -qF -- 'domains'; then
    ok "the strict mode's error carries the same frp suggestions"
else
    bad "the strict mode's error carries the same frp suggestions" "$g19_strict"
fi


# ── frp parity, round twenty: client.client_id, the stable name the client sends
# in its auth request. The server uses it wherever it names the client instead of
# the session's random identifier.

G20_DIR="$WORK/features20"
mkdir -p "$G20_DIR"
G20_CONTROL_PORT="$(free_port)"
G20_DASH_PORT="$(free_port)"
G20_PUBLIC_PORT="$(free_port)"

cat > "$G20_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G20_CONTROL_PORT
auth_token = "$TOKEN"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $G20_DASH_PORT
token = "dashboard-token"
EOF

cat > "$G20_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G20_CONTROL_PORT"
auth_token = "$TOKEN"
client_id = "edge-01"

[[proxies]]
name = "named"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G20_PUBLIC_PORT
EOF

"$SERVER" --config "$G20_DIR/server.toml" > "$G20_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G20_DIR/client.toml" > "$G20_DIR/client.log" 2>&1 &
PIDS+=($!)

if wait_for_tcp "$G20_PUBLIC_PORT" 120; then
    ok "a client that names itself with client_id connects and publishes"
else
    bad "a client that names itself with client_id connects and publishes" "$(tail -3 "$G20_DIR/client.log")"
fi
if grep -qF 'client_id "edge-01"' "$G20_DIR/server.log"; then
    ok "the server's log names the client by its client_id"
else
    bad "the server's log names the client by its client_id" "$(grep -F 'connected as' "$G20_DIR/server.log" | tail -2)"
fi

# g20_names reads the client name the dashboard reports for the session and for
# the proxy's pool member, which are the two places a human reads it.
g20_names() {
    python3 - "$G20_DASH_PORT" <<'PY'
import json, sys, urllib.request

port = sys.argv[1]
def get(path):
    request = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path))
    request.add_header("Authorization", "Bearer dashboard-token")
    with urllib.request.urlopen(request, timeout=10) as answer:
        return json.loads(answer.read())

clients = get("/api/clients").get("clients", [])
session_name = clients[0].get("client_id", "-") if clients else "-"
proxies = get("/api/proxies").get("proxies", [])
member_name = "-"
for row in proxies:
    if row.get("name") == "named":
        members = row.get("members") or []
        if members:
            member_name = members[0].get("client_id", "-")
print("%s %s" % (session_name, member_name))
PY
}
check "$(g20_names)" "edge-01 edge-01" \
    "the dashboard and the pool member are named by client_id"


# ── frp parity, round twenty-one: feature_gates, the switch that turns one of
# this program's capabilities off. frp uses the key to opt into staged features;
# here the gated features ship enabled, so the gate forbids one.

G21_DIR="$WORK/features21"
mkdir -p "$G21_DIR"

cat > "$G21_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G20_CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "gated"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "s3cret"
auth_method = "snark"

[client.feature_gates]
Snark = false
EOF

g21_check="$("$CLIENT" --check -config "$G21_DIR/client.toml" 2>&1)"
if printf '%s' "$g21_check" | grep -qF 'feature_gates.Snark'; then
    ok "a client entry that needs a turned-off gate is refused at configuration time"
else
    bad "a client entry that needs a turned-off gate is refused at configuration time" "$g21_check"
fi

sed '/^\[client.feature_gates\]/,+2d' "$G21_DIR/client.toml" > "$G21_DIR/client-ungated.toml"
if "$CLIENT" --check -config "$G21_DIR/client-ungated.toml" >/dev/null 2>&1; then
    ok "the same entry is accepted once the gate is gone"
else
    bad "the same entry is accepted once the gate is gone" \
        "$("$CLIENT" --check -config "$G21_DIR/client-ungated.toml" 2>&1 | tail -3)"
fi


# ── frp parity, round twenty-two: server.max_pool_count, frp's maxPoolCount. The
# cap is on the parked connections a client may hold; a low value costs the extra
# offers a dial, never the visitor a stream.

G22_DIR="$WORK/features22"
mkdir -p "$G22_DIR"
G22_CONTROL_PORT="$(free_port)"
G22_PUBLIC_PORT="$(free_port)"

cat > "$G22_DIR/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $G22_CONTROL_PORT
auth_token = "$TOKEN"
max_pool_count = 1
EOF

cat > "$G22_DIR/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$G22_CONTROL_PORT"
auth_token = "$TOKEN"
pool_count = 3

[[proxies]]
name = "capped"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $G22_PUBLIC_PORT
EOF

"$SERVER" --config "$G22_DIR/server.toml" > "$G22_DIR/server.log" 2>&1 &
PIDS+=($!)
"$CLIENT" --config "$G22_DIR/client.toml" > "$G22_DIR/client.log" 2>&1 &
PIDS+=($!)

if wait_for_tcp "$G22_PUBLIC_PORT" 120; then
    check "$(tcp_via "$G22_PUBLIC_PORT")" "ok" \
        "a stream still works when the client's pool is larger than the server's cap"
else
    bad "a stream still works when the client's pool is larger than the server's cap" "$(tail -3 "$G22_DIR/client.log")"
fi

g22_refused=0
for _ in $(seq 1 120); do
    if grep -q 'pooled connection from.*refused' "$G22_DIR/server.log" 2>/dev/null; then
        g22_refused=1
        break
    fi
    sleep 0.25
done
check "$g22_refused" "1" "the offers beyond max_pool_count are refused by name"


echo "checks passed: $PASSES"
echo "checks failed: $FAILURES"
[ "$FAILURES" -eq 0 ] || exit 1
echo "ALL LINUX FUNCTIONAL CHECKS PASSED"
