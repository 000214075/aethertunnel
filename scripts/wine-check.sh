#!/usr/bin/env bash
# Runs the program's own checks against the windows/amd64 binaries on a Linux host,
# through Wine. It is the Windows counterpart of scripts/functional-linux.sh: the same
# kind of checks (the control port, a published proxy and bytes through it, a private
# proxy with its visitor, the socks5 exit and its allow list, the audit trail, the
# metrics, the ledger, every security layer at once, and the ban window) driven against
# real PE files instead of ELF ones.
#
# Why it exists: the Windows numbers in docs/PLATFORMS.md came from runs of a script
# that was never in the repository, so the only way to refresh them was to rebuild that
# script. This one is in the repository and takes the two PE files as arguments.
#
# Each side may be any of three kinds, decided per argument:
#
#   *.exe            windows/amd64, run through Wine
#   qemu-arm64:<path> linux/arm64, run through qemu-aarch64 (it is a Linux ELF, so the
#                    paths in its configuration stay Unix ones)
#   anything else    executed directly, so linux/amd64
#
# That covers the cross-system pairs docs/PLATFORMS.md 2.1 lists, from two arguments.
# QEMU_AARCH64 names the qemu binary and QEMU_SYSROOT the optional -L directory; a pair
# with no qemu side needs neither. A pair of two native binaries is the same 55 checks
# with two ELF files, which is how to tell a platform difference from a script bug.
#
# The arm64 side is a cross-compiled copy of the same two programs, which is all it takes
# to make one:
#
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/at-arm64/aethertunnel-server .
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/at-arm64/aethertunnel-client ./client
#   wine-check.sh 'qemu-arm64:/tmp/at-arm64/aethertunnel-server' bin/aethertunnel-client.exe
#
# usage: wine-check.sh <server> <client> [wine binary]
#
# Wine is not configured here beyond what the environment already says: WINEPREFIX,
# WINEDLLPATH and LD_LIBRARY_PATH come from the caller, and the wine binary comes from
# the third argument or from PATH. A root-free way to assemble that tree (packages
# unpacked with dpkg-deb, no install) is recorded in docs/PLATFORMS.md 3.3. Two things
# about it are worth knowing before running this:
#
#   * wineserver loads its locale data from a compiled-in /usr/share/wine/nls. That path
#     has to exist and hold the nls files from the wine packages. Inside a mount
#     namespace it can be bind mounted there (`unshare -m` plus `mount --bind`), which
#     needs root: an unprivileged user namespace is refused on a host with
#     kernel.apparmor_restrict_unprivileged_userns = 1, which is where the Wine runs in
#     3.1 and 3.9 stopped. With root available, a symlink is enough.
#   * the Windows binaries take Windows paths. Wine maps Z: to /, so a file the script
#     created at /tmp/x/audit.jsonl is Z:\tmp\x\audit.jsonl to them, and it has to be
#     written that way in the configuration too — as a TOML *literal* string, because
#     'Z:\...' in a normal string is an invalid escape (the lesson recorded in
#     docs/PLATFORMS.md 3.6). The native binary on the other side of a mixed pair keeps
#     the Unix path, which is why every path in this script goes through spath() or
#     cpath() rather than a single winpath().
set -u

SERVER="${1:?the server PE is required}"
CLIENT="${2:?the client PE is required}"
WINE="${3:-${WINE_BIN:-wine}}"
WINESERVER=""

TOKEN="wine-check-token-0123456789abcdef"
DASHBOARD_TOKEN="wine-check-dashboard-0123456789"
METRICS_TOKEN="wine-check-metrics-0123456789"
STCP_SECRET="wine-check-stcp-secret-0123456789"
PASSPHRASE="wine-check-passphrase"
SALT="wine-check-salt"

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
    # Wine keeps a wineserver per prefix; asking it to exit also takes down any
    # Windows process this script started that outlived its launcher.
    [ -n "$WINESERVER" ] && "$WINESERVER" -k >/dev/null 2>&1 || true
    rm -rf "${WORK:-/nonexistent}"
}
trap cleanup EXIT

