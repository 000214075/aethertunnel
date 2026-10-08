#!/usr/bin/env bash
# Checks the published linux binaries for the behaviour that only a Linux kernel can
# show. It checks the artifacts published on the release page, so a release whose
# Linux binaries do not behave is red.
#
#   - both binaries match the SHA256SUMS published beside them
#   - both report the version that was released
#   - a server built from them starts, answers /healthz and reports a healthy audit log
#   - the audit log is recreated at the configured path after it is renamed away, which
#     Windows cannot show (a rename of an open file is refused there)
#   - audit.keep leaves exactly the generations it was asked for
#   - the server exits on SIGTERM and says so
#
# usage: verify-release-linux.sh <directory with the published binaries> <version>
set -u
DIR="${1:?a directory with the published binaries is required}"
VERSION="${2:?the released version is required}"
WORK="$DIR/verify-run"
TOKEN="release-verify-token-0123456789"
# A fixed port turns an unrelated listener on the machine into a red release; ask
# the kernel for free ones instead. What has been handed out is remembered in a
# file, because the kernel gives a just-released ephemeral port out again and each
# call runs in the command substitution's own subshell, where a shell variable
# would not survive to the next one.
free_port() {
    python3 - "$WORK/free-ports" <<'PY'
import socket, sys
claimed = set()
try:
    with open(sys.argv[1]) as ledger:
        claimed = {int(line) for line in ledger if line.strip()}
except OSError:
    pass
for _ in range(200):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    if port not in claimed:
        with open(sys.argv[1], "a") as ledger:
            ledger.write(str(port) + "\n")
        print(port)
        break
PY
}
PASSES=0
FAILURES=0
PIDS=()

ok() { PASSES=$((PASSES + 1)); echo "PASS  $1"; }
bad() { FAILURES=$((FAILURES + 1)); echo "FAIL  $1 -> $2"; }
check() { if [ "$1" = "$2" ]; then ok "$3"; else bad "$3" "want '$2' got '$1'"; fi; }
status_field() {
    curl -s -m 10 -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$DASHBOARD/api/status" |
        python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"
}
cleanup() {
    for pid in "${PIDS[@]:-}"; do kill -9 "$pid" 2>/dev/null || true; done
    # The run tree holds configs, logs and rotated audit generations; leaving it
    # behind next to the published binaries is litter on both exit paths.
    rm -rf "$WORK"
}
trap cleanup EXIT

SERVER="$DIR/aethertunnel-server-linux-amd64"
CLIENT="$DIR/aethertunnel-client-linux-amd64"
for name in aethertunnel-server-linux-amd64 aethertunnel-client-linux-amd64; do
    [ -x "$DIR/$name" ] || { bad "$name is executable" "not found or not executable"; exit 1; }
done
rm -rf "$WORK"
mkdir -p "$WORK"
# The three ports are claimed here, where the ledger they share can live in the run
# tree that was just created.
CONTROL="$(free_port)"
DASHBOARD="$(free_port)"
ROT_CONTROL="$(free_port)"
cd "$WORK" || exit 1

echo "== the published linux binaries"
for name in aethertunnel-server-linux-amd64 aethertunnel-client-linux-amd64; do
    if [ -f "$DIR/SHA256SUMS" ]; then
        # The sums name their entries as ./<file>, and a leading space is accepted
        # too so that a release built before build-release.ps1 wrote the prefix can
        # still be checked. The carriage return is dropped because a sums file
        # written on Windows ends its lines with CRLF, and the $ anchor would then
        # match nothing, reporting every artifact as missing from the file.
        want=$(tr -d '\r' < "$DIR/SHA256SUMS" | grep -E "(^|[ /])${name}\$" | head -n 1 | awk '{print $1}')
        got=$(sha256sum "$DIR/$name" | awk '{print $1}')
        if [ -z "$want" ]; then
            # Without this the check reports "want '' got <hash>", which reads like a
            # hash mismatch when the real fault is that the file lists no entry.
            bad "$name matches the published SHA256SUMS" "SHA256SUMS lists no entry for $name"
        else
            check "$got" "$want" "$name matches the published SHA256SUMS"
        fi
    else
        # A missing sums file must fail loudly: skipping the check silently is
        # exactly what a broken upload or a partial download looks like, and
        # the script would otherwise end in "ALL CHECKS PASSED" anyway.
        bad "$name matches the published SHA256SUMS" "SHA256SUMS is missing from $DIR"
    fi
    reported=$("$DIR/$name" --version)
    case "$reported" in
        *"$VERSION"*) ok "$name reports $VERSION" ;;
        *) bad "$name reports $VERSION" "$reported" ;;
    esac
done

echo "== the audit log of a running server"
cat > server.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $CONTROL
auth_token = "$TOKEN"
deny_cidrs = ["127.0.0.2/32"]

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $DASHBOARD
token = "$TOKEN"

[metrics]
enabled = true

