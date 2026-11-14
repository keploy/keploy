#!/bin/sh
# build-windows-release.sh binary amd64 OUTPUT DSN - build the windows release binary
# build-windows-release.sh pgtest amd64 OUTPUT     - build the Postgres parser's
#                                                    tests, linked the same way
# build-windows-release.sh probe amd64 OUTPUT      - build a control that imports
#                                                    libwinpthread-1.dll, as
#                                                    v3.6.216 did
# build-windows-release.sh extldflags              - print the C link flags the
#                                                    other modes link with
#
# The one recipe for the windows release binary (release.yml's build-windows)
# and for the PR lane that builds it the release way
# (release-windows-static.yml). Run from the repository root in bash (Git
# Bash on the windows runner), after setup-private-parsers (the Postgres
# parser comes with the private integrations module) and
# llvm-mingw-toolchain.sh.
#
# Why: the Postgres parser (pg_query_go, libpg_query) is C, so the windows
# build uses cgo. Linked by the MinGW-Builds gcc on the runner's PATH,
# v3.6.216 and the releases before it imported libwinpthread-1.dll, a MinGW
# DLL Windows does not ship, so keploy did not start on a machine without
# MinGW on its PATH (STATUS_DLL_NOT_FOUND, before main). They also carried
# gcc's runtime (libgcc, GPL-3.0 with the GCC runtime exception). The C side
# is now compiled and linked by llvm-mingw's clang and lld against the
# mingw-w64 runtime and LLVM's compiler-rt:
#
#   * -static: every library the C side links comes from its static archive,
#     so no MinGW or LLVM runtime DLL is imported; what is left is Windows'
#     own (KERNEL32 and the Universal CRT's api-ms-win-crt-* sets, as before).
#   * -Wl,--stack,2097152: a pin of the 2 MiB stack reserve gcc's linker
#     wrote, which lld would make 1 MiB. Windows sizes the main thread's
#     stack from it, and every thread cgo starts (_beginthread with size 0),
#     and the parser's C code, which recurses on nested expressions, runs on
#     the stack of the thread it is called from.
#
# check-windows-release-binary.sh, run on every binary built here, refuses one
# that imports a DLL that is not part of Windows, carries gcc's runtime, is
# not stripped or has a smaller stack reserve.
#
# Env: KEPLOY_LLVM_MINGW (the llvm-mingw to link with: llvm-mingw-toolchain.sh's
# DIR/llvm-mingw); for `binary`, VERSION, SERVER_URL and GITHUB_APP_CLIENT_ID,
# as the release sets them (set -u refuses a run with one unset, before
# go build; GITHUB_APP_CLIENT_ID may be empty, as on a fork).
set -eu

usage() { echo "usage: build-windows-release.sh binary amd64 OUTPUT DSN | pgtest amd64 OUTPUT | probe amd64 OUTPUT | extldflags" >&2; exit 2; }
# The C link flags (the Why above). check-windows-release-binary.tests.sh
# links its passing rows with them, so a change to them is checked there too.
EXTLDFLAGS='-static -Wl,--stack,2097152'
if [ "${1:-}" = extldflags ]; then
  [ $# -eq 1 ] || usage
  printf '%s\n' "$EXTLDFLAGS"
  exit 0
fi
[ $# -ge 3 ] || usage
mode="$1"; arch="$2"; out="$3"
case "$arch" in
  amd64) target=x86_64-w64-mingw32 ;;
  *) echo "build-windows-release: unknown arch '$arch' (amd64)" >&2; exit 2 ;;
esac
: "${KEPLOY_LLVM_MINGW:?KEPLOY_LLVM_MINGW must name the pinned llvm-mingw (.github/scripts/windows/llvm-mingw-toolchain.sh)}"
here="$(cd "$(dirname "$0")" && pwd)"
# go.exe takes CC as a Windows path; Git Bash's /d/a/... form it cannot run.
tc="$KEPLOY_LLVM_MINGW"
if command -v cygpath >/dev/null 2>&1; then tc="$(cygpath -m "$tc")"; fi
cc="$tc/bin/$target-clang"

export CGO_ENABLED=1 GOOS=windows GOARCH="$arch"
export CC="$cc" CXX="$tc/bin/$target-clang++"
static="-linkmode=external -extldflags '$EXTLDFLAGS'"
# The parser's tests: the classifier and extractor the Postgres v3 matcher
# runs on every query, over pg_query_go's Parse, Normalize and Fingerprint.
# check-windows-release-start.ps1 runs the whole package's tests, from an
# empty directory, with no -test.run: a name filter would pass with no test
# run once the tests it names are renamed. So a test in this package that
# cannot pass there (one that reads testdata, or does not run on windows)
# stops the windows release, and the PR lane before it, until it is fixed
# or this names another package.
PG_PKG=github.com/keploy/integrations/pkg/postgres/shared/match

case "$mode" in
  binary)
    [ $# -eq 4 ] || usage
    dsn="$4"
    echo "build-windows-release: windows/$arch with CC=$cc ($("$cc" --version | head -n 1)), static"
    go build \
      -tags=viper_bind_struct \
      -ldflags="-s -w \
        -X main.dsn=${dsn} \
        -X main.version=${VERSION} \
        -X main.apiServerURI=${SERVER_URL} \
        -X main.gitHubClientID=${GITHUB_APP_CLIENT_ID} \
        $static" \
      -o "$out" \
      ./main.go
    sh "$here/check-windows-release-binary.sh" "$out"
    ;;
  pgtest)
    [ $# -eq 3 ] || usage
    echo "build-windows-release: $PG_PKG's tests for windows/$arch, linked as the release is"
    # Stripped as the release is (-s -w), so the binary check holds it to
    # the same rules.
    go test -c -ldflags="-s -w $static" -o "$out" "$PG_PKG"
    sh "$here/check-windows-release-binary.sh" "$out"
    ;;
  probe)
    [ $# -eq 3 ] || usage
    # Linked the way v3.6.216 was in the one respect that broke it: the
    # thread library comes from its DLL, libwinpthread-1.dll, which llvm-mingw
    # links by default. check-windows-release-start.ps1 must see it refused
    # to start, and check-windows-release-binary.sh must refuse it: so each
    # check is shown able to fail on the machine it passes on.
    src="$(mktemp -d)"
    trap 'rm -rf "$src"' EXIT
    printf '%s\n' '#include <pthread.h>' '#include <stdio.h>' \
      'int main(void) { printf("probe %lu\n", (unsigned long)pthread_self()); return 0; }' > "$src/probe.c"
    "$cc" -O1 -o "$out" "$src/probe.c" -lwinpthread
    ;;
  *) usage ;;
esac
