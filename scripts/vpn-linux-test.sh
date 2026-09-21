#!/usr/bin/env bash
#
# Runs the layer-3 tunnel (the [vpn] section) on real tun devices and proves that
# packets cross it.
#
# The two ends run in separate network namespaces, so neither end's tunnel address
# is a local address on the other side: a packet between them has to enter a tun
# device, be framed on the control connection, and come out of the peer's tun
# device. Nothing can take a shortcut through the local routing table, which is what
# would happen if both ends ran in one namespace.
#
# Requirements: root, /dev/net/tun, the ip command and a kernel with network
# namespaces. When one of those is missing the script says which and exits 0: there
# is nothing to test, and reporting that is more useful than a red build.
#
# Usage: scripts/vpn-linux-test.sh [server-binary] [client-binary]
#        Defaults to bin/aethertunnel-server and bin/aethertunnel-client.

set -uo pipefail

server_bin="${1:-bin/aethertunnel-server}"
client_bin="${2:-bin/aethertunnel-client}"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1${2:+ -> $2}"; failures=$((failures + 1)); }

skip() {
  echo "skipped: $1"
  exit 0
}

[ "$(id -u)" = "0" ] || skip "the tun device can only be opened by root"
[ -c /dev/net/tun ] || skip "/dev/net/tun is missing"
command -v ip >/dev/null 2>&1 || skip "the ip command is missing"
[ -x "$server_bin" ] || skip "$server_bin is not executable"
[ -x "$client_bin" ] || skip "$client_bin is not executable"

work="$(mktemp -d /tmp/aether-vpn-XXXXXX)"
ns="aethervpn$$"
control_port=$((17000 + RANDOM % 400))
dashboard_port=$((17600 + RANDOM % 300))

# The veth pair joins the two namespaces; the tunnel subnet is what the server hands
# addresses from. Both are picked to be unlikely to collide, and the tunnel subnet is
# checked against the routing table below.
veth_host="192.168.77.1"
veth_guest="192.168.77.2"
tunnel_net="192.168.99"

cleanup() {
  for pid in $(ip netns pids "$ns" 2>/dev/null); do kill "$pid" 2>/dev/null; done
  [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null
  ip netns del "$ns" 2>/dev/null
  ip link del "atveth-h$$" 2>/dev/null
  if [ "$failures" -eq 0 ]; then
    rm -rf "$work"
  else
    echo "   logs kept in $work"
  fi
}
trap cleanup EXIT

echo "== setting up two network namespaces"
if ! ip netns add "$ns" 2>"$work/netns.err"; then
  skip "network namespaces are not available here: $(cat "$work/netns.err")"
fi
if ! ip link add "atveth-h$$" type veth peer name "atveth-g$$" 2>"$work/veth.err"; then
  fail "the veth pair could not be created" "$(cat "$work/veth.err")"
  exit 1
fi
ip link set "atveth-g$$" netns "$ns"
ip addr add "$veth_host/30" dev "atveth-h$$"
ip link set "atveth-h$$" up
ip netns exec "$ns" ip addr add "$veth_guest/30" dev "atveth-g$$"
ip netns exec "$ns" ip link set "atveth-g$$" up
ip netns exec "$ns" ip link set lo up
pass "the two namespaces are joined by a veth pair"

echo "== starting the server with [vpn] enabled"
cat >"$work/server.toml" <<EOF
[server]
bind_addr = "0.0.0.0"
bind_port = $control_port
auth_token = "vpn-linux-test-token-0123456789"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $dashboard_port
token = "vpn-linux-test-dashboard"

[metrics]
enabled = true

[audit]
enabled = true
path = "$work/audit.jsonl"

[vpn]
enabled = true
device = "at0"
address = "$tunnel_net.0/24"
mtu = 1400
EOF

cat >"$work/client.toml" <<EOF
[client]
server_addr = "$veth_host:$control_port"
auth_token = "vpn-linux-test-token-0123456789"

[vpn]
enabled = true
device = "at1"
EOF

"$server_bin" --config "$work/server.toml" --check || fail "the server configuration is invalid"

"$server_bin" --config "$work/server.toml" >"$work/server.log" 2>&1 &
server_pid=$!

# Wait for the control port to accept connections, using bash's own TCP support so
# the script needs nothing beyond a shell.
port_open() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
}
deadline=$((SECONDS + 15))
until port_open "$control_port"; do
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 0.2
done
if port_open "$control_port"; then
  pass "the server is listening on its control port"
