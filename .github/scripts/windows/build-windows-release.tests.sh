#!/bin/sh
# build-windows-release.tests.sh - build-windows-release.sh refuses what it
# cannot build (naming why) before it builds anything; with a stand-in go, it
# runs go with the release's tags, -X values and link flags, and checks what
# go wrote, refusing a binary that fails check-windows-release-binary.sh; and
# its probe imports libwinpthread-1.dll, so check-windows-release-binary.sh
# refuses it.
# check-windows-release-binary.tests.sh links its passing rows with the
# recipe's own C link flags (build-windows-release.sh extldflags).
#
# Needs llvm-mingw (KEPLOY_LLVM_MINGW, as llvm-mingw-toolchain.sh stages it),
# so it runs wherever that does: the windows runner's Git Bash, or a linux
# machine with the linux-hosted one.
set -eu

: "${KEPLOY_LLVM_MINGW:?KEPLOY_LLVM_MINGW must name the pinned llvm-mingw (.github/scripts/windows/llvm-mingw-toolchain.sh)}"
here="$(cd "$(dirname "$0")" && pwd)"
recipe="$here/build-windows-release.sh"
check="$here/check-windows-release-binary.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# An empty directory to run in: a mode that got past its checks finds no
# main.go or module there, so it fails at once instead of building keploy.
mkdir "$work/empty"

fails=0
# row NAME RC WANT [VAR=VALUE...] -- ARGS... : run the recipe with ARGS in an
# empty directory, with VERSION, SERVER_URL and GITHUB_APP_CLIENT_ID unset
# but for the VAR=VALUEs; want exit RC ("any" for any non-zero) and WANT in
# what it printed.
row() {
  name="$1"; want_rc="$2"; want="$3"; shift 3
  set -- -u VERSION -u SERVER_URL -u GITHUB_APP_CLIENT_ID "$@"
  # Move the VAR=VALUEs and the ARGS apart: env takes VAR=VALUE before the
  # command.
  envs=""
  while [ "$1" != -- ]; do envs="$envs $1"; shift; done
  shift
  rc=0
  # shellcheck disable=SC2086 # envs is env's words
  out="$(cd "$work/empty" && env $envs sh "$recipe" "$@" 2>&1)" || rc=$?
  case "$want_rc:$rc" in
    any:0) echo "FAIL $name: exit 0, want non-zero: $out"; fails=$((fails + 1)); return 0 ;;
    any:*) ;;
    "$rc:$rc") ;;
    *) echo "FAIL $name: exit $rc, want $want_rc: $out"; fails=$((fails + 1)); return 0 ;;
  esac
  case "$out" in
    *"$want"*) echo "ok   $name: $(printf '%s' "$out" | head -n 1)" ;;
    *) echo "FAIL $name: printed '$out', want '$want'"; fails=$((fails + 1)) ;;
  esac
}

usage="usage: build-windows-release.sh binary amd64 OUTPUT DSN"
exe="$work/out.exe"
row noargs 2 "$usage" --
row twoargs 2 "$usage" -- binary amd64
row binary-no-dsn 2 "$usage" VERSION=1 SERVER_URL=u GITHUB_APP_CLIENT_ID= -- binary amd64 "$exe"
row binary-extra 2 "$usage" VERSION=1 SERVER_URL=u GITHUB_APP_CLIENT_ID= -- binary amd64 "$exe" dsn extra
row pgtest-extra 2 "$usage" -- pgtest amd64 "$exe" extra
row probe-extra 2 "$usage" -- probe amd64 "$exe" extra
row nosuchmode 2 "$usage" -- nosuch amd64 "$exe"
row extldflags-extra 2 "$usage" -- extldflags extra
row arm64 2 "build-windows-release: unknown arch 'arm64' (amd64)" -- probe arm64 "$exe"
row no-toolchain any "KEPLOY_LLVM_MINGW must name the pinned llvm-mingw" -u KEPLOY_LLVM_MINGW -- probe amd64 "$exe"
row binary-no-version any "VERSION" SERVER_URL=u GITHUB_APP_CLIENT_ID= -- binary amd64 "$exe" dsn
row binary-no-server any "SERVER_URL" VERSION=1 GITHUB_APP_CLIENT_ID= -- binary amd64 "$exe" dsn
row binary-no-client-id any "GITHUB_APP_CLIENT_ID" VERSION=1 SERVER_URL=u -- binary amd64 "$exe" dsn
# An empty client id is what a fork's release has: it builds (here it gets as
# far as go build, which finds no main.go in the empty directory).
row binary-empty-client-id any "build-windows-release: windows/amd64 with CC=" VERSION=1 SERVER_URL=u GITHUB_APP_CLIENT_ID= -- binary amd64 "$exe" dsn
row extldflags 0 "-static -Wl,--stack," -- extldflags

