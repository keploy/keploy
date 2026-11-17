#!/bin/sh
# check-windows-release-binary.tests.sh - check-windows-release-binary.sh passes
# a binary linked as build-windows-release.sh links one, and refuses each way
# a windows release binary can be wrong, naming why.
#
# Builds small C programs with llvm-mingw (KEPLOY_LLVM_MINGW, as
# llvm-mingw-toolchain.sh stages it), so it runs wherever that does: the
# windows runner's Git Bash, or a linux machine with the linux-hosted one.
set -eu

: "${KEPLOY_LLVM_MINGW:?KEPLOY_LLVM_MINGW must name the pinned llvm-mingw (.github/scripts/windows/llvm-mingw-toolchain.sh)}"
here="$(cd "$(dirname "$0")" && pwd)"
check="$here/check-windows-release-binary.sh"
recipe="$here/build-windows-release.sh"
tc="$KEPLOY_LLVM_MINGW"
if command -v cygpath >/dev/null 2>&1; then tc="$(cygpath -m "$tc")"; fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf '%s\n' '#include <stdio.h>' 'int main(void) { puts("ok"); return 0; }' > "$work/hello.c"
printf '%s\n' '#include <pthread.h>' '#include <stdio.h>' \
  'int main(void) { printf("%lu\n", (unsigned long)pthread_self()); return 0; }' > "$work/thread.c"

amd() { t="$1"; shift; "$tc/bin/x86_64-w64-mingw32-clang" -O1 -o "$work/$t.exe" "$@"; }

# The C link flags the release links with (build-windows-release.sh's own),
# so the passing rows fail if those flags stop giving a binary that passes.
extld="$(sh "$recipe" extldflags)" || { echo "FAIL extldflags: build-windows-release.sh extldflags exited $?" >&2; exit 1; }
# As the release links, stripped (go build's -s).
# shellcheck disable=SC2086 # the flags are words
amd ok "$work/hello.c" -s $extld
# The C side calls into the thread library: with the release's flags it is
# linked statically, still only Windows' DLLs.
# shellcheck disable=SC2086
amd okthread "$work/thread.c" -s $extld -lwinpthread
# libwinpthread-1.dll imported, as v3.6.216 did.
amd dll "$work/thread.c" -s -Wl,--stack,2097152 -lwinpthread
# A gcc-compiled object's ident, as libgcc's carry.
cp "$work/ok.exe" "$work/gcc.exe"
printf 'GCC: (x86_64-posix-seh-rev1, Built by MinGW-Builds project) 15.2.0\n' >> "$work/gcc.exe"
# winlibs' gcc (the one the self-hosted windows runners carry) spells its
# ident another way.
cp "$work/ok.exe" "$work/gccwinlibs.exe"
printf 'GCC: (MinGW-W64 x86_64-ucrt-posix-seh, built by Brecht Sanders, r3) 14.2.0\n' >> "$work/gccwinlibs.exe"
# Idents counted one by one, not by line: three on one line, NUL-separated as
# a binary's .rdata holds them.
cp "$work/ok.exe" "$work/gcc3.exe"
printf 'GCC: (x86_64-posix-seh-rev1, Built by MinGW-Builds project) 15.2.0\000GCC: (x86_64-posix-seh-rev1, Built by MinGW-Builds project) 15.2.0\000GCC: (MinGW-W64 x86_64-ucrt-posix-seh, built by Brecht Sanders, r3) 14.2.0\n' >> "$work/gcc3.exe"
# Not stripped.
amd syms "$work/hello.c" -static -Wl,--stack,2097152
# lld's own 1 MiB stack reserve, and one byte under the pin.
amd stack1m "$work/hello.c" -static -s
amd stackunder "$work/hello.c" -static -s -Wl,--stack,2097151
# windows/arm64.
"$tc/bin/aarch64-w64-mingw32-clang" -O1 -o "$work/arm64.exe" "$work/hello.c" -static -s -Wl,--stack,2097152
# Not a PE at all.
printf 'not a binary\n' > "$work/text.exe"

# A stand-in llvm-readobj whose output lacks what the check reads, as a
# changed llvm-readobj's could: the check must refuse, not pass for want of
# reading.
mkdir -p "$work/stub/bin"
cat > "$work/stub/bin/llvm-readobj" <<'EOF'
#!/bin/sh
case "$1" in
  --file-headers) echo "  Machine: IMAGE_FILE_MACHINE_AMD64"; [ -n "${STUB_SYMS:-}" ] && echo "  SymbolCount: 0"; [ -n "${STUB_STACK:-}" ] && echo "  SizeOfStackReserve: $STUB_STACK"; exit 0 ;;
  --coff-imports) [ -n "${STUB_IMPORTS_FAIL:-}" ] && exit 1; for d in ${STUB_DLL:-}; do printf 'Import {\n  Name: %s\n}\n' "$d"; done; exit 0 ;;
