#!/bin/sh
# llvm-mingw-toolchain.tests.sh - llvm-mingw-toolchain.sh stages only the
# pinned archive, for the host it runs on, keeps a staged one, and refuses
# (naming why) a host it has no pin for, a fetch that keeps failing and an
# archive that is not the pinned one.
#
# curl, uname and sleep are stand-ins on PATH, so no row downloads anything.
# LLVM_MINGW_ARCHIVE, when set, names a copy of the pinned linux-hosted
# archive: the rows that unpack it run only then (a linux machine), and say
# so when they are skipped. On the windows runner the workflow's own "Stage
# llvm-mingw" step is that path.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/llvm-mingw-toolchain.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The pin, read from the script, so a bump needs no change here.
rel="$(sed -n 's/^LLVM_MINGW_RELEASE=//p' "$script")"
# Its two SHA256 lines: the Windows-hosted archive's first, then the linux one.
shas="$(sed -n 's/^ *SHA256=//p' "$script")"
win_sha="$(printf '%s\n' "$shas" | sed -n 1p)"
lin_sha="$(printf '%s\n' "$shas" | sed -n 2p)"
lin_asset="llvm-mingw-$rel-ucrt-ubuntu-22.04-x86_64.tar.xz"
win_asset="llvm-mingw-$rel-ucrt-x86_64.zip"
[ -n "$rel" ] && [ -n "$lin_sha" ] && [ -n "$win_sha" ] || { echo "FAIL: cannot read the pin from $script" >&2; exit 1; }

mkdir -p "$work/bin"
# uname: STUB_UNAME_S and STUB_UNAME_M.
cat > "$work/bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) echo "${STUB_UNAME_S:-Linux}" ;;
  -m) echo "${STUB_UNAME_M:-x86_64}" ;;
  *) exit 1 ;;
esac
EOF
# curl: counts its calls in $STUB_LOG/curl; fails the first STUB_CURL_FAILS
# calls (all of them for "all"), then writes STUB_CURL_FILE's bytes to its -o
# (or "not the archive").
cat > "$work/bin/curl" <<'EOF'
#!/bin/sh
echo "$*" >> "$STUB_LOG/curl"
n="$(wc -l < "$STUB_LOG/curl")"
case "${STUB_CURL_FAILS:-0}" in
  all) exit 22 ;;
  *) [ "$n" -gt "${STUB_CURL_FAILS:-0}" ] || exit 22 ;;
esac
o=""
while [ $# -gt 0 ]; do
  case "$1" in -o) o="$2"; shift ;; esac
  shift
done
if [ -n "${STUB_CURL_FILE:-}" ]; then cp "$STUB_CURL_FILE" "$o"; else echo "not the archive" > "$o"; fi
EOF
# sleep: counted, not slept.
cat > "$work/bin/sleep" <<'EOF'
#!/bin/sh
echo "$*" >> "$STUB_LOG/sleep"
EOF
chmod +x "$work/bin/uname" "$work/bin/curl" "$work/bin/sleep"

fails=0
fail() { echo "FAIL $*"; fails=$((fails + 1)); }
# stage NAME RC WANT DIR [VAR=VALUE...] -- ARGS...: run the script with ARGS
# (the stand-ins first on PATH, with VAR=VALUE), want exit RC and WANT in what
# it printed. Leaves the curl and sleep call counts in $curls and $sleeps.
stage() {
  name="$1"; want_rc="$2"; want="$3"; shift 3
  log="$work/log.$name"
  rm -rf "$log"; mkdir -p "$log"; : > "$log/curl"; : > "$log/sleep"
  envs=""
  while [ "$1" != -- ]; do envs="$envs $1"; shift; done
  shift
  rc=0
  # shellcheck disable=SC2086 # envs is env's words
  out="$(env PATH="$work/bin:$PATH" STUB_LOG="$log" $envs sh "$script" "$@" 2>&1)" || rc=$?
  curls="$(wc -l < "$log/curl" | tr -d ' ')"
  sleeps="$(wc -l < "$log/sleep" | tr -d ' ')"
  if [ "$rc" -ne "$want_rc" ]; then fail "$name: exit $rc, want $want_rc: $out"; return 0; fi
  case "$out" in
    *"$want"*) echo "ok   $name: $(printf '%s' "$out" | tail -n 1)" ;;
    *) fail "$name: printed '$out', want '$want'" ;;
  esac
}
# fake DIR STAMP [EXE]: a staged llvm-mingw at DIR, its stamp STAMP.
fake() {
  mkdir -p "$1/llvm-mingw/bin"
  printf '#!/bin/sh\necho fake clang\n' > "$1/llvm-mingw/bin/x86_64-w64-mingw32-clang${3:-}"
  chmod +x "$1/llvm-mingw/bin/x86_64-w64-mingw32-clang${3:-}"
  if [ -n "$2" ]; then echo "$2" > "$1/llvm-mingw/.release"; fi
}
want_calls() { [ "$curls" = "$2" ] && [ "$sleeps" = "$3" ] || fail "$1: $curls curl and $sleeps sleep calls, want $2 and $3"; }