# The probe builds, and imports libwinpthread-1.dll as v3.6.216 did, so the
# binary check refuses it.
row probe 0 "" -- probe amd64 "$exe"
rc=0
probe_out="$(sh "$check" "$exe" 2>&1)" || rc=$?
case "$rc:$probe_out" in
  "1:"*"imports DLLs Windows does not ship: libwinpthread-1.dll"*) echo "ok   probe refused: $probe_out" ;;
  *) echo "FAIL probe refused: exit $rc: $probe_out"; fails=$((fails + 1)) ;;
esac

# With a stand-in go first on PATH: binary and pgtest run go with the
# release's settings, and run the binary check on what go wrote, so each
# refuses a binary that fails it. The stand-in records go's cgo environment
# and its arguments, one per line, runs of spaces made one, and copies
# GO_EMIT to its -o.
cp "$exe" "$work/dll.exe"
tc="$KEPLOY_LLVM_MINGW"
if command -v cygpath >/dev/null 2>&1; then tc="$(cygpath -m "$tc")"; fi
printf '%s\n' '#include <stdio.h>' 'int main(void) { puts("ok"); return 0; }' > "$work/hello.c"
# shellcheck disable=SC2046 # the flags are words
"$tc/bin/x86_64-w64-mingw32-clang" -O1 -o "$work/ok.exe" "$work/hello.c" -s $(sh "$recipe" extldflags)
mkdir "$work/gobin"
cat > "$work/gobin/go" <<'EOF2'
#!/bin/sh
{
  printf 'env CGO_ENABLED=%s GOOS=%s GOARCH=%s CC=%s CXX=%s\n' "${CGO_ENABLED-}" "${GOOS-}" "${GOARCH-}" "${CC-}" "${CXX-}"
  for a in "$@"; do printf 'arg %s\n' "$a"; done
} | tr -s ' ' > "$GO_ARGV"
o=""; prev=""
for a in "$@"; do [ "$prev" = -o ] && o="$a"; prev="$a"; done
cp "$GO_EMIT" "$o"
EOF2
chmod +x "$work/gobin/go"
goout="$work/go-out.exe"
cgoenv="env CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=$tc/bin/x86_64-w64-mingw32-clang CXX=$tc/bin/x86_64-w64-mingw32-clang++"
link="-linkmode=external -extldflags '-static -Wl,--stack,2097152'"
# gorow NAME EMIT RC WANT ARGV -- ARGS...: run the recipe with ARGS and the
# stand-in go, which writes EMIT; want exit RC, WANT in what it printed and,
# when ARGV is not empty, go run with exactly ARGV (lines).
gorow() {
  name="$1"; emit="$2"; want_rc="$3"; want="$4"; want_argv="$5"; shift 6
  rm -f "$goout" "$work/argv"
  rc=0
  out="$(cd "$work/empty" && PATH="$work/gobin:$PATH" GO_ARGV="$work/argv" GO_EMIT="$emit" \
    VERSION=1.2.3 SERVER_URL=https://api.example GITHUB_APP_CLIENT_ID=cid sh "$recipe" "$@" 2>&1)" || rc=$?
  got_argv="$(cat "$work/argv" 2>/dev/null || true)"
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL $name: exit $rc, want $want_rc: $out"; fails=$((fails + 1))
  elif case "$out" in *"$want"*) false ;; *) true ;; esac; then
    echo "FAIL $name: printed '$out', want '$want'"; fails=$((fails + 1))
  elif [ -n "$want_argv" ] && [ "$got_argv" != "$want_argv" ]; then
    echo "FAIL $name: go ran as:"; echo "$got_argv"; echo "want:"; echo "$want_argv"; fails=$((fails + 1))
  else
    echo "ok   $name: $(printf '%s' "$out" | tail -n 1)"
  fi
}
binary_argv="$cgoenv
arg build
arg -tags=viper_bind_struct
arg -ldflags=-s -w -X main.dsn=the-dsn -X main.version=1.2.3 -X main.apiServerURI=https://api.example -X main.gitHubClientID=cid $link
arg -o
arg $goout
arg ./main.go"
pgtest_argv="$cgoenv
arg test
arg -c
arg -ldflags=-s -w $link
arg -o
arg $goout
arg github.com/keploy/integrations/pkg/postgres/shared/match"
gorow go-binary "$work/ok.exe" 0 "check-windows-release-binary: $goout imports only KERNEL32.dll" "$binary_argv" -- binary amd64 "$goout" the-dsn
gorow go-binary-refused "$work/dll.exe" 1 "imports DLLs Windows does not ship: libwinpthread-1.dll" "$binary_argv" -- binary amd64 "$goout" the-dsn
gorow go-pgtest "$work/ok.exe" 0 "check-windows-release-binary: $goout imports only KERNEL32.dll" "$pgtest_argv" -- pgtest amd64 "$goout"
gorow go-pgtest-refused "$work/dll.exe" 1 "imports DLLs Windows does not ship: libwinpthread-1.dll" "$pgtest_argv" -- pgtest amd64 "$goout"

if [ "$fails" -ne 0 ]; then
  echo "build-windows-release.tests: $fails FAILED" >&2
  exit 1
fi
echo "build-windows-release.tests: all passed"