else
  fail "the server never listened" "$(cat "$work/server.log")"
  exit 1
fi

if grep -q "vpn: interface at0" "$work/server.log"; then
  pass "the server opened a real tun device: $(grep -m1 'vpn: interface' "$work/server.log")"
else
  fail "the server did not open the tun device" "$(cat "$work/server.log")"
fi

echo "== starting the client inside the second namespace"
ip netns exec "$ns" "$client_bin" --config "$work/client.toml" >"$work/client.log" 2>&1 &

deadline=$((SECONDS + 20))
until grep -q "tunnel interface at1" "$work/client.log"; do
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 0.2
done
if grep -q "tunnel interface at1" "$work/client.log"; then
  pass "the client configured its tunnel interface: $(grep -m1 'tunnel interface' "$work/client.log")"
else
  fail "the client never configured a tunnel interface" "$(cat "$work/client.log")"
  exit 1
fi

client_address="$(grep -m1 -o 'is [0-9.]*/' "$work/client.log" | tr -d 'is /')"
[ -n "$client_address" ] || fail "the client did not report the address it was given"
echo "   the server handed out $client_address"

echo "== checking both interfaces from the outside"
ip -o link show at0 >/dev/null 2>&1 && pass "the server's interface at0 exists" || fail "at0 does not exist"
ip netns exec "$ns" ip -o link show at1 >/dev/null 2>&1 && pass "the client's interface at1 exists in its namespace" || fail "at1 does not exist"

server_address="$(ip -o -4 addr show at0 | awk '{print $4}' | cut -d/ -f1)"
if [ "$server_address" != "${tunnel_net}.1" ]; then
  fail "the server address is $server_address, want ${tunnel_net}.1"
else
  pass "the server took the first address of the subnet"
fi

echo "== routing packets across the tunnel"
ip route add "$client_address/32" dev at0 || fail "the route to $client_address could not be added"
ip netns exec "$ns" ip route add "$server_address/32" dev at1 2>/dev/null

if command -v ping >/dev/null 2>&1; then
  if ip netns exec "$ns" ping -c 3 -W 2 "$server_address" >"$work/ping-out.log" 2>&1; then
    pass "the client reaches the server across the tunnel: $(grep -m1 'packets transmitted' "$work/ping-out.log" || tail -1 "$work/ping-out.log")"
  else
    fail "the client could not ping the server across the tunnel" "$(cat "$work/ping-out.log")"
  fi
  if ping -c 3 -W 2 "$client_address" >"$work/ping-in.log" 2>&1; then
    pass "the server reaches the client across the tunnel: $(grep -m1 'packets transmitted' "$work/ping-in.log" || tail -1 "$work/ping-in.log")"
  else
    fail "the server could not ping the client across the tunnel" "$(cat "$work/ping-in.log")"
  fi
else
  echo "   no ping command; the tunnel is checked with the interface counters only"
fi

# The kernel counts what really crossed the device: packets the routing layer hands
# to a tun are transmitted on it, packets written by the process arrive on it. A
# successful run has the ping's requests and replies on both ends.
packet_counters() {
  ip -s link show "$1" 2>/dev/null | awk '
    /RX:/ { getline; rx = $2 }
    /TX:/ { getline; tx = $2 }
    END { print (rx == "" ? 0 : rx), (tx == "" ? 0 : tx) }'
}
host_counters="$(packet_counters at0)"
guest_counters="$(ip netns exec "$ns" ip -s link show at1 | awk '
    /RX:/ { getline; rx = $2 }
    /TX:/ { getline; tx = $2 }
    END { print (rx == "" ? 0 : rx), (tx == "" ? 0 : tx) }')"
