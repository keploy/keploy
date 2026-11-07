#!/bin/sh
# llvm-mingw-toolchain.sh DIR - stage the pinned llvm-mingw toolchain at
# DIR/llvm-mingw (DIR/llvm-mingw/bin/x86_64-w64-mingw32-clang and its tools).
#
# Why: the windows release binary links C (the Postgres parser, pg_query_go,
# embeds libpg_query, so cgo is on). Linked by the MinGW-Builds gcc on the
# runner's PATH, v3.6.216 and the releases before it imported
# libwinpthread-1.dll, which Windows does not ship: on a machine without MinGW
# on its PATH keploy exited before main ("libwinpthread-1.dll was not found",
# STATUS_DLL_NOT_FOUND). They also carried gcc's own runtime (libgcc, GPL-3.0
# with the GCC runtime exception). llvm-mingw is clang and lld with the
# mingw-w64 runtime and LLVM's compiler-rt, no gcc code: build-windows-release.sh
# links the windows binary with it, -static, so the binary imports only DLLs
# that are part of Windows (check-windows-release-binary.sh holds it to that).
#
# The archive is pinned by release and sha256 (the llvm-mingw release's own
# asset digests). Its UCRT flavour, as the binary was before: it links against
# the Universal CRT, part of Windows 10 and later. On Windows the
# Windows-hosted archive is staged, elsewhere the linux-hosted one: the same
# clang, lld and mingw-w64 runtime, for local builds. The release and PR
# workflows keep DIR in the Actions cache, keyed on this file's hash.
#
# Idempotent: a DIR/llvm-mingw that holds the pinned release is kept.
set -eu

LLVM_MINGW_RELEASE=20261020
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    ASSET="llvm-mingw-${LLVM_MINGW_RELEASE}-ucrt-x86_64.zip"
    SHA256=fd5462b7f5d93b2ff981fcd3f9c7e427e47f3203c64ea91c27bcc444eebd74cd
    EXE=.exe ;;
  Linux)
    [ "$(uname -m)" = x86_64 ] || { echo "llvm-mingw-toolchain: no pinned llvm-mingw for linux host $(uname -m)" >&2; exit 2; }
    ASSET="llvm-mingw-${LLVM_MINGW_RELEASE}-ucrt-ubuntu-22.04-x86_64.tar.xz"
    SHA256=55064b60b991a1e1e8090b8c2f84e82b22cfe5903d12c159d65064cf5e8ae335
    EXE= ;;
  *) echo "llvm-mingw-toolchain: no pinned llvm-mingw for host $(uname -s)" >&2; exit 2 ;;
esac

[ $# -eq 1 ] || { echo "usage: llvm-mingw-toolchain.sh DIR" >&2; exit 2; }
DIR="$1"
UPSTREAM="https://github.com/mstorsjo/llvm-mingw/releases/download/${LLVM_MINGW_RELEASE}/${ASSET}"
STAMP="$DIR/llvm-mingw/.release"

if [ -x "$DIR/llvm-mingw/bin/x86_64-w64-mingw32-clang$EXE" ] && [ "$(cat "$STAMP" 2>/dev/null)" = "$ASSET $SHA256" ]; then
  echo "llvm-mingw-toolchain: llvm-mingw $LLVM_MINGW_RELEASE already at $DIR/llvm-mingw"
  exit 0
fi

mkdir -p "$DIR"
PART="$DIR/$ASSET.part"
rm -f "$PART"
# At most 5 attempts, 10 s apart; an attempt whose transfer stays under
# 1000 B/s for 30 s is given up.
n=1
until curl --silent --show-error --fail --location --speed-limit 1000 --speed-time 30 -o "$PART" "$UPSTREAM"; do
  if [ "$n" -ge 5 ]; then
    echo "llvm-mingw-toolchain: FAILED to fetch $UPSTREAM after $n attempts" >&2
    exit 1
  fi
  echo "llvm-mingw-toolchain: fetch attempt $n of $UPSTREAM failed; retrying in 10 s" >&2
  n=$((n + 1))
  sleep 10
done
# Hashed from stdin: given a name with a backslash in it (the windows
# runner's DIR, D:\a\...), GNU sha256sum escapes the name and starts its
# line with a backslash, so the hash read off the line would not be the pin.
got="$(sha256sum < "$PART" | cut -d' ' -f1)"
if [ "$got" != "$SHA256" ]; then
  echo "llvm-mingw-toolchain: $UPSTREAM hashes to $got, not the pin $SHA256" >&2
  rm -f "$PART"
  exit 1
fi

rm -rf "$DIR/llvm-mingw" "$DIR/llvm-mingw.unpack"
mkdir -p "$DIR/llvm-mingw.unpack"
case "$ASSET" in
  # Windows' own bsdtar reads zip (Git Bash's GNU tar does not).
  *.zip) "${SYSTEMROOT:-C:/Windows}/System32/tar.exe" -xf "$PART" -C "$DIR/llvm-mingw.unpack" ;;
  # GNU tar reads a backslash in a name as an escape (\7 as one byte) unless
  # told not to.
  *) tar --no-unquote -xJf "$PART" -C "$DIR/llvm-mingw.unpack" ;;
esac
rm -f "$PART"
top="$DIR/llvm-mingw.unpack/${ASSET%.zip}"
top="${top%.tar.xz}"
[ -x "$top/bin/x86_64-w64-mingw32-clang$EXE" ] || { echo "llvm-mingw-toolchain: $ASSET has no bin/x86_64-w64-mingw32-clang$EXE" >&2; exit 1; }
mv "$top" "$DIR/llvm-mingw"
rm -rf "$DIR/llvm-mingw.unpack"
echo "$ASSET $SHA256" > "$STAMP"
echo "llvm-mingw-toolchain: llvm-mingw $LLVM_MINGW_RELEASE ($("$DIR/llvm-mingw/bin/x86_64-w64-mingw32-clang$EXE" --version | head -n 1)) at $DIR/llvm-mingw, from $UPSTREAM"
