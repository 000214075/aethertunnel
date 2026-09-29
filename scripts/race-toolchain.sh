#!/usr/bin/env bash
#
# Print a C compiler path, so that `go test -race` can run on a machine with no
# compiler installed and no permission to install one.
#
#   CC="$(scripts/race-toolchain.sh)" CGO_ENABLED=1 go test -race ./...
#   CC="$(scripts/race-toolchain.sh)" CGO_ENABLED=1 go test -race -count=3 ./pkg/server/
#
# Why this exists: the race detector needs cgo, cgo needs a C compiler, and on this
# project's development machine there is neither a compiler nor root. Everything the
# compiler needs is in .deb packages, and `dpkg-deb -x` unpacks them without root, so
# the toolchain is unpacked into a cache directory and driven from there.
#
# `gcc-14` on its own is not enough. That package holds the driver only; the compiler
# proper (`cc1`) is in `gcc-14-x86-64-linux-gnu`, and the driver looks for it under
# /usr/libexec. The wrapper written here therefore passes -B for the unpacked cc1,
# collect2, as and ld, and -isystem/-L for the unpacked libc headers and startup
# objects, while leaving the dynamic linker path alone so the test binary still runs
# against the system libc.
#
# Nothing is installed and nothing outside the cache directory is written. The
# download is about 43 MB and happens once; later runs reuse the cache.
#
# Usage: scripts/race-toolchain.sh [prefix]
#   prefix  where to unpack (default: $XDG_CACHE_HOME/aethertunnel-race-toolchain,
#           or ~/.cache/aethertunnel-race-toolchain)
#
# Output: the compiler path on stdout, and progress on stderr. Exit non-zero if the
# toolchain could not be assembled.

set -euo pipefail

# The packages that together make one working C compiler out of nothing:
#   gcc-14                       the driver (`gcc-14`)
#   gcc-14-x86-64-linux-gnu      cc1, collect2, lto-wrapper
#   cpp-14-x86-64-linux-gnu      the preprocessor's own binary
#   gcc-14-base                  the version metadata the driver reads
#   libgcc-14-dev                the driver's internal headers and crtbegin/crtend
#   binutils                     as and ld
#   libc6-dev, linux-libc-dev    the C library and kernel headers cgo includes
PACKAGES=(
    gcc-14
    gcc-14-x86-64-linux-gnu
    cpp-14-x86-64-linux-gnu
    gcc-14-base
    libgcc-14-dev
    binutils
    libc6-dev
    linux-libc-dev
)

prefix="${1:-${XDG_CACHE_HOME:-$HOME/.cache}/aethertunnel-race-toolchain}"
wrapper="$prefix/cc"

# A compiler that is already installed is the better answer: no download, and it is
# what CI uses.
if command -v cc >/dev/null 2>&1; then
    echo cc
    exit 0
fi

# The cache is considered complete when the wrapper and the compiler it drives are
# both there. The wrapper is only written after it has compiled a test program, so a
# half-finished run cannot leave a wrapper that looks usable.
ccc1_glob=("$prefix"/usr/libexec/gcc/*/*/cc1)
if [ -x "$wrapper" ] && [ -e "${ccc1_glob[0]}" ]; then
    echo "$wrapper"
    exit 0
fi

for tool in apt-get dpkg-deb; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "race-toolchain: $tool is needed to unpack a compiler" >&2
        exit 2
    }
done

echo "race-toolchain: unpacking a C compiler into $prefix" >&2
mkdir -p "$prefix/debs"
(
    cd "$prefix/debs"
    # apt-get download needs no root and writes into the current directory. Its
    # progress goes to stderr: stdout carries the compiler path and nothing else.
    if ! apt-get download "${PACKAGES[@]}" >&2; then
        echo "race-toolchain: apt-get could not fetch the packages; this step needs a" >&2
        echo "  working package source, and a compiler that is already installed is" >&2
        echo "  what CI uses if you would rather not do this." >&2
        exit 2
    fi
    for deb in ./*.deb; do
        dpkg-deb -x "$deb" "$prefix"
    done
    # The archives are only the source of the unpacked tree, which is what the cache
    # is for; keeping them would hold another 43 MB for no reason.
    rm -f ./*.deb
)

driver="$prefix/usr/bin/gcc-14"
[ -x "$driver" ] || {
    echo "race-toolchain: $driver is missing after unpacking" >&2
    exit 2
}
# Derive the include and library directories from what was unpacked rather than
# hardcoding the triple, so this keeps working on another architecture. Each glob is
# expected to match once; the first match is used and a missing one is caught by the
# compile check below.
crtbegin=("$prefix"/usr/lib/gcc/*/*/crtbegin.o)
libc_so=("$prefix"/usr/lib/*/libc.so)
gcc_lib_dir="$(dirname "${crtbegin[0]}")"
libexec_dir="$(dirname "${ccc1_glob[0]}")"
multiarch_dir="$(dirname "${libc_so[0]}")"
multiarch="$(basename "$multiarch_dir")"

cat >"$wrapper" <<EOF
#!/bin/sh
# Written by scripts/race-toolchain.sh. Unpacked toolchain, no root involved.
exec "$driver" \\
  -B"$libexec_dir/" \\
  -B"$gcc_lib_dir/" \\
  -B"$prefix/usr/bin/" \\
  -isystem "$prefix/usr/include" \\
  -isystem "$prefix/usr/include/$multiarch" \\
  -L"$multiarch_dir" \\
  "\$@"
EOF
chmod +x "$wrapper"

# Prove it before handing it out: a wrapper that cannot compile would otherwise show
# up much later, as a cgo build failure that looks like a project problem.
check_dir="$(mktemp -d)"
trap 'rm -rf "$check_dir"' EXIT
printf '#include <stdio.h>\nint main(void){return 0;}\n' >"$check_dir/t.c"
if ! "$wrapper" "$check_dir/t.c" -o "$check_dir/t" >"$check_dir/log" 2>&1; then
    echo "race-toolchain: the unpacked compiler does not work:" >&2
    cat "$check_dir/log" >&2
    exit 3
fi
if ! "$check_dir/t"; then
    echo "race-toolchain: the compiled test program did not run" >&2
    exit 3
fi

echo "race-toolchain: ready, use CC=$wrapper CGO_ENABLED=1" >&2
echo "$wrapper"