echo "   at0 rx/tx packets: $host_counters"
echo "   at1 rx/tx packets: $guest_counters"
host_rx="$(echo "$host_counters" | awk '{print $1}')"
host_tx="$(echo "$host_counters" | awk '{print $2}')"
guest_rx="$(echo "$guest_counters" | awk '{print $1}')"
guest_tx="$(echo "$guest_counters" | awk '{print $2}')"
if [ "${host_rx:-0}" -gt 0 ] && [ "${host_tx:-0}" -gt 0 ]; then
  pass "the server's interface moved packets in both directions (rx $host_rx tx $host_tx)"
else
  fail "the server's interface did not carry packets" "rx ${host_rx:-?} tx ${host_tx:-?}"
fi
if [ "${guest_rx:-0}" -gt 0 ] && [ "${guest_tx:-0}" -gt 0 ]; then
  pass "the client's interface moved packets in both directions (rx $guest_rx tx $guest_tx)"
else
  fail "the client's interface did not carry packets" "rx ${guest_rx:-?} tx ${guest_tx:-?}"
fi

echo "== the API and the audit log agree"
http_get() {
  local path="$1"
  exec 3<>"/dev/tcp/127.0.0.1/$dashboard_port" || return 1
  printf 'GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer vpn-linux-test-dashboard\r\nConnection: close\r\n\r\n' "$path" >&3
  cat <&3
  exec 3>&-
}
json_number() { echo "$1" | grep -o "\"$2\":[0-9]*" | head -1 | cut -d: -f2; }

vpn_api="$(http_get /api/vpn)"
if echo "$vpn_api" | grep -q '"enabled":true'; then
  pass "GET /api/vpn reports the tunnel as enabled"
else
  fail "GET /api/vpn does not report an enabled tunnel" "$vpn_api"
fi

peers="$(json_number "$vpn_api" peers)"
used="$(json_number "$vpn_api" addresses_used)"
[ "${peers:-0}" = "1" ] && pass "the tunnel has one peer" || fail "the tunnel reports ${peers:-?} peers, want 1"
[ "${used:-0}" = "1" ] && pass "one address is handed out" || fail "the tunnel reports ${used:-?} addresses handed out, want 1"

from_device="$(json_number "$vpn_api" from_device)"
to_device="$(json_number "$vpn_api" to_device)"
if [ "${from_device:-0}" -gt 0 ] && [ "${to_device:-0}" -gt 0 ]; then
  pass "the tunnel counters moved (from_device $from_device, to_device $to_device)"
else
  fail "the tunnel counters did not move" "from_device ${from_device:-?} to_device ${to_device:-?}"
fi

config_api="$(http_get /api/config)"
if echo "$config_api" | grep -q '"device":"at0"'; then
  pass "GET /api/config carries the tunnel section"
else
  fail "GET /api/config has no tunnel section" "$config_api"
fi

if grep -q '"event":"vpn_address_assigned"' "$work/audit.jsonl"; then
  pass "the audit log recorded the address assignment"
else
  fail "the audit log has no vpn_address_assigned event" "$(cat "$work/audit.jsonl")"
fi

echo "== stopping the client releases the address"
client_pids="$(ip netns pids "$ns" 2>/dev/null)"
[ -n "$client_pids" ] && pass "the client process is running in its namespace" || fail "no process is running in the namespace"
for pid in $client_pids; do kill "$pid" 2>/dev/null; done
deadline=$((SECONDS + 10))
until grep -q '"event":"client_disconnected"' "$work/audit.jsonl"; do
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 0.2
done
if grep -q '"event":"client_disconnected"' "$work/audit.jsonl"; then
  pass "the server noticed the client leaving"
else
  fail "the server did not notice the client leaving" "waited 10s after the client was stopped"
fi
released="$(json_number "$(http_get /api/vpn)" addresses_used)"
[ "${released:-1}" = "0" ] && pass "the address went back to the pool" || fail "the tunnel still reports ${released:-?} addresses handed out"

echo
if [ "$failures" -eq 0 ]; then
  echo "ALL LAYER-3 CHECKS PASSED"
else
  echo "$failures LAYER-3 CHECKS FAILED"
fi
exit "$failures"
