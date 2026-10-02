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
# relies on). The two are separate on purpose: this one runs on Linux and on the
# arm64 runner in CI, that one on windows-latest. It does not cover the vpn section
# (scripts/vpn-linux-test.sh needs root for that) or the pool strategies.
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

# A port nobody is listening on. The window between closing this socket and the
# server binding it is small, and the alternative (fixed ports) collides with
# whatever else runs on the machine.
free_port() {
    python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
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
RANGE_BASE_PORT="$(free_port)"
STATIC_FILE_PORT="$(free_port)"
PROXY_PROTO_PORT="$(free_port)"
PROXY_PROTO_BACKEND_PORT="$(free_port)"
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
# reason.
published=""
for _ in $(seq 1 120); do
    published="$(api_field /api/proxies 'len(d["proxies"])' 2>/dev/null || echo 0)"
    [ "$published" = "12" ] && break
    sleep 0.25
done
check "$published" "18" "the owner published all eighteen proxies"

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
conn, _ = s.accept()
line = conn.makefile().readline().strip()
conn.sendall(("saw: " + line).encode())
conn.close()
PPY
PIDS+=($!)

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
    bad "a name lookup resolves a tcp proxy to the port it is published on" "$(tail -1 dht-public.err)"
fi
if [ -n "$dht_control" ]; then
    ok "a client-role lookup resolves a private name to the control port ($dht_control)"
else
    bad "a client-role lookup resolves a private name to the control port" "$(tail -1 dht-control.err)"
fi

# A name nobody announced has to fail rather than resolve to something else.
unknown_name="$("$CLIENT" --config dht-client.toml --discover no-such-proxy-anywhere 2>&1 | tail -1)"
case "$unknown_name" in
    *no-such-proxy-anywhere*) ok "an unknown name is reported as unresolved" ;;
    *) bad "an unknown name is reported as unresolved" "$unknown_name" ;;
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

if grep -q "proxy_removed" "$WORK/audit.jsonl" 2>/dev/null; then
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

cm_check="$(python3 - "$HERE/deploy/kubernetes/configmap.yaml" \
    "$WORK/cm-server.toml" "$WORK" "$CM_CONTROL" "$CM_DASHBOARD" "$CM_P2P" "$CM_DHT" <<'PY'
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
check "$(python3 - "$WORK/audit.jsonl" <<'PY'
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
        panel_bare=(--bare-url "http://127.0.0.1:$CAP_DASHBOARD" \
                    --bare-token "$DASHBOARD_TOKEN")
    else
        echo "SKIP  the off-state dashboard: the capacity server is not answering"
    fi

    panel_output="$(python3 "$HERE/scripts/panel-checks.py" \
        --url "http://127.0.0.1:$DASHBOARD_PORT" --token "$DASHBOARD_TOKEN" \
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
for _ in $(seq 1 120); do
    if grep -q 'outside allow_ports' "$ALLOWPORTS_DIR/server.log" 2>/dev/null \
        && grep -q 'outside allow_ports' "$ALLOWPORTS_DIR/client.log" 2>/dev/null \
        && grep -q '"inside"' "$ALLOWPORTS_DIR/client.log" 2>/dev/null; then
        ok_awaited=1
        break
    fi
    sleep 0.5
done
check "$ok_awaited" "1" "the server refused a remote port outside allow_ports and told the client"


echo "checks passed: $PASSES"
echo "checks failed: $FAILURES"
[ "$FAILURES" -eq 0 ] || exit 1
echo "ALL LINUX FUNCTIONAL CHECKS PASSED"
