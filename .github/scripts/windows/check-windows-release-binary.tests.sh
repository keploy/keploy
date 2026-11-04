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
tc="$KEPLOY_LLVM_MINGW"
if command -v cygpath >/dev/null 2>&1; then tc="$(cygpath -m "$tc")"; fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf '%s\n' '#include <stdio.h>' 'int main(void) { puts("ok"); return 0; }' > "$work/hello.c"
printf '%s\n' '#include <pthread.h>' '#include <stdio.h>' \
  'int main(void) { printf("%lu\n", (unsigned long)pthread_self()); return 0; }' > "$work/thread.c"

amd() { t="$1"; shift; "$tc/bin/x86_64-w64-mingw32-clang" -O1 -o "$work/$t.exe" "$@"; }

# As the release links: static, stripped, 2 MiB stack.
amd ok "$work/hello.c" -static -s -Wl,--stack,2097152
# The C side calls into the thread library, linked statically: still only
# Windows' DLLs.
amd okthread "$work/thread.c" -static -s -Wl,--stack,2097152 -lwinpthread
# libwinpthread-1.dll imported, as v3.6.216 did.
amd dll "$work/thread.c" -s -Wl,--stack,2097152 -lwinpthread
# A gcc-compiled object's ident, as libgcc's carry.
cp "$work/ok.exe" "$work/gcc.exe"
printf 'GCC: (x86_64-posix-seh-rev1, Built by MinGW-Builds project) 15.2.0\n' >> "$work/gcc.exe"
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
  --coff-imports) [ -n "${STUB_DLL:-}" ] && printf 'Import {\n  Name: %s\n}\n' "$STUB_DLL"; exit 0 ;;
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
expect missing 2 "no such file"

if [ "$fails" -ne 0 ]; then
  echo "check-windows-release-binary.tests: $fails FAILED" >&2
  exit 1
fi
echo "check-windows-release-binary.tests: all passed"