stage darwin 2 "llvm-mingw-toolchain: no pinned llvm-mingw for host Darwin" STUB_UNAME_S=Darwin -- "$work/d0"
stage linux-arm64 2 "llvm-mingw-toolchain: no pinned llvm-mingw for linux host aarch64" STUB_UNAME_M=aarch64 -- "$work/d0"
stage noargs 2 "usage: llvm-mingw-toolchain.sh DIR" --
stage twoargs 2 "usage: llvm-mingw-toolchain.sh DIR" -- "$work/d0" "$work/d0"
want_calls twoargs 0 0

# A staged one with the pinned stamp is kept, and nothing is fetched.
fake "$work/kept" "$lin_asset $lin_sha"
stage kept 0 "llvm-mingw-toolchain: llvm-mingw $rel already at $work/kept/llvm-mingw" STUB_CURL_FAILS=all -- "$work/kept"
want_calls kept 0 0
fake "$work/keptwin" "$win_asset $win_sha" .exe
stage kept-windows 0 "llvm-mingw-toolchain: llvm-mingw $rel already at $work/keptwin/llvm-mingw" STUB_UNAME_S=MINGW64_NT-10.0-20348 STUB_CURL_FAILS=all -- "$work/keptwin"
want_calls kept-windows 0 0
# One with another release's stamp, no stamp, or no clang is fetched again
# (each fetch here fails: a fetch is all these rows look for).
fake "$work/oldrel" "llvm-mingw-20250101-ucrt-ubuntu-22.04-x86_64.tar.xz $lin_sha"
stage other-release 1 "FAILED to fetch https://github.com/mstorsjo/llvm-mingw/releases/download/$rel/$lin_asset after 5 attempts" STUB_CURL_FAILS=all -- "$work/oldrel"
want_calls other-release 5 4
fake "$work/nostamp" ""
stage no-stamp 1 "FAILED to fetch" STUB_CURL_FAILS=all -- "$work/nostamp"
fake "$work/winstamp" "$win_asset $win_sha"
stage other-host-stamp 1 "FAILED to fetch" STUB_CURL_FAILS=all -- "$work/winstamp"
mkdir -p "$work/noclang/llvm-mingw"; echo "$lin_asset $lin_sha" > "$work/noclang/llvm-mingw/.release"
stage no-clang 1 "FAILED to fetch" STUB_CURL_FAILS=all -- "$work/noclang"
want_calls no-clang 5 4

