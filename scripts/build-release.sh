#!/usr/bin/env bash
# Cross-compiles the release matrix into dist/ and writes SHA256SUMS.
#
# Builds only what this version actually supports: a server and a client binary
# per platform. The v1 script claimed fourteen platforms (and failed on all of
# them because it built a "./server" package that does not exist).
#
# Usage:  ./scripts/build-release.sh [version]

set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="dist"

# linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64
PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

rm -rf "$OUT"
mkdir -p "$OUT"

echo "AetherTunnel $VERSION ($COMMIT, $DATE)"
echo

for platform in "${PLATFORMS[@]}"; do
  GOOS="${platform%%/*}"
  GOARCH="${platform##*/}"
  suffix=""
  if [ "$GOOS" = "windows" ]; then
    suffix=".exe"
  fi

  for target in server client; do
    if [ "$target" = "server" ]; then
      pkg="."
    else
      pkg="./client"
    fi

    name="aethertunnel-${target}-${GOOS}-${GOARCH}${suffix}"
    printf '  %-44s' "$name"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.buildTime=$DATE -X main.gitCommit=$COMMIT" \
      -o "$OUT/$name" "$pkg"
    echo "ok"
  done
done

echo
echo "checksums:"
(
  cd "$OUT"
  sha256sum ./* > SHA256SUMS
  cat SHA256SUMS
)

echo
echo "artifacts in $OUT/:"
ls -1 "$OUT" | wc -l
