#!/bin/sh
# check-linux-release-distros.sh BINARY ARCH PGTEST - run a linux release
# binary on the oldest distributions keploy supports, before it is published.
#
# Why: v3.6.164 and the releases before it were linked against the runner's
# glibc and needed GLIBC_2.34. On Debian 11, Ubuntu 20.04, RHEL 8 and Amazon
# Linux 2 they exited before main ("version `GLIBC_2.34' not found"), and no
# lane noticed: every lane runs keploy on a new distribution. The linux
# builds are now static against musl (build-linux-release.sh), and
# check-linux-release-binary.sh refuses one that is not. This runs the binary
# where the regression showed, so one that still cannot start there fails the
# workflow:
#
#   in debian:11, ubuntu:20.04, amazonlinux:2 and rockylinux:8 (glibc 2.31,
#   2.31, 2.26 and 2.28):
#     * the release binary's `--version` exits 0 and prints a Keploy version;
#     * PGTEST, the Postgres parser's own tests (libpg_query, the C code that
#       turned cgo on) built by `build-linux-release.sh pgtest`, so linked as
#       the release is, pass: a real parse path.
#
# ARCH must be this runner's: release.yml builds each arch on its own native
# runner, and runs this there. The containers share the runner's kernel, so
# this proves the user space (the libc), not the kernel.
set -eu

[ $# -eq 3 ] || { echo "usage: check-linux-release-distros.sh BINARY amd64|arm64 PGTEST" >&2; exit 2; }
bin="$1"; arch="$2"; pgtest="$3"
case "$arch" in
  amd64) host_arch=x86_64 ;;
  arm64) host_arch=aarch64 ;;
  *) echo "check-linux-release-distros: unknown arch '$arch' (amd64 or arm64)" >&2; exit 2 ;;
esac
die() { echo "check-linux-release-distros: $*" >&2; exit 1; }
[ "$(uname -m)" = "$host_arch" ] || die "this runner is $(uname -m), not $host_arch: run it on the $arch runner that built the binary"
[ -f "$bin" ] || die "no binary at '$bin'"
[ -f "$pgtest" ] || die "no Postgres parser test binary at '$pgtest'"
IMAGES="debian:11 ubuntu:20.04 amazonlinux:2 rockylinux:8"
PG_PKG=github.com/keploy/integrations/pkg/postgres/shared/match
# The tests run in their package's directory, as `go test` runs them.
pg_dir="$(go list -f '{{.Dir}}' "$PG_PKG")"
[ -d "$pg_dir" ] || die "cannot find $PG_PKG's directory"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp "$bin" "$work/keploy"
cp "$pgtest" "$work/pgparse.test"
chmod 755 "$work" "$work/keploy" "$work/pgparse.test"

# pull IMAGE: at most 5 attempts, 10 s apart (a registry blip is not the
# binary's fault).
pull() {
  n=1
  until docker pull -q --platform "linux/$arch" "$1" >/dev/null; do
    [ "$n" -lt 5 ] || return 1
    echo "check-linux-release-distros: pull $1 attempt $n failed; retrying in 10 s" >&2
    n=$((n + 1)); sleep 10
  done
}

fail=0
for img in $IMAGES; do
  echo "=== $img linux/$arch"
  pull "$img" || die "could not pull $img for linux/$arch"
  rc=0
  docker run --rm --platform "linux/$arch" -v "$work:/b:ro" -v "$pg_dir:/pg:ro" -w /pg "$img" sh -c '
    set -u
    . /etc/os-release
    echo "distro: $PRETTY_NAME ($(uname -m)), libc: $(ldd --version 2>&1 | head -n 1)"
    # keploy opens keploy-logs.txt in its working directory: a writable one.
    out="$(cd /tmp && /b/keploy --version 2>&1)"; rc=$?
    printf "%s\n" "$out" | tail -n 3
    if [ "$rc" -ne 0 ]; then echo "FAIL: --version exited $rc"; exit 1; fi
    case "$out" in *"Keploy "*) ;; *) echo "FAIL: --version printed no Keploy version"; exit 1 ;; esac
    /b/pgparse.test -test.count=1 > /tmp/pgparse.log 2>&1; rc=$?
    tail -n 3 /tmp/pgparse.log
    if [ "$rc" -ne 0 ]; then echo "FAIL: the Postgres parser tests exited $rc:"; tail -n 40 /tmp/pgparse.log; exit 1; fi
    echo "ok"
  ' || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-linux-release-distros: FAILED on $img linux/$arch" >&2
    fail=1
  fi
done

[ "$fail" -eq 0 ] || die "the linux/$arch release binary does not run on every supported distribution (above)"
echo "check-linux-release-distros: linux/$arch runs, and the Postgres parser passes, on $IMAGES"