abspath() {
    case "$1" in
        /*) printf '%s' "$1" ;;
        *) printf '%s/%s' "$(pwd)" "$1" ;;
    esac
}

# The path the Windows binaries see for a file this script created. Wine maps the Unix
# root to Z:, and Windows accepts backslashes in every path.
winpath() {
    printf 'Z:%s' "$(printf '%s' "$1" | tr '/' '\\')"
}

# The path each side understands for a file this script created. Both forms are needed
# in a mixed pair: the Windows side cannot open a Unix path, and the native side must not
# be handed "Z:\..." (Wine's Z: means nothing to an ELF binary).
spath() {
    if [ "$SERVER_IS_WIN" = yes ]; then winpath "$1"; else printf '%s' "$1"; fi
}
cpath() {
    if [ "$CLIENT_IS_WIN" = yes ]; then winpath "$1"; else printf '%s' "$1"; fi
}

# qemu_run is a function of its own so the sysroot arguments stay in one place: -L is
# how qemu finds the aarch64 libraries, and the binaries here are static, so it is only
# needed when qemu was built to look up a dynamic loader.
qemu_run() {
    local binary="$1"
    shift
    if [ -n "$QEMU_SYSROOT" ]; then
        "$QEMU_AARCH64" -L "$QEMU_SYSROOT" "$binary" "$@"
    else
        "$QEMU_AARCH64" "$binary" "$@"
    fi
}
server_run() {
    case "$SERVER_KIND" in
        win) "$WINE" "$SERVER" "$@" ;;
        qemu) qemu_run "$SERVER" "$@" ;;
        *) "$SERVER" "$@" ;;
    esac
}
client_run() {
    case "$CLIENT_KIND" in
        win) "$WINE" "$CLIENT" "$@" ;;
        qemu) qemu_run "$CLIENT" "$@" ;;
        *) "$CLIENT" "$@" ;;
    esac
}

# Each argument is one of three kinds; a qemu one carries its prefix into this parse and
# the prefix is stripped so the rest of the script sees a plain path.
strip_qemu() {
    case "$1" in
        qemu-arm64:*) printf '%s' "${1#qemu-arm64:}" ;;
        *) printf '%s' "$1" ;;
    esac
}
SIDE_KIND() {
    case "$1" in
        qemu-arm64:*) printf 'qemu' ;;
        *.exe) printf 'win' ;;
        *) printf 'native' ;;
    esac
}

SERVER_KIND="$(SIDE_KIND "$SERVER")"
CLIENT_KIND="$(SIDE_KIND "$CLIENT")"
SERVER="$(abspath "$(strip_qemu "$SERVER")")"
CLIENT="$(abspath "$(strip_qemu "$CLIENT")")"
SERVER_IS_WIN=no
CLIENT_IS_WIN=no
[ "$SERVER_KIND" = win ] && SERVER_IS_WIN=yes
[ "$CLIENT_KIND" = win ] && CLIENT_IS_WIN=yes

if [ "$SERVER_KIND" = qemu ] || [ "$CLIENT_KIND" = qemu ]; then
    QEMU_AARCH64="${QEMU_AARCH64:-$(command -v qemu-aarch64 2>/dev/null || true)}"
    [ -n "$QEMU_AARCH64" ] && [ -x "$QEMU_AARCH64" ] ||
        { echo "a qemu side needs QEMU_AARCH64=<qemu-aarch64 binary>" >&2; exit 2; }
    QEMU_SYSROOT="${QEMU_SYSROOT:-}"
fi

if [ "$SERVER_IS_WIN" = yes ] || [ "$CLIENT_IS_WIN" = yes ]; then
    if ! command -v "$WINE" >/dev/null 2>&1 && [ ! -x "$WINE" ]; then
        echo "cannot run $WINE: not executable and not in PATH" >&2
        exit 2
    fi
    WINESERVER="$(dirname "$(command -v "$WINE" 2>/dev/null || printf '%s' "$WINE")")/wineserver"
    [ -x "$WINESERVER" ] || WINESERVER="$(command -v wineserver 2>/dev/null || true)"
fi

for binary in "$SERVER" "$CLIENT"; do
    [ -f "$binary" ] || { echo "not a file: $binary" >&2; exit 2; }
done

WORK="$(mktemp -d)"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$WORK" || exit 2

if [ "$SERVER_IS_WIN" = yes ] || [ "$CLIENT_IS_WIN" = yes ]; then
    echo "wine:   $("$WINE" --version 2>&1 | head -1)"
    echo "prefix: ${WINEPREFIX:-$HOME/.wine}"
fi
kind_name() {
    case "$1" in
        win) printf 'windows/amd64 (wine)' ;;
        qemu) printf 'linux/arm64 (qemu-aarch64)' ;;
        *) printf 'linux/amd64 (native)' ;;
    esac
}
echo "server: $SERVER ($(kind_name "$SERVER_KIND"))"
echo "client: $CLIENT ($(kind_name "$CLIENT_KIND"))"
echo "work:   $WORK"
echo

# --- helpers -----------------------------------------------------------------

free_port() {
    python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

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

# wait_for_log <file> <needle> <tries>: succeeds once the file contains the needle.
wait_for_log() {
    for _ in $(seq 1 "$3"); do
        if grep -qF "$2" "$1" 2>/dev/null; then return 0; fi
        sleep 0.25
    done
    return 1
}

http_status() {
    python3 - "$1" "${2:-}" <<'PY'
import sys, urllib.error, urllib.request
url, token = sys.argv[1], sys.argv[2]
req = urllib.request.Request(url)
if token:
    req.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(resp.status)
except urllib.error.HTTPError as exc:
    print(exc.code)
except Exception as exc:  # noqa: BLE001
    print("error: %s" % exc)
PY
}

http_body() {
    python3 - "$1" "${2:-}" <<'PY'
import sys, urllib.error, urllib.request
url, token = sys.argv[1], sys.argv[2]
req = urllib.request.Request(url)
if token:
    req.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        sys.stdout.write(resp.read().decode("utf-8", "replace"))
except Exception as exc:  # noqa: BLE001
    sys.stdout.write("error: %s" % exc)
PY
}

metric_at() {
    http_body "http://127.0.0.1:$1/metrics" "$2" |
        awk -v name="$3" '$1 == name { print $2; found = 1 } END { if (!found) print "" }'
}

# wait_metric <port> <token> <name> <want> [tries]: polls until the series reads want,
# and prints whatever it read last either way. A fixed number of polls rather than a
# bare read: a ban takes three failed handshakes and a window of seconds to show up.
wait_metric() {
    tries="${5:-40}"
    value=""
    for _ in $(seq 1 "$tries"); do
        value="$(metric_at "$1" "$2" "$3")"
        if [ "${value:-0}" = "$4" ] 2>/dev/null; then printf '%s' "$value"; return 0; fi
        sleep 0.25
    done
    printf '%s' "${value:-}"
}

api_field_at() {
    python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, sys, urllib.request
port, token, path, expr = sys.argv[1:5]
req = urllib.request.Request("http://127.0.0.1:%s%s" % (port, path))
req.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = json.loads(resp.read())
except Exception as exc:  # noqa: BLE001
    print("error: %s" % exc)
    sys.exit(0)
print(eval(expr, {"d": body}))
PY
}

# through <port> <payload> <want>: connects to a proxy endpoint, sends the payload and
# reports ok when the answer starts with what the tunnel carried back.
through() {
    python3 - "$1" "$2" "$3" <<'PY'
import socket, sys, time
port, payload, want = int(sys.argv[1]), sys.argv[2].encode(), sys.argv[3]
deadline = time.time() + 30
last = ""
while time.time() < deadline:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=5) as sock:
            sock.sendall(payload)
            last = sock.recv(4096).decode("utf-8", "replace")
            if last.startswith(want):
                print("ok")
                sys.exit(0)
    except OSError as exc:
        last = str(exc)
    time.sleep(0.5)
print("no answer: %s" % last)
PY
}

start_server() {
    server_run --config "$(spath "$1")" > "$2" 2>&1 &
    PIDS+=($!)
}

start_client() {
    client_run --config "$(cpath "$1")" > "$2" 2>&1 &
    PIDS+=($!)
}

# --- the local services the tunnels point at ---------------------------------

HELPER="${HELPER:-}"
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
    python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]].split(":")[1])' \
        "$WORK/services.json" "$1"
}
TCP_ECHO_PORT="$(service_port tcp)"
UDP_ECHO_PORT="$(service_port udp)"
HTTP_ECHO_PORT="$(service_port http)"
HALFCLOSE_PORT="$(service_port halfclose)"

"$HELPER" -cert-dir "$WORK" -cert-hosts "127.0.0.1,localhost" > /dev/null 2>&1
[ -s "$WORK/server.crt" ] || { echo "the helper wrote no certificate" >&2; exit 2; }

# --- the command line --------------------------------------------------------

echo "== the command line"

server_version="$(server_run --version 2>&1)"
case "$server_version" in
    *"protocol 4"*) ok "the server reports its version: $server_version" ;;
    *) bad "the server reports its version" "$server_version" ;;
esac
client_version="$(client_run --version 2>&1)"
case "$client_version" in
    *"protocol 4"*) ok "the client reports its version: $client_version" ;;
    *) bad "the client reports its version" "$client_version" ;;
esac

# The shipped examples are read back as a configuration by the server and the client,
# which is where a path or an escape only one platform accepts shows up.
if server_run --config "$(spath "$HERE/server.toml.example")" --check > check-server.log 2>&1; then
    ok "the shipped server example validates on the server side"
else
    bad "the shipped server example validates on the server side" "$(tail -1 check-server.log)"
fi
if client_run --config "$(cpath "$HERE/client.toml.example")" --check > check-client.log 2>&1; then
    ok "the shipped client example validates on the client side"
else
    bad "the shipped client example validates on the client side" "$(tail -1 check-client.log)"
fi

# A configuration error has to be reported, not swallowed: the frame padding limit is a
# value the program knows about.
cat > bad.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = 1
auth_token = "$TOKEN"

[obfuscation]
enabled = true
pad_to = 100000000
EOF
if server_run --config "$(spath "$WORK/bad.toml")" --check > bad-check.log 2>&1; then
    bad "a value out of range is rejected" "$(tail -1 bad-check.log)"
else
    case "$(cat bad-check.log)" in
        *pad_to*) ok "a value out of range is rejected, naming the key" ;;
        *) bad "a value out of range is rejected, naming the key" "$(tail -1 bad-check.log)" ;;
    esac
fi

# --- the control port, the dashboard, the metrics ----------------------------

echo
echo "== control port, dashboard, metrics"

CONTROL_PORT="$(free_port)"
DASHBOARD_PORT="$(free_port)"
PROXY_PORT="$(free_port)"
HTTP_PORT="$(free_port)"
DHT_PORT="$(free_port)"

cat > server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $CONTROL_PORT
auth_token = "$TOKEN"
http_port = $HTTP_PORT

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
path = '$(spath "$WORK/audit.jsonl")'
max_bytes = 1048576

# A name in this directory resolves to the endpoint it is published on: a tcp proxy
# to its public port, a private proxy to the control port.
[dht]
enabled = true
listen_addr = "127.0.0.1:$DHT_PORT"
advertise_host = "127.0.0.1"
signing_key_file = '$(spath "$WORK/dht.key")'
EOF

start_server "$WORK/server.toml" "$WORK/server.log"
if wait_for_tcp "$DASHBOARD_PORT" 60; then
    ok "the server started and opened its dashboard port"
else
    bad "the server started and opened its dashboard port" "$(tail -2 "$WORK/server.log")"
fi

check "$(http_status "http://127.0.0.1:$DASHBOARD_PORT/healthz")" "200" \
    "the server answers /healthz"
check "$(http_status "http://127.0.0.1:$DASHBOARD_PORT/api/status")" "401" \
    "/api/status needs the token in this pair"
check "$(http_status "http://127.0.0.1:$DASHBOARD_PORT/api/status" "$DASHBOARD_TOKEN")" "200" \
    "/api/status answers with the token"

series="$(http_body "http://127.0.0.1:$DASHBOARD_PORT/metrics" "$METRICS_TOKEN")"
missing=""
for name in aethertunnel_control_connections_total aethertunnel_streams_total \
            aethertunnel_bytes_from_clients_total aethertunnel_dashboard_unauthorized_total; do
    case "$series" in
        *"$name"*) ;;
        *) missing="$missing $name" ;;
    esac
done
if [ -z "$missing" ]; then
    ok "/metrics under this pair carries the documented series"
else
    bad "/metrics under this pair carries the documented series" "missing:$missing"
fi

# The counter added for refusals on the dashboard listener: the same probe the Linux
# suite makes, so the two platforms are known to agree on it.
refused_before="$(metric_at "$DASHBOARD_PORT" "$METRICS_TOKEN" aethertunnel_dashboard_unauthorized_total)"
http_status "http://127.0.0.1:$DASHBOARD_PORT/api/status" "wrong-token" > /dev/null
refused_after="$(metric_at "$DASHBOARD_PORT" "$METRICS_TOKEN" aethertunnel_dashboard_unauthorized_total)"
if [ -n "$refused_before" ] && [ "$refused_after" = "$((refused_before + 1))" ]; then
    ok "a refused request is counted ($refused_before -> $refused_after)"
else
    bad "a refused request is counted" "$refused_before -> $refused_after"
fi

echo
echo "== a published proxy and bytes through it"

# One client publishes both: the tcp proxy the byte checks go through, and the http
# proxy the shared listener routes to by Host header.
cat > client.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "web"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $PROXY_PORT

[[proxies]]
name = "site"
type = "http"
local_ip = "127.0.0.1"
local_port = $HTTP_ECHO_PORT
domains = ["web.wine-check.test"]
EOF

start_client "$WORK/client.toml" "$WORK/client.log"
if wait_for_log "$WORK/client.log" "as session" 60; then
    ok "the client connected and logged its session"
else
    bad "the client connected and logged its session" "$(tail -2 "$WORK/client.log")"
fi

connected="$(wait_metric "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total 1 60)"
check "$connected" "1" "the server counted the client's control connection"

check "$(through "$PROXY_PORT" "wine-windows" "wine-windows")" "ok" \
    "bytes moved client -> local service and back"

streams="$(wait_metric "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" aethertunnel_streams_total 1 60)"
check "$streams" "1" "the server counted the stream it carried"

# Both directions, because a relay that copies one way and not the other still moves
# what the check above looks at.
bytes_in="$(metric_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" aethertunnel_bytes_from_clients_total)"
bytes_out="$(metric_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" aethertunnel_bytes_to_clients_total)"
if [ "${bytes_in:-0}" -ge 1 ] 2>/dev/null && [ "${bytes_out:-0}" -ge 1 ] 2>/dev/null; then
    ok "the byte counters moved in both directions (in=$bytes_in out=$bytes_out)"
else
    bad "the byte counters moved in both directions" "in=$bytes_in out=$bytes_out"
fi

audit_events="$(cat "$WORK/audit.jsonl" 2>/dev/null)"
case "$audit_events" in
    *control_accepted*) ok "the audit log recorded the client's connection" ;;
    *) bad "the audit log recorded the client's connection" "events: $(wc -l < "$WORK/audit.jsonl" 2>/dev/null)" ;;
esac
case "$audit_events" in
    *proxy_registered*) ok "the audit log recorded the proxy it published" ;;
    *) bad "the audit log recorded the proxy it published" "no proxy_registered" ;;
esac

http_result="$(python3 - "$HTTP_PORT" <<'CHECK'
import sys, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "web.wine-check.test")
try:
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
except Exception as exc:  # noqa: BLE001
    print("no answer: %s" % exc)
else:
    print("ok" if "smoketest-http" in body else "got %r" % body)
CHECK
)"
check "$http_result" "ok" "an http proxy is reachable on the shared port through its domain"

unknown_host="$(python3 - "$HTTP_PORT" <<'CHECK'
import sys, urllib.error, urllib.request
req = urllib.request.Request("http://127.0.0.1:%s/" % sys.argv[1])
req.add_header("Host", "no-policy-for-this-name.wine-check.test")
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read().decode()
except urllib.error.HTTPError as exc:
    print("refused" if exc.code >= 400 else "served %d" % exc.code)
except Exception:  # noqa: BLE001
    # A reset or a closed connection is a refusal too.
    print("refused")
else:
    print("served" if "smoketest-http" in body else "unknown answer")
CHECK
)"
check "$unknown_host" "refused" "a host with no policy behind the shared port is refused"

check "$(api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" /api/dht 'd["enabled"]')" \
    "True" "/api/dht reports the directory is on"
check "$(api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" /api/dht 'len(d["node_id"])')" \
    "40" "the node identifier is a 160-bit hex id"
check "$(api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" /api/dht 'd["namespace"]')" \
    "aethertunnel" "the key namespace is the default one"
check "$(api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" /api/dht 'd["addr"]')" \
    "127.0.0.1:$DHT_PORT" "the directory reports the address it bound"
check "$(api_field_at "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" /api/dht '"web" in d["announced"] and "site" in d["announced"]')" \
    "True" "both of the client's proxies are announced in the directory"

# --- a half-close on its way through -----------------------------------------

echo
echo "== a half-close"

HALFCLOSE_PROXY_PORT="$(free_port)"

# The helper's service answers only after its peer half-closes, so the answer is what
# shows the relay did not turn the half-close into a full close. A Windows socket has no
# close-write of its own — it is a shutdown call on the connection — which makes this the
# platform where that distinction is easiest to get wrong.
cat > halfclose.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "half-close"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $HALFCLOSE_PORT
remote_port = $HALFCLOSE_PROXY_PORT
EOF

start_client "$WORK/halfclose.toml" "$WORK/halfclose.log"
if wait_for_log "$WORK/halfclose.log" "half-close" 60; then
    ok "the client published the half-close proxy"
else
    bad "the client published the half-close proxy" "$(tail -2 "$WORK/halfclose.log")"
fi

halfclose_result="$(python3 - "$HALFCLOSE_PROXY_PORT" <<'PY'
import socket, sys
try:
    sock = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
except OSError as exc:
    print("no connection: %s" % exc)
    sys.exit(0)
sock.settimeout(15)
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
check "$halfclose_result" "ok" "a half-close reaches the local service and its answer comes back"

# --- a private proxy with its visitor ----------------------------------------

echo
echo "== a private proxy and its visitor"

STCP_PROXY_PORT="$(free_port)"
STCP_VISITOR_PORT="$(free_port)"

cat > stcp-publisher.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "db"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
secret_key = "$STCP_SECRET"
EOF

cat > stcp-visitor.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[visitors]]
name = "db"
type = "stcp"
server_name = "db"
bind_addr = "127.0.0.1"
bind_port = $STCP_VISITOR_PORT
secret_key = "$STCP_SECRET"
EOF

start_client "$WORK/stcp-publisher.toml" "$WORK/stcp-publisher.log"
if wait_for_log "$WORK/stcp-publisher.log" "db" 60; then
    ok "the publisher registered a private proxy"
else
    bad "the publisher registered a private proxy" "$(tail -2 "$WORK/stcp-publisher.log")"
fi

start_client "$WORK/stcp-visitor.toml" "$WORK/stcp-visitor.log"
if wait_for_tcp "$STCP_VISITOR_PORT" 60; then
    ok "the visitor bound the port it was told to"
else
    bad "the visitor bound the port it was told to" "$(tail -2 "$WORK/stcp-visitor.log")"
fi

check "$(through "$STCP_VISITOR_PORT" "wine-stcp" "wine-stcp")" "ok" \
    "bytes moved through the private tunnel to the visitor"
if grep -q visitor_accepted "$WORK/audit.jsonl" 2>/dev/null; then
    ok "the server recorded the visitor in the audit log"
else
    bad "the server recorded the visitor in the audit log" "no visitor_accepted"
fi

# --- the socks5 exit and its allow list --------------------------------------

echo
echo "== the socks5 exit and its allow list"

SOCKS_PORT="$(free_port)"

# A socks5 tunnel has no local service to name, so it carries neither local_ip nor
# local_port; the server publishes it on remote_port and allow_targets is what keeps it
# from becoming an exit for everything the machine can reach.
cat > socks.toml <<EOF
[client]
server_addr = "127.0.0.1:$CONTROL_PORT"
auth_token = "$TOKEN"

[[proxies]]
name = "exit"
type = "socks5"
remote_port = $SOCKS_PORT
allow_targets = ["127.0.0.1/32"]
EOF

start_client "$WORK/socks.toml" "$WORK/socks.log"
started=no
for _ in $(seq 1 60); do
    if grep -q "exit" "$WORK/socks.log" 2>/dev/null; then started=yes; break; fi
    sleep 0.25
done
if [ "$started" = yes ] && wait_for_tcp "$SOCKS_PORT" 60; then
    ok "the socks5 exit came up and the server published its port"
else
    bad "the socks5 exit came up and the server published its port" \
        "$(tail -2 "$WORK/socks.log")"
fi

socks_allowed="$(python3 - "$SOCKS_PORT" "$TCP_ECHO_PORT" <<'PY'
import socket, struct, sys

def open_target(port, host, target_port):
    sock = socket.create_connection(("127.0.0.1", int(port)), timeout=15)
    sock.settimeout(15)
    sock.sendall(b"\x05\x01\x00")
    reply = sock.recv(2)
    if len(reply) != 2 or reply[1] != 0:
        return None, "greeting %r" % reply
    sock.sendall(b"\x05\x01\x00\x01" + socket.inet_aton(host) + struct.pack(">H", int(target_port)))
    head = sock.recv(4)
    if len(head) < 4 or head[1] != 0:
        return None, "reply %r" % head
    if head[3] == 1:
        sock.recv(6)
    elif head[3] == 4:
        sock.recv(18)
    elif head[3] == 3:
        sock.recv(1 + sock.recv(1)[0] + 2)
    return sock, None

sock, err = open_target(sys.argv[1], "127.0.0.1", sys.argv[2])
if sock is None:
    print("refused: %s" % err)
else:
    payload = b"wine-socks-through-the-exit"
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
check "$socks_allowed" "ok" "the socks5 exit reaches a target inside allow_targets under this pair"

socks_denied="$(python3 - "$SOCKS_PORT" <<'PY'
import socket, struct, sys
sock = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=15)
sock.settimeout(10)
sock.sendall(b"\x05\x01\x00")
if sock.recv(2)[1] != 0:
    print("refused")
    sys.exit()
# 127.0.0.2 is outside the configured 127.0.0.1/32.
sock.sendall(b"\x05\x01\x00\x01" + socket.inet_aton("127.0.0.2") + struct.pack(">H", 9))
print("refused" if sock.recv(4)[1] != 0 else "allowed")
PY
)"
check "$socks_denied" "refused" "the socks5 exit refuses a target outside allow_targets under this pair"

socks_requests="$(wait_metric "$DASHBOARD_PORT" "$DASHBOARD_TOKEN" aethertunnel_socks5_requests_total 1 40)"
check "$socks_requests" "1" "the socks5 request that was served is counted"

# --- every security layer at once --------------------------------------------

echo
echo "== encryption, post-quantum, TLS, identity and disguise together"

SEC_CONTROL="$(free_port)"
SEC_DASHBOARD="$(free_port)"
SEC_PROXY="$(free_port)"

# The identity key is created on first use, so this both proves the flag works on
# Windows and produces the public half the server will allow.
cat > sec-identity.toml <<EOF
[client]
server_addr = "127.0.0.1:1"
auth_token = "$TOKEN"

[identity]
enabled = true
key_file = '$(cpath "$WORK/owner-identity.key")'
EOF
SEC_IDENTITY="$(client_run --identity --config "$(cpath "$WORK/sec-identity.toml")" 2>&1 | tail -1)"
if [ -s "$WORK/owner-identity.key" ] && printf '%s' "$SEC_IDENTITY" | grep -qE '^[0-9a-f]{64}$'; then
    ok "the client created an identity key and printed its public half"
else
    bad "the client created an identity key and printed its public half" "$SEC_IDENTITY"
fi

# The key file is the identity's memory: a second start with the same file has to read
# it back and report the same public half, or every restart would look like a new client.
SEC_IDENTITY_AGAIN="$(client_run --identity --config "$(cpath "$WORK/sec-identity.toml")" 2>&1 | tail -1)"
check "$SEC_IDENTITY_AGAIN" "$SEC_IDENTITY" \
    "starting again with the same identity file reports the same public key"

cat > sec-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $SEC_CONTROL
auth_token = "$TOKEN"

[encryption]
enabled = true
algorithm = "xchacha20-poly1305"
passphrase = "$PASSPHRASE"
salt = "$SALT"
post_quantum = true

[transport]
enable_tls = true
cert_file = '$(spath "$WORK/server.crt")'
key_file = '$(spath "$WORK/server.key")'

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

[metrics]
enabled = true
EOF

cat > sec-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$SEC_CONTROL"
auth_token = "$TOKEN"

[transport]
enable_tls = true
ca_file = '$(cpath "$WORK/server.crt")'

[encryption]
enabled = true
algorithm = "xchacha20-poly1305"
passphrase = "$PASSPHRASE"
salt = "$SALT"
post_quantum = true

[obfuscation]
enabled = true
pad_to = 256
disguise = "tls-record"

[identity]
enabled = true
key_file = '$(cpath "$WORK/owner-identity.key")'

[[proxies]]
name = "secure-tcp"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $SEC_PROXY
EOF

start_server "$WORK/sec-server.toml" "$WORK/sec-server.log"
if wait_for_tcp "$SEC_DASHBOARD" 60; then
    ok "the server started with every security layer on"
else
    bad "the server started with every security layer on" "$(tail -2 "$WORK/sec-server.log")"
fi
start_client "$WORK/sec-client.toml" "$WORK/sec-client.log"
if wait_for_log "$WORK/sec-client.log" "as session" 60; then
    ok "the client connected through TLS, identity and disguise"
else
    bad "the client connected through TLS, identity and disguise" \
        "$(tail -2 "$WORK/sec-client.log")"
fi
check "$(through "$SEC_PROXY" "wine-secure" "wine-secure")" "ok" \
    "a transfer survives encryption, post-quantum, TLS, identity and disguise"

# Each layer announces itself and the lines differ per layer, so this catches a layer
# that stopped being applied rather than one that stopped working. These are the checks
# the Windows column of docs/PLATFORMS.md never had.
for layer in "post-quantum:post-quantum" "tls:wrapped in TLS" "identity:require_identity=true" \
             "disguise:connection disguise"; do
    label="${layer%%:*}"
    needle="${layer#*:}"
    if grep -qF "$needle" "$WORK/sec-server.log" "$WORK/sec-client.log" 2>/dev/null; then
        ok "the $label layer announced itself in the logs"
    else
        bad "the $label layer announced itself in the logs" "no line containing '$needle'"
    fi
done

# --- the ban window ----------------------------------------------------------

echo
echo "== a banned source, and the same source after the window"

BAN_CONTROL="$(free_port)"
BAN_DASHBOARD="$(free_port)"

cat > ban-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $BAN_CONTROL
auth_token = "$TOKEN"
ban_after_failures = 3
# Longer than the 10 seconds scripts/functional-linux.sh uses: a Windows process
# takes a few seconds to start under Wine, and the window has to still be open when
# the client that is meant to be refused gets there.
ban_seconds = 25

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $BAN_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = '$(spath "$WORK/ban-audit.jsonl")'
EOF

# The failing client retries every second, which is what accumulates three failures
# inside the check. Nothing about it but its token is wrong.
cat > ban-bad.toml <<EOF
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
remote_port = $(free_port)
EOF

start_server "$WORK/ban-server.toml" "$WORK/ban-server.log"
if wait_for_tcp "$BAN_DASHBOARD" 60; then
    ok "the server with ban_after_failures came up under this pair"
else
    bad "the server with ban_after_failures came up under this pair" "$(tail -2 "$WORK/ban-server.log")"
fi

start_client "$WORK/ban-bad.toml" "$WORK/ban-bad.log"
bans="$(wait_metric "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_sources_banned_total 1 80)"
check "$bans" "1" "a source that fails the token three times is banned under this pair"

# The client that is already banned: the connection is refused before the handshake, so
# nothing in its log says it got a session.
cat > ban-good.toml <<EOF
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
remote_port = $(free_port)
EOF

start_client "$WORK/ban-good.toml" "$WORK/ban-good.log"
sleep 3
# The bad client retries every second for as long as the window stays open, so the
# count is at least one rather than exactly one.
refused="$(wait_metric "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_banned_connections_refused_total 1 40)"
if [ "${refused:-0}" -ge 1 ] 2>/dev/null; then
    ok "the refusals of the banned source are counted under this pair ($refused)"
else
    bad "the refusals of the banned source are counted under this pair" "$refused"
fi
check "$(grep -c 'as session' "$WORK/ban-good.log" 2>/dev/null || true)" "0" \
    "the client refused by the ban logged no session"

# A ban is a window rather than a removal: once ban_seconds have passed the same client
# gets in, which is what shows the refusal above was the ban and not a client that could
# never connect. This and the check above are the two that were never run under this pair.
session_after="$(wait_metric "$BAN_DASHBOARD" "$DASHBOARD_TOKEN" aethertunnel_control_connections_total 1 120)"
check "$session_after" "1" "the source is let in again when the ban expires under this pair"
if [ "$(grep -c 'as session' "$WORK/ban-good.log" 2>/dev/null || true)" -ge 1 ]; then
    ok "the client that waited out the ban established its session"
else
    bad "the client that waited out the ban established its session" \
        "$(tail -2 "$WORK/ban-good.log")"
fi

# --- the bandwidth ledger ----------------------------------------------------

echo
echo "== the bandwidth ledger"

LEDGER_CONTROL="$(free_port)"
LEDGER_DASHBOARD="$(free_port)"
LEDGER_PROXY="$(free_port)"

cat > ledger-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $LEDGER_CONTROL
auth_token = "$TOKEN"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $LEDGER_DASHBOARD
token = "$DASHBOARD_TOKEN"

[metrics]
enabled = true

[ledger]
enabled = true
path = '$(spath "$WORK/ledger.jsonl")'
signing_key_file = '$(spath "$WORK/ledger.key")'
EOF

cat > ledger-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$LEDGER_CONTROL"
auth_token = "$TOKEN"

[[proxies]]
name = "metered"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $TCP_ECHO_PORT
remote_port = $LEDGER_PROXY
EOF

start_server "$WORK/ledger-server.toml" "$WORK/ledger-server.log"
if wait_for_tcp "$LEDGER_DASHBOARD" 60; then
    check "$(api_field_at "$LEDGER_DASHBOARD" "$DASHBOARD_TOKEN" /api/ledger 'd.get("enabled", False)')" \
        "True" "the server reports the ledger is on"
else
    bad "the server reports the ledger is on" "$(tail -2 "$WORK/ledger-server.log")"
fi

start_client "$WORK/ledger-client.toml" "$WORK/ledger-client.log"
if wait_for_log "$WORK/ledger-client.log" "as session" 60; then
    ok "the metered client connected under this pair"
else
    bad "the metered client connected under this pair" "$(tail -2 "$WORK/ledger-client.log")"
fi
check "$(through "$LEDGER_PROXY" "wine-ledger" "wine-ledger")" "ok" \
    "the metered tunnel carried its payload"

# The entry is appended when the session ends, and the way to end one from outside is
# the dashboard's disconnect action — which is also where the dashboard_action audit
# record comes from.
client_id="$(api_field_at "$LEDGER_DASHBOARD" "$DASHBOARD_TOKEN" /api/clients 'd["clients"][0]["id"]')"
disconnected="$(python3 - "$LEDGER_DASHBOARD" "$DASHBOARD_TOKEN" "$client_id" <<'PY'
import sys, urllib.request
port, token, client_id = sys.argv[1:4]
req = urllib.request.Request("http://127.0.0.1:%s/api/clients/%s" % (port, client_id), method="DELETE")
req.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(resp.status)
except Exception as exc:  # noqa: BLE001
    print("error: %s" % exc)
PY
)"
check "$disconnected" "200" "the dashboard can disconnect a client"

ledger_count=""
for _ in $(seq 1 60); do
    ledger_count="$(api_field_at "$LEDGER_DASHBOARD" "$DASHBOARD_TOKEN" /api/ledger 'd.get("count", 0)')"
    [ "${ledger_count:-0}" -ge 1 ] 2>/dev/null && break
    sleep 0.5
done
if [ "${ledger_count:-0}" -ge 1 ] 2>/dev/null; then
    ok "the ledger gained an entry when the session ended ($ledger_count)"
else
    bad "the ledger gained an entry when the session ended" "count=$ledger_count"
fi

if [ -s "$WORK/ledger.jsonl" ]; then
    ok "the ledger file is on disk where the server was told to write it"
    entry_proxy="$(python3 -c 'import json,sys;print(json.loads(open(sys.argv[1]).readline())["proxy"])' "$WORK/ledger.jsonl")"
    check "$entry_proxy" "metered" "the entry names the proxy the client published"
    entry_bytes="$(python3 -c 'import json,sys;e=json.loads(open(sys.argv[1]).readline());print(e["bytes_in"]+e["bytes_out"])' "$WORK/ledger.jsonl")"
    if [ "${entry_bytes:-0}" -ge 1 ] 2>/dev/null; then
        ok "the entry carries the bytes the server relayed ($entry_bytes)"
    else
        bad "the entry carries the bytes the server relayed" "sum=$entry_bytes"
    fi
else
    bad "the ledger file is on disk where the server was told to write it" "no $WORK/ledger.jsonl"
fi

# Offline verification with the public half only, run by the Windows binary itself.
ledger_key="$(api_field_at "$LEDGER_DASHBOARD" "$DASHBOARD_TOKEN" /api/ledger 'd.get("public_key", "")')"
verify="$(server_run --verify-ledger "$(spath "$WORK/ledger.jsonl")" \
    --ledger-key "$ledger_key" 2>&1)"
case "$verify" in
    *"entries verified"*) ok "the server verifies its own ledger offline: $(printf '%s' "$verify" | head -1)" ;;
    *) bad "the server verifies its own ledger offline" "$verify" ;;
esac

# A key of the right shape must not verify this chain, or the command above would pass
# whatever it was given.
wrong_key="$(python3 - "$ledger_key" <<'PY'
import sys
key = sys.argv[1]
print(("1" if key[0] != "1" else "2") + key[1:])
PY
)"
wrong_verify="$(server_run --verify-ledger "$(spath "$WORK/ledger.jsonl")" \
    --ledger-key "$wrong_key" 2>&1)"
case "$wrong_verify" in
    *"ledger verification failed"*) ok "a different key of the same shape does not verify the chain under this pair" ;;
    *) bad "a different key of the same shape does not verify the chain under this pair" "$wrong_verify" ;;
esac

# The proof goes to stdout as JSONL and its summary to stderr, so the two have to be
# kept apart: dropping the summary into the proof makes the file unparsable, and the
# verify below would fail on the tool's own log line rather than on a bad proof.
proof_rc="$(server_run --ledger-proof "$(spath "$WORK/ledger.jsonl")" --proof-index 0 \
    > "$WORK/proof.jsonl" 2> "$WORK/proof.err"; printf '%s' "$?")"
if [ "$proof_rc" = "0" ] && [ -s "$WORK/proof.jsonl" ]; then
    ok "the server exports a one-entry proof"
    if grep -q "proof of 1 entries" "$WORK/proof.err" 2>/dev/null; then
        ok "and says how much it wrote, on stderr where the proof is not"
    else
        bad "and says how much it wrote, on stderr where the proof is not" "$(tail -1 "$WORK/proof.err")"
    fi
    proof_verify="$(server_run --verify-ledger "$(spath "$WORK/proof.jsonl")" \
        --ledger-key "$ledger_key" 2>&1)"
    case "$proof_verify" in
        *"entries verified"*) ok "a one-entry proof verifies with the public key alone" ;;
        *) bad "a one-entry proof verifies with the public key alone" "$proof_verify" ;;
    esac
else
    bad "the server exports a one-entry proof" "exit=$proof_rc $(tail -1 "$WORK/proof.err" 2>/dev/null)"
fi

echo
echo "== a control port that is already taken"

TAKEN_PORT="$(free_port)"

# Hold the port so the server has something real to fail on, the way a second instance
# or an unrelated process would.
python3 -c 'import socket, time, sys
sock = socket.socket()
sock.bind(("127.0.0.1", int(sys.argv[1])))
sock.listen(1)
time.sleep(25)' "$TAKEN_PORT" &
PIDS+=($!)
sleep 1

cat > taken.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $TAKEN_PORT
auth_token = "$TOKEN"
EOF

taken_out="$(server_run --config "$(spath "$WORK/taken.toml")" 2>&1)"
taken_rc=$?
check "$taken_rc" "1" "a control port that is already taken fails with its own exit code"
check "$(printf '%s\n' "$taken_out" | grep -c .)" "1" "and it says so in one line"

# net's Windows text for this failure is a number ("winapi error #10048"); the message
# has to name the reason instead, and has to name it the same way on both platforms.
case "$taken_out" in
    *"the address is already in use (another process is listening on it)"*)
        ok "the message names the reason instead of a Windows error code" ;;
    *)
        bad "the message names the reason instead of a Windows error code" "$taken_out" ;;
esac

echo
echo "== the audit log rotates when it outgrows max_bytes"

ROT_CONTROL="$(free_port)"
ROT_DASHBOARD="$(free_port)"

cat > rot-server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $ROT_CONTROL
auth_token = "$TOKEN"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $ROT_DASHBOARD
token = "$DASHBOARD_TOKEN"

[audit]
enabled = true
path = '$(spath "$WORK/rot-audit.jsonl")'
max_bytes = 700
EOF

start_server "$WORK/rot-server.toml" "$WORK/rot-server.log"
if wait_for_tcp "$ROT_DASHBOARD" 60; then
    ok "the server with a 700-byte audit file came up"
else
    bad "the server with a 700-byte audit file came up" "$(tail -2 "$WORK/rot-server.log")"
fi

# A client for this server in particular: one that pointed at the section above would
# fill that server's audit file and leave this one at zero bytes.
cat > rot-client.toml <<EOF
[client]
server_addr = "127.0.0.1:$ROT_CONTROL"
auth_token = "$TOKEN"
reconnect_seconds = 1
max_reconnect_seconds = 1
EOF

# Each control connection writes records at both ends; five of them overshoot 700 bytes
# and have to push the file into its first generation.
for round in 1 2 3 4 5; do
    start_client "$WORK/rot-client.toml" "$WORK/rot-client-$round.log"
    wait_for_log "$WORK/rot-client-$round.log" "as session" 60 >/dev/null || true
    kill "${PIDS[-1]}" 2>/dev/null || true
    sleep 1
done
sleep 2

if [ -s "$WORK/rot-audit.jsonl.1" ]; then
    ok "the audit log rotated into a numbered generation"
else
    bad "the audit log rotated into a numbered generation" \
        "no $WORK/rot-audit.jsonl.1 (rot-audit.jsonl is $(wc -c < "$WORK/rot-audit.jsonl" 2>/dev/null) bytes)"
fi
if [ -s "$WORK/rot-audit.jsonl" ]; then
    ok "the current audit file is being written after the rotation"
else
    bad "the current audit file is being written after the rotation" "empty or missing"
fi

echo
echo "== --reject-unknown-keys"

cat > unknown.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $(free_port)
auth_token = "$TOKEN"
bananas = true
EOF

if server_run --config "$(spath "$WORK/unknown.toml")" --check > unknown-warn.log 2>&1; then
    case "$(cat unknown-warn.log)" in
        *bananas*) ok "an unknown key is reported by --check and the file still validates" ;;
        *) bad "an unknown key is reported by --check" "$(tail -1 unknown-warn.log)" ;;
    esac
else
    bad "an unknown key is reported by --check and the file still validates" \
        "$(tail -1 unknown-warn.log)"
fi

if server_run --config "$(spath "$WORK/unknown.toml")" --check --reject-unknown-keys \
    > unknown-strict.log 2>&1; then
    bad "--reject-unknown-keys turns the unknown key into an error" "$(tail -1 unknown-strict.log)"
else
    case "$(cat unknown-strict.log)" in
        *bananas*) ok "--reject-unknown-keys turns the unknown key into an error, naming it" ;;
        *) bad "--reject-unknown-keys turns the unknown key into an error, naming it" \
            "$(tail -1 unknown-strict.log)" ;;
    esac
fi

echo
echo "== the report"

echo
echo "checks passed: $PASSES"
echo "checks failed: $FAILURES"
if [ "$FAILURES" -ne 0 ]; then
    exit 1
fi
echo "ALL CHECKS PASSED"
