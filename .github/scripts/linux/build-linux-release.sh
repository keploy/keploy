#!/bin/sh
# build-linux-release.sh binary ARCH OUTPUT DSN - build a linux release binary
# build-linux-release.sh pgtest ARCH OUTPUT     - build the Postgres parser's
#                                                 tests, linked the same way
#
# The one recipe for the linux release binaries (release.yml's build-linux,
# both the public and the docker binary) and for the PR lane that builds them
# the release way (release-linux-static.yml). Run from the repository root,
# after setup-private-parsers (the Postgres parser comes with the private
# integrations module) and zig-toolchain.sh.
#
# Why: the Postgres parser (pg_query_go, libpg_query) is C, so the linux
# builds use cgo. Linked by the runner's gcc against its glibc, v3.6.164 and
# the releases before it needed GLIBC_2.34 and exited at once on Debian 11,
# Ubuntu 20.04, RHEL 8 and Amazon Linux 2 ("version `GLIBC_2.34' not found").
# So the C side is compiled and linked by zig cc against musl, FULLY STATIC:
# the binary loads no library of the host at run time and runs on any Linux.
#
#   * netgo, osusergo: the DNS resolver and user lookups stay pure Go (a
#     static binary cannot load the host's NSS modules), as they were in the
#     static pure-Go binaries before cgo was turned on.
#   * -z stack-size=8 MiB: musl gives a thread 128 KiB of stack unless the
#     binary asks for more in its PT_GNU_STACK header, and cgo's C code (the
#     parser, which recurses on nested expressions) runs on the stack of the
#     thread it is called from. 8 MiB is glibc's default (the usual stack
#     rlimit), so the parser keeps the room it had.
#   * --strip-all: zig cc does not act on the -s Go's linker passes it;
#     without it the binary keeps its symbol table and the C code's DWARF.
#
# check-linux-release-binary.sh, run on every binary built here, refuses one
# that is not static, not stripped or without the 8 MiB thread stack.
#
# Env: KEPLOY_ZIG (the zig to link with; zig-toolchain.sh); for `binary`,
# VERSION, SERVER_URL and GITHUB_APP_CLIENT_ID, as the release sets them.
set -eu

usage() { echo "usage: build-linux-release.sh binary amd64|arm64 OUTPUT DSN | pgtest amd64|arm64 OUTPUT" >&2; exit 2; }
[ $# -ge 3 ] || usage
mode="$1"; arch="$2"; out="$3"
case "$arch" in
  amd64) target=x86_64-linux-musl ;;
  arm64) target=aarch64-linux-musl ;;
  *) echo "build-linux-release: unknown arch '$arch' (amd64 or arm64)" >&2; exit 2 ;;
esac
: "${KEPLOY_ZIG:?KEPLOY_ZIG must name the pinned zig (.github/scripts/linux/zig-toolchain.sh)}"
here="$(cd "$(dirname "$0")" && pwd)"

export CGO_ENABLED=1 GOOS=linux GOARCH="$arch"
export CC="$KEPLOY_ZIG cc -target $target"
static="-linkmode=external -extldflags '-static -Wl,-z,stack-size=8388608 -Wl,--strip-all'"
# The parser's tests: the classifier and extractor the Postgres v3 matcher
# runs on every query, over pg_query_go's Parse, Normalize and Fingerprint.
PG_PKG=github.com/keploy/integrations/pkg/postgres/shared/match

case "$mode" in
  binary)
    [ $# -eq 4 ] || usage
    dsn="$4"
    : "${VERSION:?}" "${SERVER_URL:?}" "${GITHUB_APP_CLIENT_ID?}"
    echo "build-linux-release: linux/$arch with CC=zig cc -target $target ($("$KEPLOY_ZIG" version)), static"
    go build \
      -tags=viper_bind_struct,netgo,osusergo \
      -ldflags="-s -w \
        -X main.dsn=${dsn} \
        -X main.version=${VERSION} \
        -X main.apiServerURI=${SERVER_URL} \
        -X main.gitHubClientID=${GITHUB_APP_CLIENT_ID} \
        $static" \
      -o "$out" \
      ./main.go
    sh "$here/check-linux-release-binary.sh" "$out" "$arch"
    ;;
  pgtest)
    [ $# -eq 3 ] || usage
    echo "build-linux-release: $PG_PKG's tests for linux/$arch, linked as the release is"
    go test -c -tags=netgo,osusergo -ldflags="$static" -o "$out" "$PG_PKG"
    ;;
  *) usage ;;
esac