# A fetch that fails, then works, is retried; an archive that is not the
# pinned one is refused and not kept.
stage not-the-pin 1 "llvm-mingw-toolchain: https://github.com/mstorsjo/llvm-mingw/releases/download/$rel/$lin_asset hashes to " STUB_CURL_FAILS=2 -- "$work/d1"
want_calls not-the-pin 3 2
case "$out" in *", not the pin $lin_sha"*) ;; *) fail "not-the-pin: printed '$out', want the pin named" ;; esac
[ ! -e "$work/d1/$lin_asset.part" ] || fail "not-the-pin: kept $work/d1/$lin_asset.part"
[ ! -e "$work/d1/llvm-mingw" ] || fail "not-the-pin: staged $work/d1/llvm-mingw"
# On Windows (Git Bash) the Windows-hosted archive and its pin.
stage not-the-pin-windows 1 "https://github.com/mstorsjo/llvm-mingw/releases/download/$rel/$win_asset hashes to " STUB_UNAME_S=MINGW64_NT-10.0-20348 -- "$work/d2"
case "$out" in *", not the pin $win_sha"*) ;; *) fail "not-the-pin-windows: printed '$out', want the pin named" ;; esac
stage not-the-pin-msys 1 "/$win_asset hashes to " STUB_UNAME_S=MSYS_NT-10.0 -- "$work/d3"
stage not-the-pin-cygwin 1 "/$win_asset hashes to " STUB_UNAME_S=CYGWIN_NT-10.0 -- "$work/d4"

# The pinned archive is unpacked, stamped and kept.
if [ -n "${LLVM_MINGW_ARCHIVE:-}" ]; then
  stage pinned 0 "at $work/d5/llvm-mingw, from https://github.com/mstorsjo/llvm-mingw/releases/download/$rel/$lin_asset" STUB_CURL_FILE="$LLVM_MINGW_ARCHIVE" -- "$work/d5"
  want_calls pinned 1 0
  [ -x "$work/d5/llvm-mingw/bin/x86_64-w64-mingw32-clang" ] || fail "pinned: no $work/d5/llvm-mingw/bin/x86_64-w64-mingw32-clang"
  [ "$(cat "$work/d5/llvm-mingw/.release" 2>/dev/null)" = "$lin_asset $lin_sha" ] || fail "pinned: stamp '$(cat "$work/d5/llvm-mingw/.release" 2>/dev/null)'"
  for left in "$work/d5/$lin_asset.part" "$work/d5/llvm-mingw.unpack"; do
    [ ! -e "$left" ] || fail "pinned: left $left"
  done
  stage pinned-again 0 "already at $work/d5/llvm-mingw" STUB_CURL_FAILS=all -- "$work/d5"
  want_calls pinned-again 0 0
  # A DIR with a backslash in its name, as the windows runner's
  # GITHUB_WORKSPACE (D:\a\...) gives: the pinned archive is still the pin.
  stage pinned-backslash-dir 0 "from https://github.com/mstorsjo/llvm-mingw/releases/download/$rel/$lin_asset" STUB_CURL_FILE="$LLVM_MINGW_ARCHIVE" -- "$work/d\\7"
  [ -x "$work/d\\7/llvm-mingw/bin/x86_64-w64-mingw32-clang" ] || fail "pinned-backslash-dir: no clang staged"
  # One left unstamped (a cut-off run's) is replaced, not unpacked into.
  fake "$work/d6" ""
  stage pinned-over-stale 0 "at $work/d6/llvm-mingw, from" STUB_CURL_FILE="$LLVM_MINGW_ARCHIVE" -- "$work/d6"
  ! grep -q 'fake clang' "$work/d6/llvm-mingw/bin/x86_64-w64-mingw32-clang" || fail "pinned-over-stale: kept the stale clang"
  [ ! -e "$work/d6/llvm-mingw/${lin_asset%.tar.xz}" ] || fail "pinned-over-stale: unpacked into the stale one, at $work/d6/llvm-mingw/${lin_asset%.tar.xz}"
  [ "$(cat "$work/d6/llvm-mingw/.release" 2>/dev/null)" = "$lin_asset $lin_sha" ] || fail "pinned-over-stale: stamp '$(cat "$work/d6/llvm-mingw/.release" 2>/dev/null)'"
else
  echo "skip pinned, pinned-again, pinned-backslash-dir, pinned-over-stale: LLVM_MINGW_ARCHIVE names no copy of $lin_asset"
fi

if [ "$fails" -ne 0 ]; then
  echo "llvm-mingw-toolchain.tests: $fails FAILED" >&2
  exit 1
fi
echo "llvm-mingw-toolchain.tests: all passed"