esac
exit 1
EOF
chmod +x "$work/stub/bin/llvm-readobj"

fails=0
# expect NAME RC WANT ENV... : run the check on $work/NAME.exe (with ENV), want
# exit RC and, when WANT is not empty, WANT in what it printed.
expect() {
  name="$1"; want_rc="$2"; want="$3"; shift 3
  rc=0
  out="$(env "$@" sh "$check" "$work/$name.exe" 2>&1)" || rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL $name: exit $rc, want $want_rc: $out"; fails=$((fails + 1)); return 0
  fi
  case "$out" in
    *"$want"*) echo "ok   $name: $out" ;;
    *) echo "FAIL $name: printed '$out', want '$want'"; fails=$((fails + 1)) ;;
  esac
}

expect ok 0 "imports only KERNEL32.dll api-ms-win-crt-"
expect okthread 0 "no gcc runtime, stripped, stack reserve 2097152"
expect dll 1 "imports DLLs Windows does not ship: libwinpthread-1.dll"
expect gcc 1 "carries objects compiled by gcc (1 'GCC: (' idents)"
expect gccwinlibs 1 "carries objects compiled by gcc (1 'GCC: (' idents)"
expect gcc3 1 "carries objects compiled by gcc (3 'GCC: (' idents)"
expect syms 1 "is not stripped (SymbolCount: "
expect stack1m 1 "has a 1048576-byte stack reserve, under the 2 MiB pin"
expect stackunder 1 "has a 2097151-byte stack reserve, under the 2 MiB pin"
expect arm64 1 "not a windows/amd64 PE binary"
expect text 1 "llvm-readobj cannot read it as a PE binary"
expect ok 1 "imports no DLL at all" KEPLOY_LLVM_MINGW="$work/stub"
expect ok 1 "imports DLLs Windows does not ship: msvcrt.dll" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=msvcrt.dll
expect ok 1 "is not stripped (SymbolCount: unread)" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=KERNEL32.dll
expect ok 1 "its stack reserve cannot be read (SizeOfStackReserve: '')" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=KERNEL32.dll STUB_SYMS=1
expect ok 1 "its stack reserve cannot be read (SizeOfStackReserve: '2M')" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=kernel32.DLL STUB_SYMS=1 STUB_STACK=2M
expect ok 0 "imports only kernel32.DLL" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=kernel32.DLL STUB_SYMS=1 STUB_STACK=2097152
expect ok 1 "imports DLLs Windows does not ship: api-ms-win-crt-runtime-l1-1-0.dll.bak.dll" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL=api-ms-win-crt-runtime-l1-1-0.dll.bak.dll STUB_SYMS=1 STUB_STACK=2097152
expect ok 1 "llvm-readobj cannot read its import table" KEPLOY_LLVM_MINGW="$work/stub" STUB_IMPORTS_FAIL=1
# Each DLL named once, sorted: the import and delay-import tables can both
# name one.
expect ok 0 "imports only KERNEL32.dll api-ms-win-crt-heap-l1-1-0.dll, no gcc runtime" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL="api-ms-win-crt-heap-l1-1-0.dll KERNEL32.dll KERNEL32.dll" STUB_SYMS=1 STUB_STACK=2097152
# Only the Universal CRT's api sets at version l1-1-0, as Windows 10 has them.
expect ok 1 "imports DLLs Windows does not ship: api-ms-win-core-synch-l1-1-0.dll" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL="KERNEL32.dll api-ms-win-core-synch-l1-1-0.dll" STUB_SYMS=1 STUB_STACK=2097152
expect ok 1 "imports DLLs Windows does not ship: api-ms-win-crt-runtime-l1-2-0.dll" KEPLOY_LLVM_MINGW="$work/stub" STUB_DLL="KERNEL32.dll api-ms-win-crt-runtime-l1-2-0.dll" STUB_SYMS=1 STUB_STACK=2097152
expect missing 2 "no such file"

# usage: exactly one BINARY.
for args in "" "$work/ok.exe $work/ok.exe"; do
  rc=0
  # shellcheck disable=SC2086 # the arguments are words
  out="$(sh "$check" $args 2>&1)" || rc=$?
  case "$rc:$out" in
    "2:usage: check-windows-release-binary.sh BINARY") echo "ok   usage ($args): $out" ;;
    *) echo "FAIL usage ($args): exit $rc: $out"; fails=$((fails + 1)) ;;
  esac
done

if [ "$fails" -ne 0 ]; then
  echo "check-windows-release-binary.tests: $fails FAILED" >&2
  exit 1
fi
echo "check-windows-release-binary.tests: all passed"
