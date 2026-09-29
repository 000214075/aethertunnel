#!/usr/bin/env bash
# Runs this checkout's tests on linux/arm64 where no arm64 machine is available: Go
# cross-compiles everything, and qemu-aarch64 executes it. This is how the arm64
# column in docs/PLATFORMS.md is produced locally, and it is the same two things the
# CI job on ubuntu-24.04-arm does, on emulated instructions instead of real ones.
#
# The binaries Go produces here are static, so qemu needs no sysroot for them; pass
# one only if your qemu was built to look up a dynamic loader.
#
# usage: emulate-linux-arm64.sh <qemu-aarch64> [sysroot]
#   qemu-aarch64  a qemu-user aarch64 binary (package qemu-user-static, or the
#                 qemu-aarch64 file inside it)
#   sysroot       optional -L directory for qemu
#
# example:
#   scripts/emulate-linux-arm64.sh /usr/bin/qemu-aarch64-static
set -u

QEMU="${1:?the qemu-aarch64 binary is required}"
SYSROOT="${2:-}"
[ -x "$QEMU" ] || { echo "not executable: $QEMU" >&2; exit 2; }

HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE" || exit 2

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "== $("$QEMU" --version | head -1)"

# One wrapper per binary, so the launchers themselves are what the test suite
# starts: it has no idea an emulator is involved.
wrapper() {
    target="$1"
    path="$WORK/w-$(basename "$target")"
    if [ -n "$SYSROOT" ]; then
        printf '#!/bin/sh\nexec %s -L %s %s "$@"\n' "$QEMU" "$SYSROOT" "$target" > "$path"
    else
        printf '#!/bin/sh\nexec %s %s "$@"\n' "$QEMU" "$target" > "$path"
    fi
    chmod +x "$path"
    printf '%s' "$path"
}

# go test -exec runs "<command> <test binary> <flags>", so this one passes its
# arguments straight through rather than naming a binary of its own.
execWrapper="$WORK/w-exec"
if [ -n "$SYSROOT" ]; then
    printf '#!/bin/sh\nexec %s -L %s "$@"\n' "$QEMU" "$SYSROOT" > "$execWrapper"
else
    printf '#!/bin/sh\nexec %s "$@"\n' "$QEMU" > "$execWrapper"
fi
chmod +x "$execWrapper"

echo
echo "== cross-compiling for linux/arm64"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$WORK/server" . || exit 1
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$WORK/client" ./client || exit 1
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$WORK/smoketest" ./scripts/smoketest || exit 1

echo
echo "== the unit suite, executed under qemu"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -exec "$execWrapper" ./... -count=1 -timeout 900s
unit=$?
[ "$unit" -eq 0 ] || { echo "the arm64 unit suite failed" >&2; exit 1; }

echo
echo "== the functional suite, executed under qemu"
bash scripts/functional-linux.sh \
    "$(wrapper "$WORK/server")" "$(wrapper "$WORK/client")" "$(wrapper "$WORK/smoketest")"
functional=$?
[ "$functional" -eq 0 ] || { echo "the arm64 functional suite failed" >&2; exit 1; }

echo
echo "ALL ARM64 CHECKS PASSED (emulated)"
