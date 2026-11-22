#!/bin/sh
# zig-toolchain.sh DIR - stage the pinned Zig toolchain at DIR/zig/zig.
#
# Why: the linux release binaries link C (the Postgres parser, pg_query_go,
# embeds libpg_query, so cgo is on). Linked by the runner's gcc against its
# glibc (ubuntu-24.04: 2.39), v3.6.164 and the releases before it needed
# GLIBC_2.34 and exited at once on Debian 11, Ubuntu 20.04, RHEL 8 and Amazon
# Linux 2 ("version `GLIBC_2.34' not found"). `zig cc` carries musl and builds
# for both linux arches, so build-linux-release.sh links the linux binaries
# fully static against musl: no library of the host is loaded at run time.
#
# The tarball is pinned by version and sha256 (ziglang.org's download index).
# The release and PR workflows keep DIR in the Actions cache, keyed on this
# file's hash, so ziglang.org is asked once per pin (it asks CI not to fetch
# from it on every run). Whatever the source, the bytes must hash to the pin.
#
# Idempotent: a DIR/zig whose `zig version` is the pinned one is kept.
set -eu

ZIG_VERSION=0.16.0
case "$(uname -m)" in
  x86_64|amd64)  ZIG_HOST=x86_64-linux;  ZIG_SHA256=70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00 ;;
  aarch64|arm64) ZIG_HOST=aarch64-linux; ZIG_SHA256=ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17 ;;
  *) echo "zig-toolchain: no pinned zig for host $(uname -m)" >&2; exit 2 ;;
esac

[ $# -eq 1 ] || { echo "usage: zig-toolchain.sh DIR" >&2; exit 2; }
DIR="$1"
TARBALL="zig-${ZIG_HOST}-${ZIG_VERSION}.tar.xz"
UPSTREAM="https://ziglang.org/download/${ZIG_VERSION}/${TARBALL}"

if [ -x "$DIR/zig/zig" ] && [ "$("$DIR/zig/zig" version 2>/dev/null)" = "$ZIG_VERSION" ]; then
  echo "zig-toolchain: zig $ZIG_VERSION already at $DIR/zig"
  exit 0
fi

mkdir -p "$DIR"
PART="$DIR/$TARBALL.part"
rm -f "$PART"
# At most 5 attempts, 10 s apart; an attempt whose transfer stays under
# 1000 B/s for 30 s is given up.
n=1
until curl --silent --show-error --fail --location --speed-limit 1000 --speed-time 30 -o "$PART" "$UPSTREAM"; do
  if [ "$n" -ge 5 ]; then
    echo "zig-toolchain: FAILED to fetch $UPSTREAM after $n attempts" >&2
    exit 1
  fi
  echo "zig-toolchain: fetch attempt $n of $UPSTREAM failed; retrying in 10 s" >&2
  n=$((n + 1))
  sleep 10
done
got="$(sha256sum "$PART" | cut -d' ' -f1)"
if [ "$got" != "$ZIG_SHA256" ]; then
  echo "zig-toolchain: $UPSTREAM hashes to $got, not the pin $ZIG_SHA256" >&2
  rm -f "$PART"
  exit 1
fi

rm -rf "$DIR/zig" "$DIR/zig.unpack"
mkdir -p "$DIR/zig.unpack"
tar -xJf "$PART" -C "$DIR/zig.unpack" --strip-components=1
rm -f "$PART"
mv "$DIR/zig.unpack" "$DIR/zig"
got="$("$DIR/zig/zig" version)"
[ "$got" = "$ZIG_VERSION" ] || { echo "zig-toolchain: unpacked zig says $got, not $ZIG_VERSION" >&2; exit 1; }
echo "zig-toolchain: zig $ZIG_VERSION ($ZIG_HOST) at $DIR/zig, from $UPSTREAM"
