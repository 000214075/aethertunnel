#!/usr/bin/env bash
#
# Runs the layer-3 tunnel ([vpn]) on real utun interfaces on this machine, and
# proves what one machine can prove: both ends open their interfaces, the addresses
# the server hands out land on them, the state reaches /api/vpn, and a client that
# must be on the tunnel is refused without it. A packet between two interfaces on
# ONE machine cannot be proven to have entered the kernel's tun - the local routing
# table shortcuts it - so the end-to-end packet path is proven on Linux by
# scripts/vpn-linux-test.sh, which shares every forwarding line with this build.
#
# Requirements: root (giving a utun interface an address needs it) and python3.
# Opening the utun control socket itself is unprivileged.
#
# Usage: sudo scripts/vpn-macos-test.sh [server-binary] [client-binary]

set -uo pipefail

server_bin="${1:-bin/aethertunnel-server}"
client_bin="${2:-bin/aethertunnel-client}"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1${2:+ -> $2}"; failures=$((failures + 1)); }
skip() { echo "skipped: $1"; exit 0; }

[ "$(uname)" = "Darwin" ] || skip "this script runs on macOS"
[ "$(id -u)" = "0" ] || skip "configuring a utun address needs root"
command -v python3 >/dev/null 2>&1 || skip "python3 is missing"
[ -x "$server_bin" ] || skip "$server_bin is not executable"
[ -x "$client_bin" ] || skip "$client_bin is not executable"

work="$(mktemp -d /tmp/aether-vpn-XXXXXX)"
control_port=$((17000 + RANDOM % 400))
dashboard_port=$((17600 + RANDOM % 300))
net="10.63.9"
server_device="utun8"
client_device="utun9"

cleanup() {
  [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null
  [ -n "${client_pid:-}" ] && kill "$client_pid" 2>/dev/null
  [ -n "${novpn_pid:-}" ] && kill "$novpn_pid" 2>/dev/null
  if [ "$failures" -eq 0 ]; then
    rm -rf "$work"
  else
    echo "   logs kept in $work"
  fi
}
trap cleanup EXIT

cat >"$work/server.toml" <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $control_port
auth_token = "vpn-macos-test-token-0123456789"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $dashboard_port
token = "vpn-macos-test-dashboard"

[vpn]
enabled = true
device = "$server_device"
address = "$net.0/24"
require = true
EOF

cat >"$work/client.toml" <<EOF
[client]
server_addr = "127.0.0.1:$control_port"
auth_token = "vpn-macos-test-token-0123456789"

[vpn]
enabled = true
device = "$client_device"
EOF

cat >"$work/novpn.toml" <<EOF
[client]
server_addr = "127.0.0.1:$control_port"
auth_token = "vpn-macos-test-token-0123456789"
EOF

port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; }

"$server_bin" --config "$work/server.toml" >"$work/server.log" 2>&1 &
server_pid=$!

deadline=$((SECONDS + 20))
until port_open "$control_port"; do
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 0.2
done
if port_open "$control_port"; then
  pass "the server is listening on its control port"
else
  fail "the server never listened" "$(tail -3 "$work/server.log")"
  exit 1
fi

if grep -q "vpn: interface $server_device" "$work/server.log"; then
  pass "the server opened a real utun interface"
else
  fail "the server did not open the utun interface" "$(tail -3 "$work/server.log")"
fi

"$client_bin" --config "$work/client.toml" >"$work/client.log" 2>&1 &
client_pid=$!

deadline=$((SECONDS + 25))
until grep -q "tunnel interface $client_device" "$work/client.log"; do
  [ "$SECONDS" -lt "$deadline" ] || break
  sleep 0.2
done
if grep -q "tunnel interface $client_device" "$work/client.log"; then
  pass "the client opened its interface and took the address the server handed out"
else
  fail "the client never configured a tunnel interface" "$(tail -3 "$work/client.log")"
  exit 1
fi

client_address="$(grep -m1 -o "is [0-9.]*/" "$work/client.log" | tr -d "is /")"
if [ "$client_address" = "$net.2" ]; then
  pass "the client's interface has $net.2"
else
  fail "the client's interface address is '$client_address', want $net.2"
fi
server_address="$(ifconfig "$server_device" 2>/dev/null | awk '/inet /{print $2}')"
if [ "$server_address" = "$net.1" ]; then
  pass "the server's interface has $net.1"
else
  fail "the server's interface address is '$server_address', want $net.1"
fi

sleep 1
api_ok="no"
deadline=$((SECONDS + 15))
while [ "$SECONDS" -lt "$deadline" ]; do
  if curl -s -H "Authorization: Bearer vpn-macos-test-dashboard" \
      "http://127.0.0.1:$dashboard_port/api/vpn" | grep -q "$net.2"; then
    api_ok="yes"
    break
  fi
  sleep 1
done
if [ "$api_ok" = "yes" ]; then
  pass "the dashboard's /api/vpn reports the client's tunnel address"
else
  fail "/api/vpn does not report the client's tunnel address" \
    "$(curl -s -H 'Authorization: Bearer vpn-macos-test-dashboard' "http://127.0.0.1:$dashboard_port/api/vpn" | head -c 400)"
fi

"$client_bin" --config "$work/novpn.toml" >"$work/novpn.log" 2>&1 &
novpn_pid=$!
sleep 3
if grep -qE "refus|require" "$work/novpn.log" "$work/server.log" 2>/dev/null; then
  pass "a client that is not on the tunnel is refused (vpn.require)"
else
  fail "a client that is not on the tunnel was not refused" "$(tail -3 "$work/novpn.log")"
fi

if [ "$failures" -eq 0 ]; then
  echo "ALL MACOS VPN CHECKS PASSED"
  exit 0
fi
echo "checks failed: $failures   logs kept in $work"
exit 1