[audit]
enabled = true
path = "$WORK/audit.jsonl"
EOF
"$SERVER" --config server.toml > server.log 2>&1 &
SERVER_PID=$!
PIDS+=("$SERVER_PID")
code=""
for _ in $(seq 1 60); do
    code=$(curl -s -m 2 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$DASHBOARD/healthz" || true)
    [ "$code" = "200" ] && break
    sleep 0.25
done
check "$code" "200" "the published server answers /healthz"

# One refused connection is one record, and the denial needs no second source address.
refuse() {
    python3 - "$1" <<'PY'
import socket, sys
s = socket.socket(); s.bind(('127.0.0.2', 0)); s.settimeout(2)
try: s.connect(('127.0.0.1', int(sys.argv[1])))
except Exception: pass
s.close()
PY
}
for _ in $(seq 1 5); do refuse "$CONTROL"; done
for _ in $(seq 1 40); do
    [ -f audit.jsonl ] && [ "$(wc -l < audit.jsonl)" -ge 1 ] && break
    sleep 0.25
done
check "$([ -s audit.jsonl ] && echo yes || echo no)" "yes" "the server writes its audit log"
check "$(status_field '["audit"]["writable"]')" "True" "it reports the log as writable"
check "$(status_field '["audit"]["records_lost"]')" "0" "no record was reported lost"

echo "== the log is rotated away underneath the server"
before=$(wc -l < audit.jsonl)
mv audit.jsonl audit.jsonl.rotated
for _ in $(seq 1 5); do refuse "$CONTROL"; done
for _ in $(seq 1 40); do
    [ -s audit.jsonl ] && break
    sleep 0.25
done
check "$([ -s audit.jsonl ] && echo yes || echo no)" "yes" "the server recreates the configured path"
check "$(status_field '["audit"]["writable"]')" "True" "it still reports the log as writable"
check "$(status_field '["audit"]["records_lost"]')" "0" "no record was lost across the rename"
after=$(wc -l < audit.jsonl.rotated 2>/dev/null || echo 0)
check "$([ "$after" -ge "$before" ] && [ "$after" -ge 1 ] && echo yes || echo no)" "yes" "the renamed file kept the records written before it was moved"

kill -TERM "$SERVER_PID" 2>/dev/null || true
for _ in $(seq 1 40); do
    kill -0 "$SERVER_PID" 2>/dev/null || break
    sleep 0.25
done
check "$(kill -0 "$SERVER_PID" 2>/dev/null && echo running || echo stopped)" "stopped" "the published server exits on SIGTERM"
sleep 0.3
check "$(grep -q 'shutting down' server.log && echo yes || echo no)" "yes" "it logged the stop"

echo "== audit.keep decides how many generations are kept"
ROT="$WORK/rotate"
mkdir -p "$ROT"
cat > rotate.toml <<EOF
[server]
bind_addr = "127.0.0.1"
bind_port = $ROT_CONTROL
auth_token = "$TOKEN"
deny_cidrs = ["127.0.0.2/32"]

[audit]
enabled = true
path = "$ROT/audit.jsonl"
max_bytes = 1024
keep = 2
EOF
"$SERVER" --config rotate.toml > rotate.log 2>&1 &
ROT_PID=$!
PIDS+=("$ROT_PID")
for _ in $(seq 1 40); do
    python3 - "$ROT_CONTROL" <<'PY' && break
import socket, sys
s = socket.socket(); s.settimeout(0.3)
sys.exit(0 if s.connect_ex(('127.0.0.1', int(sys.argv[1]))) == 0 else 1)
PY
    sleep 0.25
done
for _ in $(seq 1 60); do refuse "$ROT_CONTROL"; done
# Rotation renames audit.jsonl.1 to .2 and then audit.jsonl to .1, so .2 appears
# while .1 is briefly absent. Waiting for the state the checks assert — both
# generations present — is what makes the first check about .1 stable.
for _ in $(seq 1 40); do
    [ -f "$ROT/audit.jsonl.2" ] && [ -f "$ROT/audit.jsonl.1" ] && break
    sleep 0.25
done
# The last rotation may still be pruning the generation past audit.keep, and the
# check below would otherwise catch audit.jsonl.3 in that window.
sleep 0.5
check "$([ -f "$ROT/audit.jsonl.1" ] && echo yes || echo no)" "yes" "the log rotated once the limit was reached"
check "$([ -f "$ROT/audit.jsonl.2" ] && echo yes || echo no)" "yes" "a second generation was kept, as audit.keep asks"
check "$([ -f "$ROT/audit.jsonl.3" ] && echo yes || echo no)" "no" "no generation beyond audit.keep was left behind"
whole=yes
for file in "$ROT/audit.jsonl.1" "$ROT/audit.jsonl.2"; do
    [ -f "$file" ] || continue
    python3 - "$file" <<'PY' || whole=no
import json, sys
for line in open(sys.argv[1]):
    if line.strip(): json.loads(line)
PY
done
check "$whole" "yes" "every kept generation is whole records"
kill -TERM "$ROT_PID" 2>/dev/null || true

echo
echo "checks passed: $PASSES"
if [ "$FAILURES" -gt 0 ]; then
    echo "checks failed: $FAILURES"
    exit 1
fi
echo "ALL PUBLISHED LINUX CHECKS PASSED"
